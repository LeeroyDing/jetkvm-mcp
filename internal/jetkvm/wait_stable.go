package jetkvm

import (
	"context"
	"fmt"
	"image"
	"math"
	"time"
)

const (
	// DefaultWaitStableThreshold is the maximum fraction of pixels that may
	// change between two consecutive frames for that comparison to count as
	// stable.
	DefaultWaitStableThreshold = 0.01

	// DefaultWaitStableFrames is the number of consecutive stable comparisons
	// required before WaitStable reports that the screen has settled.
	DefaultWaitStableFrames = 2

	// MaxWaitStableFrames keeps all public adapters within the signed 32-bit
	// integer range, avoiding architecture-dependent typed decode overflow.
	MaxWaitStableFrames = 1<<31 - 1

	// DefaultWaitStablePollInterval is the minimum gap between the starts of
	// two successive fresh-frame polls.
	DefaultWaitStablePollInterval = 250 * time.Millisecond
)

// WaitStableOptions configures a WaitStable call. Nil fields use the defaults
// above. Pointers preserve the distinction between an omitted field and the
// valid explicit zero values for Threshold and PollInterval.
type WaitStableOptions struct {
	Threshold    *float64
	StableFrames *int
	PollInterval *time.Duration
}

// WaitStableResult reports the observations made while waiting for a screen
// to settle. On a timeout, WaitStable returns the partial result together with
// a classified timeout error.
type WaitStableResult struct {
	Settled             bool
	FramesSampled       int
	FinalChangeFraction float64
	Elapsed             time.Duration
}

type resolvedWaitStableOptions struct {
	threshold    float64
	stableFrames int
	pollInterval time.Duration
}

// ValidateWaitStableOptions resolves omitted fields to their defaults and
// validates every supplied value. It performs no I/O.
func ValidateWaitStableOptions(opts WaitStableOptions) error {
	_, err := resolveWaitStableOptions(opts)
	return err
}

func resolveWaitStableOptions(opts WaitStableOptions) (resolvedWaitStableOptions, error) {
	resolved := resolvedWaitStableOptions{
		threshold:    DefaultWaitStableThreshold,
		stableFrames: DefaultWaitStableFrames,
		pollInterval: DefaultWaitStablePollInterval,
	}
	if opts.Threshold != nil {
		resolved.threshold = *opts.Threshold
	}
	if opts.StableFrames != nil {
		resolved.stableFrames = *opts.StableFrames
	}
	if opts.PollInterval != nil {
		resolved.pollInterval = *opts.PollInterval
	}

	if math.IsNaN(resolved.threshold) || math.IsInf(resolved.threshold, 0) ||
		resolved.threshold < 0 || resolved.threshold > 1 {
		return resolvedWaitStableOptions{}, fmt.Errorf(
			"invalid Threshold: must be a finite fraction in [0.0,1.0], got %v", resolved.threshold)
	}
	if resolved.stableFrames < 1 {
		return resolvedWaitStableOptions{}, fmt.Errorf(
			"invalid StableFrames: must be at least 1, got %d", resolved.stableFrames)
	}
	if resolved.stableFrames > MaxWaitStableFrames {
		return resolvedWaitStableOptions{}, fmt.Errorf(
			"invalid StableFrames: must be at most %d, got %d", MaxWaitStableFrames, resolved.stableFrames)
	}
	if resolved.pollInterval < 0 {
		return resolvedWaitStableOptions{}, fmt.Errorf(
			"invalid PollInterval: must be non-negative, got %s", resolved.pollInterval)
	}
	return resolved, nil
}

// WaitStable polls request-fresh decoded video frames until the fraction of
// changed pixels stays at or below Threshold for StableFrames consecutive
// comparisons. It holds the Client command lock for the complete operation,
// so Status, CaptureScreenshot, and legacy RPC control calls cannot interleave
// with the sampled sequence.
//
// Every poll records the current completed frame generation immediately
// before waiting for a strictly newer one. Frames cached during a poll gap are
// therefore skipped rather than compared as though they were fresh.
func (c *Client) WaitStable(ctx context.Context, opts WaitStableOptions) (WaitStableResult, error) {
	resolved, err := resolveWaitStableOptions(opts)
	if err != nil {
		return WaitStableResult{}, err
	}

	started := time.Now()
	result := WaitStableResult{FinalChangeFraction: 1}
	finish := func() WaitStableResult {
		result.Elapsed = time.Since(started)
		return result
	}
	timeout := func(operation, stage string, cause error) (WaitStableResult, error) {
		result = finish()
		return result, timeoutError(operation, fmt.Errorf(
			"%s: %w (settled=%v framesSampled=%d finalChangeFraction=%.6f elapsed=%s)",
			stage, cause, result.Settled, result.FramesSampled,
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
			return timeout("waiting for screen stability", "command lock was not acquired", ctxErr)
		}
		return finish(), err
	}
	defer unlock()

	var (
		previous          image.Image
		stableComparisons int
		lastPollStarted   time.Time
	)
	for {
		if err := waitForStablePoll(ctx, lastPollStarted, resolved.pollInterval); err != nil {
			return timeout("waiting for screen stability", "poll interval did not complete", err)
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
					"no video frame available while waiting for screen stability: %w (%s; settled=%v framesSampled=%d finalChangeFraction=%.6f elapsed=%s)",
					err, c.VideoDiagnostics().Summary(), result.Settled,
					result.FramesSampled, result.FinalChangeFraction, result.Elapsed))
			}
			return finish(), fmt.Errorf(
				"jetkvm: no video frame available while waiting for screen stability: %w (%s)",
				err, c.VideoDiagnostics().Summary())
		}

		c.sess.diag.decodeAttempted(len(fr.annexB))
		current, err := c.decoder.DecodeFrame(ctx, fr.annexB)
		if err != nil {
			c.sess.diag.decodeFailed(classifyDecodeError(err))
			if ctxErr := ctx.Err(); ctxErr != nil {
				return timeout("waiting for screen stability", "video frame decode did not complete", ctxErr)
			}
			return finish(), err
		}
		if current == nil {
			err := fmt.Errorf("jetkvm: video decoder returned a nil image")
			c.sess.diag.decodeFailed(classifyDecodeError(err))
			return finish(), err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return timeout("waiting for screen stability", "call canceled after video frame decode", ctxErr)
		}
		result.FramesSampled++

		if previous == nil {
			previous = current
			continue
		}

		fraction, sameResolution, err := changedPixelFraction(ctx, previous, current)
		if err != nil {
			return timeout("waiting for screen stability", "pixel comparison did not complete", err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return timeout("waiting for screen stability", "call canceled after pixel comparison", ctxErr)
		}
		result.FinalChangeFraction = fraction
		if sameResolution && fraction <= resolved.threshold {
			stableComparisons++
		} else {
			stableComparisons = 0
		}
		if stableComparisons >= resolved.stableFrames {
			result.Settled = true
			return finish(), nil
		}
		previous = current
	}
}

func waitForStablePoll(ctx context.Context, previousStart time.Time, interval time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if previousStart.IsZero() || interval == 0 {
		return nil
	}
	remaining := interval - time.Since(previousStart)
	if remaining <= 0 {
		return nil
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// changedPixelFraction compares corresponding pixels in two images. A
// resolution mismatch reports a full change and marks the comparison
// ineligible for stability; the separate boolean matters when Threshold is
// explicitly 1.0. Bounds origins may differ, so coordinates are relative to
// each image's minimum point.
func changedPixelFraction(ctx context.Context, previous, current image.Image) (fraction float64, sameResolution bool, err error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	previousBounds := previous.Bounds()
	currentBounds := current.Bounds()
	width, height := previousBounds.Dx(), previousBounds.Dy()
	if width != currentBounds.Dx() || height != currentBounds.Dy() {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		return 1, false, nil
	}
	if width == 0 || height == 0 {
		if err := ctx.Err(); err != nil {
			return 0, true, err
		}
		return 0, true, nil
	}
	// FFmpeg's PNG output normally decodes to one of these two four-byte
	// image types. Compare their backing rows directly so the common path
	// avoids millions of interface calls and temporary color values.
	if previousRGBA, ok := previous.(*image.RGBA); ok {
		if currentRGBA, ok := current.(*image.RGBA); ok {
			return changedFourBytePixelFraction(
				ctx, previousRGBA.Pix, previousRGBA.Stride,
				currentRGBA.Pix, currentRGBA.Stride, width, height)
		}
	}
	if previousNRGBA, ok := previous.(*image.NRGBA); ok {
		if currentNRGBA, ok := current.(*image.NRGBA); ok {
			return changedFourBytePixelFraction(
				ctx, previousNRGBA.Pix, previousNRGBA.Stride,
				currentNRGBA.Pix, currentNRGBA.Stride, width, height)
		}
	}

	var changed, compared uint64
	for y := 0; y < height; y++ {
		if err := ctx.Err(); err != nil {
			return 0, true, err
		}
		for x := 0; x < width; x++ {
			// Keep even unusually large or synthetic images bounded by ctx
			// without paying for a cancellation check on every pixel.
			if compared&0x3fff == 0 {
				if err := ctx.Err(); err != nil {
					return 0, true, err
				}
			}
			pr, pg, pb, pa := previous.At(previousBounds.Min.X+x, previousBounds.Min.Y+y).RGBA()
			cr, cg, cb, ca := current.At(currentBounds.Min.X+x, currentBounds.Min.Y+y).RGBA()
			if pr != cr || pg != cg || pb != cb || pa != ca {
				changed++
			}
			compared++
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, true, err
	}
	return float64(changed) / float64(compared), true, nil
}

func changedFourBytePixelFraction(
	ctx context.Context,
	previous []byte,
	previousStride int,
	current []byte,
	currentStride int,
	width int,
	height int,
) (float64, bool, error) {
	var changed, compared uint64
	for y := 0; y < height; y++ {
		if err := ctx.Err(); err != nil {
			return 0, true, err
		}
		previousRow := previous[y*previousStride : y*previousStride+width*4]
		currentRow := current[y*currentStride : y*currentStride+width*4]
		for offset := 0; offset < width*4; offset += 4 {
			if compared&0x3fff == 0 {
				if err := ctx.Err(); err != nil {
					return 0, true, err
				}
			}
			if previousRow[offset] != currentRow[offset] ||
				previousRow[offset+1] != currentRow[offset+1] ||
				previousRow[offset+2] != currentRow[offset+2] ||
				previousRow[offset+3] != currentRow[offset+3] {
				changed++
			}
			compared++
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, true, err
	}
	return float64(changed) / float64(compared), true, nil
}
