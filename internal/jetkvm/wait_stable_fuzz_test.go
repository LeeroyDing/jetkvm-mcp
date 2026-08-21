package jetkvm

import (
	"math"
	"testing"
	"time"
)

func FuzzWaitStableOptionValidation(f *testing.F) {
	for _, seed := range []struct {
		thresholdBits uint64
		stableFrames  int
		pollNanos     int64
		present       uint8
	}{
		{math.Float64bits(DefaultWaitStableThreshold), DefaultWaitStableFrames, int64(DefaultWaitStablePollInterval), 0},
		{math.Float64bits(0), 1, 0, 0b111},
		{math.Float64bits(1), 100, int64(time.Hour), 0b111},
		{math.Float64bits(-0.01), 0, -1, 0b111},
		{math.Float64bits(math.NaN()), 1, 0, 0b001},
		{math.Float64bits(math.Inf(1)), 1, 0, 0b001},
	} {
		f.Add(seed.thresholdBits, seed.stableFrames, seed.pollNanos, seed.present)
	}

	f.Fuzz(func(t *testing.T, thresholdBits uint64, stableFrames int, pollNanos int64, present uint8) {
		threshold := math.Float64frombits(thresholdBits)
		pollInterval := time.Duration(pollNanos)
		opts := WaitStableOptions{}
		if present&0b001 != 0 {
			opts.Threshold = &threshold
		}
		if present&0b010 != 0 {
			opts.StableFrames = &stableFrames
		}
		if present&0b100 != 0 {
			opts.PollInterval = &pollInterval
		}

		wantThreshold := DefaultWaitStableThreshold
		if opts.Threshold != nil {
			wantThreshold = threshold
		}
		wantStableFrames := DefaultWaitStableFrames
		if opts.StableFrames != nil {
			wantStableFrames = stableFrames
		}
		wantPollInterval := DefaultWaitStablePollInterval
		if opts.PollInterval != nil {
			wantPollInterval = pollInterval
		}
		wantValid := !math.IsNaN(wantThreshold) && !math.IsInf(wantThreshold, 0) &&
			wantThreshold >= 0 && wantThreshold <= 1 &&
			wantStableFrames >= 1 && wantPollInterval >= 0

		resolved, err := resolveWaitStableOptions(opts)
		validateErr := ValidateWaitStableOptions(opts)
		if (err == nil) != (validateErr == nil) {
			t.Fatalf("resolver error = %v, validator error = %v", err, validateErr)
		}
		if !wantValid {
			if err == nil {
				t.Fatalf("invalid values resolved successfully: threshold=%v stableFrames=%d pollInterval=%v present=%03b",
					wantThreshold, wantStableFrames, wantPollInterval, present&0b111)
			}
			return
		}
		if err != nil {
			t.Fatalf("valid values were rejected: threshold=%v stableFrames=%d pollInterval=%v present=%03b: %v",
				wantThreshold, wantStableFrames, wantPollInterval, present&0b111, err)
		}
		if resolved.threshold != wantThreshold || resolved.stableFrames != wantStableFrames || resolved.pollInterval != wantPollInterval {
			t.Fatalf("resolved = %+v, want threshold=%v stableFrames=%d pollInterval=%v",
				resolved, wantThreshold, wantStableFrames, wantPollInterval)
		}
	})
}
