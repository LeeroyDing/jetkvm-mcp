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
	BufferedAmount() uint64
	SetBufferedAmountLowThreshold(th uint64)
	OnBufferedAmountLow(f func())
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
	// shutdown, or a failed handshake). Classified closed-channel errors
	// retain this sentinel for errors.Is without exposing an unredacted cause.
	ErrHIDClosed = errors.New("jetkvm: HID control channel is closed")

	// ErrStaleControlToken means a send was validated against a control
	// lease that has since been released, expired, or been replaced. The
	// frame was dropped before reaching the device - it is not a "maybe".
	ErrStaleControlToken = errors.New("jetkvm: control lease token is stale; the queued input was dropped and never sent")

	// ErrNeutralizeUnverified means release-all could not be confirmed on
	// the wire. Callers must treat held input as possibly still held.
	ErrNeutralizeUnverified = errors.New("jetkvm: neutral HID state is not confirmed on the wire; assume input may still be held")

	// ErrHIDBufferFull means a frame was rejected before Send because
	// accepting it would exceed the bounded Pion/SCTP outbound buffer. The
	// caller may retry under a fresh lease after the peer starts draining.
	ErrHIDBufferFull = errors.New("jetkvm: outbound HID buffer is full; frame was not sent")
)

const (
	// hidSendQueueDepth bounds how many input frames may be queued behind
	// the single writer. Backpressure (a blocked, context-bounded enqueue)
	// is deliberate: an unbounded queue would let a slow device accumulate
	// arbitrarily many stale keystrokes.
	hidSendQueueDepth = 16

	// hidPriorityQueueDepth bounds the neutralization/handshake queue. It
	// only ever holds the three possible release-all frames plus the handshake.
	hidPriorityQueueDepth = 4

	// hidMaxBufferedAmount is a hard bound on application bytes this client
	// will allow Pion to retain for the HID data channel. Four KiB covers the
	// largest valid drag (259 ten-byte reports) without changing its healthy
	// path, while preventing a stalled SCTP peer from accumulating input
	// indefinitely.
	hidMaxBufferedAmount uint64 = 4 * 1024

	// hidNeutralBufferReserve is kept unavailable to ordinary input so the
	// complete canonical neutral set (8-byte keyboard + 4-byte relative mouse
	// + 10-byte absolute pointer) can still be enqueued when input has reached
	// its lower limit.
	hidNeutralBufferReserve uint64 = 22

	// Zero is the only unambiguous confirmation threshold: a positive value
	// could report success while one of the final neutral reports remained
	// buffered. In pinned Pion, reaching zero means SCTP acknowledged every
	// application byte queued before it, including every required neutral report.
	hidBufferedAmountLowThreshold uint64 = 0

	// hidReleaseAttempts is how many times release-all is retried before it
	// is reported as unverified.
	hidReleaseAttempts = 2

	// hidReleaseRetryDelay spaces out release-all retries.
	hidReleaseRetryDelay = 20 * time.Millisecond
)

// heldInput is this client's conservative record of whether any keyboard or
// mouse-button state may still be held. The firmware routes absolute and
// relative mouse reports to separate USB HID gadget interfaces, so their
// uncertainty must remain separate too. Ordinary reports can add uncertainty,
// but only a transport-confirmed release-all may clear it: Send returning nil
// says only that Pion accepted the bytes, not that the peer acknowledged them.
//
// absoluteX/Y retain the most recent absolute report offered to Pion in writer
// order. They are updated even for a zero-button report, and before Send, so an
// ambiguous Send error can still be neutralized at the coordinates it may have
// delivered. absolutePositionKnown prevents an invariant failure from silently
// moving the pointer to (0,0).
type heldInput struct {
	keyboard              bool
	absoluteButtons       bool
	relativeButtons       bool
	absolutePositionKnown bool
	absoluteX             int32
	absoluteY             int32
}

func (h heldInput) any() bool {
	return h.keyboard || h.absoluteButtons || h.relativeButtons
}

func (h *heldInput) add(next heldInput) {
	h.keyboard = h.keyboard || next.keyboard
	h.absoluteButtons = h.absoluteButtons || next.absoluteButtons
	h.relativeButtons = h.relativeButtons || next.relativeButtons
	if next.absolutePositionKnown {
		h.absolutePositionKnown = true
		h.absoluteX = next.absoluteX
		h.absoluteY = next.absoluteY
	}
}

// hidRequest is one frame handed to the single writer goroutine.
type hidRequest struct {
	frame []byte

	// ctx is the context that authorized this request. The writer checks it
	// again at the final send boundary so a caller that abandons a queued
	// frame cannot have that frame delivered later under an otherwise-live
	// lease token.
	ctx context.Context

	// token is the control-lease generation this frame was authorized
	// under. It is re-validated at the final point before the write, not
	// at the call site, so a lease that ends while this frame sits in the
	// queue causes the frame to be dropped rather than delivered late.
	token uint64

	// privileged marks handshake and neutralization frames. They bypass
	// lease validation (they are how a lease is established or ended) and
	// are served from a separate queue that pre-empts queued input.
	privileged bool

	// held, when non-nil, records additional state and absolute coordinates
	// that may reach the peer if this frame reaches Send. A zero report never
	// clears prior uncertainty; only confirmed release-all does that.
	held *heldInput

	// drain keeps the single writer on this request until Pion reports that
	// every byte through this frame has been acknowledged by the SCTP peer.
	// Because the writer cannot service a later ordinary request meanwhile,
	// this is also an ordering barrier. Lease invalidation interrupts the wait
	// so terminal neutralization can pre-empt it.
	drain bool

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
//	controlLease.slot -> hidClient.lifecycle -> hidClient.stateMu
//
// The first two are channel semaphores. stateMu is never held while acquiring
// either semaphore, across a channel send or receive, or - critically - by a
// Pion callback. handleMessage uses its own leaf mutex (keydownMu) so that the
// data channel's read pump can never block on stateMu.
type hidClient struct {
	channel hidTransport

	// lifecycle serializes lease creation with complete neutralization
	// transactions. That makes it impossible for a fresh generation to put
	// input after the neutral reports while release-all is waiting for drain, and
	// leaves only one BufferedAmount-low waiter at a time.
	lifecycle chan struct{}

	stateMu    sync.Mutex
	state      hidState
	closeCause error
	// activeGen is the single lease generation currently permitted to
	// send. Zero means no holder: every non-privileged frame is dropped.
	activeGen uint64
	// activeDone is closed whenever activeGen is invalidated or replaced.
	// Drain barriers capture it at final validation so terminal release can
	// wake the writer without competing for bufferedAmountLow.
	activeDone chan struct{}
	// genCounter only ever increases, so a token is never reused - not
	// across release, expiry, holder replacement, or channel teardown.
	genCounter uint64
	held       heldInput

	sendCh     chan hidRequest
	priorityCh chan hidRequest
	stop       chan struct{}
	stopOnce   sync.Once
	writerDone chan struct{}

	// bufferedAmountLow is edge-triggered by Pion's callback and level-
	// checked through BufferedAmount. Keeping both is necessary: the edge
	// may arrive before a waiter starts, and a stale edge may be observed
	// after new bytes have been queued.
	bufferedAmountLow chan struct{}

	handshakeDone chan struct{}
	handshakeOnce sync.Once
}

func newHIDClient(channel hidTransport) *hidClient {
	h := &hidClient{
		channel:           channel,
		lifecycle:         make(chan struct{}, 1),
		state:             hidStateNegotiating,
		sendCh:            make(chan hidRequest, hidSendQueueDepth),
		priorityCh:        make(chan hidRequest, hidPriorityQueueDepth),
		stop:              make(chan struct{}),
		writerDone:        make(chan struct{}),
		bufferedAmountLow: make(chan struct{}, 1),
		handshakeDone:     make(chan struct{}),
	}
	channel.SetBufferedAmountLowThreshold(hidBufferedAmountLowThreshold)
	channel.OnBufferedAmountLow(h.signalBufferedAmountLow)
	go h.writeLoop()
	return h
}

func (h *hidClient) signalBufferedAmountLow() {
	select {
	case h.bufferedAmountLow <- struct{}{}:
	default:
	}
}

// handleMessage processes one inbound HID-RPC frame. It runs on Pion's
// data channel read goroutine and deliberately never touches stateMu: if it
// could block on that lock, a slow write could stall the read pump and
// deadlock the channel.
//
// Only the handshake echo is acted on. The device also sends unsolicited
// keydown-state reports (hidproto.DecodeKeydownState decodes them), but
// nothing here consumes them: release-all confirms the peer SCTP transport
// acknowledged the neutral bytes, not a separate firmware state report.
func (h *hidClient) handleMessage(data []byte) {
	m, err := hidproto.Unmarshal(data)
	if err != nil {
		return
	}
	if m.Type == hidproto.TypeHandshake && len(m.Payload) == 1 && m.Payload[0] == hidproto.ProtocolVersion {
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

// write is the single final validation point before any byte is offered to
// Pion. Nothing else in this package calls channel.Send.
func (h *hidClient) write(req hidRequest) {
	var activeDone <-chan struct{}
	err := req.contextErr()
	if err == nil {
		h.stateMu.Lock()
		err = h.checkWritableLocked(req)
		h.stateMu.Unlock()
	}

	if err == nil {
		err = h.checkBufferedAmount(req)
	}
	if err == nil {
		h.stateMu.Lock()
		// Revalidate after consulting Pion so a release or close that raced
		// the buffer check still drops this request before Send.
		err = h.checkWritableLocked(req)
		if err == nil {
			// A request may have been canceled while it waited in either queue.
			// This is the last check before bytes are offered to the transport.
			err = req.contextErr()
		}
		if err == nil && req.held != nil {
			// Record only additional uncertainty, plus the latest absolute
			// coordinates, before the write. If Send fails partway, or succeeds
			// while SCTP is stalled, prior held state must remain until
			// release-all is transport-confirmed.
			h.held.add(*req.held)
		}
		if err == nil && req.drain {
			activeDone = h.activeDone
			if activeDone == nil {
				err = ErrStaleControlToken
			}
		}
		h.stateMu.Unlock()
	}

	if err != nil {
		req.complete(err)
		return
	}
	if sendErr := h.channel.Send(req.frame); sendErr != nil {
		err = newDeviceError(ErrorKindUnreachable, "sending HID frame", sendErr)
	}
	if err == nil && req.drain {
		err = h.waitBufferedAmountLowUntilInvalidated(req.ctx, activeDone)
	}
	req.complete(err)
}

func (r hidRequest) contextErr() error {
	if r.ctx == nil {
		return nil
	}
	if err := r.ctx.Err(); err != nil {
		return fmt.Errorf("jetkvm: HID frame canceled before write: %w", err)
	}
	return nil
}

// checkBufferedAmount rejects a frame before Send when accepting it could
// exceed the lower-layer memory bound. It is called only by the single writer,
// so no other local HID send can increase BufferedAmount between this check
// and Send. Privileged frames may use the neutralization reserve; ordinary
// input may not.
func (h *hidClient) checkBufferedAmount(req hidRequest) error {
	limit := hidMaxBufferedAmount - hidNeutralBufferReserve
	if req.privileged {
		limit = hidMaxBufferedAmount
	}

	amount := h.channel.BufferedAmount()
	frameBytes := uint64(len(req.frame))
	if frameBytes > limit || amount > limit-frameBytes {
		return fmt.Errorf("%w (buffered=%d frame=%d limit=%d)", ErrHIDBufferFull, amount, frameBytes, limit)
	}
	return nil
}

// waitBufferedAmountLow waits for Pion's outbound application-byte count to
// reach the configured low threshold. The level is checked before and after
// every edge because OnBufferedAmountLow is crossing-triggered and callbacks
// may be delivered before, or spuriously from the perspective of, this wait.
func (h *hidClient) waitBufferedAmountLow(ctx context.Context) error {
	return h.waitBufferedAmountLowUntilInvalidated(ctx, nil)
}

// waitBufferedAmountLowUntilInvalidated is the per-operation form of the
// drain wait. invalidated is nil for terminal neutralization, whose lifecycle
// exclusion already makes it uninterruptible by lease replacement. A live
// operation supplies its generation channel so release can wake the writer,
// enqueue the priority neutral frames, and become the sole remaining consumer
// of bufferedAmountLow.
func (h *hidClient) waitBufferedAmountLowUntilInvalidated(ctx context.Context, invalidated <-chan struct{}) error {
	for {
		if h.channel.BufferedAmount() <= hidBufferedAmountLowThreshold {
			return nil
		}

		select {
		case <-h.bufferedAmountLow:
			continue
		case <-invalidated:
			// As with writer teardown below, a final acknowledgement can win
			// the race with invalidation. Prefer the confirmed transport level.
			if h.channel.BufferedAmount() <= hidBufferedAmountLowThreshold {
				return nil
			}
			return fmt.Errorf("jetkvm: HID drain barrier invalidated: %w", ErrStaleControlToken)
		case <-h.writerDone:
			// A final acknowledgement can race channel teardown. Prefer the
			// confirmed level if it won, otherwise surface the close cause.
			if h.channel.BufferedAmount() <= hidBufferedAmountLowThreshold {
				return nil
			}
			return h.closedErr()
		case <-ctx.Done():
			return fmt.Errorf("jetkvm: waiting for the HID outbound buffer to drain: %w", ctx.Err())
		}
	}
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
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("jetkvm: queuing HID frame: %w", err)
	}
	req.ctx = ctx
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
		// The writer retains ctx and checks it again immediately before Send,
		// so a frame abandoned here cannot be delivered later.
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
func (h *hidClient) beginLease(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("jetkvm: beginning a control lease: %w", err)
	}
	if err := h.lockLifecycle(ctx); err != nil {
		return 0, fmt.Errorf("jetkvm: waiting to begin a control lease: %w", err)
	}
	defer h.unlockLifecycle()
	return h.beginLeaseLocked(ctx)
}

// tryBeginLease is beginLease's genuinely non-blocking form. It distinguishes
// an occupied lifecycle gate from an occupied lease slot so adapters can
// report the exact contention instead of unexpectedly waiting through close
// neutralization.
func (h *hidClient) tryBeginLease(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("jetkvm: beginning a control lease: %w", err)
	}
	select {
	case h.lifecycle <- struct{}{}:
		defer h.unlockLifecycle()
	default:
		return 0, ErrControlLifecycleBusy
	}
	return h.beginLeaseLocked(ctx)
}

// beginLeaseLocked issues a generation while the caller owns lifecycle.
func (h *hidClient) beginLeaseLocked(ctx context.Context) (uint64, error) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("jetkvm: beginning a control lease: %w", err)
	}
	switch h.state {
	case hidStateClosed:
		return 0, h.closedErrLocked()
	case hidStateReady:
	default:
		return 0, ErrHIDNotReady
	}
	// A failed terminal cleanup deliberately retains held-state uncertainty.
	// Refuse a new generation until release-all independently confirms the
	// neutral state; otherwise watchdog expiry could silently cross leases.
	if h.held.any() {
		return 0, fmt.Errorf("%w: prior lease ended without confirmed neutralization", ErrNeutralizeUnverified)
	}
	if h.activeDone != nil {
		close(h.activeDone)
	}
	h.genCounter++
	h.activeGen = h.genCounter
	h.activeDone = make(chan struct{})
	return h.activeGen, nil
}

func (h *hidClient) lockLifecycle(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case h.lifecycle <- struct{}{}:
		if err := ctx.Err(); err != nil {
			h.unlockLifecycle()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *hidClient) unlockLifecycle() {
	<-h.lifecycle
}

// invalidateLeaseLocked revokes the current holder and burns the
// generation, so neither the outgoing token nor any token that might be
// issued next can match a frame queued before this moment.
func (h *hidClient) invalidateLeaseLocked() {
	if h.activeDone != nil {
		close(h.activeDone)
		h.activeDone = nil
	}
	h.activeGen = 0
	h.genCounter++
}

// releaseAll sends canonical neutral reports for every HID interface that may
// hold state.
//
// Ordering guarantee: the lease generation is invalidated *before* the
// neutralization frames are queued. Every frame still in the application
// queue is therefore stale by construction and is dropped at the final
// validation point; the neutralization frames are served from the priority
// queue, so they jump ahead of that queued input. Bytes Pion already accepted
// cannot be pre-empted, but the ordered channel puts the neutral reports after
// them and success is withheld until Pion's entire outbound amount reaches
// zero (SCTP peer acknowledgement).
//
// The relative report uses zero deltas. When the absolute interface may hold a
// button, its zero-button report reuses the most recently recorded coordinates
// instead of moving the pointer to an arbitrary location.
func (h *hidClient) releaseAll(ctx context.Context) error {
	if err := h.lockLifecycle(ctx); err != nil {
		return fmt.Errorf("%w: waiting to serialize neutralization: %w", ErrNeutralizeUnverified, err)
	}
	defer h.unlockLifecycle()
	return h.releaseAllLocked(ctx)
}

// releaseAllAndClose performs Client.Close's neutralization and terminal HID
// transition under one lifecycle exclusion. A blocked lease creation therefore
// observes the closed state after the gate opens and can never put input after
// the neutral reports.
func (h *hidClient) releaseAllAndClose(ctx context.Context, cause error) error {
	if err := h.lockLifecycle(ctx); err != nil {
		h.closeWith(cause)
		return fmt.Errorf("%w: waiting to serialize close neutralization: %w", ErrNeutralizeUnverified, err)
	}
	defer h.unlockLifecycle()

	err := h.releaseAllLocked(ctx)
	h.closeWith(cause)
	return err
}

func (h *hidClient) releaseAllLocked(ctx context.Context) error {
	h.stateMu.Lock()
	h.invalidateLeaseLocked()
	state := h.state
	cause := h.closeCause
	held := h.held
	hadHeld := held.any()
	h.stateMu.Unlock()

	if state != hidStateReady {
		if !hadHeld {
			// Nothing could ever have been sent through this channel, so
			// there is nothing to neutralize and nothing to over-claim.
			return nil
		}
		if cause != nil {
			return fmt.Errorf("%w: channel %s: %v", ErrNeutralizeUnverified, state, cause)
		}
		return fmt.Errorf("%w: channel %s", ErrNeutralizeUnverified, state)
	}

	plan, err := buildNeutralizationPlan(held)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNeutralizeUnverified, err)
	}

	var lastErr error
	for attempt := 0; attempt < hidReleaseAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(hidReleaseRetryDelay):
			case <-ctx.Done():
				return fmt.Errorf("%w: %w", ErrNeutralizeUnverified, ctx.Err())
			}
		}

		lastErr = nil
		for _, frame := range plan.frames {
			if err := h.enqueue(ctx, hidRequest{frame: frame, privileged: true}); err != nil {
				lastErr = err
				break
			}
		}
		if lastErr == nil {
			lastErr = h.waitBufferedAmountLow(ctx)
		}
		if lastErr == nil {
			h.stateMu.Lock()
			// Each bit is cleared only after the neutral report for its own
			// interface has crossed the shared peer-SCTP drain boundary. Retain
			// the absolute coordinates for diagnostics and future cleanup.
			if plan.keyboard {
				h.held.keyboard = false
			}
			if plan.relativeButtons {
				h.held.relativeButtons = false
			}
			if plan.absoluteButtons {
				h.held.absoluteButtons = false
			}
			h.stateMu.Unlock()
			return nil
		}
		if ctx.Err() != nil {
			break
		}
	}

	// Deliberately does not clear the held model: if we could not confirm
	// the release, this client must keep believing input is held.
	return fmt.Errorf("%w: %w", ErrNeutralizeUnverified, lastErr)
}

type neutralizationPlan struct {
	frames          [][]byte
	keyboard        bool
	relativeButtons bool
	absoluteButtons bool
}

// buildNeutralizationPlan constructs the canonical neutral report for every
// interface that may hold state. Keyboard and relative mouse reports are always
// emitted. The absolute interface is included only when it may hold a button,
// because an absolute report necessarily carries coordinates.
func buildNeutralizationPlan(held heldInput) (neutralizationPlan, error) {
	kb, err := hidproto.ReleaseAllKeyboardReport()
	if err != nil {
		return neutralizationPlan{}, err
	}
	mouse, err := hidproto.ReleaseAllMouseReport()
	if err != nil {
		return neutralizationPlan{}, err
	}
	plan := neutralizationPlan{
		frames:          [][]byte{kb, mouse},
		keyboard:        true,
		relativeButtons: true,
	}
	if !held.absoluteButtons {
		return plan, nil
	}
	if !held.absolutePositionKnown {
		return neutralizationPlan{}, errors.New("jetkvm: absolute button state has no recorded pointer coordinates")
	}
	pointer, err := hidproto.EncodePointerReport(held.absoluteX, held.absoluteY, 0)
	if err != nil {
		return neutralizationPlan{}, err
	}
	plan.frames = append(plan.frames, pointer)
	plan.absoluteButtons = true
	return plan, nil
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
	detail := ""
	if h.closeCause != nil {
		detail = RedactError(h.closeCause)
	}
	return &DeviceError{
		Kind:      ErrorKindUnreachable,
		Operation: "HID control",
		Detail:    detail,
		sentinel:  ErrHIDClosed,
	}
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
	held := heldInput{keyboard: modifier != 0}
	for _, key := range keys {
		if key != 0 {
			held.keyboard = true
			break
		}
	}

	return h.enqueue(ctx, hidRequest{frame: frame, token: token, held: &held})
}

// releaseKeyboard sends the canonical all-zero keyboard report and keeps the
// single writer on that request until every byte through it has drained from
// Pion's outbound buffer. Terminal generation invalidation interrupts the
// barrier so release-all can pre-empt it with its priority neutral frames.
func (h *hidClient) releaseKeyboard(ctx context.Context, token uint64) error {
	frame, err := hidproto.ReleaseAllKeyboardReport()
	if err != nil {
		return err
	}
	return h.enqueue(ctx, hidRequest{frame: frame, token: token, drain: true})
}

// sendPointerReport queues an absolute-mouse report under the given lease
// token.
func (h *hidClient) sendPointerReport(ctx context.Context, token uint64, x, y int32, buttons byte) error {
	frame, err := hidproto.EncodePointerReport(x, y, buttons)
	if err != nil {
		return err
	}
	return h.enqueue(ctx, hidRequest{frame: frame, token: token, held: &heldInput{
		absoluteButtons:       buttons != 0,
		absolutePositionKnown: true,
		absoluteX:             x,
		absoluteY:             y,
	}})
}

// sendMouseReport queues a relative-mouse report under the given lease
// token.
func (h *hidClient) sendMouseReport(ctx context.Context, token uint64, dx, dy int8, buttons byte) error {
	frame, err := hidproto.EncodeMouseReport(dx, dy, buttons)
	if err != nil {
		return err
	}
	return h.enqueue(ctx, hidRequest{frame: frame, token: token, held: &heldInput{relativeButtons: buttons != 0}})
}

// hasHeldState reports whether this client believes any key or button is
// currently held, per its conservative local model. A frame rejected by state,
// token, or buffer-cap validation never changes it; a non-neutral frame that
// reaches Send adds uncertainty even when Send returns an ambiguous error.
// Only transport-confirmed release-all clears the model.
func (h *hidClient) hasHeldState() bool {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	return h.held.any()
}
