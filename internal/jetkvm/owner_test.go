package jetkvm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/hidproto"
)

func TestControlLeaseNilWhenDisabled(t *testing.T) {
	lease := newControlLease(nil)
	_, err := lease.Acquire(context.Background(), time.Second)
	if !errors.Is(err, ErrControlDisabled) {
		t.Fatalf("Acquire with control disabled = %v, want ErrControlDisabled", err)
	}
}

// TestControlLeaseRequiresReadinessHandshake proves the readiness gate is
// enforced at the lease boundary, not just deep in the writer: a lease
// cannot exist over a channel the device has not confirmed.
func TestControlLeaseRequiresReadinessHandshake(t *testing.T) {
	hc, _ := newUnreadyHIDClient(t)
	lease := newControlLease(hc)

	_, err := lease.Acquire(contextWithTimeout(t, time.Second), time.Second)
	if !errors.Is(err, ErrHIDNotReady) {
		t.Fatalf("Acquire before the handshake = %v, want ErrHIDNotReady", err)
	}
	// The exclusivity slot must be given back on a failed acquisition,
	// otherwise a single failure would wedge control forever.
	if len(lease.slot) != 0 {
		t.Fatal("a failed Acquire leaked the exclusivity slot")
	}
}

func TestControlLeaseAcquireSendReleaseClearsState(t *testing.T) {
	hc, fd := setupHIDPair(t)
	lease := newControlLease(hc)

	ctx := contextWithTimeout(t, 5*time.Second)
	held, err := lease.Acquire(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	if err := held.SendKeyboardReport(ctx, 0x02, []byte{0x04}); err != nil {
		t.Fatalf("SendKeyboardReport failed: %v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool {
		_, ok := fd.lastKeyboardReport()
		return ok
	})

	if err := held.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		kb, ok := fd.lastKeyboardReport()
		return ok && kb.Payload[0] == 0 && allZero(kb.Payload[1:])
	})
}

func TestControlLeaseSendAfterReleaseFails(t *testing.T) {
	hc, _ := setupHIDPair(t)
	lease := newControlLease(hc)

	ctx := contextWithTimeout(t, 5*time.Second)
	held, err := lease.Acquire(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	if err := held.SendKeyboardReport(ctx, 0x02, []byte{0x04}); !errors.Is(err, ErrStaleControlToken) {
		t.Errorf("SendKeyboardReport after release = %v, want ErrStaleControlToken", err)
	}
	if err := held.SendPointerReport(ctx, 0, 0, 0); !errors.Is(err, ErrStaleControlToken) {
		t.Errorf("SendPointerReport after release = %v, want ErrStaleControlToken", err)
	}
	if err := held.SendMouseReport(ctx, 0, 0, 0); !errors.Is(err, ErrStaleControlToken) {
		t.Errorf("SendMouseReport after release = %v, want ErrStaleControlToken", err)
	}
}

// TestControlLeaseSendRacingReleaseNeverLandsAfterNeutralization runs many
// concurrent send/release interleavings through the real state machine. Any
// send that succeeds must have been written before the neutralization
// frames; any send that loses the race must fail loudly rather than land
// late. Run with -race and -count to exercise the interleavings.
func TestControlLeaseSendRacingReleaseNeverLandsAfterNeutralization(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		hc, tr := newFakeHIDClient(t)
		lease := newControlLease(hc)

		ctx := contextWithTimeout(t, 10*time.Second)
		held, err := lease.Acquire(ctx, 5*time.Second)
		if err != nil {
			t.Fatalf("Acquire failed: %v", err)
		}

		var wg sync.WaitGroup
		const senders = 8
		errs := make([]error, senders)
		wg.Add(senders)
		for i := 0; i < senders; i++ {
			go func(i int) {
				defer wg.Done()
				errs[i] = held.SendKeyboardReport(ctx, 0, []byte{byte(0x04 + i)})
			}(i)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = held.Release()
		}()
		wg.Wait()

		for i, err := range errs {
			if err != nil && !errors.Is(err, ErrStaleControlToken) {
				t.Fatalf("iteration %d sender %d: unexpected error %v", iteration, i, err)
			}
		}

		// Whatever the interleaving, the last two frames on the wire must be
		// the neutralization pair: nothing may follow them.
		types := tr.frameTypes()
		if len(types) < 3 {
			t.Fatalf("iteration %d: only %d frames on the wire", iteration, len(types))
		}
		if types[len(types)-2] != 0x02 /* keyboard */ || types[len(types)-1] != 0x06 /* relative mouse */ {
			t.Fatalf("iteration %d: wire did not end with the neutralization pair: %v", iteration, types)
		}
		// And no input frame may appear after neutralization began.
		if hc.hasHeldState() {
			t.Fatalf("iteration %d: held input survived the release", iteration)
		}
	}
}

func TestControlLeaseExclusiveTryAcquire(t *testing.T) {
	hc, _ := setupHIDPair(t)
	lease := newControlLease(hc)

	ctx := contextWithTimeout(t, 5*time.Second)
	held, err := lease.TryAcquire(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("first TryAcquire failed: %v", err)
	}

	if _, err = lease.TryAcquire(ctx, 5*time.Second); !errors.Is(err, ErrControlHeld) {
		t.Fatalf("second TryAcquire error = %v, want ErrControlHeld", err)
	}

	if err := held.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	held2, err := lease.TryAcquire(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("TryAcquire after release failed: %v", err)
	}
	if held2.Token() == held.Token() {
		t.Error("a replacement holder must receive a fresh generation token")
	}
	_ = held2.Release()
}

func TestControlLeaseAcquireBlocksUntilReleased(t *testing.T) {
	hc, _ := setupHIDPair(t)
	lease := newControlLease(hc)

	ctx := contextWithTimeout(t, 10*time.Second)
	first, err := lease.Acquire(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}

	secondAcquired := make(chan *Held, 1)
	go func() {
		second, err := lease.Acquire(ctx, 5*time.Second)
		if err != nil {
			t.Errorf("second Acquire failed: %v", err)
			close(secondAcquired)
			return
		}
		secondAcquired <- second
	}()

	select {
	case <-secondAcquired:
		t.Fatal("second Acquire should not complete before first Release")
	case <-time.After(200 * time.Millisecond):
	}

	if err := first.Release(); err != nil {
		t.Fatalf("first Release failed: %v", err)
	}

	select {
	case second := <-secondAcquired:
		if second == nil {
			t.Fatal("second Acquire did not produce a holder")
		}
		if second.Token() == first.Token() {
			t.Error("the queued acquirer must receive a fresh generation token")
		}
		_ = second.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("second Acquire never completed after first Release")
	}
}

func TestCloseNeutralizationExcludesFreshLeaseCreation(t *testing.T) {
	hc, tr := newFakeHIDClient(t)
	lease := newControlLease(hc)
	tr.setAutoDrain(false)
	before := tr.count()

	closeCtx := contextWithTimeout(t, 3*time.Second)
	closeDone := make(chan error, 1)
	go func() { closeDone <- lease.neutralize(closeCtx) }()
	waitForCondition(t, time.Second, func() bool {
		return tr.count() == before+2 && tr.BufferedAmount() > 0
	})

	type acquireResult struct {
		held *Held
		err  error
	}
	acquireDone := make(chan acquireResult, 1)
	acquireCtx := contextWithTimeout(t, 2*time.Second)
	go func() {
		held, err := lease.Acquire(acquireCtx, time.Second)
		acquireDone <- acquireResult{held: held, err: err}
	}()

	select {
	case result := <-acquireDone:
		t.Fatalf("lease creation crossed an active close neutralization: held=%v err=%v", result.held, result.err)
	case <-time.After(50 * time.Millisecond):
	}

	tr.setBufferedAmount(0)
	if err := <-closeDone; err != nil {
		t.Fatalf("close neutralization failed after drain: %v", err)
	}

	select {
	case result := <-acquireDone:
		if result.held != nil {
			t.Fatal("lease creation succeeded after close neutralization")
		}
		if !errors.Is(result.err, ErrHIDClosed) {
			t.Fatalf("lease creation after close neutralization = %v, want ErrHIDClosed", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("lease creation did not observe the terminal HID state")
	}
	if len(lease.slot) != 0 {
		t.Fatal("failed post-close acquisition leaked the lease slot")
	}
}

// TestControlLeaseAcquireCancellationDoesNotLeakTheSlot covers the case
// where a waiter gives up: the abandoned attempt must not leave the lease
// permanently locked.
func TestControlLeaseAcquireCancellationDoesNotLeakTheSlot(t *testing.T) {
	hc, _ := setupHIDPair(t)
	lease := newControlLease(hc)

	first, err := lease.Acquire(contextWithTimeout(t, 10*time.Second), 10*time.Second)
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := lease.Acquire(waiterCtx, 5*time.Second)
		waiterDone <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancelWaiter()

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("abandoned Acquire = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled Acquire never returned")
	}

	if err := first.Release(); err != nil {
		t.Fatalf("first Release failed: %v", err)
	}
	next, err := lease.TryAcquire(contextWithTimeout(t, 3*time.Second), time.Second)
	if err != nil {
		t.Fatalf("lease was left stuck after an abandoned Acquire: %v", err)
	}
	_ = next.Release()
}

func TestControlLeaseAcquisitionRejectsAlreadyCanceledContext(t *testing.T) {
	tests := []struct {
		name    string
		acquire func(*controlLease, context.Context) (*Held, error)
	}{
		{
			name: "acquire",
			acquire: func(lease *controlLease, ctx context.Context) (*Held, error) {
				return lease.Acquire(ctx, time.Second)
			},
		},
		{
			name: "persistent",
			acquire: func(lease *controlLease, ctx context.Context) (*Held, error) {
				return lease.AcquirePersistent(ctx, time.Second)
			},
		},
		{
			name: "try acquire",
			acquire: func(lease *controlLease, ctx context.Context) (*Held, error) {
				return lease.TryAcquire(ctx, time.Second)
			},
		},
		{
			name: "try persistent",
			acquire: func(lease *controlLease, ctx context.Context) (*Held, error) {
				return lease.TryAcquirePersistent(ctx, time.Second)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc, _ := newFakeHIDClient(t)
			lease := newControlLease(hc)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			held, err := tt.acquire(lease, ctx)
			if !errors.Is(err, context.Canceled) {
				if held != nil {
					_ = held.Release()
				}
				t.Fatalf("acquisition with canceled context = held %v, error %v; want context.Canceled", held != nil, err)
			}
			if held != nil {
				_ = held.Release()
				t.Fatal("acquisition with canceled context returned a holder")
			}
			if len(lease.slot) != 0 {
				t.Fatal("acquisition with canceled context leaked the exclusivity slot")
			}
			if hc.activeGeneration() != 0 {
				t.Fatal("acquisition with canceled context installed an active generation")
			}
		})
	}
}

func TestControlLeasePersistentAcquisitionHonorsContextBeforeHolderLifetime(t *testing.T) {
	hc, _ := newFakeHIDClient(t)
	lease := newControlLease(hc)

	if err := hc.lockLifecycle(context.Background()); err != nil {
		t.Fatalf("lock lifecycle: %v", err)
	}
	lifecycleLocked := true
	t.Cleanup(func() {
		if lifecycleLocked {
			hc.unlockLifecycle()
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		held *Held
		err  error
	}
	done := make(chan result, 1)
	go func() {
		held, err := lease.AcquirePersistent(ctx, 5*time.Second)
		done <- result{held: held, err: err}
	}()
	waitForCondition(t, time.Second, func() bool { return len(lease.slot) == 1 })
	cancel()

	select {
	case got := <-done:
		if got.held != nil {
			_ = got.held.Release()
			t.Fatal("canceled persistent acquisition returned a holder")
		}
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled persistent acquisition = %v, want context.Canceled", got.err)
		}
	case <-time.After(time.Second):
		// Unblock a regressed implementation before failing so it cannot
		// leave a goroutine or lease behind in the test process.
		hc.unlockLifecycle()
		lifecycleLocked = false
		got := <-done
		if got.held != nil {
			_ = got.held.Release()
		}
		t.Fatal("persistent acquisition ignored cancellation while waiting for the HID lifecycle")
	}

	hc.unlockLifecycle()
	lifecycleLocked = false
	if len(lease.slot) != 0 {
		t.Fatal("canceled persistent acquisition leaked the exclusivity slot")
	}
	if hc.activeGeneration() != 0 {
		t.Fatal("canceled persistent acquisition installed an active generation")
	}

	next, err := lease.TryAcquire(contextWithTimeout(t, time.Second), time.Second)
	if err != nil {
		t.Fatalf("lease was not reusable after canceled persistent acquisition: %v", err)
	}
	if err := next.Release(); err != nil {
		t.Fatalf("release replacement holder: %v", err)
	}
}

func TestControlLeaseTimeoutForceReleases(t *testing.T) {
	hc, fd := setupHIDPair(t)
	lease := newControlLease(hc)

	ctx := contextWithTimeout(t, 10*time.Second)
	held, err := lease.Acquire(ctx, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	if err := held.SendKeyboardReport(ctx, 0x02, []byte{0x04}); err != nil {
		t.Fatalf("SendKeyboardReport failed: %v", err)
	}
	if err := held.SendPointerReport(ctx, 222, 333, MouseButtonLeft); err != nil {
		t.Fatalf("SendPointerReport failed: %v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool {
		hidg1, _ := fd.mouseInterfaceStates()
		return hidg1.buttons == MouseButtonLeft && hidg1.x == 222 && hidg1.y == 333
	})

	// Don't call Release; the lease's own inactivity timeout must force a
	// neutralization of both keyboard and absolute-pointer interfaces, then
	// free the lease for the next caller.
	waitForCondition(t, 5*time.Second, func() bool {
		kb, ok := fd.lastKeyboardReport()
		hidg1, hidg2 := fd.mouseInterfaceStates()
		return ok && kb.Payload[0] == 0 && allZero(kb.Payload[1:]) &&
			hidg1.buttons == 0 && hidg1.x == 222 && hidg1.y == 333 && hidg2.buttons == 0
	})
	// Peer receipt can be observed just before the sender processes the SCTP
	// acknowledgement. The watchdog does not free the lease until that drain
	// confirmation completes.
	select {
	case <-held.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed-out holder did not finish confirmed neutralization")
	}

	// The expired holder's token must now be rejected.
	if err := held.SendKeyboardReport(ctx, 0x02, []byte{0x04}); !errors.Is(err, ErrStaleControlToken) {
		t.Errorf("send after lease expiry = %v, want ErrStaleControlToken", err)
	}

	next, err := lease.TryAcquire(ctx, time.Second)
	if err != nil {
		t.Fatalf("expected the lease to be free after the timeout: %v", err)
	}
	_ = next.Release()
}

func TestControlLeaseContextCancelForceReleases(t *testing.T) {
	hc, fd := setupHIDPair(t)
	lease := newControlLease(hc)

	acquireCtx, cancel := context.WithCancel(context.Background())
	held, err := lease.Acquire(acquireCtx, 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	sendCtx := contextWithTimeout(t, 5*time.Second)
	if err := held.SendPointerReport(sendCtx, 100, 100, 0x01); err != nil {
		t.Fatalf("SendPointerReport failed: %v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool {
		_, ok := fd.lastPointerReport()
		return ok
	})
	pointerReportsBefore := fd.pointerReportCount()

	cancel() // simulate the caller's context being canceled mid-gesture

	// Firmware keeps absolute and relative buttons on separate gadget files.
	// Forced release must therefore send both mouse-interface neutral reports,
	// with the absolute report preserving the recorded coordinates.
	waitForCondition(t, 5*time.Second, func() bool {
		mouse, ok := fd.lastMouseReport()
		hidg1, hidg2 := fd.mouseInterfaceStates()
		return ok && mouse.Payload[2] == 0 && fd.pointerReportCount() == pointerReportsBefore+1 &&
			hidg1.buttons == 0 && hidg1.x == 100 && hidg1.y == 100 && hidg2.buttons == 0
	})
	if got := fd.pointerReportCount(); got != pointerReportsBefore+1 {
		t.Errorf("forced release sent %d additional absolute pointer reports, want 1", got-pointerReportsBefore)
	}
	// Peer receipt can precede the sender processing its SCTP acknowledgement.
	// The lease must remain held until the outbound buffer reaches zero, so
	// wait for the watchdog's confirmed neutralization before probing it.
	select {
	case <-held.done:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled holder did not finish confirmed neutralization")
	}
	if err := held.Release(); err != nil {
		t.Fatalf("forced release failed: %v", err)
	}

	freeCtx := contextWithTimeout(t, 5*time.Second)
	next, err := lease.TryAcquire(freeCtx, time.Second)
	if err != nil {
		t.Fatalf("expected the lease to be free after cancellation: %v", err)
	}
	_ = next.Release()
}

func TestControlLeasePersistentHolderOutlivesAcquisitionContext(t *testing.T) {
	hc, _ := newFakeHIDClient(t)
	lease := newControlLease(hc)

	acquireCtx, cancelAcquire := context.WithCancel(context.Background())
	held, err := lease.AcquirePersistent(acquireCtx, 5*time.Second)
	if err != nil {
		t.Fatalf("AcquirePersistent failed: %v", err)
	}
	cancelAcquire()

	select {
	case <-held.Done():
		t.Fatal("persistent holder ended with the acquisition context")
	case <-time.After(50 * time.Millisecond):
	}
	if err := held.SendMouseReport(contextWithTimeout(t, time.Second), 0, 0, MouseButtonRight); err != nil {
		t.Fatalf("persistent send after acquisition cancellation: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("persistent Release: %v", err)
	}
	select {
	case <-held.Done():
	case <-time.After(time.Second):
		t.Fatal("Done was not closed after Release")
	}
}

func TestControlLeaseTryAcquirePersistentOutlivesAcquisitionContext(t *testing.T) {
	hc, _ := newFakeHIDClient(t)
	lease := newControlLease(hc)

	acquireCtx, cancelAcquire := context.WithCancel(context.Background())
	held, err := lease.TryAcquirePersistent(acquireCtx, 5*time.Second)
	if err != nil {
		t.Fatalf("TryAcquirePersistent failed: %v", err)
	}
	defer held.Release()
	cancelAcquire()

	select {
	case <-held.Done():
		t.Fatal("persistent non-blocking holder ended with the acquisition context")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := lease.TryAcquire(contextWithTimeout(t, time.Second), time.Second); !errors.Is(err, ErrControlHeld) {
		t.Fatalf("competing acquisition = %v, want ErrControlHeld", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("persistent non-blocking Release: %v", err)
	}

	next, err := lease.TryAcquire(contextWithTimeout(t, time.Second), time.Second)
	if err != nil {
		t.Fatalf("TryAcquire after persistent release: %v", err)
	}
	if err := next.Release(); err != nil {
		t.Fatalf("release after reacquisition: %v", err)
	}
}

func TestHeldReleaseKeyboardPreservesMouseButtons(t *testing.T) {
	hc, tr := newFakeHIDClient(t)
	lease := newControlLease(hc)
	ctx := contextWithTimeout(t, 5*time.Second)
	held, err := lease.AcquirePersistent(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("AcquirePersistent failed: %v", err)
	}

	if err := held.SendMouseReport(ctx, 0, 0, MouseButtonLeft); err != nil {
		t.Fatalf("SendMouseReport: %v", err)
	}
	if err := held.SendKeyboardReport(ctx, 0x02, []byte{0x04}); err != nil {
		t.Fatalf("SendKeyboardReport: %v", err)
	}
	if err := held.ReleaseKeyboard(ctx); err != nil {
		t.Fatalf("ReleaseKeyboard: %v", err)
	}

	frames := tr.snapshot()
	if len(frames) != 4 {
		t.Fatalf("wire frames = %d, want handshake plus mouse/key/key-release", len(frames))
	}
	keyboard, err := hidproto.Unmarshal(frames[3])
	if err != nil {
		t.Fatalf("decode keyboard release: %v", err)
	}
	if keyboard.Type != hidproto.TypeKeyboardReport || len(keyboard.Payload) != hidproto.HIDKeyBufferSize+1 ||
		keyboard.Payload[0] != 0 || !allZero(keyboard.Payload[1:]) {
		t.Fatalf("keyboard release frame = % x, want canonical all-zero keyboard report", frames[3])
	}
	if !hc.hasHeldState() {
		t.Fatal("ReleaseKeyboard cleared the intentionally held mouse button")
	}
	if err := held.Release(); err != nil {
		t.Fatalf("terminal Release: %v", err)
	}
	if hc.hasHeldState() {
		t.Fatal("terminal Release left input held")
	}
}

// TestControlLeaseReleaseIsIdempotent covers Release racing its own
// watchdog: both paths run through sync.Once and report the same outcome.
func TestControlLeaseReleaseIsIdempotent(t *testing.T) {
	hc, _ := newFakeHIDClient(t)
	lease := newControlLease(hc)

	held, err := lease.Acquire(contextWithTimeout(t, 5*time.Second), 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = held.Release()
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		if err != nil {
			t.Errorf("concurrent Release %d = %v, want nil", i, err)
		}
	}
	if len(lease.slot) != 0 {
		t.Fatal("repeated Release must free the exclusivity slot exactly once")
	}
}

// TestControlLeaseReleaseReportsUnverifiedNeutralization proves the lease
// surfaces a failed neutralization instead of reporting a clean release.
func TestControlLeaseReleaseReportsUnverifiedNeutralization(t *testing.T) {
	hc, tr := newFakeHIDClient(t)
	lease := newControlLease(hc)

	held, err := lease.Acquire(contextWithTimeout(t, 5*time.Second), 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if err := held.SendKeyboardReport(contextWithTimeout(t, 5*time.Second), 0x02, []byte{0x04}); err != nil {
		t.Fatalf("SendKeyboardReport failed: %v", err)
	}

	tr.setFailure(-1, errors.New("channel is gone"))

	if err := held.Release(); !errors.Is(err, ErrNeutralizeUnverified) {
		t.Fatalf("Release with a dead transport = %v, want ErrNeutralizeUnverified", err)
	}
	// Even so, the lease must be free again: an unverifiable release must
	// not also wedge control permanently.
	if len(lease.slot) != 0 {
		t.Fatal("a failed release must still free the exclusivity slot")
	}
}
