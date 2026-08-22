package jetkvm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type cancelAfterInitialCheckContext struct {
	done     chan struct{}
	errCalls int
}

func newCancelAfterInitialCheckContext() *cancelAfterInitialCheckContext {
	done := make(chan struct{})
	close(done)
	return &cancelAfterInitialCheckContext{done: done}
}

func (*cancelAfterInitialCheckContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterInitialCheckContext) Done() <-chan struct{}     { return c.done }
func (c *cancelAfterInitialCheckContext) Err() error {
	c.errCalls++
	if c.errCalls == 1 {
		return nil
	}
	return context.Canceled
}
func (*cancelAfterInitialCheckContext) Value(any) any { return nil }

func TestWaitForStablePollBoundaries(t *testing.T) {
	preCanceled, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Now()
	tests := []struct {
		name          string
		ctx           context.Context
		previousStart time.Time
		startNow      bool
		interval      time.Duration
		wantErr       error
	}{
		{
			name:          "pre-canceled context",
			ctx:           preCanceled,
			previousStart: now,
			interval:      time.Hour,
			wantErr:       context.Canceled,
		},
		{
			name:          "first poll starts immediately",
			ctx:           context.Background(),
			previousStart: time.Time{},
			interval:      time.Hour,
		},
		{
			name:          "zero interval starts immediately",
			ctx:           context.Background(),
			previousStart: now,
			interval:      0,
		},
		{
			name:          "elapsed interval starts immediately",
			ctx:           context.Background(),
			previousStart: now.Add(-time.Second),
			interval:      time.Millisecond,
		},
		{
			name:     "cancellation wins while waiting",
			ctx:      newCancelAfterInitialCheckContext(),
			startNow: true,
			interval: time.Hour,
			wantErr:  context.Canceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previousStart := tt.previousStart
			if tt.startNow {
				previousStart = time.Now()
			}
			err := waitForStablePoll(tt.ctx, previousStart, tt.interval)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("waitForStablePoll error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestWaitForStablePollWaitsRemainingInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 250 * time.Millisecond
		previousStart := time.Now()

		if err := waitForStablePoll(context.Background(), previousStart, interval); err != nil {
			t.Fatalf("waitForStablePoll: %v", err)
		}
		if elapsed := time.Since(previousStart); elapsed != interval {
			t.Fatalf("waitForStablePoll elapsed = %v, want %v", elapsed, interval)
		}
	})
}

func TestValidateFourByteImageBackingBoundaries(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	tests := []struct {
		name     string
		pix      []byte
		stride   int
		width    int
		height   int
		wantText string
	}{
		{name: "zero width", width: 0, height: 1, wantText: "must be positive"},
		{name: "zero height", width: 1, height: 0, wantText: "must be positive"},
		{name: "row size overflows int", width: maxInt/4 + 1, height: 1, wantText: "overflows"},
		{name: "stride shorter than row", pix: make([]byte, 8), stride: 7, width: 2, height: 1, wantText: "stride"},
		{name: "first row truncated", pix: make([]byte, 7), stride: 8, width: 2, height: 1, wantText: "first"},
		{name: "final row truncated", pix: make([]byte, 15), stride: 8, width: 2, height: 2, wantText: "cannot cover"},
		{name: "exact two-row backing", pix: make([]byte, 16), stride: 8, width: 2, height: 2},
		{name: "padded two-row backing", pix: make([]byte, 20), stride: 12, width: 2, height: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFourByteImageBacking(tt.pix, tt.stride, tt.width, tt.height)
			if tt.wantText == "" {
				if err != nil {
					t.Fatalf("validateFourByteImageBacking: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("validateFourByteImageBacking error = %v, want text %q", err, tt.wantText)
			}
		})
	}
}

func TestChangedFourBytePixelFractionRejectsInvalidInputs(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name           string
		ctx            context.Context
		previous       []byte
		previousStride int
		current        []byte
		currentStride  int
		width          int
		height         int
		wantText       string
	}{
		{
			name:           "pre-canceled comparison",
			ctx:            canceled,
			previous:       make([]byte, 4),
			previousStride: 4,
			current:        make([]byte, 4),
			currentStride:  4,
			width:          1,
			height:         1,
			wantText:       context.Canceled.Error(),
		},
		{
			name:           "invalid previous backing",
			ctx:            context.Background(),
			previous:       make([]byte, 3),
			previousStride: 4,
			current:        make([]byte, 4),
			currentStride:  4,
			width:          1,
			height:         1,
			wantText:       "previous four-byte image backing",
		},
		{
			name:           "invalid current backing",
			ctx:            context.Background(),
			previous:       make([]byte, 4),
			previousStride: 4,
			current:        make([]byte, 3),
			currentStride:  4,
			width:          1,
			height:         1,
			wantText:       "current four-byte image backing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := changedFourBytePixelFraction(
				tt.ctx,
				tt.previous,
				tt.previousStride,
				tt.current,
				tt.currentStride,
				tt.width,
				tt.height,
			)
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("changedFourBytePixelFraction error = %v, want text %q", err, tt.wantText)
			}
		})
	}
}
