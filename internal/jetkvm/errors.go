package jetkvm

import (
	"context"
	"errors"
	"fmt"
)

// ErrSessionTransport marks a terminal failure of the signaling, WebRTC,
// media, or data-channel transport. The concrete error text is deliberately
// bounded and contains no addresses or dependency error strings; callers may
// use errors.Is without gaining an unwrap path to sensitive transport data.
var ErrSessionTransport = errors.New("jetkvm: session transport ended")

// errSessionCleanupUnconfirmed marks teardown that was attempted but could
// not be confirmed within its safety bound. It stays private because callers
// only need the conservative retry decision exposed by IsRetryableReadOnly.
// In particular, a transport failure joined with this marker must never open
// a replacement session: the old firmware session may still be current.
var errSessionCleanupUnconfirmed = errors.New("jetkvm: session cleanup unconfirmed")

type sessionTransportError struct {
	message string
}

func (e *sessionTransportError) Error() string { return e.message }
func (e *sessionTransportError) Unwrap() error { return ErrSessionTransport }

func newSessionTransportError(message string) error {
	return &sessionTransportError{message: message}
}

func newSessionCleanupError(message string) error {
	return fmt.Errorf("%s: %w", message, errSessionCleanupUnconfirmed)
}

// IsRetryableReadOnly reports whether a failed read-only operation may be
// attempted once on a brand-new session. Only explicit terminal session
// transport failures qualify. Cancellation and deadline expiry never do,
// even when they wrap a transport marker.
func IsRetryableReadOnly(err error) bool {
	return err != nil &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, errSessionCleanupUnconfirmed) &&
		errors.Is(err, ErrSessionTransport)
}
