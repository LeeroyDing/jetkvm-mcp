package jetkvm

import (
	"context"
	"encoding/json"
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

// rpcClient sends JSON-RPC 2.0 requests over the "rpc" data channel and
// correlates responses by ID. Firmware evidence: jsonrpc.go's
// onRPCMessage/writeJSONRPCResponse.
type rpcClient struct {
	channel *webrtc.DataChannel

	nextID  int64
	mu      sync.Mutex
	pending map[int64]chan rpcCallResult

	onEvent func(method string, params json.RawMessage)
}

type rpcCallResult struct {
	response rpcResponse
	err      error
}

const maxRPCFrameBytes = 64 << 10

func newRPCClient(channel *webrtc.DataChannel) *rpcClient {
	c := &rpcClient{
		channel: channel,
		pending: make(map[int64]chan rpcCallResult),
	}
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

// RPCError represents a JSON-RPC error object returned by the device.
type RPCError struct {
	Method string
	Raw    json.RawMessage
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("jetkvm: RPC method %q returned an error: %s", e.Method, truncate(string(e.Raw), 300))
}

// call sends a JSON-RPC request and blocks for the matching response,
// decoding its result into out (if non-nil) or returning *RPCError if the
// device reported an error.
func (c *rpcClient) call(ctx context.Context, method string, params map[string]any, out any) error {
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

	if err := c.channel.SendText(string(b)); err != nil {
		return newDeviceError(ErrorKindUnreachable, "sending RPC request "+method, err)
	}

	select {
	case callResult := <-ch:
		if callResult.err != nil {
			return callResult.err
		}
		resp := callResult.response
		if len(resp.Error) > 0 && string(resp.Error) != "null" {
			return &RPCError{Method: method, Raw: resp.Error}
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return newDeviceError(ErrorKindBadFrame, "decoding RPC result for "+method, err)
			}
		}
		return nil
	case <-ctx.Done():
		return timeoutError("waiting for RPC response to "+method, ctx.Err())
	}
}
