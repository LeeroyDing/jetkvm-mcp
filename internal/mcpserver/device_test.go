package mcpserver

import (
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
	}{
		{
			name:          "send and release fail",
			sendErr:       transportErr,
			releaseErr:    releaseErr,
			wantTransport: true,
			wantRelease:   true,
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
		},
		{
			name: "send and release succeed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sendCalls := 0
			releaseCalls := 0
			sendFinished := false
			err := sendSingleReport(
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
					return test.releaseErr
				},
			)

			if got := errors.Is(err, transportErr); got != test.wantTransport {
				t.Errorf("errors.Is(result, transportErr) = %v, want %v: %v", got, test.wantTransport, err)
			}
			if got := errors.Is(err, releaseErr); got != test.wantRelease {
				t.Errorf("errors.Is(result, releaseErr) = %v, want %v: %v", got, test.wantRelease, err)
			}
			if got := errors.Is(err, jetkvm.ErrNeutralizeUnverified); got != test.wantRelease {
				t.Errorf("errors.Is(result, ErrNeutralizeUnverified) = %v, want %v: %v", got, test.wantRelease, err)
			}
			if sendCalls != 1 || releaseCalls != 1 {
				t.Errorf("calls = send %d, release %d; want 1 each", sendCalls, releaseCalls)
			}
		})
	}
}
