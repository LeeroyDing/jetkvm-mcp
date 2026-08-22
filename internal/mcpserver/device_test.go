package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func TestSendSingleReportPreservesSendAndReleaseErrors(t *testing.T) {
	transportErr := errors.New("transport send failed")
	releaseErr := fmt.Errorf("neutralizing held input: %w", jetkvm.ErrNeutralizeUnverified)

	tests := []struct {
		name          string
		sendErr       error
		releaseErr    error
		wantTransport bool
		wantRelease   bool
		wantSafety    bool
		cancelRelease bool
		wantCanceled  bool
	}{
		{
			name:          "send and release fail",
			sendErr:       transportErr,
			releaseErr:    releaseErr,
			wantTransport: true,
			wantRelease:   true,
			wantSafety:    true,
		},
		{
			name:          "send fails and release succeeds",
			sendErr:       transportErr,
			wantTransport: true,
		},
		{
			name:        "send succeeds and release fails",
			releaseErr:  releaseErr,
			wantRelease: true,
			wantSafety:  true,
		},
		{
			name: "send and release succeed",
		},
		{
			name:          "cancellation during successful release",
			cancelRelease: true,
			wantCanceled:  true,
		},
		{
			name:          "cancellation and release failure are both preserved",
			releaseErr:    releaseErr,
			cancelRelease: true,
			wantSafety:    true,
			wantCanceled:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sendCalls := 0
			releaseCalls := 0
			sendFinished := false
			err := sendSingleReport(
				ctx,
				func() error {
					sendCalls++
					sendFinished = true
					return test.sendErr
				},
				func() error {
					releaseCalls++
					if !sendFinished {
						t.Error("release ran before send completed")
					}
					if test.cancelRelease {
						cancel()
					}
					return test.releaseErr
				},
			)

			if got := errors.Is(err, transportErr); got != test.wantTransport {
				t.Errorf("errors.Is(result, transportErr) = %v, want %v: %v", got, test.wantTransport, err)
			}
			if got := errors.Is(err, releaseErr); got != test.wantRelease {
				t.Errorf("errors.Is(result, releaseErr) = %v, want %v: %v", got, test.wantRelease, err)
			}
			if got := errors.Is(err, jetkvm.ErrNeutralizeUnverified); got != test.wantSafety {
				t.Errorf("errors.Is(result, ErrNeutralizeUnverified) = %v, want %v: %v", got, test.wantSafety, err)
			}
			if got := errors.Is(err, context.Canceled); got != test.wantCanceled {
				t.Errorf("errors.Is(result, context.Canceled) = %v, want %v: %v", got, test.wantCanceled, err)
			}
			if sendCalls != 1 || releaseCalls != 1 {
				t.Errorf("calls = send %d, release %d; want 1 each", sendCalls, releaseCalls)
			}
		})
	}
}

func TestJoinCallerCancellationDropsStaleErrorAndPreservesOnlySafetyWarning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	staleErr := &jetkvm.DeviceError{
		Kind:      jetkvm.ErrorKindUnreachable,
		Operation: "stale dependency success boundary",
	}

	err := joinCallerCancellation(ctx, errors.Join(staleErr, jetkvm.ErrNeutralizeUnverified))
	if got := jetkvm.ErrorKindOf(err); got != jetkvm.ErrorKindTimeout {
		t.Fatalf("late-cancellation kind = %q, want %q: %v", got, jetkvm.ErrorKindTimeout, err)
	}
	if errors.Is(err, staleErr) {
		t.Fatalf("late cancellation retained stale dependency error: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("late cancellation lost context.Canceled: %v", err)
	}
	if !errors.Is(err, jetkvm.ErrNeutralizeUnverified) {
		t.Fatalf("late cancellation lost neutralization warning: %v", err)
	}
}

func TestWaitHoldCancellationWinsTimerRace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// With a zero-duration timer, both select arms may be ready. The timer arm
	// must still recheck ctx instead of turning an abandoned hold into success.
	for range 100 {
		if err := waitHold(ctx, 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("waitHold simultaneous timer/cancellation = %v, want context.Canceled", err)
		}
	}
}
