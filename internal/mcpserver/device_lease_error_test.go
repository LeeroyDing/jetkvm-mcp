package mcpserver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

// cancelAtErrCheckContext lets these adapter tests cancel at the boundary
// between two HID operations without adding a timing race to the loopback
// WebRTC harness. One successful HID send performs three context Err checks:
// before enqueue, on writer dequeue, and immediately before transport Send.
// Canceling on the next check therefore makes the following operation fail
// before it can put bytes on the wire.
type cancelAtErrCheckContext struct {
	context.Context
	cancelAt int32
	checks   atomic.Int32
	done     chan struct{}
	once     sync.Once
}

func newCancelAtErrCheckContext(cancelAt int32) *cancelAtErrCheckContext {
	return &cancelAtErrCheckContext{
		Context:  context.Background(),
		cancelAt: cancelAt,
		done:     make(chan struct{}),
	}
}

func (c *cancelAtErrCheckContext) Done() <-chan struct{} { return c.done }

func (c *cancelAtErrCheckContext) Err() error {
	if c.checks.Add(1) >= c.cancelAt {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	return nil
}

var (
	leaseErrorNeutralKeyboard = []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	leaseErrorNeutralMouse    = []byte{0x06, 0x00, 0x00, 0x00}
	leaseErrorRecoveryKey     = []byte{0x02, 0x00, 0x08}
)

func requireLeaseErrorWireFrames(t *testing.T, fd *fakeDevice, want ...[]byte) {
	t.Helper()
	requireDeviceCoverageFrames(t, fd, want...)

	// Give the remote OnMessage callback a short quiet window so an unsafe
	// trailing report cannot arrive just after the expected neutralization.
	timer := time.NewTimer(30 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	_, got := fd.wireCounts()
	if got != len(want) {
		t.Fatalf("HID request count = %d, want exactly %d", got, len(want))
	}
	if _, pending := fd.pendingFrames(); pending != 0 {
		t.Fatalf("unconsumed HID frames = %d, want zero", pending)
	}
}

func requireLeaseRecovery(t *testing.T, ctx context.Context, device *clientDevice) {
	t.Helper()
	if err := device.keypress(ctx, 0, 0x08); err != nil {
		t.Fatalf("keypress under fresh lease after cleanup: %v", err)
	}
}

func TestClientDeviceRetainedKeyboardReleaseFailuresNeutralizeLease(t *testing.T) {
	tests := []struct {
		name      string
		operation func(context.Context, *clientDevice) error
		press     []byte
	}{
		{
			name: "keypress release",
			operation: func(ctx context.Context, device *clientDevice) error {
				return device.keypress(ctx, 0x02, 0x04)
			},
			press: []byte{0x02, 0x02, 0x04},
		},
		{
			name: "key combo release",
			operation: func(ctx context.Context, device *clientDevice) error {
				return device.keyCombo(ctx, 0x05, []byte{0x4c, 0x2a})
			},
			press: []byte{0x02, 0x05, 0x4c, 0x2a},
		},
		{
			name: "held key release",
			operation: func(ctx context.Context, device *clientDevice) error {
				return device.holdKey(ctx, 0x01, []byte{0x06}, 1)
			},
			press: []byte{0x02, 0x01, 0x06},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fd, device, healthyCtx := newDeviceCoverageHarness(t)
			if err := device.mouseButton(healthyCtx, jetkvm.MouseButtonLeft, true); err != nil {
				t.Fatalf("retain left mouse button: %v", err)
			}

			err := test.operation(newCancelAtErrCheckContext(4), device)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("operation error = %v, want context.Canceled", err)
			}
			assertClientDeviceButtonsCleared(t, device)
			requireLeaseRecovery(t, healthyCtx, device)

			requireLeaseErrorWireFrames(t, fd,
				[]byte{0x06, 0x00, 0x00, 0x01},
				test.press,
				leaseErrorNeutralKeyboard,
				leaseErrorNeutralMouse,
				leaseErrorRecoveryKey,
				leaseErrorNeutralKeyboard,
				leaseErrorNeutralMouse,
			)
			assertClientDeviceButtonsCleared(t, device)
		})
	}
}

func TestClientDeviceRetainedPointerRestoreFailuresNeutralizeBothInterfaces(t *testing.T) {
	const (
		x = 123
		y = 456
	)
	combinedPointer := []byte{
		0x03,
		0x00, 0x00, 0x00, 0x7b,
		0x00, 0x00, 0x01, 0xc8,
		0x03,
	}
	neutralPointer := []byte{
		0x03,
		0x00, 0x00, 0x00, 0x7b,
		0x00, 0x00, 0x01, 0xc8,
		0x00,
	}
	tests := []struct {
		name      string
		operation func(context.Context, *clientDevice) error
	}{
		{
			name: "mouse move sticky-state restore",
			operation: func(ctx context.Context, device *clientDevice) error {
				return device.mouseMove(ctx, x, y, jetkvm.MouseButtonRight)
			},
		},
		{
			name: "direct drag sticky-state restore",
			operation: func(ctx context.Context, device *clientDevice) error {
				return device.drag(ctx, []jetkvm.PointerDragReport{{
					X: x, Y: y, Buttons: int(jetkvm.MouseButtonRight),
				}})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fd, device, healthyCtx := newDeviceCoverageHarness(t)
			if err := device.mouseButton(healthyCtx, jetkvm.MouseButtonLeft, true); err != nil {
				t.Fatalf("retain left mouse button: %v", err)
			}

			err := test.operation(newCancelAtErrCheckContext(4), device)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("operation error = %v, want context.Canceled", err)
			}
			assertClientDeviceButtonsCleared(t, device)
			requireLeaseRecovery(t, healthyCtx, device)

			requireLeaseErrorWireFrames(t, fd,
				[]byte{0x06, 0x00, 0x00, 0x01},
				combinedPointer,
				leaseErrorNeutralKeyboard,
				leaseErrorNeutralMouse,
				neutralPointer,
				leaseErrorRecoveryKey,
				leaseErrorNeutralKeyboard,
				leaseErrorNeutralMouse,
			)
			absolute, relative := fd.mouseInterfaceState()
			if absolute.X != x || absolute.Y != y || absolute.Buttons != 0 {
				t.Fatalf("absolute mouse after cleanup = (%d,%d)/%#02x, want (%d,%d)/0",
					absolute.X, absolute.Y, absolute.Buttons, x, y)
			}
			if relative.Buttons != 0 {
				t.Fatalf("relative mouse buttons after cleanup = %#02x, want 0", relative.Buttons)
			}
			assertClientDeviceButtonsCleared(t, device)
		})
	}
}

func TestClientDeviceFirstMutationCancellationEmitsOnlyNeutralReports(t *testing.T) {
	tests := []struct {
		name      string
		operation func(context.Context, *clientDevice) error
	}{
		{
			name: "hold key",
			operation: func(ctx context.Context, device *clientDevice) error {
				return device.holdKey(ctx, 0x01, []byte{0x06}, 1)
			},
		},
		{
			name: "drag",
			operation: func(ctx context.Context, device *clientDevice) error {
				return device.drag(ctx, []jetkvm.PointerDragReport{{
					X: 123, Y: 456, Buttons: int(jetkvm.MouseButtonLeft),
				}})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fd, device, healthyCtx := newDeviceCoverageHarness(t)
			err := test.operation(newCancelAtErrCheckContext(6), device)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("operation error = %v, want context.Canceled", err)
			}
			requireLeaseRecovery(t, healthyCtx, device)

			// The canceled key/pointer report must never reach the peer. The first
			// generation emits only its canonical terminal neutralization, after
			// which a fresh generation remains usable.
			requireLeaseErrorWireFrames(t, fd,
				leaseErrorNeutralKeyboard,
				leaseErrorNeutralMouse,
				leaseErrorRecoveryKey,
				leaseErrorNeutralKeyboard,
				leaseErrorNeutralMouse,
			)
			absolute, relative := fd.mouseInterfaceState()
			if absolute.Reports != 0 || absolute.Buttons != 0 || relative.Buttons != 0 {
				t.Fatalf("canceled mutation changed mouse state: absolute %+v relative %+v", absolute, relative)
			}
			assertClientDeviceButtonsCleared(t, device)
		})
	}
}
