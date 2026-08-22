package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

type mockDevice struct {
	statusFunc      func(context.Context) (jetkvm.StatusResult, error)
	keypressFunc    func(context.Context, byte, byte) error
	keyComboFunc    func(context.Context, byte, []byte) error
	holdKeyFunc     func(context.Context, byte, []byte, int) error
	mouseMoveFunc   func(context.Context, int32, int32, byte) error
	mouseButtonFunc func(context.Context, byte, bool) error
	scrollFunc      func(context.Context, int8, int8) error
	dragFunc        func(context.Context, []jetkvm.PointerDragReport) error
	closeFunc       func(context.Context) error
	screenshotFunc  func(context.Context) (jetkvm.Screenshot, error)
	waitStableFunc  func(context.Context, jetkvm.WaitStableOptions) (jetkvm.WaitStableResult, error)
	releaseAllFunc  func(context.Context) (bool, error)
}

type countingCaptureDecoder struct {
	checkCalls  atomic.Int32
	decodeCalls atomic.Int32
}

func (d *countingCaptureDecoder) CheckAvailable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.checkCalls.Add(1)
	return nil
}

func (d *countingCaptureDecoder) DecodeFrame(ctx context.Context, _ []byte) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.decodeCalls.Add(1)
	return image.NewRGBA(image.Rect(0, 0, 2, 2)), nil
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

func (d *mockDevice) waitStable(ctx context.Context, opts jetkvm.WaitStableOptions) (jetkvm.WaitStableResult, error) {
	if d.waitStableFunc != nil {
		return d.waitStableFunc(ctx, opts)
	}
	return jetkvm.WaitStableResult{}, errors.New("unexpected wait-stable call")
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

func (d *mockDevice) keyCombo(ctx context.Context, modifier byte, keys []byte) error {
	if d.keyComboFunc != nil {
		return d.keyComboFunc(ctx, modifier, keys)
	}
	return errors.New("unexpected key combo call")
}

func (d *mockDevice) holdKey(ctx context.Context, modifier byte, keys []byte, holdMS int) error {
	if d.holdKeyFunc != nil {
		return d.holdKeyFunc(ctx, modifier, keys, holdMS)
	}
	return errors.New("unexpected hold key call")
}

func (d *mockDevice) mouseMove(ctx context.Context, x, y int32, buttons byte) error {
	if d.mouseMoveFunc != nil {
		return d.mouseMoveFunc(ctx, x, y, buttons)
	}
	return errors.New("unexpected mouse move call")
}

func (d *mockDevice) mouseButton(ctx context.Context, button byte, pressed bool) error {
	if d.mouseButtonFunc != nil {
		return d.mouseButtonFunc(ctx, button, pressed)
	}
	return errors.New("unexpected mouse button call")
}

func (d *mockDevice) scroll(ctx context.Context, dx, dy int8) error {
	if d.scrollFunc != nil {
		return d.scrollFunc(ctx, dx, dy)
	}
	return errors.New("unexpected scroll call")
}

func (d *mockDevice) drag(ctx context.Context, reports []jetkvm.PointerDragReport) error {
	if d.dragFunc != nil {
		return d.dragFunc(ctx, reports)
	}
	return errors.New("unexpected drag call")
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
	client.decoderPreflight = func(context.Context) error { return wantErr }

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

func TestRetryingDeviceWaitStableValidatesBeforePreflightAndConnect(t *testing.T) {
	preflightCalls := 0
	connectAttempts := 0
	connector := func(context.Context) (device, error) {
		connectAttempts++
		return &mockDevice{}, nil
	}
	client := newRetryingDeviceWithConnector(false, connector, immediateRetryPolicy(1, nil))
	client.decoderPreflight = func(context.Context) error {
		preflightCalls++
		return nil
	}

	stableFrames := 0
	_, err := client.waitStable(context.Background(), jetkvm.WaitStableOptions{StableFrames: &stableFrames})
	if err == nil || !strings.Contains(err.Error(), "stable frame count") {
		t.Fatalf("waitStable validation error = %v, want stable frame count error", err)
	}
	if preflightCalls != 0 || connectAttempts != 0 {
		t.Fatalf("invalid options performed work: preflights=%d connects=%d", preflightCalls, connectAttempts)
	}
}

func TestRetryingDeviceWaitStablePreflightAvoidsConnect(t *testing.T) {
	connectAttempts := 0
	connector := func(context.Context) (device, error) {
		connectAttempts++
		return &mockDevice{}, nil
	}
	client := newRetryingDeviceWithConnector(false, connector, immediateRetryPolicy(1, nil))
	wantErr := errors.New("ffmpeg unavailable")
	client.decoderPreflight = func(context.Context) error { return wantErr }

	if _, err := client.waitStable(context.Background(), jetkvm.WaitStableOptions{}); !errors.Is(err, wantErr) {
		t.Fatalf("waitStable error = %v, want preflight error", err)
	}
	if connectAttempts != 0 {
		t.Fatalf("wait-stable preflight opened %d device sessions, want 0", connectAttempts)
	}
}

func TestRetryingDeviceProductionCapturePathsPreflightOnce(t *testing.T) {
	for _, operation := range []string{"screenshot", "wait-stable"} {
		t.Run(operation, func(t *testing.T) {
			fd := startFakeDevice(t)
			decoder := &countingCaptureDecoder{}
			client := newRetryingDeviceWithDecoder(Options{
				BaseURL: fd.baseURL(),
			}, decoder)
			t.Cleanup(func() { _ = client.close(context.Background()) })

			ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 15*time.Second))
			defer cancel()

			wantDecodes := int32(1)
			switch operation {
			case "screenshot":
				shot, err := client.captureScreenshot(ctx)
				if err != nil {
					t.Fatalf("production screenshot chain: %v", err)
				}
				if !shot.Fresh || shot.Width != 2 || shot.Height != 2 {
					t.Fatalf("production screenshot = %+v, want fresh 2x2 frame", shot.ScreenshotResult)
				}
			case "wait-stable":
				stableFrames := 1
				pollInterval := time.Duration(0)
				result, err := client.waitStable(ctx, jetkvm.WaitStableOptions{
					StableFrames: &stableFrames,
					PollInterval: &pollInterval,
				})
				if err != nil {
					t.Fatalf("production wait-stable chain: %v", err)
				}
				if !result.Settled || result.FramesSampled != 2 {
					t.Fatalf("production wait-stable result = %+v, want settled after 2 frames", result)
				}
				wantDecodes = 2
			}

			if got := decoder.checkCalls.Load(); got != 1 {
				t.Errorf("CheckAvailable calls across retryingDevice -> clientDevice -> Client = %d, want 1", got)
			}
			if got := decoder.decodeCalls.Load(); got != wantDecodes {
				t.Errorf("DecodeFrame calls = %d, want %d", got, wantDecodes)
			}
		})
	}
}

func TestRetryingDeviceSerializesConcurrentScreenshotPreflights(t *testing.T) {
	testRetryingDeviceSerializesDecoderPreflights(t, func(ctx context.Context, client *retryingDevice) error {
		_, err := client.captureScreenshot(ctx)
		return err
	})
}

func TestRetryingDeviceSerializesConcurrentWaitStablePreflights(t *testing.T) {
	testRetryingDeviceSerializesDecoderPreflights(t, func(ctx context.Context, client *retryingDevice) error {
		_, err := client.waitStable(ctx, jetkvm.WaitStableOptions{})
		return err
	})
}

func testRetryingDeviceSerializesDecoderPreflights(
	t *testing.T,
	invoke func(context.Context, *retryingDevice) error,
) {
	t.Helper()
	const concurrentCalls = 8

	synctest.Test(t, func(t *testing.T) {
		var operationCalls atomic.Int32
		mock := &mockDevice{
			screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
				operationCalls.Add(1)
				return jetkvm.Screenshot{}, nil
			},
			waitStableFunc: func(context.Context, jetkvm.WaitStableOptions) (jetkvm.WaitStableResult, error) {
				operationCalls.Add(1)
				return jetkvm.WaitStableResult{}, nil
			},
		}
		client := newRetryingDeviceWithConnector(false, func(context.Context) (device, error) {
			return mock, nil
		}, immediateRetryPolicy(1, nil))

		preflightRelease := make(chan struct{})
		var preflightCalls atomic.Int32
		var activePreflights atomic.Int32
		var maxActivePreflights atomic.Int32
		client.decoderPreflight = func(context.Context) error {
			preflightCalls.Add(1)
			active := activePreflights.Add(1)
			defer activePreflights.Add(-1)
			for {
				maximum := maxActivePreflights.Load()
				if active <= maximum || maxActivePreflights.CompareAndSwap(maximum, active) {
					break
				}
			}
			<-preflightRelease
			return nil
		}

		results := make(chan error, concurrentCalls)
		for range concurrentCalls {
			go func() {
				results <- invoke(t.Context(), client)
			}()
		}

		// Wait until every call is either in preflight or blocked on the
		// device-call gate. Exactly one preflight may be active at this point.
		synctest.Wait()
		callsBeforeRelease := preflightCalls.Load()
		activeBeforeRelease := activePreflights.Load()
		close(preflightRelease)
		synctest.Wait()

		for range concurrentCalls {
			if err := <-results; err != nil {
				t.Errorf("concurrent device call: %v", err)
			}
		}
		if callsBeforeRelease != 1 || activeBeforeRelease != 1 {
			t.Errorf("preflights before release: calls=%d active=%d, want 1/1",
				callsBeforeRelease, activeBeforeRelease)
		}
		if got := maxActivePreflights.Load(); got != 1 {
			t.Errorf("maximum concurrent preflights = %d, want 1", got)
		}
		if got := preflightCalls.Load(); got != concurrentCalls {
			t.Errorf("preflight calls = %d, want %d", got, concurrentCalls)
		}
		if got := operationCalls.Load(); got != concurrentCalls {
			t.Errorf("device operation calls = %d, want %d", got, concurrentCalls)
		}
	})
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

func TestRetryingDeviceNormalizesAttemptLimitToOne(t *testing.T) {
	connectAttempts := 0
	connector := func(context.Context) (device, error) {
		connectAttempts++
		return nil, deviceFailure(jetkvm.ErrorKindUnreachable, "connect")
	}
	client := newRetryingDeviceWithConnector(false, connector, retryPolicy{maxAttempts: 0})

	_, err := client.status(context.Background())
	if jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindUnreachable {
		t.Fatalf("error kind = %q, want unreachable: %v", jetkvm.ErrorKindOf(err), err)
	}
	if connectAttempts != 1 {
		t.Fatalf("connect attempts = %d, want normalized bound of 1", connectAttempts)
	}
	if !strings.Contains(err.Error(), "after 1 attempts") || !strings.Contains(err.Error(), "bounded retry limit") {
		t.Fatalf("normalized bounded failure is not stable: %v", err)
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

func TestRetryingDeviceControlOperationsRetryConnectionBeforeStarting(t *testing.T) {
	for _, operation := range []string{"keypress", "key-combo", "hold-key", "mouse-move", "mouse-button", "scroll", "drag", "release-all"} {
		t.Run(operation, func(t *testing.T) {
			connectAttempts := 0
			operationCalls := 0
			mock := &mockDevice{}
			dragReports := []jetkvm.PointerDragReport{
				{X: 1, Y: 2, Buttons: 1},
				{X: 3, Y: 4, Buttons: 0},
			}
			var invoke func(*retryingDevice) error
			switch operation {
			case "keypress":
				mock.keypressFunc = func(_ context.Context, modifier, key byte) error {
					operationCalls++
					if modifier != 2 || key != 4 {
						t.Errorf("keypress arguments = modifier %d key %d, want 2/4", modifier, key)
					}
					return nil
				}
				invoke = func(client *retryingDevice) error {
					return client.keypress(context.Background(), 2, 4)
				}
			case "key-combo":
				mock.keyComboFunc = func(_ context.Context, modifier byte, keys []byte) error {
					operationCalls++
					if modifier != 3 || !bytes.Equal(keys, []byte{4, 5}) {
						t.Errorf("key combo arguments = modifier %d keys %v, want 3/[4 5]", modifier, keys)
					}
					return nil
				}
				invoke = func(client *retryingDevice) error {
					return client.keyCombo(context.Background(), 3, []byte{4, 5})
				}
			case "hold-key":
				mock.holdKeyFunc = func(_ context.Context, modifier byte, keys []byte, holdMS int) error {
					operationCalls++
					if modifier != 3 || !bytes.Equal(keys, []byte{4, 5}) || holdMS != 250 {
						t.Errorf("hold key arguments = modifier %d keys %v holdMS %d, want 3/[4 5]/250", modifier, keys, holdMS)
					}
					return nil
				}
				invoke = func(client *retryingDevice) error {
					return client.holdKey(context.Background(), 3, []byte{4, 5}, 250)
				}
			case "mouse-move":
				mock.mouseMoveFunc = func(_ context.Context, x, y int32, buttons byte) error {
					operationCalls++
					if x != 123 || y != 456 || buttons != 3 {
						t.Errorf("mouse arguments = %d/%d/%d, want 123/456/3", x, y, buttons)
					}
					return nil
				}
				invoke = func(client *retryingDevice) error {
					return client.mouseMove(context.Background(), 123, 456, 3)
				}
			case "mouse-button":
				mock.mouseButtonFunc = func(_ context.Context, button byte, pressed bool) error {
					operationCalls++
					if button != jetkvm.MouseButtonRight || !pressed {
						t.Errorf("mouse-button arguments = %d/%v, want right/press", button, pressed)
					}
					return nil
				}
				invoke = func(client *retryingDevice) error {
					return client.mouseButton(context.Background(), jetkvm.MouseButtonRight, true)
				}
			case "scroll":
				mock.scrollFunc = func(_ context.Context, dx, dy int8) error {
					operationCalls++
					if dx != -5 || dy != 7 {
						t.Errorf("scroll arguments = %d/%d, want -5/7", dx, dy)
					}
					return nil
				}
				invoke = func(client *retryingDevice) error {
					return client.scroll(context.Background(), -5, 7)
				}
			case "drag":
				mock.dragFunc = func(_ context.Context, reports []jetkvm.PointerDragReport) error {
					operationCalls++
					if len(reports) != len(dragReports) || reports[0] != dragReports[0] || reports[1] != dragReports[1] {
						t.Errorf("drag reports = %+v, want %+v", reports, dragReports)
					}
					return nil
				}
				invoke = func(client *retryingDevice) error {
					return client.drag(context.Background(), dragReports)
				}
			case "release-all":
				mock.releaseAllFunc = func(context.Context) (bool, error) {
					operationCalls++
					return true, nil
				}
				invoke = func(client *retryingDevice) error {
					released, err := client.releaseAll(context.Background())
					if err == nil && !released {
						t.Error("release-all lost the successful release result")
					}
					return err
				}
			}

			connector := func(context.Context) (device, error) {
				connectAttempts++
				if connectAttempts == 1 {
					return nil, deviceFailure(jetkvm.ErrorKindUnreachable, "connect")
				}
				return mock, nil
			}
			client := newRetryingDeviceWithConnector(true, connector, immediateRetryPolicy(3, nil))

			if err := invoke(client); err != nil {
				t.Fatalf("%s after pre-operation reconnect: %v", operation, err)
			}
			if connectAttempts != 2 || operationCalls != 1 {
				t.Fatalf("%s counts: connects=%d operations=%d, want 2/1", operation, connectAttempts, operationCalls)
			}
		})
	}
}

func TestRetryingDeviceDragRejectsNoButtonBeforeConnect(t *testing.T) {
	connectAttempts := 0
	client := newRetryingDeviceWithConnector(true, func(context.Context) (device, error) {
		connectAttempts++
		return &mockDevice{}, nil
	}, immediateRetryPolicy(1, nil))

	err := client.drag(context.Background(), []jetkvm.PointerDragReport{
		{X: 1, Y: 2, Buttons: 0},
		{X: 3, Y: 4, Buttons: 0},
	})
	if err == nil || !strings.Contains(err.Error(), "nonzero button mask") {
		t.Fatalf("movement-only drag error = %v, want nonzero-mask rejection", err)
	}
	if connectAttempts != 0 {
		t.Fatalf("movement-only drag opened %d connections, want zero", connectAttempts)
	}
}

func TestRetryingDeviceNeverRepeatsStateChangingOperation(t *testing.T) {
	for _, operation := range []string{"keypress", "key-combo", "hold-key", "mouse-move", "mouse-button", "scroll", "drag", "release-all"} {
		t.Run(operation, func(t *testing.T) {
			connectAttempts := 0
			operationCalls := 0
			mock := &mockDevice{}
			var invoke func(*retryingDevice) error
			switch operation {
			case "keypress":
				mock.keypressFunc = func(context.Context, byte, byte) error {
					operationCalls++
					return deviceFailure(jetkvm.ErrorKindUnreachable, "sending keypress")
				}
				invoke = func(client *retryingDevice) error {
					return client.keypress(context.Background(), 0, 4)
				}
			case "key-combo":
				mock.keyComboFunc = func(context.Context, byte, []byte) error {
					operationCalls++
					return deviceFailure(jetkvm.ErrorKindUnreachable, "sending key combo")
				}
				invoke = func(client *retryingDevice) error {
					return client.keyCombo(context.Background(), 1, []byte{4, 5})
				}
			case "hold-key":
				mock.holdKeyFunc = func(context.Context, byte, []byte, int) error {
					operationCalls++
					return deviceFailure(jetkvm.ErrorKindUnreachable, "holding key combo")
				}
				invoke = func(client *retryingDevice) error {
					return client.holdKey(context.Background(), 1, []byte{4, 5}, 250)
				}
			case "mouse-move":
				mock.mouseMoveFunc = func(context.Context, int32, int32, byte) error {
					operationCalls++
					return deviceFailure(jetkvm.ErrorKindUnreachable, "sending mouse move")
				}
				invoke = func(client *retryingDevice) error {
					return client.mouseMove(context.Background(), 123, 456, 3)
				}
			case "mouse-button":
				mock.mouseButtonFunc = func(context.Context, byte, bool) error {
					operationCalls++
					return deviceFailure(jetkvm.ErrorKindUnreachable, "sending mouse button")
				}
				invoke = func(client *retryingDevice) error {
					return client.mouseButton(context.Background(), jetkvm.MouseButtonLeft, true)
				}
			case "scroll":
				mock.scrollFunc = func(context.Context, int8, int8) error {
					operationCalls++
					return deviceFailure(jetkvm.ErrorKindUnreachable, "sending scroll")
				}
				invoke = func(client *retryingDevice) error {
					return client.scroll(context.Background(), -5, 7)
				}
			case "drag":
				mock.dragFunc = func(context.Context, []jetkvm.PointerDragReport) error {
					operationCalls++
					return deviceFailure(jetkvm.ErrorKindUnreachable, "sending drag")
				}
				invoke = func(client *retryingDevice) error {
					return client.drag(context.Background(), []jetkvm.PointerDragReport{
						{X: 1, Y: 2, Buttons: 1},
						{X: 3, Y: 4, Buttons: 0},
					})
				}
			case "release-all":
				mock.releaseAllFunc = func(context.Context) (bool, error) {
					operationCalls++
					return true, deviceFailure(jetkvm.ErrorKindUnreachable, "releasing input")
				}
				invoke = func(client *retryingDevice) error {
					_, err := client.releaseAll(context.Background())
					return err
				}
			}

			connector := func(context.Context) (device, error) {
				connectAttempts++
				return mock, nil
			}
			client := newRetryingDeviceWithConnector(true, connector, immediateRetryPolicy(3, nil))

			err := invoke(client)
			if jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindUnreachable {
				t.Fatalf("error kind = %q, want unreachable: %v", jetkvm.ErrorKindOf(err), err)
			}
			if connectAttempts != 1 || operationCalls != 1 {
				t.Fatalf("%s was repeated: connects=%d operations=%d", operation, connectAttempts, operationCalls)
			}
		})
	}
}

func TestRetryingDeviceDiscardsClosedHIDSessionWithoutReplayingMutation(t *testing.T) {
	var firstCloses atomic.Int32
	connectAttempts := 0
	firstCalls := 0
	secondCalls := 0
	first := &mockDevice{
		keypressFunc: func(context.Context, byte, byte) error {
			firstCalls++
			return errors.Join(jetkvm.ErrHIDClosed, jetkvm.ErrNeutralizeUnverified)
		},
		closeFunc: func(context.Context) error {
			firstCloses.Add(1)
			return nil
		},
	}
	second := &mockDevice{keypressFunc: func(context.Context, byte, byte) error {
		secondCalls++
		return nil
	}}
	client := newRetryingDeviceWithConnector(true, func(context.Context) (device, error) {
		connectAttempts++
		if connectAttempts == 1 {
			return first, nil
		}
		return second, nil
	}, immediateRetryPolicy(3, nil))
	t.Cleanup(func() { _ = client.close(context.Background()) })

	err := client.keypress(context.Background(), 0, 0x04)
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindUnreachable {
		t.Fatalf("closed HID error kind = %q, want %q: %v", kind, jetkvm.ErrorKindUnreachable, err)
	}
	if !errors.Is(err, jetkvm.ErrNeutralizeUnverified) {
		t.Fatalf("closed HID error lost neutralization warning: %v", err)
	}
	if connectAttempts != 1 || firstCalls != 1 || secondCalls != 0 {
		t.Fatalf("failed mutation was replayed: connects=%d first=%d second=%d, want 1/1/0",
			connectAttempts, firstCalls, secondCalls)
	}

	if err := client.keypress(context.Background(), 0, 0x05); err != nil {
		t.Fatalf("mutation after HID reconnect: %v", err)
	}
	if connectAttempts != 2 || firstCalls != 1 || secondCalls != 1 || firstCloses.Load() != 1 {
		t.Fatalf("replacement counts: connects=%d first=%d second=%d closes=%d, want 2/1/1/1",
			connectAttempts, firstCalls, secondCalls, firstCloses.Load())
	}
}

func TestRetryingDeviceMouseButtonValidatesBeforeConnecting(t *testing.T) {
	for _, button := range []byte{0, 3, 0xff} {
		connectAttempts := 0
		client := newRetryingDeviceWithConnector(true, func(context.Context) (device, error) {
			connectAttempts++
			return &mockDevice{}, nil
		}, immediateRetryPolicy(1, nil))

		if err := client.mouseButton(context.Background(), button, true); err == nil {
			t.Errorf("mouseButton accepted invalid mask %#02x", button)
		}
		if connectAttempts != 0 {
			t.Errorf("invalid mask %#02x made %d connection attempts, want zero", button, connectAttempts)
		}
	}
}

func TestRetryingDeviceScrollRequiresControlBeforeConnecting(t *testing.T) {
	connectAttempts := 0
	client := newRetryingDeviceWithConnector(false, func(context.Context) (device, error) {
		connectAttempts++
		return &mockDevice{scrollFunc: func(context.Context, int8, int8) error {
			t.Fatal("scroll operation was reached without control")
			return nil
		}}, nil
	}, immediateRetryPolicy(3, nil))

	err := client.scroll(context.Background(), 0, 1)
	if !errors.Is(err, jetkvm.ErrControlDisabled) {
		t.Fatalf("scroll without control = %v, want ErrControlDisabled", err)
	}
	if connectAttempts != 0 {
		t.Fatalf("scroll without control made %d connection attempts, want zero", connectAttempts)
	}
}

func TestRetryingDeviceJoinedNeutralizationFailureIsNotRetried(t *testing.T) {
	connectAttempts := 0
	operationCalls := 0
	var delays []time.Duration
	transportErr := deviceFailure(jetkvm.ErrorKindUnreachable, "sending keypress")
	neutralizeErr := fmt.Errorf("releasing held input: %w", jetkvm.ErrNeutralizeUnverified)
	mock := &mockDevice{keypressFunc: func(context.Context, byte, byte) error {
		operationCalls++
		return errors.Join(transportErr, neutralizeErr)
	}}
	connector := func(context.Context) (device, error) {
		connectAttempts++
		return mock, nil
	}
	client := newRetryingDeviceWithConnector(true, connector, immediateRetryPolicy(3, &delays))

	err := client.keypress(context.Background(), 0, 4)
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindUnreachable {
		t.Fatalf("error kind = %q, want unreachable: %v", kind, err)
	}
	if !errors.Is(err, transportErr) {
		t.Errorf("joined error lost transport failure: %v", err)
	}
	if !errors.Is(err, jetkvm.ErrNeutralizeUnverified) {
		t.Errorf("joined error lost neutralization warning: %v", err)
	}
	if connectAttempts != 1 || operationCalls != 1 {
		t.Errorf("joined failure was retried: connects=%d operations=%d, want 1/1", connectAttempts, operationCalls)
	}
	if len(delays) != 0 {
		t.Errorf("joined failure used retry backoff delays %v, want none", delays)
	}
}

func TestRetryingDeviceDragTimeoutRetiresUnverifiedSession(t *testing.T) {
	firstCalls := 0
	firstClosed := make(chan struct{})
	first := &mockDevice{
		dragFunc: func(context.Context, []jetkvm.PointerDragReport) error {
			firstCalls++
			return errors.Join(context.DeadlineExceeded, jetkvm.ErrNeutralizeUnverified)
		},
		closeFunc: func(context.Context) error {
			close(firstClosed)
			return nil
		},
	}
	secondCalls := 0
	second := &mockDevice{dragFunc: func(context.Context, []jetkvm.PointerDragReport) error {
		secondCalls++
		return nil
	}}
	connectAttempts := 0
	connector := func(context.Context) (device, error) {
		connectAttempts++
		if connectAttempts == 1 {
			return first, nil
		}
		return second, nil
	}
	client := newRetryingDeviceWithConnector(true, connector, immediateRetryPolicy(3, nil))
	t.Cleanup(func() { _ = client.close(context.Background()) })

	reports := []jetkvm.PointerDragReport{
		{X: 1, Y: 2, Buttons: 1},
		{X: 3, Y: 4, Buttons: 0},
	}
	err := client.drag(context.Background(), reports)
	if jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindTimeout {
		t.Fatalf("error kind = %q, want timeout: %v", jetkvm.ErrorKindOf(err), err)
	}
	if !errors.Is(err, jetkvm.ErrNeutralizeUnverified) {
		t.Fatalf("drag timeout lost neutralization warning: %v", err)
	}
	if firstCalls != 1 {
		t.Fatalf("timed-out drag calls = %d, want 1", firstCalls)
	}
	select {
	case <-firstClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("unverified session was not retired")
	}

	if err := client.drag(context.Background(), reports); err != nil {
		t.Fatalf("drag after successful cleanup: %v", err)
	}
	if firstCalls != 1 || secondCalls != 1 || connectAttempts != 2 {
		t.Fatalf("replacement counts: first=%d second=%d connects=%d, want 1/1/2",
			firstCalls, secondCalls, connectAttempts)
	}
}

func TestRetryingDeviceAcquireRespectsCanceledCaller(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	statusCalls := 0
	connectAttempts := 0
	mock := &mockDevice{statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
		statusCalls++
		close(firstStarted)
		<-releaseFirst
		return jetkvm.StatusResult{RPCReachable: true}, nil
	}}
	connector := func(context.Context) (device, error) {
		connectAttempts++
		return mock, nil
	}
	client := newRetryingDeviceWithConnector(false, connector, immediateRetryPolicy(1, nil))

	firstDone := make(chan error, 1)
	go func() {
		_, err := client.status(context.Background())
		firstDone <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		close(releaseFirst)
		t.Fatal("first status call did not acquire the device gate")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.status(ctx)
	if jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindTimeout || !strings.Contains(err.Error(), "waiting for another MCP device call") {
		t.Fatalf("contended canceled call = %v, want stable acquire timeout", err)
	}
	if connectAttempts != 1 || statusCalls != 1 {
		t.Fatalf("canceled waiter reached device: connects=%d status=%d", connectAttempts, statusCalls)
	}

	close(releaseFirst)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first status call: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first status call did not finish after its test gate was released")
	}
}

func TestRetryBackoffCancellationStopsBeforeAnotherAttempt(t *testing.T) {
	connectAttempts := 0
	sleepStarted := make(chan struct{})
	connector := func(context.Context) (device, error) {
		connectAttempts++
		return nil, deviceFailure(jetkvm.ErrorKindUnreachable, "connect")
	}
	policy := retryPolicy{
		maxAttempts: 3,
		baseDelay:   time.Hour,
		maxDelay:    time.Hour,
		jitter:      func(delay time.Duration) time.Duration { return delay },
		sleep: func(ctx context.Context, delay time.Duration) error {
			close(sleepStarted)
			return sleepContext(ctx, delay)
		},
	}
	client := newRetryingDeviceWithConnector(false, connector, policy)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.status(ctx)
		done <- err
	}()
	select {
	case <-sleepStarted:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("retry call did not enter backoff")
	}
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retry call did not stop after cancellation")
	}
	if jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindTimeout || !strings.Contains(err.Error(), "during retry backoff") {
		t.Fatalf("backoff cancellation = %v, want stable timeout", err)
	}
	if connectAttempts != 1 {
		t.Fatalf("backoff cancellation allowed %d connect attempts, want 1", connectAttempts)
	}
}

func TestRetryingDeviceBadFrameDiscardsWithoutRetryingOperation(t *testing.T) {
	connectAttempts := 0
	statusCalls := 0
	closed := 0
	first := &mockDevice{
		statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
			statusCalls++
			return jetkvm.StatusResult{}, deviceFailure(jetkvm.ErrorKindBadFrame, "status response")
		},
		closeFunc: func(context.Context) error { closed++; return nil },
	}
	second := &mockDevice{statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
		statusCalls++
		return jetkvm.StatusResult{RPCReachable: true}, nil
	}}
	connector := func(context.Context) (device, error) {
		connectAttempts++
		if connectAttempts == 1 {
			return first, nil
		}
		return second, nil
	}
	client := newRetryingDeviceWithConnector(false, connector, immediateRetryPolicy(3, nil))

	if _, err := client.status(context.Background()); jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindBadFrame {
		t.Fatalf("first status error = %v, want bad-frame", err)
	}
	if connectAttempts != 1 || statusCalls != 1 || closed != 1 {
		t.Fatalf("bad-frame path retried or retained session: connects=%d calls=%d closed=%d", connectAttempts, statusCalls, closed)
	}
	status, err := client.status(context.Background())
	if err != nil || !status.RPCReachable {
		t.Fatalf("status after bad-frame discard = %+v, %v", status, err)
	}
	if connectAttempts != 2 || statusCalls != 2 {
		t.Fatalf("next call did not reconnect once: connects=%d calls=%d", connectAttempts, statusCalls)
	}
}

func TestRetryingDeviceOrdersDiscardCleanupBeforeReplacementControl(t *testing.T) {
	closeStarted := make(chan struct{})
	allowClose := make(chan struct{})
	closeDone := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-allowClose:
		default:
			close(allowClose)
		}
	})

	first := &mockDevice{
		statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
			return jetkvm.StatusResult{}, deviceFailure(jetkvm.ErrorKindBadFrame, "status response")
		},
		closeFunc: func(context.Context) error {
			close(closeStarted)
			<-allowClose
			close(closeDone)
			return nil
		},
	}
	pressCalls := 0
	releaseCalls := 0
	second := &mockDevice{
		statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
			return jetkvm.StatusResult{RPCReachable: true}, nil
		},
		releaseAllFunc: func(context.Context) (bool, error) {
			releaseCalls++
			return true, nil
		},
		mouseButtonFunc: func(_ context.Context, button byte, pressed bool) error {
			pressCalls++
			if button != jetkvm.MouseButtonRight || !pressed {
				t.Errorf("replacement mouse-button = %d/%v, want right/press", button, pressed)
			}
			return nil
		},
	}
	connectAttempts := 0
	connector := func(context.Context) (device, error) {
		connectAttempts++
		if connectAttempts == 1 {
			return first, nil
		}
		return second, nil
	}
	client := newRetryingDeviceWithConnector(true, connector, immediateRetryPolicy(1, nil))

	if _, err := client.status(context.Background()); jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindBadFrame {
		t.Fatalf("first status error = %v, want bad-frame", err)
	}
	select {
	case <-closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("discarded control session did not begin cleanup")
	}

	// Read-only recovery is not delayed by the old session's safety close.
	statusCtx, cancelStatus := context.WithTimeout(context.Background(), time.Second)
	status, err := client.status(statusCtx)
	cancelStatus()
	if err != nil || !status.RPCReachable {
		t.Fatalf("replacement read-only status = %+v, %v", status, err)
	}
	// A replacement's generic zero state cannot clear an old absolute-button
	// report without the old session's exact coordinates. Release-all must wait
	// behind the same cleanup barrier as every other control mutation.
	releaseCtx, cancelRelease := context.WithTimeout(context.Background(), 50*time.Millisecond)
	released, err := client.releaseAll(releaseCtx)
	cancelRelease()
	if released || jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindTimeout ||
		!strings.Contains(err.Error(), "waiting for prior device cleanup") {
		t.Fatalf("release-all during cleanup = %v, %v, want stable cleanup-wait timeout", released, err)
	}
	if releaseCalls != 0 {
		t.Fatalf("replacement release-all ran %d times before old cleanup, want zero", releaseCalls)
	}

	// A control mutation must not cross the adapter boundary until cleanup is
	// complete. Its own deadline still bounds the wait.
	pressCtx, cancelPress := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err = client.mouseButton(pressCtx, jetkvm.MouseButtonRight, true)
	cancelPress()
	if jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindTimeout ||
		!strings.Contains(err.Error(), "waiting for prior device cleanup") {
		t.Fatalf("mouse-button during cleanup = %v, want stable cleanup-wait timeout", err)
	}
	if pressCalls != 0 {
		t.Fatalf("replacement control ran %d times before old cleanup, want zero", pressCalls)
	}

	close(allowClose)
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("discarded control session cleanup did not finish")
	}
	if released, err := client.releaseAll(context.Background()); err != nil || !released {
		t.Fatalf("release-all after cleanup = %v, %v", released, err)
	}
	if err := client.mouseButton(context.Background(), jetkvm.MouseButtonRight, true); err != nil {
		t.Fatalf("mouse-button after cleanup: %v", err)
	}
	if releaseCalls != 1 || pressCalls != 1 || connectAttempts != 2 {
		t.Fatalf("ordered replacement counts: releases=%d presses=%d connects=%d, want 1/1/2", releaseCalls, pressCalls, connectAttempts)
	}
}

func TestRetryingDeviceFailsClosedAfterCleanupFailure(t *testing.T) {
	cleanupDone := make(chan struct{})
	cleanupErr := fmt.Errorf("old absolute-button cleanup: %w", jetkvm.ErrNeutralizeUnverified)
	first := &mockDevice{
		statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
			return jetkvm.StatusResult{}, deviceFailure(jetkvm.ErrorKindBadFrame, "status response")
		},
		closeFunc: func(context.Context) error {
			defer close(cleanupDone)
			return cleanupErr
		},
	}
	releaseCalls := 0
	pressCalls := 0
	second := &mockDevice{
		statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
			return jetkvm.StatusResult{RPCReachable: true}, nil
		},
		releaseAllFunc: func(context.Context) (bool, error) {
			releaseCalls++
			return true, nil
		},
		mouseButtonFunc: func(context.Context, byte, bool) error {
			pressCalls++
			return nil
		},
	}
	connectAttempts := 0
	client := newRetryingDeviceWithConnector(true, func(context.Context) (device, error) {
		connectAttempts++
		if connectAttempts == 1 {
			return first, nil
		}
		return second, nil
	}, immediateRetryPolicy(1, nil))

	if _, err := client.status(context.Background()); jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindBadFrame {
		t.Fatalf("first status error = %v, want bad-frame", err)
	}
	select {
	case <-cleanupDone:
	case <-time.After(2 * time.Second):
		t.Fatal("discarded session cleanup did not finish")
	}

	// Read-only recovery remains available on a replacement connection.
	status, err := client.status(context.Background())
	if err != nil || !status.RPCReachable {
		t.Fatalf("status after failed cleanup = %+v, %v", status, err)
	}
	if released, err := client.releaseAll(context.Background()); released || !errors.Is(err, jetkvm.ErrNeutralizeUnverified) {
		t.Fatalf("release-all after failed cleanup = %v, %v, want fail-closed warning", released, err)
	}
	if err := client.mouseButton(context.Background(), jetkvm.MouseButtonLeft, true); !errors.Is(err, jetkvm.ErrNeutralizeUnverified) {
		t.Fatalf("mouse-button after failed cleanup = %v, want fail-closed warning", err)
	}
	if releaseCalls != 0 || pressCalls != 0 || connectAttempts != 2 {
		t.Fatalf("replacement control crossed failed cleanup: releases=%d presses=%d connects=%d", releaseCalls, pressCalls, connectAttempts)
	}
}

func TestRetryingDeviceDiscardsScrollSessionMarkedClosedOnTimeout(t *testing.T) {
	cleanupDone := make(chan struct{})
	scrollCalls := 0
	first := &mockDevice{
		scrollFunc: func(context.Context, int8, int8) error {
			scrollCalls++
			return errors.Join(
				&jetkvm.DeviceError{Kind: jetkvm.ErrorKindTimeout, Operation: "waiting for wheelReport response"},
				jetkvm.ErrHIDClosed,
			)
		},
		closeFunc: func(context.Context) error {
			close(cleanupDone)
			return nil
		},
	}
	second := &mockDevice{statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
		return jetkvm.StatusResult{RPCReachable: true}, nil
	}}
	connectAttempts := 0
	client := newRetryingDeviceWithConnector(true, func(context.Context) (device, error) {
		connectAttempts++
		if connectAttempts == 1 {
			return first, nil
		}
		return second, nil
	}, immediateRetryPolicy(3, nil))

	err := client.scroll(context.Background(), 0, 1)
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindTimeout {
		t.Fatalf("ambiguous scroll error kind = %q, want timeout: %v", kind, err)
	}
	if scrollCalls != 1 || connectAttempts != 1 {
		t.Fatalf("ambiguous scroll was retried: calls=%d connects=%d", scrollCalls, connectAttempts)
	}
	select {
	case <-cleanupDone:
	case <-time.After(2 * time.Second):
		t.Fatal("closed scroll session was not discarded")
	}
	status, err := client.status(context.Background())
	if err != nil || !status.RPCReachable {
		t.Fatalf("status after closed scroll session = %+v, %v", status, err)
	}
	if connectAttempts != 2 {
		t.Fatalf("post-scroll status connects = %d, want replacement connection", connectAttempts)
	}
}

func TestRetryingDeviceCloseWaitsForPendingCleanupAndReportsItsFailure(t *testing.T) {
	closeStarted := make(chan struct{})
	allowClose := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-allowClose:
		default:
			close(allowClose)
		}
	})
	wantCloseErr := errors.New("discarded session neutralization failed")
	first := &mockDevice{
		statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
			return jetkvm.StatusResult{}, deviceFailure(jetkvm.ErrorKindBadFrame, "status response")
		},
		closeFunc: func(context.Context) error {
			close(closeStarted)
			<-allowClose
			return wantCloseErr
		},
	}
	client := newRetryingDeviceWithConnector(true, func(context.Context) (device, error) {
		return first, nil
	}, immediateRetryPolicy(1, nil))

	if _, err := client.status(context.Background()); jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindBadFrame {
		t.Fatalf("status error = %v, want bad-frame", err)
	}
	select {
	case <-closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("discarded session did not begin cleanup")
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := client.close(closeCtx)
	cancelClose()
	if jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindTimeout ||
		!strings.Contains(err.Error(), "waiting for prior device cleanup") {
		t.Fatalf("close during pending cleanup = %v, want stable timeout", err)
	}

	close(allowClose)
	if err := client.close(context.Background()); !errors.Is(err, wantCloseErr) {
		t.Fatalf("close after cleanup = %v, want accumulated %v", err, wantCloseErr)
	}
}

func TestRetryingDeviceScreenshotPreflightPrecedesSuccessfulCapture(t *testing.T) {
	var events []string
	want := jetkvm.Screenshot{ScreenshotResult: jetkvm.ScreenshotResult{Width: 32, Height: 32, Fresh: true}}
	mock := &mockDevice{screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
		events = append(events, "capture")
		return want, nil
	}}
	connector := func(context.Context) (device, error) {
		events = append(events, "connect")
		return mock, nil
	}
	client := newRetryingDeviceWithConnector(false, connector, immediateRetryPolicy(1, nil))
	client.decoderPreflight = func(context.Context) error {
		events = append(events, "preflight")
		return nil
	}

	got, err := client.captureScreenshot(context.Background())
	if err != nil {
		t.Fatalf("captureScreenshot: %v", err)
	}
	if got.Width != want.Width || got.Height != want.Height || !got.Fresh {
		t.Fatalf("screenshot = %+v, want %+v", got, want)
	}
	if strings.Join(events, ",") != "preflight,connect,capture" {
		t.Fatalf("screenshot event order = %v, want preflight/connect/capture", events)
	}
}

func TestRetryingDeviceWaitStablePreflightAndOptionForwarding(t *testing.T) {
	var events []string
	threshold := 0.25
	stableFrames := 4
	pollInterval := 75 * time.Millisecond
	opts := jetkvm.WaitStableOptions{
		Threshold:    &threshold,
		StableFrames: &stableFrames,
		PollInterval: &pollInterval,
	}
	want := jetkvm.WaitStableResult{
		Settled:             true,
		FramesSampled:       7,
		FinalChangeFraction: 0.125,
		Elapsed:             time.Second,
	}
	mock := &mockDevice{waitStableFunc: func(_ context.Context, got jetkvm.WaitStableOptions) (jetkvm.WaitStableResult, error) {
		events = append(events, "wait")
		if got.Threshold != opts.Threshold || got.StableFrames != opts.StableFrames || got.PollInterval != opts.PollInterval {
			t.Errorf("options were not forwarded unchanged: got=%+v want=%+v", got, opts)
		}
		return want, nil
	}}
	connector := func(context.Context) (device, error) {
		events = append(events, "connect")
		return mock, nil
	}
	client := newRetryingDeviceWithConnector(false, connector, immediateRetryPolicy(1, nil))
	client.decoderPreflight = func(context.Context) error {
		events = append(events, "preflight")
		return nil
	}

	got, err := client.waitStable(context.Background(), opts)
	if err != nil {
		t.Fatalf("waitStable: %v", err)
	}
	if got != want {
		t.Fatalf("result = %+v, want %+v", got, want)
	}
	if strings.Join(events, ",") != "preflight,connect,wait" {
		t.Fatalf("event order = %v, want preflight/connect/wait", events)
	}
}

func TestRetryingDeviceRetriesWaitStableAfterUnreachableOperation(t *testing.T) {
	preflightCalls := 0
	connectAttempts := 0
	waitCalls := 0
	want := jetkvm.WaitStableResult{Settled: true, FramesSampled: 3}
	connector := func(context.Context) (device, error) {
		connectAttempts++
		attempt := connectAttempts
		return &mockDevice{waitStableFunc: func(context.Context, jetkvm.WaitStableOptions) (jetkvm.WaitStableResult, error) {
			waitCalls++
			if attempt == 1 {
				return jetkvm.WaitStableResult{}, deviceFailure(jetkvm.ErrorKindUnreachable, "wait stable")
			}
			return want, nil
		}}, nil
	}
	client := newRetryingDeviceWithConnector(false, connector, immediateRetryPolicy(2, nil))
	client.decoderPreflight = func(context.Context) error {
		preflightCalls++
		return nil
	}

	got, err := client.waitStable(context.Background(), jetkvm.WaitStableOptions{})
	if err != nil {
		t.Fatalf("waitStable: %v", err)
	}
	if got != want {
		t.Fatalf("result = %+v, want %+v", got, want)
	}
	if preflightCalls != 1 || connectAttempts != 2 || waitCalls != 2 {
		t.Fatalf("retry path: preflights=%d connects=%d calls=%d, want 1/2/2",
			preflightCalls, connectAttempts, waitCalls)
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
