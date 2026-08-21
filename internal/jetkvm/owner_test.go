package jetkvm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
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

func TestControlLeaseTimeoutForceReleases(t *testing.T) {
	hc, fd := setupHIDPair(t)
	lease := newControlLease(hc)

	ctx := contextWithTimeout(t, 10*time.Second)
	held, err := lease.Acquire(ctx, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	if err := held.SendKeyboardReport(ctx, 0x02, []byte{0x04}); err != nil {
		t.Fatalf("SendKeyboardReport failed: %v", err)
	}

	// Don't call Release; the lease's own inactivity timeout must force a
	// neutralization and free the lease for the next caller.
	waitForCondition(t, 5*time.Second, func() bool {
		kb, ok := fd.lastKeyboardReport()
		return ok && kb.Payload[0] == 0 && allZero(kb.Payload[1:])
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

	// Buttons must be cleared via a relative report, so a forced release
	// cannot move the cursor.
	waitForCondition(t, 5*time.Second, func() bool {
		mouse, ok := fd.lastMouseReport()
		return ok && mouse.Payload[2] == 0
	})
	if got := fd.pointerReportCount(); got != pointerReportsBefore {
		t.Errorf("forced release sent %d absolute pointer reports, want 0", got-pointerReportsBefore)
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
