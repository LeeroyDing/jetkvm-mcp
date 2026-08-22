package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func TestCoverageToolHandlersReturnOperationalFailuresAsToolErrors(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 4, 3)

	tests := []struct {
		name       string
		tool       string
		args       map[string]any
		wantText   []string
		newSession func(*testing.T) (*mcp.ClientSession, func(*testing.T))
	}{
		{
			name:     "screenshot capture failure",
			tool:     "jetkvm_screenshot",
			wantText: []string{"synthetic screenshot capture failure"},
			newSession: func(t *testing.T) (*mcp.ClientSession, func(*testing.T)) {
				captures := 0
				dev := &mockDevice{screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
					captures++
					return jetkvm.Screenshot{}, errors.New("synthetic screenshot capture failure")
				}}
				return newTestServerSessionForDevice(t, dev, false), func(t *testing.T) {
					t.Helper()
					if captures != 1 {
						t.Errorf("screenshot captures = %d, want 1", captures)
					}
				}
			},
		},
		{
			name:     "read text capture failure",
			tool:     "jetkvm_read_text",
			wantText: []string{"synthetic read-text capture failure"},
			newSession: func(t *testing.T) (*mcp.ClientSession, func(*testing.T)) {
				captures := 0
				dev := &mockDevice{screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
					captures++
					return jetkvm.Screenshot{}, errors.New("synthetic read-text capture failure")
				}}
				engine := &recordingOCREngine{}
				return newReadTextToolTestSession(t, dev, engine, false), func(t *testing.T) {
					t.Helper()
					if captures != 1 || engine.checkCalls != 1 || engine.readCalls != 0 {
						t.Errorf("capture/check/read calls = %d/%d/%d, want 1/1/0", captures, engine.checkCalls, engine.readCalls)
					}
				}
			},
		},
		{
			name:     "read text OCR failure",
			tool:     "jetkvm_read_text",
			wantText: []string{"synthetic read-text OCR failure"},
			newSession: func(t *testing.T) (*mcp.ClientSession, func(*testing.T)) {
				captures := 0
				dev := &mockDevice{screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
					captures++
					return shot, nil
				}}
				engine := &recordingOCREngine{readErr: errors.New("synthetic read-text OCR failure")}
				return newReadTextToolTestSession(t, dev, engine, false), func(t *testing.T) {
					t.Helper()
					if captures != 1 || engine.checkCalls != 1 || engine.readCalls != 1 {
						t.Errorf("capture/check/read calls = %d/%d/%d, want 1/1/1", captures, engine.checkCalls, engine.readCalls)
					}
				}
			},
		},
		{
			name:     "wait for text OCR availability failure preserves observations",
			tool:     "jetkvm_wait_for_text",
			args:     map[string]any{"text": "ready"},
			wantText: []string{"synthetic wait-for-text OCR failure", "matched=false", "frameCount=0"},
			newSession: func(t *testing.T) (*mcp.ClientSession, func(*testing.T)) {
				captures := 0
				dev := &mockDevice{screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
					captures++
					return shot, nil
				}}
				engine := &recordingOCREngine{checkErr: errors.New("synthetic wait-for-text OCR failure")}
				return newReadTextToolTestSession(t, dev, engine, true), func(t *testing.T) {
					t.Helper()
					if captures != 0 || engine.checkCalls != 1 || engine.readCalls != 0 {
						t.Errorf("capture/check/read calls = %d/%d/%d, want 0/1/0", captures, engine.checkCalls, engine.readCalls)
					}
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs, verify := tc.newSession(t)
			params := &mcp.CallToolParams{Name: tc.tool}
			if tc.args != nil {
				params.Arguments = tc.args
			}
			result, err := cs.CallTool(context.Background(), params)
			if err != nil {
				t.Fatalf("CallTool returned protocol error: %v", err)
			}
			if result == nil || !result.IsError {
				t.Fatalf("operational failure result = %+v, want MCP tool error", result)
			}
			text := toolResultTextTB(t, result)
			for _, want := range tc.wantText {
				if !strings.Contains(text, want) {
					t.Errorf("tool error %q does not contain %q", text, want)
				}
			}
			verify(t)
		})
	}
}

func TestCoverageControlGatesRemainUncallableWithoutAllowControl(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, false)
	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{name: "wait stable", tool: "jetkvm_wait_stable"},
		{name: "wait for text", tool: "jetkvm_wait_for_text", args: map[string]any{"text": "ready"}},
		{name: "keypress", tool: "jetkvm_keypress", args: map[string]any{"key": 4}},
		{name: "type", tool: "jetkvm_type", args: map[string]any{"text": "a"}},
		{name: "mouse move", tool: "jetkvm_mouse_move", args: map[string]any{"x": 1, "y": 2}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      tc.tool,
				Arguments: tc.args,
			})
			if err == nil {
				t.Fatalf("%s was callable without --allow-control: %+v", tc.tool, result)
			}
			if result != nil {
				t.Fatalf("catalog-gated %s returned a tool result %+v, want protocol rejection", tc.tool, result)
			}
		})
	}
}

func TestCoverageProtocolMiddlewareLeavesNonValidationOutcomesUntouched(t *testing.T) {
	sentinel := errors.New("synthetic downstream protocol failure")
	listResult := &mcp.ListToolsResult{}
	ordinaryToolResult := textResult("ordinary handler result")
	tests := []struct {
		name       string
		method     string
		nextResult mcp.Result
		nextErr    error
	}{
		{name: "non-tool result", method: "tools/call", nextResult: listResult},
		{name: "ordinary tool result", method: "tools/call", nextResult: ordinaryToolResult},
		{name: "downstream protocol error", method: "tools/call", nextErr: sentinel},
		{name: "different method", method: "tools/list", nextResult: listResult},
	}

	request := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
		Name:      "test",
		Arguments: []byte(`{}`),
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nextCalls := 0
			handler := invalidToolArgumentsAsProtocolErrors(func(context.Context, string, mcp.Request) (mcp.Result, error) {
				nextCalls++
				return tc.nextResult, tc.nextErr
			})
			result, err := handler(context.Background(), tc.method, request)
			if nextCalls != 1 {
				t.Fatalf("downstream calls = %d, want 1", nextCalls)
			}
			if result != tc.nextResult {
				t.Errorf("middleware replaced result %T with %T", tc.nextResult, result)
			}
			if !errors.Is(err, tc.nextErr) {
				t.Errorf("middleware error = %v, want %v", err, tc.nextErr)
			}
		})
	}
}

func TestCoverageSendTypeKeypressesRejectsMismatchedSequencesBeforeWireSend(t *testing.T) {
	tests := []struct {
		name       string
		keypresses []jetkvm.TypeKeypress
		runes      []rune
	}{
		{name: "keypress without character", keypresses: []jetkvm.TypeKeypress{{HIDUsageCode: 4}}},
		{name: "character without keypress", runes: []rune{'a'}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sendCalls := 0
			waitCalls := 0
			err := sendTypeKeypresses(
				context.Background(), tc.keypresses, tc.runes, time.Millisecond,
				func(context.Context, byte, byte) error {
					sendCalls++
					return nil
				},
				func(context.Context, time.Duration) error {
					waitCalls++
					return nil
				},
			)
			if err == nil || !strings.Contains(err.Error(), "mapped keypress count") {
				t.Fatalf("mismatched sequence error = %v, want count mismatch", err)
			}
			if sendCalls != 0 || waitCalls != 0 {
				t.Fatalf("mismatched sequence made send/wait calls = %d/%d, want 0/0", sendCalls, waitCalls)
			}
		})
	}
}

func TestCoverageWaitInterKeyDelayReturnsPreexistingContextErrors(t *testing.T) {
	tests := []struct {
		name    string
		newCtx  func() (context.Context, context.CancelFunc)
		wantErr error
	}{
		{
			name: "canceled",
			newCtx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			wantErr: context.Canceled,
		},
		{
			name: "deadline exceeded",
			newCtx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Unix(0, 0))
			},
			wantErr: context.DeadlineExceeded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.newCtx()
			defer cancel()
			if err := waitInterKeyDelay(ctx, time.Hour); !errors.Is(err, tc.wantErr) {
				t.Fatalf("waitInterKeyDelay error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestCoverageWithDefaultTimeoutLeavesDisabledContextsUnchanged(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
	}{
		{name: "zero", timeout: 0},
		{name: "negative", timeout: -time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			got, cancel := withDefaultTimeout(ctx, tc.timeout)
			cancel()
			if got != ctx {
				t.Fatal("disabled default timeout replaced the caller context")
			}
			select {
			case <-got.Done():
				t.Fatalf("disabled default timeout canceled caller context: %v", got.Err())
			default:
			}
		})
	}
}
