package mcpserver

import (
	"math"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

// FuzzWaitStableArgs exercises the MCP adapter's millisecond conversion and
// the complete wait-stable validation contract. The adapter always supplies
// all three option pointers because schema defaults have already been applied
// before waitStableOptionsFromArgs is called.
func FuzzWaitStableArgs(f *testing.F) {
	add := func(threshold float64, stableFrames int, pollIntervalMS int64) {
		f.Add(math.Float64bits(threshold), stableFrames, pollIntervalMS)
	}

	add(jetkvm.DefaultWaitStableThreshold, jetkvm.DefaultWaitStableFrames, jetkvm.DefaultWaitStablePollInterval.Milliseconds())
	add(0, 1, 0)
	add(1, jetkvm.MaxWaitStableFrames, maxWaitStablePollIntervalMS)
	add(math.SmallestNonzeroFloat64, 1, 1)
	add(-math.SmallestNonzeroFloat64, 1, 1)
	add(math.Nextafter(1, 2), 1, 1)
	add(math.NaN(), 1, 1)
	add(math.Inf(1), 1, 1)
	add(math.Inf(-1), 1, 1)
	add(jetkvm.DefaultWaitStableThreshold, 0, 1)
	add(jetkvm.DefaultWaitStableThreshold, 1, -1)
	add(jetkvm.DefaultWaitStableThreshold, 1, maxWaitStablePollIntervalMS+1)
	add(jetkvm.DefaultWaitStableThreshold, 1, math.MinInt64)
	add(jetkvm.DefaultWaitStableThreshold, 1, math.MaxInt64)

	maxInt := int(^uint(0) >> 1)
	minInt := -maxInt - 1
	add(jetkvm.DefaultWaitStableThreshold, minInt, 1)
	add(jetkvm.DefaultWaitStableThreshold, maxInt, 1)
	if int64(maxInt) > int64(jetkvm.MaxWaitStableFrames) {
		add(jetkvm.DefaultWaitStableThreshold, int(int64(jetkvm.MaxWaitStableFrames)+1), 1)
	}

	f.Fuzz(func(t *testing.T, thresholdBits uint64, stableFrames int, pollIntervalMS int64) {
		threshold := math.Float64frombits(thresholdBits)
		wantValid := !math.IsNaN(threshold) && !math.IsInf(threshold, 0) &&
			threshold >= 0 && threshold <= 1 &&
			stableFrames >= 1 && stableFrames <= jetkvm.MaxWaitStableFrames &&
			pollIntervalMS >= 0 && pollIntervalMS <= maxWaitStablePollIntervalMS

		opts, err := waitStableOptionsFromArgs(waitStableArgs{
			Threshold:      threshold,
			StableFrames:   stableFrames,
			PollIntervalMS: pollIntervalMS,
		})
		if !wantValid {
			if err == nil {
				t.Fatalf("adapter accepted threshold=%v stable_frames=%d poll_interval_ms=%d", threshold, stableFrames, pollIntervalMS)
			}
			return
		}
		if err != nil {
			t.Fatalf("adapter rejected threshold=%v stable_frames=%d poll_interval_ms=%d: %v", threshold, stableFrames, pollIntervalMS, err)
		}
		if opts.Threshold == nil || opts.StableFrames == nil || opts.PollInterval == nil {
			t.Fatalf("adapter omitted supplied option pointers: %+v", opts)
		}

		wantPollInterval := time.Duration(pollIntervalMS) * time.Millisecond
		if *opts.Threshold != threshold || *opts.StableFrames != stableFrames || *opts.PollInterval != wantPollInterval {
			t.Fatalf("adapter options = threshold %v stable_frames %d poll_interval %s, want %v/%d/%s",
				*opts.Threshold, *opts.StableFrames, *opts.PollInterval,
				threshold, stableFrames, wantPollInterval)
		}
		if *opts.PollInterval < 0 || int64(*opts.PollInterval/time.Millisecond) != pollIntervalMS || *opts.PollInterval%time.Millisecond != 0 {
			t.Fatalf("poll interval multiplication wrapped or lost precision: %d ms became %s", pollIntervalMS, *opts.PollInterval)
		}
		if err := jetkvm.ValidateWaitStableOptions(opts); err != nil {
			t.Fatalf("adapter returned options rejected by core validation: %v", err)
		}
	})
}

// FuzzWaitForTextDurationArgs pins the adapter boundary before caller-provided
// milliseconds are multiplied into time.Duration values. Text is fixed and
// valid so acceptance depends only on the two duration fields.
func FuzzWaitForTextDurationArgs(f *testing.F) {
	minIntervalMS := jetkvm.MinWaitForTextInterval.Milliseconds()
	maxIntervalMS := jetkvm.MaxWaitForTextInterval.Milliseconds()
	minTimeoutMS := jetkvm.MinWaitForTextTimeout.Milliseconds()
	maxTimeoutMS := jetkvm.MaxWaitForTextTimeout.Milliseconds()

	for _, seed := range [][2]int64{
		{jetkvm.DefaultWaitForTextInterval.Milliseconds(), jetkvm.DefaultWaitForTextTimeout.Milliseconds()},
		{minIntervalMS, minTimeoutMS},
		{maxIntervalMS, maxTimeoutMS},
		{maxIntervalMS, maxIntervalMS},
		{minIntervalMS - 1, minTimeoutMS},
		{maxIntervalMS + 1, maxTimeoutMS},
		{minIntervalMS, minTimeoutMS - 1},
		{minIntervalMS, maxTimeoutMS + 1},
		{minIntervalMS + 1, minTimeoutMS},
		{0, minTimeoutMS},
		{-1, minTimeoutMS},
		{minIntervalMS, 0},
		{minIntervalMS, -1},
		{maxWaitForTextDurationMS, maxWaitForTextDurationMS},
		{maxWaitForTextDurationMS + 1, maxTimeoutMS},
		{minIntervalMS, maxWaitForTextDurationMS + 1},
		{math.MinInt64, math.MaxInt64},
		{math.MaxInt64, math.MinInt64},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, intervalMS, timeoutMS int64) {
		wantValid := intervalMS >= minIntervalMS && intervalMS <= maxIntervalMS &&
			timeoutMS >= minTimeoutMS && timeoutMS <= maxTimeoutMS &&
			intervalMS <= timeoutMS

		opts, err := waitForTextOptionsFromArgs(waitForTextArgs{
			Text:       "ready",
			IntervalMS: intervalMS,
			TimeoutMS:  timeoutMS,
		})
		if !wantValid {
			if err == nil {
				t.Fatalf("adapter accepted interval_ms=%d timeout_ms=%d", intervalMS, timeoutMS)
			}
			return
		}
		if err != nil {
			t.Fatalf("adapter rejected interval_ms=%d timeout_ms=%d: %v", intervalMS, timeoutMS, err)
		}
		if opts.Text != "ready" || opts.Regex || opts.Interval == nil || opts.Timeout == nil {
			t.Fatalf("adapter changed or omitted options: %+v", opts)
		}

		wantInterval := time.Duration(intervalMS) * time.Millisecond
		wantTimeout := time.Duration(timeoutMS) * time.Millisecond
		if *opts.Interval != wantInterval || *opts.Timeout != wantTimeout {
			t.Fatalf("adapter durations = %s/%s, want %s/%s", *opts.Interval, *opts.Timeout, wantInterval, wantTimeout)
		}
		if *opts.Interval < 0 || *opts.Timeout < 0 ||
			int64(*opts.Interval/time.Millisecond) != intervalMS || *opts.Interval%time.Millisecond != 0 ||
			int64(*opts.Timeout/time.Millisecond) != timeoutMS || *opts.Timeout%time.Millisecond != 0 {
			t.Fatalf("duration multiplication wrapped or lost precision: %d/%d ms became %s/%s", intervalMS, timeoutMS, *opts.Interval, *opts.Timeout)
		}
		if err := jetkvm.ValidateWaitForTextOptions(opts); err != nil {
			t.Fatalf("adapter returned options rejected by core validation: %v", err)
		}
	})
}
