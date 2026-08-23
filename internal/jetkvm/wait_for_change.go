package jetkvm

import (
	"context"
	"fmt"
	"time"
)

const (
	// DefaultWaitForChangeThreshold is the fraction of pixels that a frame
	// must exceed to count as different from the fixed baseline frame.
	DefaultWaitForChangeThreshold = DefaultWaitStableThreshold

	// DefaultWaitForChangePollInterval is the minimum gap between the starts
	// of two successive fresh-frame polls.
	DefaultWaitForChangePollInterval = DefaultWaitStablePollInterval
)

// WaitForChangeOptions configures a WaitForChange call. Nil fields use the
// defaults above. Pointers preserve the distinction between an omitted field
// and the valid explicit zero values for Threshold and PollInterval.
type WaitForChangeOptions struct {
	Threshold    *float64
	PollInterval *time.Duration
}

// WaitForChangeResult reports the observations made while waiting for the
// screen to differ from its fixed baseline. On a timeout, WaitForChange
// returns the partial result together with a classified timeout error.
type WaitForChangeResult struct {
	Changed             bool
	FramesSampled       int
	FinalChangeFraction float64
	Elapsed             time.Duration
}

type resolvedWaitForChangeOptions struct {
	threshold    float64
	pollInterval time.Duration
}

// ValidateWaitForChangeOptions resolves omitted fields to their defaults and
// validates every supplied value. It performs no I/O.
func ValidateWaitForChangeOptions(opts WaitForChangeOptions) error {
	_, err := resolveWaitForChangeOptions(opts)
	return err
}

func resolveWaitForChangeOptions(opts WaitForChangeOptions) (resolvedWaitForChangeOptions, error) {
	resolved := resolvedWaitForChangeOptions{
		threshold:    DefaultWaitForChangeThreshold,
		pollInterval: DefaultWaitForChangePollInterval,
	}
	if opts.Threshold != nil {
		resolved.threshold = *opts.Threshold
	}
	if opts.PollInterval != nil {
		resolved.pollInterval = *opts.PollInterval
	}
	if err := validateFrameDifferenceOptions(resolved.threshold, resolved.pollInterval); err != nil {
		return resolvedWaitForChangeOptions{}, err
	}
	return resolved, nil
}

// WaitForChange captures one request-fresh decoded video frame as a fixed
// baseline, then polls request-fresh frames until the changed-pixel fraction
// is strictly greater than Threshold. A resolution change always counts as a
// change, including when Threshold is 1. It holds the Client command lock for
// the complete operation, so Status, CaptureScreenshot, and legacy RPC
// control calls cannot interleave with the sampled sequence.
//
// Every poll records the current completed frame generation immediately
// before waiting for a strictly newer one. Frames cached during a poll gap are
// therefore skipped rather than compared as though they were fresh.
func (c *Client) WaitForChange(ctx context.Context, opts WaitForChangeOptions) (WaitForChangeResult, error) {
	resolved, err := resolveWaitForChangeOptions(opts)
	if err != nil {
		return WaitForChangeResult{}, err
	}

	started := time.Now()
	result := WaitForChangeResult{}
	finish := func() WaitForChangeResult {
		result.Elapsed = time.Since(started)
		return result
	}
	timeout := func(operation, stage string, cause error) (WaitForChangeResult, error) {
		result = finish()
		return result, timeoutError(operation, fmt.Errorf(
			"%s: %w (changed=%v framesSampled=%d finalChangeFraction=%.6f elapsed=%s)",
			stage, cause, result.Changed, result.FramesSampled,
			result.FinalChangeFraction, result.Elapsed))
	}

	if checker, ok := c.decoder.(interface{ CheckAvailable(context.Context) error }); ok {
		if err := checker.CheckAvailable(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return timeout("checking video decoder availability", "decoder preflight did not complete", ctxErr)
			}
			return finish(), err
		}
	}

	unlock, err := c.lock(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return timeout("waiting for screen change", "command lock was not acquired", ctxErr)
		}
		return finish(), err
	}
	defer unlock()

	var (
		baseline        validatedWaitStableImage
		haveBaseline    bool
		lastPollStarted time.Time
	)
	for {
		if err := waitForFramePoll(ctx, lastPollStarted, resolved.pollInterval); err != nil {
			return timeout("waiting for screen change", "poll interval did not complete", err)
		}
		lastPollStarted = time.Now()

		// Take a new boundary for every poll. Reusing the preceding frame's
		// generation would allow a frame cached during decode/the poll gap to
		// satisfy this wait immediately.
		boundary := c.sess.video.generationBoundary()
		fr, err := c.sess.video.waitForFrameAfter(ctx, boundary)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				result = finish()
				return result, timeoutError("waiting for a video frame", fmt.Errorf(
					"no video frame available while waiting for screen change: %w (%s; changed=%v framesSampled=%d finalChangeFraction=%.6f elapsed=%s)",
					err, c.VideoDiagnostics().Summary(), result.Changed,
					result.FramesSampled, result.FinalChangeFraction, result.Elapsed))
			}
			return finish(), fmt.Errorf(
				"jetkvm: no video frame available while waiting for screen change: %w (%s)",
				err, c.VideoDiagnostics().Summary())
		}

		c.sess.diag.decodeAttempted(len(fr.annexB))
		decoded, err := c.decoder.DecodeFrame(ctx, fr.annexB)
		if err != nil {
			c.sess.diag.decodeFailed(classifyDecodeError(err))
			if ctxErr := ctx.Err(); ctxErr != nil {
				return timeout("waiting for screen change", "video frame decode did not complete", ctxErr)
			}
			return finish(), err
		}
		if decoded == nil {
			err := fmt.Errorf("jetkvm: video decoder returned a nil image")
			c.sess.diag.decodeFailed(classifyDecodeError(err))
			return finish(), err
		}
		current, err := validateWaitStableImage(decoded)
		if err != nil {
			err = fmt.Errorf("jetkvm: video decoder returned an invalid image: %w", err)
			c.sess.diag.decodeFailed(classifyDecodeError(err))
			return finish(), err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return timeout("waiting for screen change", "call canceled after video frame decode", ctxErr)
		}
		result.FramesSampled++

		if !haveBaseline {
			baseline = current
			haveBaseline = true
			continue
		}

		fraction, sameResolution, err := changedValidatedPixelFraction(ctx, baseline, current)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return timeout("waiting for screen change", "pixel comparison did not complete", ctxErr)
			}
			c.sess.diag.decodeFailed(classifyDecodeError(err))
			return finish(), fmt.Errorf("jetkvm: comparing decoded video frames: %w", err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return timeout("waiting for screen change", "call canceled after pixel comparison", ctxErr)
		}
		result.FinalChangeFraction = fraction
		if !sameResolution || fraction > resolved.threshold {
			result.Changed = true
			return finish(), nil
		}
	}
}
