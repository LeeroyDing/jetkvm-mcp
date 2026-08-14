package jetkvm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DefaultControlLeaseTimeout bounds how long a caller may hold the control
// lease without releasing it. This is the "timeout" half of the
// release-all guarantee: a caller that simply stops calling us (crash,
// hang, forgotten Release) cannot leave input held indefinitely.
const DefaultControlLeaseTimeout = 30 * time.Second

// neutralizeTimeout bounds the release-all that ends every lease. It is
// deliberately independent of the holder's context, which is usually
// already dead by the time a lease ends.
const neutralizeTimeout = 2 * time.Second

// controlLease is the single point through which keyboard/mouse commands
// flow for a Client. What it actually guarantees, and what the tests in
// owner_test.go and hid_test.go prove:
//
//   - Exclusivity: at most one holder at a time. Acquire waits (bounded by
//     its context); TryAcquire reports ErrControlHeld instead of queuing.
//   - Generation validation: every holder gets a fresh, never-reused token
//     from the HID state machine, and every frame is re-validated against
//     the currently-active token at the last moment before it is written.
//     A frame authorized by an ended lease is dropped, not delivered late.
//   - Terminal neutralization: however the lease ends - explicit Release,
//     context cancellation, inactivity timeout, disconnect, or Client
//     shutdown - the lease generation is revoked first and neutralization
//     frames are then written from a priority queue, so they are the last
//     HID frames written for that generation.
//   - Truthful failure: if neutralization cannot be confirmed on the wire,
//     the error says so (ErrNeutralizeUnverified) instead of reporting a
//     clean release.
//
// It is created disabled (hid == nil) unless the Client was connected with
// AllowControl: true, so a caller that never opted into control cannot
// construct a working lease at all.
//
// Lock ordering: the lease's slot semaphore is acquired before
// hidClient.stateMu, never the other way around. The semaphore is a
// channel rather than a mutex specifically so acquisition can respect a
// context without leaking a goroutine per waiter.
type controlLease struct {
	hid  *hidClient
	slot chan struct{}
}

func newControlLease(hid *hidClient) *controlLease {
	return &controlLease{hid: hid, slot: make(chan struct{}, 1)}
}

// ErrControlHeld is returned by TryAcquire when another caller already
// holds the lease.
var ErrControlHeld = errors.New("jetkvm: control lease is already held")

// ErrControlDisabled is returned when control was never enabled for this
// connection, so no lease can exist.
var ErrControlDisabled = errors.New("jetkvm: control is not enabled for this connection")

// Held is a live handle on an acquired control lease. Every method is
// bounded by the caller's context, and every method after the lease ends
// returns an error rather than silently no-op'ing.
type Held struct {
	lease  *controlLease
	token  uint64
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	err    error
}

// Acquire blocks until the lease is free (or ctx is done), then holds it
// with a watchdog: the lease is force-released, with neutralization, if
// ctx is canceled, if timeout elapses without a Release, or if Release is
// called explicitly. Pass timeout <= 0 for DefaultControlLeaseTimeout.
//
// Acquire fails if the HID readiness handshake has not been confirmed by
// the device, so a lease can never exist over a channel the device is not
// actually honoring.
func (l *controlLease) Acquire(ctx context.Context, timeout time.Duration) (*Held, error) {
	if l == nil || l.hid == nil {
		return nil, ErrControlDisabled
	}
	select {
	case l.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("jetkvm: waiting for the control lease: %w", ctx.Err())
	}
	return l.hold(ctx, timeout)
}

// TryAcquire is Acquire's non-blocking sibling: it returns ErrControlHeld
// immediately instead of waiting. Used by adapters (MCP tools) that would
// rather report "busy" than queue.
func (l *controlLease) TryAcquire(ctx context.Context, timeout time.Duration) (*Held, error) {
	if l == nil || l.hid == nil {
		return nil, ErrControlDisabled
	}
	select {
	case l.slot <- struct{}{}:
	default:
		return nil, ErrControlHeld
	}
	return l.hold(ctx, timeout)
}

// hold completes an acquisition that already owns the exclusivity slot.
func (l *controlLease) hold(ctx context.Context, timeout time.Duration) (*Held, error) {
	token, err := l.hid.beginLease()
	if err != nil {
		<-l.slot
		return nil, err
	}
	if timeout <= 0 {
		timeout = DefaultControlLeaseTimeout
	}

	watchdogCtx, cancel := context.WithTimeout(ctx, timeout)
	h := &Held{lease: l, token: token, cancel: cancel, done: make(chan struct{})}
	go h.watch(watchdogCtx)
	return h, nil
}

// watch force-releases the lease as soon as the watchdog context ends,
// unless Release already did so. Exactly one watchdog goroutine exists per
// acquisition, and exclusivity bounds that to one at a time.
func (h *Held) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = h.Release()
	case <-h.done:
	}
}

// Release ends the lease: it revokes the lease generation, writes the
// neutralization frames, and frees the lease for the next acquirer. Safe
// to call more than once and concurrently with the watchdog firing; the
// first call's result is returned to all callers.
//
// A non-nil error means neutralization could not be confirmed (see
// ErrNeutralizeUnverified). The lease is freed either way - a lease that
// could not be neutralized must not also become permanently stuck.
func (h *Held) Release() error {
	h.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), neutralizeTimeout)
		h.err = h.lease.hid.releaseAllRequired(ctx)
		cancel()

		h.cancel()
		<-h.lease.slot
		close(h.done)
	})
	return h.err
}

// Token returns the lease generation this holder sends under. Frames are
// validated against it at the last moment before they are written.
func (h *Held) Token() uint64 { return h.token }

// checkAlive is an early-out for callers, not the authoritative check. The
// authoritative check is the token validation performed inside the HID
// writer immediately before the frame is written, which is what closes the
// race between an in-flight send and a concurrent release.
func (h *Held) checkAlive() error {
	select {
	case <-h.done:
		return fmt.Errorf("jetkvm: control lease already released: %w", ErrStaleControlToken)
	default:
		return nil
	}
}

// SendKeyboardReport sends a full keyboard state report through the held
// lease, bounded by ctx. See internal/hidproto for wire format details.
func (h *Held) SendKeyboardReport(ctx context.Context, modifier byte, keys []byte) error {
	if err := h.checkAlive(); err != nil {
		return err
	}
	return h.lease.hid.sendKeyboardReport(ctx, h.token, modifier, keys)
}

// SendPointerReport sends an absolute-mouse report through the held lease,
// bounded by ctx.
func (h *Held) SendPointerReport(ctx context.Context, x, y int32, buttons byte) error {
	if err := h.checkAlive(); err != nil {
		return err
	}
	return h.lease.hid.sendPointerReport(ctx, h.token, x, y, buttons)
}

// SendMouseReport sends a relative-mouse report through the held lease,
// bounded by ctx.
func (h *Held) SendMouseReport(ctx context.Context, dx, dy int8, buttons byte) error {
	if err := h.checkAlive(); err != nil {
		return err
	}
	return h.lease.hid.sendMouseReport(ctx, h.token, dx, dy, buttons)
}

// neutralize performs a release-all outside of any lease. It is used by
// Client.Close, where the lease may or may not be held and a redundant
// neutralization is harmless but a missed one is not.
func (l *controlLease) neutralize(ctx context.Context) error {
	if l == nil || l.hid == nil {
		return nil
	}
	return l.hid.releaseAll(ctx)
}
