package jetkvm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// deviceRPCChannel wires up peer b's side of an "rpc" data channel to
// behave like jsonrpc.go's onRPCMessage/writeJSONRPCResponse: it decodes
// each request, and for method "ping" replies with a fixed result; for
// method "echo" it replies with the params it received; for method
// "boom" it replies with a JSON-RPC error object. This is enough surface
// to test rpcClient's request/response correlation and error handling
// without a real device.
func deviceRPCChannel(t *testing.T, pc *webrtc.PeerConnection) chan *webrtc.DataChannel {
	t.Helper()
	ch := make(chan *webrtc.DataChannel, 1)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "rpc" {
			return
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			var req rpcRequest
			if err := json.Unmarshal(msg.Data, &req); err != nil {
				return
			}
			switch req.Method {
			case "ping":
				resp := rpcResponse{JSONRPC: "2.0", Result: json.RawMessage(`"pong"`), ID: json.Number(itoa(req.ID))}
				b, _ := json.Marshal(resp)
				_ = dc.SendText(string(b))
			case "echo":
				paramsJSON, _ := json.Marshal(req.Params)
				resp := rpcResponse{JSONRPC: "2.0", Result: paramsJSON, ID: json.Number(itoa(req.ID))}
				b, _ := json.Marshal(resp)
				_ = dc.SendText(string(b))
			case "boom":
				resp := rpcResponse{JSONRPC: "2.0", Error: json.RawMessage(`{"code":-32000,"message":"boom failed"}`), ID: json.Number(itoa(req.ID))}
				b, _ := json.Marshal(resp)
				_ = dc.SendText(string(b))
			}
		})
		ch <- dc
	})
	return ch
}

func itoa(id int64) string {
	b, _ := json.Marshal(id)
	return string(b)
}

type fakeRPCDataChannel struct {
	buffered         uint64
	bufferedCalls    int
	sent             []string
	onBufferedAmount func()
	onSend           func(string)
	sendErr          error
}

func (c *fakeRPCDataChannel) BufferedAmount() uint64 {
	c.bufferedCalls++
	if c.onBufferedAmount != nil {
		c.onBufferedAmount()
	}
	return c.buffered
}

func (c *fakeRPCDataChannel) SendText(frame string) error {
	c.sent = append(c.sent, frame)
	if c.onSend != nil {
		c.onSend(frame)
	}
	return c.sendErr
}

func respondingRPCClient(t *testing.T, channel *fakeRPCDataChannel) *rpcClient {
	t.Helper()
	rpc := newRPCClientWithChannel(channel)
	channel.onSend = func(frame string) {
		var req rpcRequest
		if err := json.Unmarshal([]byte(frame), &req); err != nil {
			t.Fatalf("decoding fake RPC request: %v", err)
		}
		response, err := json.Marshal(rpcResponse{
			JSONRPC: "2.0",
			Result:  json.RawMessage(`null`),
			ID:      json.Number(itoa(req.ID)),
		})
		if err != nil {
			t.Fatalf("encoding fake RPC response: %v", err)
		}
		rpc.handleMessage(response)
	}
	return rpc
}

func TestRPCClientCallRejectsPreCanceledContextBeforeAnySendWork(t *testing.T) {
	channel := &fakeRPCDataChannel{}
	rpc := newRPCClientWithChannel(channel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := rpc.call(ctx, "ping", nil, nil)
	if kind := ErrorKindOf(err); kind != ErrorKindTimeout {
		t.Fatalf("pre-canceled call error kind = %q, want %q: %v", kind, ErrorKindTimeout, err)
	}
	if channel.bufferedCalls != 0 || len(channel.sent) != 0 {
		t.Fatalf("pre-canceled call touched channel: buffered calls=%d sends=%d", channel.bufferedCalls, len(channel.sent))
	}
	if rpc.nextID != 0 || len(rpc.pending) != 0 {
		t.Fatalf("pre-canceled call mutated RPC state: nextID=%d pending=%d", rpc.nextID, len(rpc.pending))
	}
}

func TestRPCClientCallRechecksContextImmediatelyBeforeSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	channel := &fakeRPCDataChannel{onBufferedAmount: cancel}
	rpc := newRPCClientWithChannel(channel)

	err := rpc.call(ctx, "ping", nil, nil)
	if kind := ErrorKindOf(err); kind != ErrorKindTimeout {
		t.Fatalf("send-boundary cancellation error kind = %q, want %q: %v", kind, ErrorKindTimeout, err)
	}
	if len(channel.sent) != 0 {
		t.Fatalf("send-boundary cancellation sent %d frames, want none", len(channel.sent))
	}
	if len(rpc.pending) != 0 {
		t.Fatalf("send-boundary cancellation left %d pending calls", len(rpc.pending))
	}
}

func TestRPCClientCapsBufferedAmountPlusFrame(t *testing.T) {
	request, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "ping", ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	frameBytes := uint64(len(request))

	t.Run("exact limit is accepted", func(t *testing.T) {
		channel := &fakeRPCDataChannel{buffered: maxRPCBufferedAmount - frameBytes}
		rpc := respondingRPCClient(t, channel)
		if err := rpc.call(context.Background(), "ping", nil, nil); err != nil {
			t.Fatalf("call at exact buffered limit: %v", err)
		}
		if len(channel.sent) != 1 {
			t.Fatalf("calls sent = %d, want 1", len(channel.sent))
		}
	})

	t.Run("one byte over is rejected", func(t *testing.T) {
		channel := &fakeRPCDataChannel{buffered: maxRPCBufferedAmount - frameBytes + 1}
		rpc := newRPCClientWithChannel(channel)
		err := rpc.call(context.Background(), "ping", nil, nil)
		if !errors.Is(err, ErrRPCBufferFull) {
			t.Fatalf("call over buffered limit = %v, want ErrRPCBufferFull", err)
		}
		if len(channel.sent) != 0 {
			t.Fatalf("over-limit call sent %d frames, want none", len(channel.sent))
		}
	})

	t.Run("single oversized frame is rejected", func(t *testing.T) {
		channel := &fakeRPCDataChannel{}
		rpc := newRPCClientWithChannel(channel)
		err := rpc.call(context.Background(), "echo", map[string]any{
			"payload": strings.Repeat("x", int(maxRPCBufferedAmount)),
		}, nil)
		if !errors.Is(err, ErrRPCBufferFull) {
			t.Fatalf("oversized RPC frame = %v, want ErrRPCBufferFull", err)
		}
		if len(channel.sent) != 0 {
			t.Fatalf("oversized call sent %d frames, want none", len(channel.sent))
		}
	})
}

func TestRPCClientCallSuccess(t *testing.T) {
	pair := newPeerPair(t)
	deviceCh := deviceRPCChannel(t, pair.b)

	clientDC, err := pair.a.CreateDataChannel("rpc", nil)
	if err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	pair.connect(t)

	ctx := contextWithTimeout(t, 10*time.Second)
	waitDataChannelOpen(t, ctx, clientDC)
	<-deviceCh // ensure the device side has registered its handler

	rpc := newRPCClient(clientDC)

	var result string
	if err := rpc.call(ctx, "ping", nil, &result); err != nil {
		t.Fatalf("call(ping) failed: %v", err)
	}
	if result != "pong" {
		t.Errorf("result = %q, want pong", result)
	}
}

func TestRPCClientCallWithParams(t *testing.T) {
	pair := newPeerPair(t)
	deviceCh := deviceRPCChannel(t, pair.b)

	clientDC, err := pair.a.CreateDataChannel("rpc", nil)
	if err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	pair.connect(t)

	ctx := contextWithTimeout(t, 10*time.Second)
	waitDataChannelOpen(t, ctx, clientDC)
	<-deviceCh

	rpc := newRPCClient(clientDC)

	var echoed map[string]any
	if err := rpc.call(ctx, "echo", map[string]any{"x": float64(42)}, &echoed); err != nil {
		t.Fatalf("call(echo) failed: %v", err)
	}
	if echoed["x"] != float64(42) {
		t.Errorf("echoed x = %v, want 42", echoed["x"])
	}
}

func TestRPCClientCallReturnsDeviceError(t *testing.T) {
	pair := newPeerPair(t)
	deviceCh := deviceRPCChannel(t, pair.b)

	clientDC, err := pair.a.CreateDataChannel("rpc", nil)
	if err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	pair.connect(t)

	ctx := contextWithTimeout(t, 10*time.Second)
	waitDataChannelOpen(t, ctx, clientDC)
	<-deviceCh

	rpc := newRPCClient(clientDC)

	err = rpc.call(ctx, "boom", nil, nil)
	if err == nil {
		t.Fatal("expected an error from the boom method")
	}
	rpcErr, ok := err.(*RPCError)
	if !ok {
		t.Fatalf("expected *RPCError, got %T: %v", err, err)
	}
	if rpcErr.Method != "boom" {
		t.Errorf("method = %q, want boom", rpcErr.Method)
	}
}

func TestRPCClientCallTimesOutWithoutResponse(t *testing.T) {
	pair := newPeerPair(t)
	// No device-side handler at all: nothing ever replies.
	pair.b.OnDataChannel(func(dc *webrtc.DataChannel) {})

	clientDC, err := pair.a.CreateDataChannel("rpc", nil)
	if err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	pair.connect(t)

	openCtx := contextWithTimeout(t, 10*time.Second)
	waitDataChannelOpen(t, openCtx, clientDC)

	rpc := newRPCClient(clientDC)

	callCtx := contextWithTimeout(t, 300*time.Millisecond)
	err = rpc.call(callCtx, "ping", nil, nil)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestRPCClientDeliversEvents(t *testing.T) {
	pair := newPeerPair(t)
	pair.b.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			ev, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"method":  "videoStateUpdate",
				"params":  map[string]any{"ready": true},
			})
			_ = dc.SendText(string(ev))
		})
	})

	clientDC, err := pair.a.CreateDataChannel("rpc", nil)
	if err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}

	// Register the RPC client (and its event handler) before connecting:
	// the fake device pushes its event as soon as its side of the channel
	// opens, which can race the client's own open callback, so the message
	// handler must already be in place before negotiation completes.
	rpc := newRPCClient(clientDC)
	events := make(chan string, 1)
	rpc.onEvent = func(method string, params json.RawMessage) {
		events <- method
	}

	pair.connect(t)

	ctx := contextWithTimeout(t, 10*time.Second)
	waitDataChannelOpen(t, ctx, clientDC)

	select {
	case method := <-events:
		if method != "videoStateUpdate" {
			t.Errorf("event method = %q, want videoStateUpdate", method)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event delivery")
	}
}
