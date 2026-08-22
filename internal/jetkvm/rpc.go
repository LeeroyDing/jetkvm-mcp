package jetkvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/pion/webrtc/v4"
)

// rpcRequest / rpcResponse mirror jsonrpc.go's JSONRPCRequest/JSONRPCResponse
// on the "rpc" WebRTC data channel: a standard JSON-RPC 2.0 shape, matched
// by numeric ID. The device also sends unsolicited JSONRPCEvent messages
// (method+params, no id) on the same channel; those are delivered to any
// registered event handler instead of the pending-request map.
type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
	ID      int64          `json:"id"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
	ID      json.Number     `json:"id"`
}

type rpcEvent struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// rpcDataChannel is the outbound subset of Pion's DataChannel used by calls.
// Keeping this boundary minimal makes the buffered-amount gate deterministic
// under test without replacing the real callback wiring in newRPCClient.
type rpcDataChannel interface {
	SendText(string) error
	BufferedAmount() uint64
}

// rpcClient sends JSON-RPC 2.0 requests over the "rpc" data channel and
// correlates responses by ID. Firmware evidence: jsonrpc.go's
// onRPCMessage/writeJSONRPCResponse.
type rpcClient struct {
	channel rpcDataChannel
	sendMu  sync.Mutex

	nextID  int64
	mu      sync.Mutex
	pending map[int64]chan rpcCallResult

	onEvent func(method string, params json.RawMessage)
}

type rpcCallResult struct {
	response rpcResponse
	err      error
}

var ErrRPCBufferFull = errors.New("jetkvm: outbound RPC buffer is full; request was not sent")

// errRPCAmbiguousDelivery marks an RPC call that reached SendText (or completed
// it) but whose matching response was never observed. Read-only callers may
// simply report the failure. Scroll must additionally retire the session
// before releasing its HID lease because wheelReport travels on a different
// channel and cannot carry the lease generation token.
var errRPCAmbiguousDelivery = errors.New("jetkvm: RPC request delivery is ambiguous because no matching response was received")

const (
	maxRPCFrameBytes = 64 << 10

	// maxRPCBufferedAmount caps the application bytes this client will allow
	// Pion/SCTP to retain for the RPC data channel. Sixty-four KiB admits one
	// maximum-size protocol frame (routine ping and wheel requests are under a
	// hundred bytes) while preventing a stalled peer from accumulating an
	// unbounded queue. sendMu makes the BufferedAmount-plus-frame check atomic
	// with respect to every local RPC sender.
	maxRPCBufferedAmount uint64 = 64 << 10
)

func newRPCClient(channel *webrtc.DataChannel) *rpcClient {
	c := newRPCClientWithChannel(channel)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		c.handleMessage(msg.Data)
	})
	channel.OnClose(func() {
		c.failPending(newDeviceError(ErrorKindUnreachable, "reading RPC response", fmt.Errorf("RPC data channel closed")))
	})
	channel.OnError(func(err error) {
		c.failPending(newDeviceError(ErrorKindUnreachable, "reading RPC response", err))
	})
	return c
}

func newRPCClientWithChannel(channel rpcDataChannel) *rpcClient {
	c := &rpcClient{
		channel: channel,
		pending: make(map[int64]chan rpcCallResult),
	}
	return c
}

func (c *rpcClient) handleMessage(data []byte) {
	if len(data) > maxRPCFrameBytes {
		c.failPending(&DeviceError{
			Kind:      ErrorKindBadFrame,
			Operation: "reading RPC response",
			Detail:    fmt.Sprintf("frame exceeded %d-byte limit", maxRPCFrameBytes),
		})
		return
	}

	// Responses have an "id"; events don't. Peek generically first.
	var probe struct {
		ID *json.Number `json:"id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		c.failPending(newDeviceError(ErrorKindBadFrame, "decoding RPC frame", err))
		return
	}
	if probe.ID == nil {
		var ev rpcEvent
		if err := json.Unmarshal(data, &ev); err == nil && c.onEvent != nil {
			c.onEvent(ev.Method, ev.Params)
		}
		return
	}

	var resp rpcResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		c.failPending(newDeviceError(ErrorKindBadFrame, "decoding RPC response", err))
		return
	}
	id, err := strconv.ParseInt(resp.ID.String(), 10, 64)
	if err != nil {
		c.failPending(newDeviceError(ErrorKindBadFrame, "decoding RPC response ID", err))
		return
	}

	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()

	if ok {
		ch <- rpcCallResult{response: resp}
	}
}

func (c *rpcClient) failPending(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan rpcCallResult)
	c.mu.Unlock()

	for _, ch := range pending {
		ch <- rpcCallResult{err: err}
	}
}

// RPCError represents a JSON-RPC error returned by the device. Only its
// numeric code is retained; the remote message and data fields are
// device-controlled and may contain credentials or private diagnostics.
type RPCError struct {
	Method string
	Code   *int64
}

func (e *RPCError) Error() string {
	if e.Code != nil {
		return fmt.Sprintf("jetkvm: RPC method %q returned error code %d", e.Method, *e.Code)
	}
	return fmt.Sprintf("jetkvm: RPC method %q returned an error", e.Method)
}

// call sends a JSON-RPC request and blocks for the matching response,
// decoding its result into out (if non-nil) or returning *RPCError if the
// device reported an error.
func (c *rpcClient) call(ctx context.Context, method string, params map[string]any, out any) error {
	if err := ctx.Err(); err != nil {
		return timeoutError("sending RPC request "+method, err)
	}

	id := atomic.AddInt64(&c.nextID, 1)
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params, ID: id}

	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("jetkvm: encoding RPC request %q: %w", method, err)
	}

	ch := make(chan rpcCallResult, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.send(ctx, method, b); err != nil {
		if ErrorKindOf(err) == ErrorKindUnreachable {
			// SendText was reached, so delivery remains ambiguous even when
			// cancellation raced its transport failure. Keep the caller's
			// deadline authoritative, matching the response-side boundary
			// below, without pretending the request was never accepted.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return errors.Join(
					timeoutError("sending RPC request "+method, ctxErr),
					errRPCAmbiguousDelivery,
				)
			}
			return errors.Join(err, errRPCAmbiguousDelivery)
		}
		return err
	}

	select {
	case callResult := <-ch:
		if callResult.err != nil {
			// Channel failure and cancellation can become observable together.
			// Cancellation wins the public taxonomy, while the ambiguous marker
			// remains because no matching response was received.
			if err := ctx.Err(); err != nil {
				return errors.Join(
					timeoutError("waiting for RPC response to "+method, err),
					errRPCAmbiguousDelivery,
				)
			}
			return errors.Join(callResult.err, errRPCAmbiguousDelivery)
		}
		// A matching response and ctx.Done can become ready together. The
		// select may choose this arm, so make the caller's cancellation
		// authoritative before accepting any device result. Because the
		// matching response was observed, delivery is no longer ambiguous.
		if err := ctx.Err(); err != nil {
			return timeoutError("processing RPC response to "+method, err)
		}
		resp := callResult.response
		if len(resp.Error) > 0 && string(resp.Error) != "null" {
			remote := struct {
				Code *int64 `json:"code"`
			}{}
			// Keep malformed or non-standard error objects caller-safe too: the
			// method remains actionable even when no valid numeric code exists.
			_ = json.Unmarshal(resp.Error, &remote)
			if err := ctx.Err(); err != nil {
				return timeoutError("processing RPC response to "+method, err)
			}
			return &RPCError{Method: method, Code: remote.Code}
		}
		if out != nil && len(resp.Result) > 0 {
			decodeErr := json.Unmarshal(resp.Result, out)
			if err := ctx.Err(); err != nil {
				return timeoutError("processing RPC response to "+method, err)
			}
			if decodeErr != nil {
				return newDeviceError(ErrorKindBadFrame, "decoding RPC result for "+method, decodeErr)
			}
		}
		if err := ctx.Err(); err != nil {
			return timeoutError("processing RPC response to "+method, err)
		}
		return nil
	case <-ctx.Done():
		return errors.Join(
			timeoutError("waiting for RPC response to "+method, ctx.Err()),
			errRPCAmbiguousDelivery,
		)
	}
}

// send serializes the lower-layer buffered-amount check with SendText. It is
// deliberately separate from response waiting: one slow RPC response must not
// prevent a different request from reaching the peer.
func (c *rpcClient) send(ctx context.Context, method string, frame []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	buffered := c.channel.BufferedAmount()
	frameBytes := uint64(len(frame))
	// Re-check after inspecting Pion's buffer and immediately before SendText:
	// cancellation that wins this boundary must leave no ambiguous RPC bytes.
	if err := ctx.Err(); err != nil {
		return timeoutError("sending RPC request "+method, err)
	}
	if frameBytes > maxRPCBufferedAmount || buffered > maxRPCBufferedAmount-frameBytes {
		return fmt.Errorf("%w (buffered=%d frame=%d limit=%d)",
			ErrRPCBufferFull, buffered, frameBytes, maxRPCBufferedAmount)
	}
	if err := c.channel.SendText(string(frame)); err != nil {
		return newDeviceError(ErrorKindUnreachable, "sending RPC request "+method, err)
	}
	return nil
}
