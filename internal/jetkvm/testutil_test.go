package jetkvm

import (
	"context"
	"testing"
	"time"
)

// contextWithTimeout returns a context canceled after d, with cancellation
// registered via t.Cleanup so tests never leak the timer.
func contextWithTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
