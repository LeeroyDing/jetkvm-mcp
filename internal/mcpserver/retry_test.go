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

type mockDevice struct {
	statusFunc     func(context.Context) (jetkvm.StatusResult, error)
	keypressFunc   func(context.Context, byte, byte) error
	closeFunc      func(context.Context) error
	screenshotFunc func(context.Context) (jetkvm.Screenshot, error)
	releaseAllFunc func(context.Context) (bool, error)
}

func (d *mockDevice) status(ctx context.Context) (jetkvm.StatusResult, error) {
	if d.statusFunc != nil {
		return d.statusFunc(ctx)
	}
	return jetkvm.StatusResult{DeviceID: "mock", FirmwareVersion: "test", RPCReachable: true}, nil
}

func (d *mockDevice) captureScreenshot(ctx context.Context) (jetkvm.Screenshot, error) {
	if d.screenshotFunc != nil {
		return d.screenshotFunc(ctx)
	}
	return jetkvm.Screenshot{}, errors.New("unexpected screenshot call")
}

func (d *mockDevice) releaseAll(ctx context.Context) (bool, error) {
	if d.releaseAllFunc != nil {
		return d.releaseAllFunc(ctx)
	}
	return false, nil
}

func (d *mockDevice) keypress(ctx context.Context, modifier, key byte) error {
	if d.keypressFunc != nil {
		return d.keypressFunc(ctx, modifier, key)
	}
	return errors.New("unexpected keypress call")
}

func (d *mockDevice) mouseMove(context.Context, int32, int32, byte) error {
	return errors.New("unexpected mouse move call")
}

func (d *mockDevice) close(ctx context.Context) error {
	if d.closeFunc != nil {
		return d.closeFunc(ctx)
	}
	return nil
}

func deviceFailure(kind jetkvm.ErrorKind, operation string) error {
	return &jetkvm.DeviceError{Kind: kind, Operation: operation, Detail: "mock failure"}
}

func immediateRetryPolicy(maxAttempts int, delays *[]time.Duration) retryPolicy {
	return retryPolicy{
		maxAttempts: maxAttempts,
		baseDelay:   10 * time.Millisecond,
		maxDelay:    15 * time.Millisecond,
		jitter:      func(delay time.Duration) time.Duration { return delay + 5*time.Millisecond },
		sleep: func(ctx context.Context, delay time.Duration) error {
			if delays != nil {
				*delays = append(*delays, delay)
			}
			return nil
		},
	}
}

func TestRetryingDeviceRecoversWithinOneCall(t *testing.T) {
	connectAttempts := 0
	var delays []time.Duration
	connector := func(context.Context) (device, error) {
		connectAttempts++
		if connectAttempts < 3 {
			return nil, deviceFailure(jetkvm.ErrorKindUnreachable, "connect")
		}
		return &mockDevice{}, nil
	}
	client := newRetryingDeviceWithConnector(false, connector, immediateRetryPolicy(3, &delays))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	status, err := client.status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !status.RPCReachable {
		t.Fatal("recovered status did not report RPC reachable")
	}
	if connectAttempts != 3 {
		t.Fatalf("connect attempts = %d, want 3", connectAttempts)
	}
	if len(delays) != 2 {
		t.Fatalf("backoff sleeps = %d, want 2", len(delays))
	}
	for i, delay := range delays {
		if delay > 15*time.Millisecond {
			t.Errorf("delay %d = %v, exceeded hard cap", i+1, delay)
		}
	}
}

func TestRetryingDeviceScreenshotPreflightAvoidsConnect(t *testing.T) {
	connectAttempts := 0
	connector := func(context.Context) (device, error) {
		connectAttempts++
		return &mockDevice{}, nil
	}
	client := newRetryingDeviceWithConnector(false, connector, immediateRetryPolicy(1, nil))
	wantErr := errors.New("ffmpeg unavailable")
	client.screenshotPreflight = func(context.Context) error { return wantErr }

	if _, err := client.captureScreenshot(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("screenshot error = %v, want preflight error", err)
	}
	if connectAttempts != 0 {
		t.Fatalf("screenshot preflight opened %d device sessions, want 0", connectAttempts)
	}
	if _, err := client.status(context.Background()); err != nil {
		t.Fatalf("status must remain usable without FFmpeg: %v", err)
	}
	if connectAttempts != 1 {
		t.Fatalf("status connect attempts = %d, want 1", connectAttempts)
	}
}

func TestRetryingDeviceStopsAtBoundedAttemptLimit(t *testing.T) {
	connectAttempts := 0
	connector := func(context.Context) (device, error) {
		connectAttempts++
		return nil, deviceFailure(jetkvm.ErrorKindUnreachable, "connect")
	}
	client := newRetryingDeviceWithConnector(false, connector, immediateRetryPolicy(3, nil))

	_, err := client.status(context.Background())
	if jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindUnreachable {
		t.Fatalf("error kind = %q, want unreachable: %v", jetkvm.ErrorKindOf(err), err)
	}
	if connectAttempts != 3 {
		t.Fatalf("connect attempts = %d, want exactly 3", connectAttempts)
	}
	if !strings.Contains(err.Error(), "after 3 attempts") || !strings.Contains(err.Error(), "bounded retry limit") {
		t.Fatalf("bounded failure is not clear: %v", err)
	}
}

func TestRetryingDeviceReconnectsAfterReadOnlyOperationFailure(t *testing.T) {
	connectAttempts := 0
	statusCalls := 0
	closed := 0
	connector := func(context.Context) (device, error) {
		connectAttempts++
		if connectAttempts == 1 {
			return &mockDevice{
				statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
					statusCalls++
					return jetkvm.StatusResult{}, deviceFailure(jetkvm.ErrorKindUnreachable, "ping")
				},
				closeFunc: func(context.Context) error { closed++; return nil },
			}, nil
		}
		return &mockDevice{statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
			statusCalls++
			return jetkvm.StatusResult{RPCReachable: true}, nil
		}}, nil
	}
	client := newRetryingDeviceWithConnector(false, connector, immediateRetryPolicy(3, nil))

	status, err := client.status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !status.RPCReachable || connectAttempts != 2 || statusCalls != 2 || closed != 1 {
		t.Fatalf("reconnect path: reachable=%v connects=%d statusCalls=%d closed=%d",
			status.RPCReachable, connectAttempts, statusCalls, closed)
	}
}

func TestRetryingDeviceNeverRetriesAuthenticationFailure(t *testing.T) {
	connectAttempts := 0
	var delays []time.Duration
	connector := func(context.Context) (device, error) {
		connectAttempts++
		return nil, deviceFailure(jetkvm.ErrorKindAuthFailed, "login")
	}
	client := newRetryingDeviceWithConnector(false, connector, immediateRetryPolicy(3, &delays))

	_, err := client.status(context.Background())
	if jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindAuthFailed {
		t.Fatalf("error kind = %q, want auth-failed: %v", jetkvm.ErrorKindOf(err), err)
	}
	if connectAttempts != 1 || len(delays) != 0 {
		t.Fatalf("auth failure was retried: connects=%d sleeps=%d", connectAttempts, len(delays))
	}
}

func TestRetryingDeviceNeverRepeatsStateChangingOperation(t *testing.T) {
	connectAttempts := 0
	keypressCalls := 0
	connector := func(context.Context) (device, error) {
		connectAttempts++
		return &mockDevice{keypressFunc: func(context.Context, byte, byte) error {
			keypressCalls++
			return deviceFailure(jetkvm.ErrorKindUnreachable, "sending keypress")
		}}, nil
	}
	client := newRetryingDeviceWithConnector(true, connector, immediateRetryPolicy(3, nil))

	err := client.keypress(context.Background(), 0, 4)
	if jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindUnreachable {
		t.Fatalf("error kind = %q, want unreachable: %v", jetkvm.ErrorKindOf(err), err)
	}
	if connectAttempts != 1 || keypressCalls != 1 {
		t.Fatalf("state-changing call was repeated: connects=%d keypresses=%d", connectAttempts, keypressCalls)
	}
}

func TestRetryBackoffNeverExceedsCallBudget(t *testing.T) {
	connectAttempts := 0
	connector := func(context.Context) (device, error) {
		connectAttempts++
		return nil, deviceFailure(jetkvm.ErrorKindUnreachable, "connect")
	}
	policy := defaultRetryPolicy()
	policy.baseDelay = 200 * time.Millisecond
	policy.maxDelay = 200 * time.Millisecond
	client := newRetryingDeviceWithConnector(false, connector, policy)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.status(ctx)
	elapsed := time.Since(start)
	if jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindTimeout {
		t.Fatalf("error kind = %q, want timeout: %v", jetkvm.ErrorKindOf(err), err)
	}
	if connectAttempts != 1 {
		t.Fatalf("connect attempts = %d, want 1 when no backoff fits", connectAttempts)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("call took %v despite a 25ms budget", elapsed)
	}
}

func TestStatusToolReturnsDistinctFailureKinds(t *testing.T) {
	for _, kind := range []jetkvm.ErrorKind{
		jetkvm.ErrorKindAuthFailed,
		jetkvm.ErrorKindUnreachable,
		jetkvm.ErrorKindTimeout,
		jetkvm.ErrorKindBadFrame,
	} {
		t.Run(string(kind), func(t *testing.T) {
			mock := &mockDevice{statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
				return jetkvm.StatusResult{}, deviceFailure(kind, "status")
			}}
			cs := newTestServerSessionForDevice(t, mock, false)
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "jetkvm_status"})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if !res.IsError {
				t.Fatalf("result was not marked as an error: %+v", res.Content)
			}
			var message string
			for _, content := range res.Content {
				if text, ok := content.(*mcp.TextContent); ok {
					message += text.Text
				}
			}
			if !strings.Contains(message, "jetkvm: "+string(kind)+":") {
				t.Fatalf("MCP error %q does not expose kind %q", message, kind)
			}
		})
	}
}
