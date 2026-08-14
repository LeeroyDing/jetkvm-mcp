package jetkvm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/hidproto"
)

// hidTransport is the narrow slice of *webrtc.DataChannel this package
// needs to write HID frames. Keeping it an interface lets the concurrency
// tests drive the state machine against a transport whose sends can be
// blocked, failed and observed in order, which is the only way to prove
// the send-versus-release ordering guarantees below deterministically.
type hidTransport interface {
	Send(b []byte) error
}

// hidState is the explicit control-plane state of one "hidrpc" data
// channel. Every rule about "may this frame reach the device" is expressed
// as a function of (state, lease generation) and evaluated at exactly one
// place - hidClient.write - rather than being spread across call sites.
//
//	negotiating --handshake()--> handshaking --device echo--> ready
//	     |                            |                         |
//	     +----------------------------+-------------------------+
//	                                  v
//	                               closed (terminal)
//
// There is no transition out of closed: a dropped or failed hidrpc channel
// is not silently reused, and a new channel means a new hidClient with a
// fresh generation counter, so tokens can never be replayed across a
// reconnect.
type hidState int

const (
	// hidStateNegotiating is the initial state: the data channel exists but
	// the device has not confirmed it is honoring HID-RPC yet. No frame of
	// any kind may be written.
	hidStateNegotiating hidState = iota
	// hidStateHandshaking means the readiness handshake frame is in flight.
	// Only privileged frames (the handshake itself) may be written.
	hidStateHandshaking
	// hidStateReady means the device echoed the handshake, which is what
	// flips the firmware's hidRPCAvailable to true. Input frames may be
	// written, subject to lease-generation validation.
	hidStateReady
	// hidStateClosed is terminal: disconnect, handshake failure, or an
	// explicit shutdown. No frame may be written.
	hidStateClosed
)

func (s hidState) String() string {
	switch s {
	case hidStateNegotiating:
		return "negotiating"
	case hidStateHandshaking:
		return "handshaking"
	case hidStateReady:
		return "ready"
	case hidStateClosed:
		return "closed"
	}
	return "unknown"
}

// Control-plane errors. These are the truthful, non-sensitive failure
// states callers (CLI, MCP tools) are expected to surface verbatim.
var (
	// ErrHIDNotReady means the device never confirmed the HID-RPC readiness
	// handshake, so this client refuses to pretend input was delivered.
	ErrHIDNotReady = errors.New("jetkvm: HID control channel is not ready (readiness handshake not confirmed by the device)")

	// ErrHIDClosed means the HID control channel is gone (disconnect,
	// shutdown, or a failed handshake). It wraps the underlying cause.
	ErrHIDClosed = errors.New("jetkvm: HID control channel is closed")

	// ErrStaleControlToken means a send was validated against a control
	// lease that has since been released, expired, or been replaced. The
	// frame was dropped before reaching the device - it is not a "maybe".
	ErrStaleControlToken = errors.New("jetkvm: control lease token is stale; the queued input was dropped and never sent")

	// ErrNeutralizeUnverified means release-all could not be confirmed on
	// the wire. Callers must treat held input as possibly still held.
	ErrNeutralizeUnverified = errors.New("jetkvm: release-all could not be confirmed on the wire; assume input may still be held")
)

const (
	// hidSendQueueDepth bounds how many input frames may be queued behind
	// the single writer. Backpressure (a blocked, context-bounded enqueue)
	// is deliberate: an unbounded queue would let a slow device accumulate
	// arbitrarily many stale keystrokes.
	hidSendQueueDepth = 16

	// hidPriorityQueueDepth bounds the neutralization/handshake queue. It
	// only ever holds the two release-all frames plus the handshake.
	hidPriorityQueueDepth = 4

	// hidReleaseAttempts is how many times release-all is retried before it
	// is reported as unverified.
	hidReleaseAttempts = 2

	// hidReleaseRetryDelay spaces out release-all retries.
	hidReleaseRetryDelay = 20 * time.Millisecond
)

// heldInput is this client's local model of what the attached computer
// currently believes is held down. It exists so release-all is
// deterministic without waiting on a device round trip.
type heldInput struct {
	modifier byte
	keys     [hidproto.HIDKeyBufferSize]byte
	buttons  byte
}

func (h heldInput) any() bool {
	if h.modifier != 0 || h.buttons != 0 {
		return true
	}
	for _, k := range h.keys {
		if k != 0 {
			return true
		}
	}
	return false
}

// hidRequest is one frame handed to the single writer goroutine.
type hidRequest struct {
	frame []byte

	// token is the control-lease generation this frame was authorized
	// under. It is re-validated at the final point before the write, not
	// at the call site, so a lease that ends while this frame sits in the
	// queue causes the frame to be dropped rather than delivered late.
	token uint64

	// privileged marks handshake and neutralization frames. They bypass
	// lease validation (they are how a lease is established or ended) and
	// are served from a separate queue that pre-empts queued input.
	privileged bool

	// held, when non-nil, is recorded as the new held-input model if and
	// only if this frame is actually written.
	held *heldInput

	result chan error
}

// hidClient owns one "hidrpc" WebRTC data channel: the readiness
// handshake, the explicit state machine above, lease-generation
// validation, and a single writer goroutine that is the only thing in the
// process permitted to put bytes on the channel.
//
// # Lock ordering
//
// There is exactly one mutex here, stateMu, and it is the innermost lock
// in this package. The full order is:
//
//	controlLease.slot (a channel semaphore, not a mutex) -> hidClient.stateMu
//
// stateMu is never held while acquiring another lock, never held across a
// channel send or a receive, and - critically - never taken by a Pion
// callback. handleMessage uses its own leaf mutex (keydownMu) so that the
// data channel's read pump can never block on stateMu.
type hidClient struct {
	channel hidTransport

	stateMu    sync.Mutex
	state      hidState
	closeCause error
	// activeGen is the single lease generation currently permitted to
	// send. Zero means no holder: every non-privileged frame is dropped.
	activeGen uint64
	// genCounter only ever increases, so a token is never reused - not
	// across release, expiry, holder replacement, or channel teardown.
	genCounter uint64
	held       heldInput

	sendCh     chan hidRequest
	priorityCh chan hidRequest
	stop       chan struct{}
	stopOnce   sync.Once
	writerDone chan struct{}

	handshakeDone chan struct{}
	handshakeOnce sync.Once
}

func newHIDClient(channel hidTransport) *hidClient {
	h := &hidClient{
		channel:       channel,
		state:         hidStateNegotiating,
		sendCh:        make(chan hidRequest, hidSendQueueDepth),
		priorityCh:    make(chan hidRequest, hidPriorityQueueDepth),
		stop:          make(chan struct{}),
		writerDone:    make(chan struct{}),
		handshakeDone: make(chan struct{}),
	}
	go h.writeLoop()
	return h
}

// handleMessage processes one inbound HID-RPC frame. It runs on Pion's
// data channel read goroutine and deliberately never touches stateMu: if it
// could block on that lock, a slow write could stall the read pump and
// deadlock the channel.
//
// Only the handshake echo is acted on. The device also sends unsolicited
// keydown-state reports (hidproto.DecodeKeydownState decodes them), but
// nothing here consumes them: this client's release-all guarantees are
// deliberately based on what it wrote to the channel, not on a device
// report it would have to wait for.
func (h *hidClient) handleMessage(data []byte) {
	m, err := hidproto.Unmarshal(data)
	if err != nil {
		return
	}
	if m.Type == hidproto.TypeHandshake {
		h.handshakeOnce.Do(func() { close(h.handshakeDone) })
	}
}

// writeLoop is the single writer. Serving priorityCh first, and again
// before every blocking select, is what makes release-all *pre-emptive*:
// a neutralization frame jumps ahead of every queued input frame, and
// those queued frames are then dropped as stale when they are dequeued.
func (h *hidClient) writeLoop() {
	defer close(h.writerDone)
	for {
		select {
		case req := <-h.priorityCh:
			h.write(req)
			continue
		default:
		}

		select {
		case req := <-h.priorityCh:
			h.write(req)
		case req := <-h.sendCh:
			h.write(req)
		case <-h.stop:
			h.drain()
			return
		}
	}
}

// drain fails every queued frame once the channel is closed, so no caller
// is left waiting on a frame that will never be written.
func (h *hidClient) drain() {
	for {
		select {
		case req := <-h.priorityCh:
			req.complete(h.closedErr())
		case req := <-h.sendCh:
			req.complete(h.closedErr())
		default:
			return
		}
	}
}

func (r hidRequest) complete(err error) {
	if r.result == nil {
		return
	}
	select {
	case r.result <- err:
	default: // buffered(1) and written once; never blocks the writer
	}
}

// write is the single final validation point before any byte reaches the
// device. Nothing else in this package calls channel.Send.
func (h *hidClient) write(req hidRequest) {
	h.stateMu.Lock()
	err := h.checkWritableLocked(req)
	if err == nil && req.held != nil {
		// Recorded before the write, not after: if the send fails partway
		// we must assume the device may have applied it, so release-all
		// still has something concrete to clear.
		h.held = *req.held
	}
	h.stateMu.Unlock()

	if err != nil {
		req.complete(err)
		return
	}
	req.complete(h.channel.Send(req.frame))
}

func (h *hidClient) checkWritableLocked(req hidRequest) error {
	switch h.state {
	case hidStateClosed:
		return h.closedErrLocked()
	case hidStateNegotiating:
		return ErrHIDNotReady
	case hidStateHandshaking:
		if !req.privileged {
			return ErrHIDNotReady
		}
		return nil
	case hidStateReady:
	default:
		return ErrHIDNotReady
	}

	if req.privileged {
		return nil
	}
	if req.token == 0 || req.token != h.activeGen {
		return ErrStaleControlToken
	}
	return nil
}

// enqueue hands a frame to the writer and waits for its outcome, bounded
// by ctx at both steps. A full queue blocks (backpressure) rather than
// growing without limit or spawning a goroutine per send.
func (h *hidClient) enqueue(ctx context.Context, req hidRequest) error {
	req.result = make(chan error, 1)
	target := h.sendCh
	if req.privileged {
		target = h.priorityCh
	}

	select {
	case target <- req:
	case <-h.writerDone:
		return h.closedErr()
	case <-ctx.Done():
		return fmt.Errorf("jetkvm: queuing HID frame: %w", ctx.Err())
	}

	// Waiting on writerDone as well as the result matters: the queues are
	// buffered, so a frame can be accepted by the channel moments after the
	// writer has stopped draining it. Without this, such a frame would sit
	// unwritten until the caller's deadline expired instead of failing
	// immediately with the real reason.
	select {
	case err := <-req.result:
		return err
	case <-h.writerDone:
		// The writer may have completed this frame on its way out; a
		// delivered result always wins over the shutdown signal.
		select {
		case err := <-req.result:
			return err
		default:
		}
		return h.closedErr()
	case <-ctx.Done():
		// The frame may still be written, but only if its lease token is
		// still valid at that moment - so abandoning the wait here can
		// never deliver input on behalf of an ended lease.
		return fmt.Errorf("jetkvm: awaiting HID frame write: %w", ctx.Err())
	}
}

// handshake performs the production readiness handshake and is the only
// path to hidStateReady. Until the device echoes the handshake back, the
// firmware ignores HID-RPC frames entirely (hidrpc.go sets
// hidRPCAvailable = true only on that echo), so sending input before this
// completes would silently do nothing while reporting success.
func (h *hidClient) handshake(ctx context.Context) error {
	h.stateMu.Lock()
	switch h.state {
	case hidStateReady:
		h.stateMu.Unlock()
		return nil
	case hidStateClosed:
		err := h.closedErrLocked()
		h.stateMu.Unlock()
		return err
	case hidStateHandshaking:
		h.stateMu.Unlock()
		return fmt.Errorf("jetkvm: HID readiness handshake is already in progress")
	}
	h.state = hidStateHandshaking
	h.stateMu.Unlock()

	frame, err := hidproto.EncodeHandshake()
	if err != nil {
		h.closeWith(err)
		return err
	}
	if err := h.enqueue(ctx, hidRequest{frame: frame, privileged: true}); err != nil {
		wrapped := fmt.Errorf("jetkvm: sending HID readiness handshake: %w", err)
		h.closeWith(wrapped)
		return wrapped
	}

	select {
	case <-h.handshakeDone:
	case <-h.writerDone:
		return h.closedErr()
	case <-ctx.Done():
		wrapped := fmt.Errorf("jetkvm: HID readiness handshake was not confirmed by the device: %w", ctx.Err())
		h.closeWith(wrapped)
		return wrapped
	}

	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if h.state == hidStateClosed {
		return h.closedErrLocked()
	}
	h.state = hidStateReady
	return nil
}

// beginLease issues a fresh, never-reused generation token to a new lease
// holder. It fails unless the readiness handshake has completed, which is
// what makes "no input without a confirmed handshake" structural rather
// than a call-site convention.
func (h *hidClient) beginLease() (uint64, error) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	switch h.state {
	case hidStateClosed:
		return 0, h.closedErrLocked()
	case hidStateReady:
	default:
		return 0, ErrHIDNotReady
	}
	h.genCounter++
	h.activeGen = h.genCounter
	return h.activeGen, nil
}

// invalidateLeaseLocked revokes the current holder and burns the
// generation, so neither the outgoing token nor any token that might be
// issued next can match a frame queued before this moment.
func (h *hidClient) invalidateLeaseLocked() {
	h.activeGen = 0
	h.genCounter++
}

// releaseAll neutralizes all keyboard and mouse state.
//
// Ordering guarantee: the lease generation is invalidated *before* the
// neutralization frames are queued. Every input frame already queued is
// therefore stale by construction and is dropped at the final validation
// point; the neutralization frames are served from the priority queue, so
// they are not stuck behind the very input they are meant to cancel. The
// net effect is that a neutralization frame is the last HID frame written
// for that generation.
//
// It never injects pointer movement: buttons are cleared with a zero-delta
// relative mouse report, never an absolute pointer report, so neutralizing
// state cannot warp the attached computer's cursor.
func (h *hidClient) releaseAll(ctx context.Context) error {
	return h.releaseAllMode(ctx, false)
}

// releaseAllRequired is used by an explicit lease Release. Unlike idle
// Client.Close cleanup, it must prove both neutral frames reached a ready
// channel even when this fresh process has no locally tracked held state: an
// explicit release-all exists precisely to clear state left by an earlier
// session. A disconnected empty local model is therefore unverified, not a
// successful no-op.
func (h *hidClient) releaseAllRequired(ctx context.Context) error {
	return h.releaseAllMode(ctx, true)
}

func (h *hidClient) releaseAllMode(ctx context.Context, requireDelivery bool) error {
	h.stateMu.Lock()
	h.invalidateLeaseLocked()
	state := h.state
	cause := h.closeCause
	hadHeld := h.held.any()
	h.stateMu.Unlock()

	if state != hidStateReady {
		if !hadHeld && !requireDelivery {
			// Nothing could ever have been sent through this channel, so
			// there is nothing to neutralize and nothing to over-claim.
			return nil
		}
		if cause != nil {
			return fmt.Errorf("%w: channel %s: %v", ErrNeutralizeUnverified, state, cause)
		}
		return fmt.Errorf("%w: channel %s", ErrNeutralizeUnverified, state)
	}

	frames, err := neutralFrames()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNeutralizeUnverified, err)
	}

	var lastErr error
	for attempt := 0; attempt < hidReleaseAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(hidReleaseRetryDelay):
			case <-ctx.Done():
				return fmt.Errorf("%w: %v", ErrNeutralizeUnverified, ctx.Err())
			}
		}

		lastErr = nil
		for _, frame := range frames {
			if err := h.enqueue(ctx, hidRequest{frame: frame, privileged: true}); err != nil {
				lastErr = err
				break
			}
		}
		if lastErr == nil {
			h.stateMu.Lock()
			h.held = heldInput{}
			h.stateMu.Unlock()
			return nil
		}
		if ctx.Err() != nil {
			break
		}
	}

	// Deliberately does not clear the held model: if we could not confirm
	// the release, this client must keep believing input is held.
	return fmt.Errorf("%w: %v", ErrNeutralizeUnverified, lastErr)
}

// neutralFrames is the canonical neutral HID state: an all-zero keyboard
// report and a zero-delta relative mouse report. See
// hidproto.ReleaseAllMouseReport for why this is not a pointer report.
func neutralFrames() ([][]byte, error) {
	kb, err := hidproto.ReleaseAllKeyboardReport()
	if err != nil {
		return nil, err
	}
	mouse, err := hidproto.ReleaseAllMouseReport()
	if err != nil {
		return nil, err
	}
	return [][]byte{kb, mouse}, nil
}

// closeWith moves the state machine to its terminal state, revokes the
// current lease generation, and stops the writer. Safe to call repeatedly
// and from any goroutine, including Pion callbacks.
func (h *hidClient) closeWith(cause error) {
	h.stateMu.Lock()
	if h.state != hidStateClosed {
		h.state = hidStateClosed
		h.closeCause = cause
		h.invalidateLeaseLocked()
	}
	h.stateMu.Unlock()
	h.stopOnce.Do(func() { close(h.stop) })
}

func (h *hidClient) closedErr() error {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	return h.closedErrLocked()
}

func (h *hidClient) closedErrLocked() error {
	if h.closeCause == nil {
		return ErrHIDClosed
	}
	return fmt.Errorf("%w: %v", ErrHIDClosed, h.closeCause)
}

// currentState reports the state machine's state, for diagnostics and
// tests.
func (h *hidClient) currentState() hidState {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	return h.state
}

// sendKeyboardReport queues a full keyboard state report (modifier + up to
// six keys) under the given lease token.
func (h *hidClient) sendKeyboardReport(ctx context.Context, token uint64, modifier byte, keys []byte) error {
	frame, err := hidproto.EncodeKeyboardReport(modifier, keys)
	if err != nil {
		return err
	}
	held := heldInput{modifier: modifier}
	copy(held.keys[:], keys)

	h.stateMu.Lock()
	held.buttons = h.held.buttons
	h.stateMu.Unlock()

	return h.enqueue(ctx, hidRequest{frame: frame, token: token, held: &held})
}

// sendPointerReport queues an absolute-mouse report under the given lease
// token.
func (h *hidClient) sendPointerReport(ctx context.Context, token uint64, x, y int32, buttons byte) error {
	frame, err := hidproto.EncodePointerReport(x, y, buttons)
	if err != nil {
		return err
	}
	return h.enqueue(ctx, hidRequest{frame: frame, token: token, held: h.heldWithButtons(buttons)})
}

// sendMouseReport queues a relative-mouse report under the given lease
// token.
func (h *hidClient) sendMouseReport(ctx context.Context, token uint64, dx, dy int8, buttons byte) error {
	frame, err := hidproto.EncodeMouseReport(dx, dy, buttons)
	if err != nil {
		return err
	}
	return h.enqueue(ctx, hidRequest{frame: frame, token: token, held: h.heldWithButtons(buttons)})
}

func (h *hidClient) heldWithButtons(buttons byte) *heldInput {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	held := h.held
	held.buttons = buttons
	return &held
}

// hasHeldState reports whether this client believes any key or button is
// currently held, per its local model. It reflects only frames that were
// actually written - frames dropped as stale never mark input as held.
func (h *hidClient) hasHeldState() bool {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	return h.held.any()
}
