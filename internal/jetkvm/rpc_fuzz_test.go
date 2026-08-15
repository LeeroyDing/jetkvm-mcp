package jetkvm

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// FuzzRPCHandleMessage feeds arbitrary data-channel frames to the RPC
// decoder exactly as webrtc's OnMessage would deliver them. Properties
// pinned:
//
//   - no panic on any frame (malformed JSON, wrong shapes, huge numbers);
//   - a frame larger than maxRPCFrameBytes always fails pending calls with
//     ErrorKindBadFrame and is never parsed;
//   - a delivered response always carries the pending call's ID;
//   - an answered call never remains in the pending map;
//   - the client stays usable after any frame: a subsequent well-formed
//     response for a newly registered call is always delivered.
//
// Seeds mirror the mock-harness reliability cases (fakedevice_test.go's
// fakeRPCTruncated/fakeRPCOversized modes, ping responses, RPC error
// objects, and unsolicited events). Under plain `go test ./...` only these
// seeds run, so this is CI-safe.
func FuzzRPCHandleMessage(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"jsonrpc":"2.0","result":"pong","id":1}`),
		[]byte(`{"jsonrpc":"2.0","result":`), // fakeRPCTruncated harness frame
		[]byte(`{"jsonrpc":"2.0","result":{"deviceId":"fuzz","authMode":"password"},"id":1}`),
		[]byte(`{"jsonrpc":"2.0","error":{"code":-32601,"message":"Method not found"},"id":1}`),
		[]byte(`{"jsonrpc":"2.0","method":"videoInputState","params":{"ready":true}}`), // event, no id
		[]byte(`{"jsonrpc":"2.0","result":"pong","id":"1"}`),                           // string id decodes via json.Number
		[]byte(`{"jsonrpc":"2.0","result":"pong","id":1.5}`),                           // non-integer id
		[]byte(`{"jsonrpc":"2.0","result":"pong","id":1e300}`),                         // overflowing id
		[]byte(`{"id":null}`),
		[]byte(`null`),
		[]byte(`[]`),
		[]byte(``),
		[]byte("\x00\x01\x02"),
		// fakeRPCOversized harness frame: over the 64 KiB cap.
		[]byte(`{"jsonrpc":"2.0","result":"` + strings.Repeat("x", 70<<10) + `","id":1}`),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		c := &rpcClient{pending: make(map[int64]chan rpcCallResult)}
		c.onEvent = func(method string, params json.RawMessage) {
			// Events must only be delivered for frames without an "id".
			var probe struct {
				ID *json.Number `json:"id"`
			}
			if err := json.Unmarshal(data, &probe); err != nil || probe.ID != nil {
				t.Fatalf("event handler invoked for non-event frame: %q", data)
			}
		}

		ch := make(chan rpcCallResult, 1)
		c.pending[1] = ch

		c.handleMessage(data)

		var got *rpcCallResult
		select {
		case r := <-ch:
			got = &r
		default:
		}

		if len(data) > maxRPCFrameBytes {
			// The cap must reject before parsing: pending calls fail with
			// bad-frame no matter what the bytes contain.
			if got == nil || got.err == nil {
				t.Fatalf("oversized frame (%d bytes) did not fail the pending call", len(data))
			}
			var de *DeviceError
			if !errors.As(got.err, &de) || de.Kind != ErrorKindBadFrame {
				t.Fatalf("oversized frame failed with %v, want %s", got.err, ErrorKindBadFrame)
			}
		}

		if got != nil {
			if got.err == nil {
				// A successful delivery must be the response to our call.
				id, err := strconv.ParseInt(got.response.ID.String(), 10, 64)
				if err != nil || id != 1 {
					t.Fatalf("delivered response ID %q is not the pending call's", got.response.ID)
				}
			}
			c.mu.Lock()
			_, still := c.pending[1]
			c.mu.Unlock()
			if still {
				t.Fatal("answered call still present in pending map")
			}
		}

		// Whatever the frame did, the client must remain usable.
		probe := make(chan rpcCallResult, 1)
		c.mu.Lock()
		c.pending[2] = probe
		c.mu.Unlock()
		c.handleMessage([]byte(`{"jsonrpc":"2.0","result":"pong","id":2}`))
		select {
		case r := <-probe:
			if r.err != nil {
				t.Fatalf("client wedged after fuzz frame: follow-up call failed: %v", r.err)
			}
		default:
			t.Fatal("client wedged after fuzz frame: well-formed response not delivered")
		}
	})
}
