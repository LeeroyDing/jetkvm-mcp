package jetkvm

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestReadOnlyRetryAllowlist(t *testing.T) {
	transport := fmt.Errorf("handoff: %w", ErrSessionTransport)
	if !IsRetryableReadOnly(transport) {
		t.Fatal("terminal session transport failure was not retryable")
	}
	for name, err := range map[string]error{
		"nil":           nil,
		"unknown":       errors.New("unknown failure"),
		"canceled":      context.Canceled,
		"deadline":      context.DeadlineExceeded,
		"joined cancel": errors.Join(transport, context.Canceled),
		"unconfirmed cleanup": errors.Join(
			transport,
			newSessionCleanupError("cleanup failed"),
		),
	} {
		if IsRetryableReadOnly(err) {
			t.Errorf("%s error was retryable: %v", name, err)
		}
	}
}
