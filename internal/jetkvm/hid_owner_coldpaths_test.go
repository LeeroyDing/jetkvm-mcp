package jetkvm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// bufferedAmountProbeTransport reuses the package's HID transport fake while
// making each BufferedAmount observation explicit. That lets the tests move
// the transport level between the pre-select and post-signal checks without
// sleeps or scheduler-dependent polling.
type bufferedAmountProbeTransport struct {
	*fakeHIDTransport
	calls   chan struct{}
	amounts chan uint64
}

func newBufferedAmountProbeTransport() *bufferedAmountProbeTransport {
	return &bufferedAmountProbeTransport{
		fakeHIDTransport: &fakeHIDTransport{},
		calls:            make(chan struct{}),
		amounts:          make(chan uint64),
	}
}

func (t *bufferedAmountProbeTransport) BufferedAmount() uint64 {
	t.calls <- struct{}{}
	return <-t.amounts
}

func answerBufferedAmount(t *testing.T, transport *bufferedAmountProbeTransport, amount uint64) {
	t.Helper()
	ctx := contextWithTimeout(t, time.Second)
	select {
	case <-transport.calls:
	case <-ctx.Done():
		t.Fatal("timed out waiting for BufferedAmount observation")
	}
	select {
	case transport.amounts <- amount:
	case <-ctx.Done():
		t.Fatal("timed out returning scripted BufferedAmount")
	}
}

func receiveColdPathError(t *testing.T, result <-chan error) error {
	t.Helper()
	ctx := contextWithTimeout(t, time.Second)
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		t.Fatal("timed out waiting for cold-path result")
		return nil
	}
}

// cancelOnSecondErrContext models cancellation in the narrow window after a
// lifecycle token is acquired but before the caller starts using it.
type lifecycleCancelOnSecondErrContext struct {
	context.Context
	cancel context.CancelFunc
	calls  int
}

func newLifecycleCancelOnSecondErrContext() *lifecycleCancelOnSecondErrContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &lifecycleCancelOnSecondErrContext{Context: ctx, cancel: cancel}
}

func (c *lifecycleCancelOnSecondErrContext) Err() error {
	c.calls++
	if c.calls == 2 {
		c.cancel()
	}
	return c.Context.Err()
}

// cancelOnDoneContext cancels when a blocked operation starts waiting on
// Done. Occupying the other select arm makes the cancellation choice exact.
type cancelOnDoneContext struct {
	context.Context
	cancel context.CancelFunc
}

func newCancelOnDoneContext() *cancelOnDoneContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &cancelOnDoneContext{Context: ctx, cancel: cancel}
}

func (c *cancelOnDoneContext) Done() <-chan struct{} {
	c.cancel()
	return c.Context.Done()
}

func TestHIDRequestCompleteColdChannels(t *testing.T) {
	want := errors.New("first completion")

	// A request with no waiter is intentionally a no-op.
	hidRequest{}.complete(want)

	// A duplicate completion must not block the single writer or replace the
	// result already waiting for the caller.
	result := make(chan error, 1)
	result <- want
	hidRequest{result: result}.complete(errors.New("duplicate completion"))
	if got := <-result; !errors.Is(got, want) {
		t.Fatalf("completion result = %v, want original error %v", got, want)
	}
}

func TestHIDDrainCompletesPriorityRequestAfterClose(t *testing.T) {
	hc := &hidClient{
		state:      hidStateClosed,
		priorityCh: make(chan hidRequest, 1),
		sendCh:     make(chan hidRequest, 1),
	}
	result := make(chan error, 1)
	hc.priorityCh <- hidRequest{result: result}

	hc.drain()
	if err := receiveColdPathError(t, result); !errors.Is(err, ErrHIDClosed) {
		t.Fatalf("drained priority request error = %v, want ErrHIDClosed", err)
	}
}

func TestHIDDrainBarrierPrefersFinalTransportLevel(t *testing.T) {
	tests := []struct {
		name        string
		invalidate  bool
		finalAmount uint64
		wantErr     error
	}{
		{
			name:        "acknowledgement wins lease invalidation",
			invalidate:  true,
			finalAmount: 0,
		},
		{
			name:        "acknowledgement wins writer teardown",
			finalAmount: 0,
		},
		{
			name:        "writer teardown with bytes outstanding",
			finalAmount: 1,
			wantErr:     ErrHIDClosed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := newBufferedAmountProbeTransport()
			hc := &hidClient{
				channel:           transport,
				state:             hidStateClosed,
				closeCause:        errors.New("synthetic teardown"),
				bufferedAmountLow: make(chan struct{}, 1),
				writerDone:        make(chan struct{}),
			}
			invalidated := make(chan struct{})
			if !tt.invalidate {
				invalidated = nil
			}

			result := make(chan error, 1)
			go func() {
				result <- hc.waitBufferedAmountLowUntilInvalidated(context.Background(), invalidated)
			}()

			answerBufferedAmount(t, transport, 1)
			if tt.invalidate {
				close(invalidated)
			} else {
				close(hc.writerDone)
			}
			answerBufferedAmount(t, transport, tt.finalAmount)

			err := receiveColdPathError(t, result)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("drain barrier error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestHIDLockLifecycleCancellationEdges(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		hc := &hidClient{lifecycle: make(chan struct{}, 1)}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := hc.lockLifecycle(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("lockLifecycle error = %v, want context.Canceled", err)
		}
		if got := len(hc.lifecycle); got != 0 {
			t.Fatalf("lifecycle tokens after rejected lock = %d, want 0", got)
		}
	})

	t.Run("canceled immediately after acquisition", func(t *testing.T) {
		hc := &hidClient{lifecycle: make(chan struct{}, 1)}
		ctx := newLifecycleCancelOnSecondErrContext()
		defer ctx.cancel()

		if err := hc.lockLifecycle(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("lockLifecycle error = %v, want context.Canceled", err)
		}
		if got := len(hc.lifecycle); got != 0 {
			t.Fatalf("lifecycle tokens after post-acquire cancellation = %d, want 0", got)
		}
	})
}

func TestHIDReleaseColdFailures(t *testing.T) {
	t.Run("release blocked by lifecycle gate", func(t *testing.T) {
		hc, _ := newFakeHIDClient(t)
		hc.lifecycle <- struct{}{}
		ctx := newCancelOnDoneContext()
		defer ctx.cancel()

		err := hc.releaseAll(ctx)
		<-hc.lifecycle
		if !errors.Is(err, ErrNeutralizeUnverified) || !errors.Is(err, context.Canceled) {
			t.Fatalf("releaseAll error = %v, want neutralization and cancellation sentinels", err)
		}
		if got := hc.currentState(); got != hidStateReady {
			t.Fatalf("state after rejected release = %s, want ready", got)
		}
	})

	t.Run("close blocked by lifecycle gate still closes channel", func(t *testing.T) {
		hc, _ := newFakeHIDClient(t)
		hc.lifecycle <- struct{}{}
		ctx := newCancelOnDoneContext()
		defer ctx.cancel()

		err := newControlLease(hc).neutralize(ctx)
		<-hc.lifecycle
		if !errors.Is(err, ErrNeutralizeUnverified) || !errors.Is(err, context.Canceled) {
			t.Fatalf("neutralize error = %v, want neutralization and cancellation sentinels", err)
		}
		if got := hc.currentState(); got != hidStateClosed {
			t.Fatalf("state after rejected close neutralization = %s, want closed", got)
		}
	})

	t.Run("unready channel retains unexplained held state", func(t *testing.T) {
		hc, transport := newUnreadyHIDClient(t)
		hc.stateMu.Lock()
		hc.held.keyboard = true
		hc.stateMu.Unlock()

		err := hc.releaseAll(context.Background())
		if !errors.Is(err, ErrNeutralizeUnverified) {
			t.Fatalf("releaseAll error = %v, want ErrNeutralizeUnverified", err)
		}
		if got := transport.count(); got != 0 {
			t.Fatalf("unready release wrote %d frames, want 0", got)
		}
		if !hc.hasHeldState() {
			t.Fatal("failed release cleared conservative held state")
		}
	})

	t.Run("invalid absolute held state cannot be neutralized", func(t *testing.T) {
		hc, transport := newFakeHIDClient(t)
		before := transport.count()
		hc.stateMu.Lock()
		hc.held.absoluteButtons = true
		hc.stateMu.Unlock()

		err := hc.releaseAll(context.Background())
		if !errors.Is(err, ErrNeutralizeUnverified) {
			t.Fatalf("releaseAll error = %v, want ErrNeutralizeUnverified", err)
		}
		if got := transport.count(); got != before {
			t.Fatalf("invalid neutralization wrote frames: %d -> %d", before, got)
		}
		if !hc.hasHeldState() {
			t.Fatal("invalid neutralization cleared conservative held state")
		}
	})
}

func TestControlLeaseAcquirePersistentColdPaths(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		var lease *controlLease
		if held, err := lease.AcquirePersistent(context.Background(), time.Second); held != nil || !errors.Is(err, ErrControlDisabled) {
			t.Fatalf("AcquirePersistent = (%v, %v), want (nil, ErrControlDisabled)", held, err)
		}
	})

	t.Run("canceled while slot occupied", func(t *testing.T) {
		hc, _ := newFakeHIDClient(t)
		lease := newControlLease(hc)
		lease.slot <- struct{}{}
		ctx := newCancelOnDoneContext()
		defer ctx.cancel()

		held, err := lease.AcquirePersistent(ctx, time.Second)
		<-lease.slot
		if held != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("AcquirePersistent = (%v, %v), want (nil, context.Canceled)", held, err)
		}
	})

	t.Run("default timeout and release keyboard after release", func(t *testing.T) {
		hc, _ := newFakeHIDClient(t)
		lease := newControlLease(hc)
		held, err := lease.AcquirePersistent(context.Background(), 0)
		if err != nil {
			t.Fatalf("AcquirePersistent: %v", err)
		}
		if err := held.Release(); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if err := held.ReleaseKeyboard(context.Background()); !errors.Is(err, ErrStaleControlToken) {
			t.Fatalf("ReleaseKeyboard after Release = %v, want ErrStaleControlToken", err)
		}
	})
}

func TestControlLeaseNeutralizeDisabledIsNoOp(t *testing.T) {
	var lease *controlLease
	if err := lease.neutralize(context.Background()); err != nil {
		t.Fatalf("neutralize disabled lease = %v, want nil", err)
	}
}
