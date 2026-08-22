package jetkvm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/leeroyding/jetkvm-mcp/internal/hidproto"
)

// ---------------------------------------------------------------------------
// Deterministic transport fake
//
// The peer-pair tests further down prove the wire format against real Pion
// data channels, but they cannot pin down *ordering* under concurrency: you
// cannot hold a real SCTP send open at an exact instant. This fake can, so
// the state machine's ordering guarantees (release-all pre-empts queued
// input; stale frames are dropped at the write boundary) become deterministic
// assertions rather than timing-dependent ones.
// ---------------------------------------------------------------------------

type fakeHIDTransport struct {
	mu     sync.Mutex
	frames [][]byte
	client *hidClient

	bufferedAmount uint64
	lowThreshold   uint64
	onLow          func()
	autoDrain      bool

	// beforeSend runs inside Send, before the frame is recorded. Tests use
	// it to park the writer mid-send and interleave a release.
	beforeSend func(frame []byte)

	// failCount fails that many sends before succeeding again; err is the
	// error to return (a default is used when nil). A negative failCount
	// fails every send.
	failCount int
	err       error
}

func (f *fakeHIDTransport) Send(b []byte) error {
	frame := append([]byte(nil), b...)

	f.mu.Lock()
	hook := f.beforeSend
	f.mu.Unlock()
	if hook != nil {
		hook(frame)
	}

	f.mu.Lock()
	if f.failCount != 0 {
		if f.failCount > 0 {
			f.failCount--
		}
		err := f.err
		if err == nil {
			err = errors.New("fake transport: send failed")
		}
		f.mu.Unlock()
		return err
	}
	f.frames = append(f.frames, frame)
	f.bufferedAmount += uint64(len(frame))
	var low func()
	if f.autoDrain {
		from := f.bufferedAmount
		f.bufferedAmount = 0
		if from > f.lowThreshold {
			low = f.onLow
		}
	}
	client := f.client
	f.mu.Unlock()
	if low != nil {
		low()
	}

	// Echo the readiness handshake back, which is what flips the real
	// firmware's hidRPCAvailable to true.
	if client != nil && len(frame) > 0 && hidproto.MessageType(frame[0]) == hidproto.TypeHandshake {
		client.handleMessage(frame)
	}
	return nil
}

func (f *fakeHIDTransport) BufferedAmount() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bufferedAmount
}

func (f *fakeHIDTransport) SetBufferedAmountLowThreshold(th uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lowThreshold = th
}

func (f *fakeHIDTransport) OnBufferedAmountLow(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onLow = fn
}

func (f *fakeHIDTransport) setAutoDrain(enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.autoDrain = enabled
}

func (f *fakeHIDTransport) setBufferedAmount(amount uint64) {
	f.mu.Lock()
	from := f.bufferedAmount
	f.bufferedAmount = amount
	low := f.onLow
	threshold := f.lowThreshold
	f.mu.Unlock()

	if low != nil && from > threshold && amount <= threshold {
		low()
	}
}

func (f *fakeHIDTransport) signalBufferedAmountLow() {
	f.mu.Lock()
	low := f.onLow
	f.mu.Unlock()
	if low != nil {
		low()
	}
}

func (f *fakeHIDTransport) bufferedAmountLowThreshold() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lowThreshold
}

func (f *fakeHIDTransport) setBeforeSend(fn func(frame []byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beforeSend = fn
}

func (f *fakeHIDTransport) setFailure(count int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCount = count
	f.err = err
}

func (f *fakeHIDTransport) frameTypes() []hidproto.MessageType {
	f.mu.Lock()
	defer f.mu.Unlock()
	types := make([]hidproto.MessageType, 0, len(f.frames))
	for _, frame := range f.frames {
		if len(frame) > 0 {
			types = append(types, hidproto.MessageType(frame[0]))
		}
	}
	return types
}

func (f *fakeHIDTransport) snapshot() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.frames...)
}

func (f *fakeHIDTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.frames)
}

// newFakeHIDClient returns a hidClient that has completed its readiness
// handshake against the fake transport.
func newFakeHIDClient(t *testing.T) (*hidClient, *fakeHIDTransport) {
	t.Helper()
	hc, tr := newUnreadyHIDClient(t)
	if err := hc.handshake(contextWithTimeout(t, 5*time.Second)); err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	if got := hc.currentState(); got != hidStateReady {
		t.Fatalf("state after handshake = %s, want ready", got)
	}
	return hc, tr
}

// newUnreadyHIDClient returns a hidClient that has *not* handshaken.
func newUnreadyHIDClient(t *testing.T) (*hidClient, *fakeHIDTransport) {
	t.Helper()
	tr := &fakeHIDTransport{autoDrain: true}
	hc := newHIDClient(tr)
	tr.mu.Lock()
	tr.client = hc
	tr.mu.Unlock()
	t.Cleanup(func() {
		hc.closeWith(errors.New("test cleanup"))
		select {
		case <-hc.writerDone:
		case <-time.After(2 * time.Second):
			t.Error("HID writer goroutine did not exit after close")
		}
	})
	return hc, tr
}

func (h *hidClient) activeGeneration() uint64 {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	return h.activeGen
}

// ---------------------------------------------------------------------------
// Readiness handshake gating
// ---------------------------------------------------------------------------

func TestHIDSendsBlockedUntilHandshakeConfirmed(t *testing.T) {
	hc, tr := newUnreadyHIDClient(t)
	ctx := contextWithTimeout(t, 2*time.Second)

	if _, err := hc.beginLease(context.Background()); !errors.Is(err, ErrHIDNotReady) {
		t.Fatalf("beginLease before handshake = %v, want ErrHIDNotReady", err)
	}
	// Even with a fabricated token, nothing may reach the device.
	if err := hc.sendKeyboardReport(ctx, 1, 0x02, []byte{0x04}); !errors.Is(err, ErrHIDNotReady) {
		t.Fatalf("send before handshake = %v, want ErrHIDNotReady", err)
	}
	if n := tr.count(); n != 0 {
		t.Fatalf("device received %d frames before the handshake, want 0", n)
	}
}

func TestHIDHandshakeFailureClosesStateMachine(t *testing.T) {
	tr := &fakeHIDTransport{} // no client wired in: the handshake is never echoed
	hc := newHIDClient(tr)
	t.Cleanup(func() { hc.closeWith(errors.New("test cleanup")) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := hc.handshake(ctx)
	if err == nil {
		t.Fatal("expected the handshake to fail when the device never echoes it")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handshake error = %v, want a deadline error", err)
	}
	if got := hc.currentState(); got != hidStateClosed {
		t.Fatalf("state after a failed handshake = %s, want closed", got)
	}
	if _, err := hc.beginLease(context.Background()); !errors.Is(err, ErrHIDClosed) {
		t.Fatalf("beginLease after a failed handshake = %v, want ErrHIDClosed", err)
	}
	select {
	case <-hc.writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("writer goroutine did not exit after a failed handshake")
	}
}

func TestHIDHandshakeIsIdempotentOnceReady(t *testing.T) {
	hc, _ := newFakeHIDClient(t)
	if err := hc.handshake(contextWithTimeout(t, time.Second)); err != nil {
		t.Fatalf("re-running the handshake on a ready channel = %v, want nil", err)
	}
}

func TestHIDHandshakeIgnoresMalformedEchoes(t *testing.T) {
	for _, test := range []struct {
		name  string
		frame []byte
	}{
		{name: "empty", frame: nil},
		{name: "type only", frame: []byte{byte(hidproto.TypeHandshake)}},
		{name: "wrong version", frame: []byte{byte(hidproto.TypeHandshake), hidproto.ProtocolVersion + 1}},
		{name: "trailing payload", frame: []byte{byte(hidproto.TypeHandshake), hidproto.ProtocolVersion, 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tr := &fakeHIDTransport{autoDrain: true}
			hc := newHIDClient(tr)
			t.Cleanup(func() { hc.closeWith(errors.New("test cleanup")) })

			ctx := contextWithTimeout(t, 2*time.Second)
			done := make(chan error, 1)
			go func() { done <- hc.handshake(ctx) }()
			waitForCondition(t, time.Second, func() bool { return tr.count() == 1 })

			hc.handleMessage(test.frame)
			select {
			case <-hc.handshakeDone:
				t.Fatalf("malformed echo % x confirmed HID readiness", test.frame)
			default:
			}

			valid, err := hidproto.EncodeHandshake()
			if err != nil {
				t.Fatal(err)
			}
			hc.handleMessage(valid)
			if err := <-done; err != nil {
				t.Fatalf("exact echo was rejected after malformed input: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Release-all: pre-emption, staleness, and cursor safety
// ---------------------------------------------------------------------------

// TestReleaseAllPreemptsAndDropsQueuedStaleSends is the central concurrency
// proof: while one send is parked inside the transport and several more sit
// in the queue behind it, a release-all must (a) jump ahead of the queued
// input, and (b) cause every one of those queued frames to be dropped rather
// than delivered after neutralization.
func TestReleaseAllPreemptsAndDropsQueuedStaleSends(t *testing.T) {
	hc, tr := newFakeHIDClient(t)
	ctx := contextWithTimeout(t, 10*time.Second)

	token, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}

	gate := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once
	tr.setBeforeSend(func(frame []byte) {
		if len(frame) == 0 || hidproto.MessageType(frame[0]) != hidproto.TypeKeyboardReport {
			return
		}
		once.Do(func() {
			close(blocked)
			<-gate
		})
	})

	// Occupy the writer with a send that is valid at the time it is written.
	firstDone := make(chan error, 1)
	go func() { firstDone <- hc.sendKeyboardReport(ctx, token, 0x02, []byte{0x04}) }()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("first send never reached the transport")
	}

	// Queue more input behind the parked writer.
	const queued = 5
	results := make(chan error, queued)
	for i := 0; i < queued; i++ {
		go func(i int) { results <- hc.sendKeyboardReport(ctx, token, 0, []byte{byte(0x05 + i)}) }(i)
	}
	waitForCondition(t, 5*time.Second, func() bool { return len(hc.sendCh) == queued })

	// Neutralize while all of that input is queued.
	releaseDone := make(chan error, 1)
	go func() { releaseDone <- hc.releaseAll(context.Background()) }()

	// Wait until the generation is actually revoked, so the assertion below
	// is about ordering rather than about who won a timing race.
	waitForCondition(t, 5*time.Second, func() bool { return hc.activeGeneration() == 0 })

	close(gate) // let the in-flight send complete

	if err := <-firstDone; err != nil {
		t.Fatalf("the send that was in flight before the release failed: %v", err)
	}
	if err := <-releaseDone; err != nil {
		t.Fatalf("releaseAll failed: %v", err)
	}
	for i := 0; i < queued; i++ {
		err := <-results
		if !errors.Is(err, ErrStaleControlToken) {
			t.Fatalf("queued send %d = %v, want ErrStaleControlToken", i, err)
		}
	}

	// The device must have seen exactly: handshake, the one valid keyboard
	// report, then the two neutralization frames. None of the five queued
	// keyboard reports may appear at all, and nothing may follow the
	// neutralization.
	types := tr.frameTypes()
	want := []hidproto.MessageType{
		hidproto.TypeHandshake,
		hidproto.TypeKeyboardReport, // the in-flight send
		hidproto.TypeKeyboardReport, // neutralization: keys cleared
		hidproto.TypeMouseReport,    // neutralization: buttons cleared, no movement
	}
	if len(types) != len(want) {
		t.Fatalf("device received %d frames (%v), want %d (%v)", len(types), types, len(want), want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("frame %d = %#x, want %#x (full sequence %v)", i, types[i], want[i], types)
		}
	}
}

// TestReleaseAllNeverMovesCursor pins the rule that neutralizing button state
// must not warp the pointer: an absolute PointerReport necessarily carries a
// coordinate, so release-all uses a zero-delta relative MouseReport instead.
func TestReleaseAllNeverMovesCursor(t *testing.T) {
	hc, tr := newFakeHIDClient(t)
	ctx := contextWithTimeout(t, 5*time.Second)

	token, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}
	if err := hc.sendKeyboardReport(ctx, token, 0x02, []byte{0x04}); err != nil {
		t.Fatalf("SendKeyboardReport failed: %v", err)
	}
	if err := hc.sendPointerReport(ctx, token, 500, 600, 0x01); err != nil {
		t.Fatalf("SendPointerReport failed: %v", err)
	}
	beforeRelease := tr.count()

	if err := hc.releaseAll(ctx); err != nil {
		t.Fatalf("releaseAll failed: %v", err)
	}

	frames := tr.snapshot()[beforeRelease:]
	if len(frames) != 2 {
		t.Fatalf("release-all wrote %d frames, want 2", len(frames))
	}

	var sawKeyboardClear, sawMouseClear bool
	for _, frame := range frames {
		m, err := hidproto.Unmarshal(frame)
		if err != nil {
			t.Fatalf("undecodable neutralization frame: %v", err)
		}
		switch m.Type {
		case hidproto.TypePointerReport:
			t.Fatal("release-all sent an absolute pointer report, which would move the cursor")
		case hidproto.TypeKeyboardReport:
			if m.Payload[0] != 0 || !allZero(m.Payload[1:]) {
				t.Errorf("neutralization keyboard report = % x, want all zero", m.Payload)
			}
			sawKeyboardClear = true
		case hidproto.TypeMouseReport:
			if len(m.Payload) != 3 {
				t.Fatalf("mouse report payload = % x, want 3 bytes", m.Payload)
			}
			if m.Payload[0] != 0 || m.Payload[1] != 0 {
				t.Errorf("neutralization mouse delta = (%d,%d), want (0,0) so the cursor cannot move",
					int8(m.Payload[0]), int8(m.Payload[1]))
			}
			if m.Payload[2] != 0 {
				t.Errorf("neutralization mouse buttons = %d, want 0", m.Payload[2])
			}
			sawMouseClear = true
		}
	}
	if !sawKeyboardClear || !sawMouseClear {
		t.Fatalf("release-all did not clear both keyboard and buttons (keyboard=%v mouse=%v)",
			sawKeyboardClear, sawMouseClear)
	}
	if hc.hasHeldState() {
		t.Error("expected the held-input model to be cleared after a confirmed release")
	}
}

func TestSendWithStaleTokenIsDroppedAtWriteBoundary(t *testing.T) {
	hc, tr := newFakeHIDClient(t)
	ctx := contextWithTimeout(t, 5*time.Second)

	token, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}
	if err := hc.releaseAll(ctx); err != nil {
		t.Fatalf("releaseAll failed: %v", err)
	}
	after := tr.count()

	if err := hc.sendKeyboardReport(ctx, token, 0x02, []byte{0x04}); !errors.Is(err, ErrStaleControlToken) {
		t.Fatalf("send with a released token = %v, want ErrStaleControlToken", err)
	}
	if got := tr.count(); got != after {
		t.Fatalf("a stale send reached the device: frame count %d -> %d", after, got)
	}
	if hc.hasHeldState() {
		t.Error("a dropped send must not be recorded as held input")
	}
}

func TestLeaseGenerationsAreNeverReused(t *testing.T) {
	hc, _ := newFakeHIDClient(t)
	ctx := contextWithTimeout(t, 5*time.Second)

	seen := map[uint64]bool{}
	var previous uint64
	for i := 0; i < 5; i++ {
		token, err := hc.beginLease(context.Background())
		if err != nil {
			t.Fatalf("beginLease %d failed: %v", i, err)
		}
		if token == 0 {
			t.Fatal("a lease token must never be zero: zero means 'no holder'")
		}
		if seen[token] {
			t.Fatalf("lease token %d was reused", token)
		}
		if token <= previous {
			t.Fatalf("lease token %d did not increase (previous %d)", token, previous)
		}
		seen[token] = true
		previous = token

		if err := hc.releaseAll(ctx); err != nil {
			t.Fatalf("releaseAll %d failed: %v", i, err)
		}
		// The just-released token must be rejected immediately.
		if err := hc.sendKeyboardReport(ctx, token, 0, []byte{0x04}); !errors.Is(err, ErrStaleControlToken) {
			t.Fatalf("send after release %d = %v, want ErrStaleControlToken", i, err)
		}
	}
}

func TestHolderReplacementInvalidatesThePreviousToken(t *testing.T) {
	hc, _ := newFakeHIDClient(t)
	ctx := contextWithTimeout(t, 5*time.Second)

	first, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("first beginLease failed: %v", err)
	}
	if err := hc.releaseAll(ctx); err != nil {
		t.Fatalf("releaseAll failed: %v", err)
	}
	second, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("second beginLease failed: %v", err)
	}

	if err := hc.sendKeyboardReport(ctx, first, 0, []byte{0x04}); !errors.Is(err, ErrStaleControlToken) {
		t.Fatalf("replaced holder's send = %v, want ErrStaleControlToken", err)
	}
	if err := hc.sendKeyboardReport(ctx, second, 0, []byte{0x05}); err != nil {
		t.Fatalf("current holder's send failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Disconnect / reconnect
// ---------------------------------------------------------------------------

func TestDisconnectInvalidatesLeaseAndDrainsQueue(t *testing.T) {
	hc, tr := newFakeHIDClient(t)
	ctx := contextWithTimeout(t, 5*time.Second)

	token, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}
	before := tr.count()

	disconnect := errors.New("hidrpc data channel closed")
	hc.closeWith(disconnect)

	if got := hc.currentState(); got != hidStateClosed {
		t.Fatalf("state after disconnect = %s, want closed", got)
	}
	if hc.activeGeneration() != 0 {
		t.Fatal("disconnect must revoke the active lease generation")
	}

	err = hc.sendKeyboardReport(ctx, token, 0, []byte{0x04})
	if !errors.Is(err, ErrHIDClosed) {
		t.Fatalf("send after disconnect = %v, want ErrHIDClosed", err)
	}
	if _, err := hc.beginLease(context.Background()); !errors.Is(err, ErrHIDClosed) {
		t.Fatalf("beginLease after disconnect = %v, want ErrHIDClosed", err)
	}
	if got := tr.count(); got != before {
		t.Fatalf("frames reached the device after disconnect: %d -> %d", before, got)
	}

	select {
	case <-hc.writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("writer goroutine leaked after disconnect")
	}
}

func TestReconnectStartsFromACleanStateMachine(t *testing.T) {
	stale, _ := newFakeHIDClient(t)
	ctx := contextWithTimeout(t, 5*time.Second)

	staleToken, err := stale.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}
	stale.closeWith(errors.New("peer connection entered failed state"))

	// A reconnect is a brand-new channel and therefore a brand-new state
	// machine; the old handle must stay dead rather than resurrect.
	fresh, freshTr := newFakeHIDClient(t)
	freshToken, err := fresh.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease on the reconnected client failed: %v", err)
	}

	if err := stale.sendKeyboardReport(ctx, staleToken, 0, []byte{0x04}); !errors.Is(err, ErrHIDClosed) {
		t.Fatalf("send on the pre-reconnect client = %v, want ErrHIDClosed", err)
	}
	// A token from the dead client must not be honored by the new one.
	if err := fresh.sendKeyboardReport(ctx, staleToken+1000, 0, []byte{0x04}); !errors.Is(err, ErrStaleControlToken) {
		t.Fatalf("foreign token on the reconnected client = %v, want ErrStaleControlToken", err)
	}
	if err := fresh.sendKeyboardReport(ctx, freshToken, 0, []byte{0x04}); err != nil {
		t.Fatalf("reconnected client's own send failed: %v", err)
	}
	if freshTr.count() == 0 {
		t.Fatal("expected the reconnected client to deliver its own frames")
	}
}

// ---------------------------------------------------------------------------
// Bounded queueing and backpressure
// ---------------------------------------------------------------------------

func TestPionBufferedAmountGateBoundsLowerLayerQueue(t *testing.T) {
	neutral, err := neutralFrames()
	if err != nil {
		t.Fatalf("neutralFrames failed: %v", err)
	}
	var neutralBytes uint64
	for _, frame := range neutral {
		neutralBytes += uint64(len(frame))
	}
	if neutralBytes != hidNeutralBufferReserve {
		t.Fatalf("neutral buffer reserve = %d, canonical frames require %d", hidNeutralBufferReserve, neutralBytes)
	}

	frame, err := hidproto.EncodePointerReport(100, 200, 0x01)
	if err != nil {
		t.Fatalf("EncodePointerReport failed: %v", err)
	}
	inputLimit := hidMaxBufferedAmount - hidNeutralBufferReserve
	frameBytes := uint64(len(frame))

	t.Run("exact boundary is accepted", func(t *testing.T) {
		hc, tr := newFakeHIDClient(t)
		if got := tr.bufferedAmountLowThreshold(); got != hidBufferedAmountLowThreshold {
			t.Fatalf("Pion low threshold = %d, want %d", got, hidBufferedAmountLowThreshold)
		}
		token, err := hc.beginLease(context.Background())
		if err != nil {
			t.Fatalf("beginLease failed: %v", err)
		}

		tr.setAutoDrain(false)
		tr.setBufferedAmount(inputLimit - frameBytes)
		before := tr.count()
		if err := hc.sendPointerReport(contextWithTimeout(t, time.Second), token, 100, 200, 0x01); err != nil {
			t.Fatalf("send at exact buffer boundary failed: %v", err)
		}
		if got := tr.BufferedAmount(); got != inputLimit {
			t.Fatalf("buffered amount after boundary send = %d, want %d", got, inputLimit)
		}
		if got := tr.count(); got != before+1 {
			t.Fatalf("accepted boundary send changed frame count %d -> %d, want one frame", before, got)
		}
	})

	t.Run("one byte over is rejected before Send", func(t *testing.T) {
		hc, tr := newFakeHIDClient(t)
		token, err := hc.beginLease(context.Background())
		if err != nil {
			t.Fatalf("beginLease failed: %v", err)
		}
		if err := hc.sendPointerReport(contextWithTimeout(t, time.Second), token, 100, 200, 0x01); err != nil {
			t.Fatalf("initial held-button report failed: %v", err)
		}
		if !hc.hasHeldState() {
			t.Fatal("expected held state before the rejected clear report")
		}

		tr.setAutoDrain(false)
		tr.setBufferedAmount(inputLimit - frameBytes + 1)
		before := tr.count()
		err = hc.sendPointerReport(contextWithTimeout(t, time.Second), token, 100, 200, 0)
		if !errors.Is(err, ErrHIDBufferFull) {
			t.Fatalf("send over Pion buffer cap = %v, want ErrHIDBufferFull", err)
		}
		if got := tr.count(); got != before {
			t.Fatalf("rejected frame reached Send: frame count %d -> %d", before, got)
		}
		if !hc.hasHeldState() {
			t.Fatal("buffer-gate rejection cleared held state for a report that was never sent")
		}
	})

	t.Run("overflowing amount fails closed", func(t *testing.T) {
		hc, tr := newFakeHIDClient(t)
		token, err := hc.beginLease(context.Background())
		if err != nil {
			t.Fatalf("beginLease failed: %v", err)
		}

		tr.setAutoDrain(false)
		tr.setBufferedAmount(^uint64(0))
		before := tr.count()
		err = hc.sendPointerReport(contextWithTimeout(t, time.Second), token, 100, 200, 0x01)
		if !errors.Is(err, ErrHIDBufferFull) {
			t.Fatalf("send with overflowing buffered amount = %v, want ErrHIDBufferFull", err)
		}
		if got := tr.count(); got != before {
			t.Fatalf("overflowing buffered amount reached Send: frame count %d -> %d", before, got)
		}
	})
}

func TestUnconfirmedOrdinaryClearKeepsConservativeHeldState(t *testing.T) {
	tests := []struct {
		name      string
		sendHeld  func(context.Context, *hidClient, uint64) error
		sendClear func(context.Context, *hidClient, uint64) error
	}{
		{
			name: "keyboard",
			sendHeld: func(ctx context.Context, hc *hidClient, token uint64) error {
				return hc.sendKeyboardReport(ctx, token, 0x02, []byte{0x04})
			},
			sendClear: func(ctx context.Context, hc *hidClient, token uint64) error {
				return hc.sendKeyboardReport(ctx, token, 0, nil)
			},
		},
		{
			name: "mouse buttons",
			sendHeld: func(ctx context.Context, hc *hidClient, token uint64) error {
				return hc.sendPointerReport(ctx, token, 100, 200, 0x01)
			},
			sendClear: func(ctx context.Context, hc *hidClient, token uint64) error {
				return hc.sendPointerReport(ctx, token, 100, 200, 0)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc, tr := newFakeHIDClient(t)
			setupCtx := contextWithTimeout(t, time.Second)
			token, err := hc.beginLease(setupCtx)
			if err != nil {
				t.Fatalf("beginLease failed: %v", err)
			}
			if err := tt.sendHeld(setupCtx, hc, token); err != nil {
				t.Fatalf("held report failed: %v", err)
			}
			if !hc.hasHeldState() {
				t.Fatal("expected conservative held state after non-neutral report")
			}

			tr.setAutoDrain(false)
			if err := tt.sendClear(setupCtx, hc, token); err != nil {
				t.Fatalf("ordinary clear report was not accepted by Pion: %v", err)
			}
			if got := tr.BufferedAmount(); got == 0 {
				t.Fatal("ordinary clear report unexpectedly drained in stalled fake transport")
			}
			if !hc.hasHeldState() {
				t.Fatal("unconfirmed ordinary clear erased conservative held state")
			}

			releaseCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			err = hc.releaseAll(releaseCtx)
			if !errors.Is(err, ErrNeutralizeUnverified) {
				t.Fatalf("releaseAll after unconfirmed ordinary clear = %v, want ErrNeutralizeUnverified", err)
			}
			if !hc.hasHeldState() {
				t.Fatal("failed releaseAll exposed an already-cleared held model")
			}
		})
	}
}

func TestSendQueueIsBoundedAndAppliesBackpressure(t *testing.T) {
	hc, tr := newFakeHIDClient(t)

	if cap(hc.sendCh) != hidSendQueueDepth {
		t.Fatalf("send queue capacity = %d, want the bounded %d", cap(hc.sendCh), hidSendQueueDepth)
	}

	token, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}

	gate := make(chan struct{})
	defer close(gate)
	blocked := make(chan struct{})
	var once sync.Once
	tr.setBeforeSend(func(frame []byte) {
		if len(frame) == 0 || hidproto.MessageType(frame[0]) != hidproto.TypeKeyboardReport {
			return
		}
		once.Do(func() {
			close(blocked)
			<-gate
		})
	})

	ctx := contextWithTimeout(t, 10*time.Second)
	go func() { _ = hc.sendKeyboardReport(ctx, token, 0, []byte{0x04}) }()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("first send never reached the transport")
	}

	// Fill the bounded queue.
	for i := 0; i < hidSendQueueDepth; i++ {
		go func(i int) { _ = hc.sendKeyboardReport(ctx, token, 0, []byte{byte(i)}) }(i)
	}
	waitForCondition(t, 5*time.Second, func() bool { return len(hc.sendCh) == hidSendQueueDepth })

	// One more must block on backpressure and then fail on its own deadline,
	// rather than growing the queue or spawning an unbounded goroutine.
	shortCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = hc.sendKeyboardReport(shortCtx, token, 0, []byte{0x09})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("send against a full queue = %v, want a deadline error", err)
	}
	if got := len(hc.sendCh); got > hidSendQueueDepth {
		t.Fatalf("queue grew past its bound: %d > %d", got, hidSendQueueDepth)
	}
}

// TestSendAfterWriterExitFailsFast is a regression test. The queues are
// buffered, so a frame can be accepted by the channel just after the writer
// stops draining it. That frame will never be written, and the caller must
// learn so immediately rather than blocking until its own deadline - the
// difference between a control command that reports a disconnect and one
// that appears to hang.
func TestSendAfterWriterExitFailsFast(t *testing.T) {
	for i := 0; i < 50; i++ {
		hc, _ := newFakeHIDClient(t)
		token, err := hc.beginLease(context.Background())
		if err != nil {
			t.Fatalf("beginLease failed: %v", err)
		}
		hc.closeWith(errors.New("disconnected"))

		// A generous deadline: if the implementation regresses, this blocks
		// on it rather than returning promptly.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		start := time.Now()
		err = hc.sendKeyboardReport(ctx, token, 0, []byte{0x04})
		elapsed := time.Since(start)
		cancel()

		if !errors.Is(err, ErrHIDClosed) {
			t.Fatalf("iteration %d: send after writer exit = %v, want ErrHIDClosed", i, err)
		}
		if elapsed > time.Second {
			t.Fatalf("iteration %d: send took %v; it must fail fast, not wait for its deadline", i, elapsed)
		}
	}
}

func TestSendRespectsCallerCancellation(t *testing.T) {
	hc, _ := newFakeHIDClient(t)
	token, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hc.sendKeyboardReport(ctx, token, 0, []byte{0x04}); !errors.Is(err, context.Canceled) {
		t.Fatalf("send with a canceled context = %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// Release failure, retry, and truthful reporting
// ---------------------------------------------------------------------------

func TestReleaseAllWaitsForBufferedAmountDrain(t *testing.T) {
	hc, tr := newFakeHIDClient(t)
	ctx := contextWithTimeout(t, 5*time.Second)

	token, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}
	if err := hc.sendKeyboardReport(ctx, token, 0x02, []byte{0x04}); err != nil {
		t.Fatalf("SendKeyboardReport failed: %v", err)
	}
	if !hc.hasHeldState() {
		t.Fatal("expected held input before release")
	}

	tr.setAutoDrain(false)
	before := tr.count()
	releaseDone := make(chan error, 1)
	go func() { releaseDone <- hc.releaseAll(ctx) }()

	waitForCondition(t, 2*time.Second, func() bool {
		return tr.count() == before+2 && tr.BufferedAmount() > hidBufferedAmountLowThreshold
	})
	select {
	case err := <-releaseDone:
		t.Fatalf("releaseAll returned before Pion drained its neutral reports: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if !hc.hasHeldState() {
		t.Fatal("held state cleared before neutral reports were confirmed")
	}

	tr.setBufferedAmount(hidBufferedAmountLowThreshold)
	if err := <-releaseDone; err != nil {
		t.Fatalf("releaseAll failed after the buffered amount drained: %v", err)
	}
	if hc.hasHeldState() {
		t.Fatal("held state remained after neutral reports were confirmed")
	}
}

func TestReleaseAllDeadlineReportsNeutralStateUnconfirmed(t *testing.T) {
	hc, tr := newFakeHIDClient(t)
	setupCtx := contextWithTimeout(t, time.Second)

	token, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}
	if err := hc.sendKeyboardReport(setupCtx, token, 0x02, []byte{0x04}); err != nil {
		t.Fatalf("SendKeyboardReport failed: %v", err)
	}

	tr.setAutoDrain(false)
	before := tr.count()
	releaseCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	releaseDone := make(chan error, 1)
	go func() { releaseDone <- hc.releaseAll(releaseCtx) }()

	waitForCondition(t, 2*time.Second, func() bool {
		return tr.count() == before+2 && tr.BufferedAmount() > hidBufferedAmountLowThreshold
	})
	// A stale/spurious callback must not turn an above-threshold level into
	// a false confirmation.
	tr.signalBufferedAmountLow()
	select {
	case err := <-releaseDone:
		t.Fatalf("releaseAll trusted a low callback without rechecking the level: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	err = <-releaseDone
	if !errors.Is(err, ErrNeutralizeUnverified) {
		t.Fatalf("stalled releaseAll = %v, want ErrNeutralizeUnverified", err)
	}
	if !strings.Contains(err.Error(), "neutral HID state is not confirmed") {
		t.Fatalf("stalled release error does not state the truthful outcome: %v", err)
	}
	if !hc.hasHeldState() {
		t.Fatal("an unconfirmed release must retain the held-input model")
	}
	if hc.activeGeneration() != 0 {
		t.Fatal("an unconfirmed release must still revoke the lease generation")
	}
}

func TestReleaseAllSerializesBufferedAmountWaiters(t *testing.T) {
	hc, tr := newFakeHIDClient(t)
	tr.setAutoDrain(false)
	before := tr.count()
	ctx := contextWithTimeout(t, 3*time.Second)

	firstDone := make(chan error, 1)
	go func() { firstDone <- hc.releaseAll(ctx) }()
	waitForCondition(t, time.Second, func() bool {
		return tr.count() == before+2 && tr.BufferedAmount() > 0
	})

	secondDone := make(chan error, 1)
	go func() { secondDone <- hc.releaseAll(ctx) }()
	select {
	case err := <-secondDone:
		t.Fatalf("second releaseAll bypassed the active drain waiter: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if got := tr.count(); got != before+2 {
		t.Fatalf("concurrent releaseAll enqueued %d frames before the first drain completed, want %d", got, before+2)
	}

	tr.setBufferedAmount(0)
	if err := <-firstDone; err != nil {
		t.Fatalf("first releaseAll failed after drain: %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		return tr.count() == before+4 && tr.BufferedAmount() > 0
	})
	tr.setBufferedAmount(0)
	if err := <-secondDone; err != nil {
		t.Fatalf("serialized releaseAll failed after its drain: %v", err)
	}
}

func TestReleaseAllRetriesAfterATransientFailure(t *testing.T) {
	hc, tr := newFakeHIDClient(t)
	ctx := contextWithTimeout(t, 5*time.Second)

	token, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}
	if err := hc.sendKeyboardReport(ctx, token, 0x02, []byte{0x04}); err != nil {
		t.Fatalf("SendKeyboardReport failed: %v", err)
	}

	// Fail the first neutralization frame; the retry must succeed.
	tr.setFailure(1, errors.New("transient channel failure"))

	if err := hc.releaseAll(ctx); err != nil {
		t.Fatalf("releaseAll did not recover from a transient failure: %v", err)
	}
	if hc.hasHeldState() {
		t.Error("held input should be cleared once the release is confirmed")
	}

	types := tr.frameTypes()
	if len(types) < 2 {
		t.Fatalf("expected the retried neutralization frames on the wire, got %v", types)
	}
	if types[len(types)-2] != hidproto.TypeKeyboardReport || types[len(types)-1] != hidproto.TypeMouseReport {
		t.Fatalf("final frames = %v, want a keyboard clear followed by a zero-delta mouse report", types)
	}
}

func TestReleaseAllReportsUnverifiedAndKeepsHeldState(t *testing.T) {
	hc, tr := newFakeHIDClient(t)
	ctx := contextWithTimeout(t, 5*time.Second)

	token, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}
	if err := hc.sendKeyboardReport(ctx, token, 0x02, []byte{0x04}); err != nil {
		t.Fatalf("SendKeyboardReport failed: %v", err)
	}
	if !hc.hasHeldState() {
		t.Fatal("expected held input before the release")
	}

	tr.setFailure(-1, errors.New("channel is gone")) // fail permanently

	err = hc.releaseAll(ctx)
	if !errors.Is(err, ErrNeutralizeUnverified) {
		t.Fatalf("releaseAll = %v, want ErrNeutralizeUnverified", err)
	}
	if !hc.hasHeldState() {
		t.Error("an unverified release must not clear the held-input model: it would claim a safety it cannot prove")
	}
	// The lease is still revoked even though neutralization failed, so no
	// further input can be sent under it.
	if hc.activeGeneration() != 0 {
		t.Error("a failed release must still revoke the lease generation")
	}
}

func TestReleaseAllOnANeverReadyChannelDoesNotOverclaim(t *testing.T) {
	hc, tr := newUnreadyHIDClient(t)

	// Nothing could ever have been sent, so there is nothing to neutralize
	// and nothing to report as unverified.
	if err := hc.releaseAll(contextWithTimeout(t, time.Second)); err != nil {
		t.Fatalf("releaseAll on an unused channel = %v, want nil", err)
	}
	if n := tr.count(); n != 0 {
		t.Fatalf("release-all wrote %d frames on a channel that was never ready, want 0", n)
	}
}

func TestStateStringsAreStable(t *testing.T) {
	for state, want := range map[hidState]string{
		hidStateNegotiating: "negotiating",
		hidStateHandshaking: "handshaking",
		hidStateReady:       "ready",
		hidStateClosed:      "closed",
	} {
		if got := state.String(); got != want {
			t.Errorf("hidState(%d).String() = %q, want %q", state, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Wire-format tests against real Pion data channels
// ---------------------------------------------------------------------------

// fakeDevice mimics hidrpc.go's handleHidRPCMessage over a real data
// channel: echo the handshake back (which is what flips the real firmware's
// hidRPCAvailable to true) and record every report received.
type fakeDevice struct {
	mu               sync.Mutex
	keyboardReports  []hidproto.Message
	pointerReports   []hidproto.Message
	mouseReports     []hidproto.Message
	handshakesEchoed int
}

func newFakeHIDDevice(t *testing.T, pc *webrtc.PeerConnection) (*fakeDevice, chan *webrtc.DataChannel) {
	t.Helper()
	fd := &fakeDevice{}
	ch := make(chan *webrtc.DataChannel, 1)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "hidrpc" {
			return
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			m, err := hidproto.Unmarshal(msg.Data)
			if err != nil {
				return
			}
			switch m.Type {
			case hidproto.TypeHandshake:
				fd.mu.Lock()
				fd.handshakesEchoed++
				fd.mu.Unlock()
				_ = dc.Send(msg.Data) // echo, as the real firmware does
			case hidproto.TypeKeyboardReport:
				fd.mu.Lock()
				fd.keyboardReports = append(fd.keyboardReports, m)
				fd.mu.Unlock()
			case hidproto.TypePointerReport:
				fd.mu.Lock()
				fd.pointerReports = append(fd.pointerReports, m)
				fd.mu.Unlock()
			case hidproto.TypeMouseReport:
				fd.mu.Lock()
				fd.mouseReports = append(fd.mouseReports, m)
				fd.mu.Unlock()
			}
		})
		ch <- dc
	})
	return fd, ch
}

func (fd *fakeDevice) lastKeyboardReport() (hidproto.Message, bool) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.keyboardReports) == 0 {
		return hidproto.Message{}, false
	}
	return fd.keyboardReports[len(fd.keyboardReports)-1], true
}

func (fd *fakeDevice) lastPointerReport() (hidproto.Message, bool) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.pointerReports) == 0 {
		return hidproto.Message{}, false
	}
	return fd.pointerReports[len(fd.pointerReports)-1], true
}

func (fd *fakeDevice) pointerReportCount() int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return len(fd.pointerReports)
}

func (fd *fakeDevice) lastMouseReport() (hidproto.Message, bool) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.mouseReports) == 0 {
		return hidproto.Message{}, false
	}
	return fd.mouseReports[len(fd.mouseReports)-1], true
}

// setupHIDPair wires a hidClient to a fake device over two real Pion peer
// connections, including the OnMessage plumbing that session.go installs in
// production, and completes the readiness handshake.
func setupHIDPair(t *testing.T) (*hidClient, *fakeDevice) {
	t.Helper()
	hc, fd, _ := setupHIDPairOn(t, newPeerPair(t))
	return hc, fd
}

func setupHIDPairOn(t *testing.T, pair *peerPair) (*hidClient, *fakeDevice, *webrtc.DataChannel) {
	t.Helper()
	fd, deviceCh := newFakeHIDDevice(t, pair.b)

	clientDC, err := pair.a.CreateDataChannel("hidrpc", nil)
	if err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	hc := newHIDClient(clientDC)
	clientDC.OnMessage(func(msg webrtc.DataChannelMessage) { hc.handleMessage(msg.Data) })
	t.Cleanup(func() { hc.closeWith(errors.New("test cleanup")) })

	pair.connect(t)

	ctx := contextWithTimeout(t, connectTimeout(t, 10*time.Second))
	waitDataChannelOpen(t, ctx, clientDC)
	<-deviceCh

	if err := hc.handshake(ctx); err != nil {
		t.Fatalf("handshake failed: %v", err)
	}

	return hc, fd, clientDC
}

func TestHIDClientHandshake(t *testing.T) {
	hc, fd := setupHIDPair(t)
	waitForCondition(t, 2*time.Second, func() bool {
		fd.mu.Lock()
		defer fd.mu.Unlock()
		return fd.handshakesEchoed == 1
	})
	if got := hc.currentState(); got != hidStateReady {
		t.Errorf("state after a real handshake = %s, want ready", got)
	}
}

func TestHIDClientSendKeyboardReport(t *testing.T) {
	hc, fd := setupHIDPair(t)
	ctx := contextWithTimeout(t, 5*time.Second)

	token, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}
	if err := hc.sendKeyboardReport(ctx, token, 0x02, []byte{0x04, 0x05}); err != nil {
		t.Fatalf("sendKeyboardReport failed: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		_, ok := fd.lastKeyboardReport()
		return ok
	})

	msg, ok := fd.lastKeyboardReport()
	if !ok {
		t.Fatal("device never received a keyboard report")
	}
	if msg.Payload[0] != 0x02 {
		t.Errorf("modifier = %d, want 2", msg.Payload[0])
	}
	if !hc.hasHeldState() {
		t.Error("expected hasHeldState() == true after sending a nonzero keyboard report")
	}
}

func TestHIDClientReleaseAllOverRealChannelClearsStateWithoutMovingCursor(t *testing.T) {
	pair := newPeerPair(t)
	hc, fd, clientDC := setupHIDPairOn(t, pair)
	ctx := contextWithTimeout(t, 5*time.Second)

	token, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}

	// Simulate a "stuck key": press modifier+key and a mouse button with no
	// matching release, as if the caller crashed mid-gesture.
	if err := hc.sendKeyboardReport(ctx, token, 0x02, []byte{0x04}); err != nil {
		t.Fatalf("sendKeyboardReport failed: %v", err)
	}
	if err := hc.sendPointerReport(ctx, token, 100, 100, 0x01); err != nil {
		t.Fatalf("sendPointerReport failed: %v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool {
		_, kbOK := fd.lastKeyboardReport()
		_, ptrOK := fd.lastPointerReport()
		return kbOK && ptrOK
	})
	if !hc.hasHeldState() {
		t.Fatal("expected held state to be nonzero before release")
	}
	pointerReportsBefore := fd.pointerReportCount()

	if err := hc.releaseAll(ctx); err != nil {
		t.Fatalf("releaseAll failed: %v", err)
	}
	if got := clientDC.BufferedAmount(); got > hidBufferedAmountLowThreshold {
		t.Fatalf("releaseAll returned with %d bytes still buffered, threshold %d", got, hidBufferedAmountLowThreshold)
	}
	if hc.hasHeldState() {
		t.Error("expected no held state after release")
	}

	waitForCondition(t, 2*time.Second, func() bool {
		kb, ok := fd.lastKeyboardReport()
		if !ok || kb.Payload[0] != 0 || !allZero(kb.Payload[1:]) {
			return false
		}
		_, mouseOK := fd.lastMouseReport()
		return mouseOK
	})

	mouse, ok := fd.lastMouseReport()
	if !ok {
		t.Fatal("device never received the neutralizing relative-mouse report")
	}
	if mouse.Payload[0] != 0 || mouse.Payload[1] != 0 || mouse.Payload[2] != 0 {
		t.Errorf("neutralizing mouse report = % x, want all zero", mouse.Payload)
	}
	if got := fd.pointerReportCount(); got != pointerReportsBefore {
		t.Errorf("release-all sent %d additional absolute pointer reports, want 0 (it must not move the cursor)",
			got-pointerReportsBefore)
	}
}

func TestHIDClientReleaseAllFailsWhileRealPionChannelIsStalled(t *testing.T) {
	pair, forward := newStallablePeerPair(t)
	hc, fd, clientDC := setupHIDPairOn(t, pair)
	setupCtx := contextWithTimeout(t, connectTimeout(t, 10*time.Second))

	token, err := hc.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease failed: %v", err)
	}
	if err := hc.sendKeyboardReport(setupCtx, token, 0x02, []byte{0x04}); err != nil {
		t.Fatalf("sendKeyboardReport failed: %v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool {
		kb, ok := fd.lastKeyboardReport()
		return ok && kb.Payload[0] == 0x02
	})
	waitForCondition(t, 2*time.Second, func() bool {
		return clientDC.BufferedAmount() == hidBufferedAmountLowThreshold
	})
	if !hc.hasHeldState() {
		t.Fatal("expected held state before stalling the channel")
	}

	// Stop every virtual-network packet without closing either peer. Pion's
	// Send still accepts the neutral frames, which recreates the audited
	// open-but-stalled channel where enqueue success is not wire confirmation.
	forward.Store(false)
	if err := hc.sendKeyboardReport(setupCtx, token, 0, nil); err != nil {
		t.Fatalf("ordinary keyboard clear was not accepted on stalled channel: %v", err)
	}
	if !hc.hasHeldState() {
		t.Fatal("unacknowledged ordinary keyboard clear erased conservative held state")
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err = hc.releaseAll(releaseCtx)
	if !errors.Is(err, ErrNeutralizeUnverified) {
		t.Fatalf("releaseAll on stalled real Pion channel = %v, want ErrNeutralizeUnverified", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled releaseAll did not retain its deadline cause: %v", err)
	}
	if got := clientDC.ReadyState(); got != webrtc.DataChannelStateOpen {
		t.Fatalf("stalled DataChannel state = %s, want open", got)
	}
	if got := clientDC.BufferedAmount(); got <= hidBufferedAmountLowThreshold {
		t.Fatalf("stalled releaseAll left BufferedAmount=%d, want above %d", got, hidBufferedAmountLowThreshold)
	}
	if !hc.hasHeldState() {
		t.Fatal("stalled releaseAll cleared held state without wire confirmation")
	}

	// Let teardown and any outstanding SCTP work finish normally.
	forward.Store(true)
}

func TestHIDClientReleaseAllIsSafeWithNothingHeld(t *testing.T) {
	hc, _ := setupHIDPair(t)
	if hc.hasHeldState() {
		t.Fatal("expected no held state on a fresh client")
	}
	if err := hc.releaseAll(contextWithTimeout(t, 5*time.Second)); err != nil {
		t.Fatalf("releaseAll on an idle client failed: %v", err)
	}
	if hc.hasHeldState() {
		t.Error("expected no held state after release on an idle client")
	}
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition not met before timeout")
	}
}
