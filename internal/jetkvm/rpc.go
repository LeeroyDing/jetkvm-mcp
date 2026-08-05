package jetkvm

import (
	"context"
	"encoding/json"
	"fmt"
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
	pending map[int64]chan rpcResponse

	onEvent func(method string, params json.RawMessage)
}

func newRPCClient(channel *webrtc.DataChannel) *rpcClient {
	c := &rpcClient{
		channel: channel,
		pending: make(map[int64]chan rpcResponse),
	}
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		c.handleMessage(msg.Data)
	})
	return c
}

func (c *rpcClient) handleMessage(data []byte) {
	// Responses have an "id"; events don't. Peek generically first.
	var probe struct {
		ID *json.Number `json:"id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
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
		return
	}
	var id int64
	if _, err := fmt.Sscanf(resp.ID.String(), "%d", &id); err != nil {
		return
	}

	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()

	if ok {
		ch <- resp
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

	ch := make(chan rpcResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.channel.SendText(string(b)); err != nil {
		return fmt.Errorf("jetkvm: sending RPC request %q: %w", method, err)
	}

	select {
	case resp := <-ch:
		if len(resp.Error) > 0 && string(resp.Error) != "null" {
			return &RPCError{Method: method, Raw: resp.Error}
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("jetkvm: decoding result of %q: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("jetkvm: RPC request %q: %w", method, ctx.Err())
	}
}
