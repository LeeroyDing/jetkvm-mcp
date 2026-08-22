package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func newDeviceCoverageHarness(t *testing.T) (*fakeDevice, *clientDevice, context.Context) {
	t.Helper()

	fd := startFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 15*time.Second))
	t.Cleanup(cancel)
	client, err := jetkvm.Connect(ctx, jetkvm.Options{
		BaseURL:      fd.baseURL(),
		AllowControl: true,
	})
	if err != nil {
		t.Fatalf("jetkvm.Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return fd, &clientDevice{client: client}, ctx
}

func requireDeviceCoverageFrames(t *testing.T, fd *fakeDevice, want ...[]byte) {
	t.Helper()
	for i, expected := range want {
		if got := fd.nextHIDFrame(t); !bytes.Equal(got, expected) {
			t.Fatalf("HID frame %d = % x, want % x", i, got, expected)
		}
	}
}

func TestClientDeviceRetainedKeyboardOperationsUseExactWireReports(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *clientDevice) error
		want []byte
	}{
		{
			name: "keypress",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.keypress(ctx, 0x02, 0x04)
			},
			want: []byte{0x02, 0x02, 0x04},
		},
		{
			name: "key combo",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.keyCombo(ctx, 0x05, []byte{0x4c, 0x2a})
			},
			want: []byte{0x02, 0x05, 0x4c, 0x2a},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fd, device, ctx := newDeviceCoverageHarness(t)
			if err := device.mouseButton(ctx, jetkvm.MouseButtonLeft, true); err != nil {
				t.Fatalf("retain left mouse button: %v", err)
			}
			if err := test.run(ctx, device); err != nil {
				t.Fatalf("operation: %v", err)
			}
			if err := device.mouseButton(ctx, jetkvm.MouseButtonLeft, false); err != nil {
				t.Fatalf("release left mouse button: %v", err)
			}

			requireDeviceCoverageFrames(t, fd,
				[]byte{0x06, 0x00, 0x00, 0x01},
				test.want,
				[]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
				[]byte{0x06, 0x00, 0x00, 0x00},
				[]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
				[]byte{0x06, 0x00, 0x00, 0x00},
			)
			_, hid := fd.wireCounts()
			if hid != 6 {
				t.Fatalf("HID request count = %d, want exactly 6", hid)
			}
			assertClientDeviceButtonsCleared(t, device)
		})
	}
}

func TestClientDeviceInvalidInputDoesNotDisturbRetainedLease(t *testing.T) {
	tests := []struct {
		name       string
		run        func(context.Context, *clientDevice) error
		wantErrSub string
	}{
		{
			name: "hold rejects neutral chord",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.holdKey(ctx, 0, nil, 1)
			},
			wantErrSub: "at least one modifier or key",
		},
		{
			name: "hold rejects duration before lease",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.holdKey(ctx, 0, []byte{0x04}, 0)
			},
			wantErrSub: "hold duration must be",
		},
		{
			name: "mouse button rejects zero mask",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.mouseButton(ctx, 0, true)
			},
			wantErrSub: "invalid mouse button mask",
		},
		{
			name: "mouse button rejects combined mask",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.mouseButton(ctx, jetkvm.MouseButtonLeft|jetkvm.MouseButtonRight, false)
			},
			wantErrSub: "invalid mouse button mask",
		},
		{
			name: "drag rejects empty reports",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.drag(ctx, nil)
			},
			wantErrSub: "must not be empty",
		},
		{
			name: "drag rejects movement-only reports",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.drag(ctx, []jetkvm.PointerDragReport{{X: 10, Y: 20, Buttons: 0}})
			},
			wantErrSub: "nonzero button mask",
		},
		{
			name: "drag rejects coordinate outside wire range",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.drag(ctx, []jetkvm.PointerDragReport{{
					X:       jetkvm.MaxAbsoluteCoordinate + 1,
					Y:       20,
					Buttons: int(jetkvm.MouseButtonLeft),
				}})
			},
			wantErrSub: "x and y must be",
		},
	}

	fd, device, ctx := newDeviceCoverageHarness(t)
	if err := device.mouseButton(ctx, jetkvm.MouseButtonLeft, true); err != nil {
		t.Fatalf("retain left mouse button: %v", err)
	}
	requireDeviceCoverageFrames(t, fd, []byte{0x06, 0x00, 0x00, 0x01})
	device.controlMu.Lock()
	retained := device.buttonLease
	device.controlMu.Unlock()
	if retained == nil {
		t.Fatal("mouse-button press did not retain its lease")
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, before := fd.wireCounts()
			err := test.run(ctx, device)
			if err == nil || !strings.Contains(err.Error(), test.wantErrSub) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErrSub)
			}
			_, after := fd.wireCounts()
			if after != before {
				t.Fatalf("invalid input emitted %d HID reports, want 0", after-before)
			}

			device.controlMu.Lock()
			gotLease, gotButtons := device.buttonLease, device.heldButtons
			device.controlMu.Unlock()
			if gotLease != retained || gotButtons != jetkvm.MouseButtonLeft {
				t.Fatalf("retained lease after rejection = (%v, %#02x), want original/%#02x",
					gotLease == retained, gotButtons, jetkvm.MouseButtonLeft)
			}
		})
	}

	if err := device.mouseButton(ctx, jetkvm.MouseButtonLeft, false); err != nil {
		t.Fatalf("release retained button: %v", err)
	}
	requireDeviceCoverageFrames(t, fd,
		[]byte{0x06, 0x00, 0x00, 0x00},
		[]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		[]byte{0x06, 0x00, 0x00, 0x00},
	)
	_, hid := fd.wireCounts()
	if hid != 4 {
		t.Fatalf("HID request count = %d, want press plus three release reports", hid)
	}
	assertClientDeviceButtonsCleared(t, device)
}

func TestClientDeviceLeaseAcquisitionCancellationEmitsNoOperationReport(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *clientDevice) error
	}{
		{
			name: "release all",
			run: func(ctx context.Context, device *clientDevice) error {
				_, err := device.releaseAll(ctx)
				return err
			},
		},
		{
			name: "keypress",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.keypress(ctx, 0x02, 0x04)
			},
		},
		{
			name: "key combo",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.keyCombo(ctx, 0x05, []byte{0x4c})
			},
		},
		{
			name: "hold key",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.holdKey(ctx, 0, []byte{0x04}, 1)
			},
		},
		{
			name: "mouse move",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.mouseMove(ctx, 123, 456, 0)
			},
		},
		{
			name: "mouse button",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.mouseButton(ctx, jetkvm.MouseButtonRight, true)
			},
		},
		{
			name: "drag",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.drag(ctx, []jetkvm.PointerDragReport{
					{X: 10, Y: 20, Buttons: int(jetkvm.MouseButtonLeft)},
					{X: 30, Y: 40, Buttons: 0},
				})
			},
		},
	}

	fd, device, ctx := newDeviceCoverageHarness(t)
	lease, err := device.client.Control()
	if err != nil {
		t.Fatalf("Control: %v", err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			blocker, err := lease.AcquirePersistent(ctx, jetkvm.DefaultControlLeaseTimeout)
			if err != nil {
				t.Fatalf("AcquirePersistent blocker: %v", err)
			}
			callCtx, cancel := context.WithCancel(context.Background())
			cancel()
			_, before := fd.wireCounts()
			gotErr := test.run(callCtx, device)
			_, after := fd.wireCounts()
			releaseErr := blocker.Release()

			if !errors.Is(gotErr, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", gotErr)
			}
			if after != before {
				t.Fatalf("canceled acquisition emitted %d HID reports, want 0", after-before)
			}
			if releaseErr != nil {
				t.Fatalf("release blocker: %v", releaseErr)
			}
			requireDeviceCoverageFrames(t, fd,
				[]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
				[]byte{0x06, 0x00, 0x00, 0x00},
			)
		})
	}
}

func TestClientDeviceRetainedOperationFailuresReleaseLeaseWithExactWireReports(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *clientDevice) error
	}{
		{
			name: "keypress",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.keypress(ctx, 0x02, 0x04)
			},
		},
		{
			name: "key combo",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.keyCombo(ctx, 0x05, []byte{0x4c, 0x2a})
			},
		},
		{
			name: "hold key",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.holdKey(ctx, 0, []byte{0x04}, 1)
			},
		},
		{
			name: "mouse move",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.mouseMove(ctx, 123, 456, jetkvm.MouseButtonRight)
			},
		},
		{
			name: "mouse button transition",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.mouseButton(ctx, jetkvm.MouseButtonRight, true)
			},
		},
		{
			name: "drag",
			run: func(ctx context.Context, device *clientDevice) error {
				return device.drag(ctx, []jetkvm.PointerDragReport{
					{X: 10, Y: 20, Buttons: int(jetkvm.MouseButtonRight)},
					{X: 30, Y: 40, Buttons: 0},
				})
			},
		},
	}

	fd, device, ctx := newDeviceCoverageHarness(t)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, before := fd.wireCounts()
			if err := device.mouseButton(ctx, jetkvm.MouseButtonLeft, true); err != nil {
				t.Fatalf("retain left mouse button: %v", err)
			}
			requireDeviceCoverageFrames(t, fd, []byte{0x06, 0x00, 0x00, 0x01})

			callCtx, cancel := context.WithCancel(context.Background())
			cancel()
			err := test.run(callCtx, device)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			requireDeviceCoverageFrames(t, fd,
				[]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
				[]byte{0x06, 0x00, 0x00, 0x00},
			)
			_, after := fd.wireCounts()
			if after-before != 3 {
				t.Fatalf("HID request delta = %d, want press plus two cleanup reports", after-before)
			}
			assertClientDeviceButtonsCleared(t, device)
		})
	}
}

func TestClientDeviceDiscardsCompletedRetainedLeaseState(t *testing.T) {
	tests := []struct {
		name          string
		recordPointer bool
		want          [][]byte
	}{
		{
			name: "relative button only",
			want: [][]byte{
				{0x06, 0x00, 0x00, 0x01},
				{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
				{0x06, 0x00, 0x00, 0x00},
			},
		},
		{
			name:          "mirrored absolute button",
			recordPointer: true,
			want: [][]byte{
				{0x06, 0x00, 0x00, 0x01},
				{0x03, 0x00, 0x00, 0x00, 0x6f, 0x00, 0x00, 0x00, 0xde, 0x01},
				{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
				{0x06, 0x00, 0x00, 0x00},
				{0x03, 0x00, 0x00, 0x00, 0x6f, 0x00, 0x00, 0x00, 0xde, 0x00},
			},
		},
	}

	fd, device, ctx := newDeviceCoverageHarness(t)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, before := fd.wireCounts()
			if err := device.mouseButton(ctx, jetkvm.MouseButtonLeft, true); err != nil {
				t.Fatalf("retain left mouse button: %v", err)
			}
			if test.recordPointer {
				if err := device.mouseMove(ctx, 111, 222, 0); err != nil {
					t.Fatalf("record absolute retained state: %v", err)
				}
			}

			// Keep the adapter watcher from winning the race to clear its own
			// bookkeeping. Ending this real holder models the state observed by
			// the next operation after watchdog or caller-driven lease completion.
			device.controlMu.Lock()
			held := device.buttonLease
			if held == nil {
				device.controlMu.Unlock()
				t.Fatal("mouse-button press did not retain its lease")
			}
			releaseErr := held.Release()
			gotHeld, gotButtons := device.liveButtonLeaseLocked()
			device.controlMu.Unlock()

			if releaseErr != nil {
				t.Fatalf("release retained holder: %v", releaseErr)
			}
			if gotHeld != nil || gotButtons != 0 {
				t.Fatalf("completed retained lease = (%v, %#02x), want nil/0", gotHeld != nil, gotButtons)
			}
			requireDeviceCoverageFrames(t, fd, test.want...)
			_, after := fd.wireCounts()
			if after-before != len(test.want) {
				t.Fatalf("HID request delta = %d, want exactly %d", after-before, len(test.want))
			}
			assertClientDeviceButtonsCleared(t, device)
		})
	}
}
