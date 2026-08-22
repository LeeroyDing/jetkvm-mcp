package jetkvm

import (
	"context"
	"fmt"
	"image"
	"image/color"
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
		previous          validatedWaitStableImage
		havePrevious      bool
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
		decoded, err := c.decoder.DecodeFrame(ctx, fr.annexB)
		if err != nil {
			c.sess.diag.decodeFailed(classifyDecodeError(err))
			if ctxErr := ctx.Err(); ctxErr != nil {
				return timeout("waiting for screen stability", "video frame decode did not complete", ctxErr)
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
			return timeout("waiting for screen stability", "call canceled after video frame decode", ctxErr)
		}
		result.FramesSampled++

		if !havePrevious {
			previous = current
			havePrevious = true
			continue
		}

		fraction, sameResolution, err := changedValidatedPixelFraction(ctx, previous, current)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return timeout("waiting for screen stability", "pixel comparison did not complete", ctxErr)
			}
			c.sess.diag.decodeFailed(classifyDecodeError(err))
			return finish(), fmt.Errorf("jetkvm: comparing decoded video frames: %w", err)
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

type waitStablePixelFormat uint8

const (
	waitStablePixelGeneric waitStablePixelFormat = iota
	waitStablePixelRGBA
	waitStablePixelNRGBA
)

// validatedWaitStableImage caches the bounds and, for the optimized image
// types, copies the backing slice metadata. Comparisons use only the layout
// fields that were checked during validation.
type validatedWaitStableImage struct {
	image  image.Image
	bounds image.Rectangle
	width  int
	height int

	format waitStablePixelFormat
	pix    []byte
	stride int
}

func validateWaitStableImage(img image.Image) (validatedWaitStableImage, error) {
	if img == nil {
		return validatedWaitStableImage{}, fmt.Errorf("image is nil")
	}

	bounds, err := waitStableImageBounds(img)
	if err != nil {
		return validatedWaitStableImage{}, err
	}
	if bounds.Max.X < bounds.Min.X || bounds.Max.Y < bounds.Min.Y {
		return validatedWaitStableImage{}, fmt.Errorf("image bounds %v have negative size", bounds)
	}

	// Unsigned subtraction after the ordering check yields the exact span even
	// when the two signed coordinates straddle zero. Cap it before converting
	// to int, then apply the shared screenshot limits.
	width64 := uint64(bounds.Max.X) - uint64(bounds.Min.X)
	height64 := uint64(bounds.Max.Y) - uint64(bounds.Min.Y)
	maxInt := uint64(^uint(0) >> 1)
	if width64 > maxInt || height64 > maxInt {
		return validatedWaitStableImage{}, fmt.Errorf(
			"image bounds %v have dimensions %dx%d that cannot be represented safely",
			bounds, width64, height64)
	}
	width, height := int(width64), int(height64)
	if err := ValidateScreenshotDimensions(width, height); err != nil {
		return validatedWaitStableImage{}, err
	}

	validated := validatedWaitStableImage{
		image:  img,
		bounds: bounds,
		width:  width,
		height: height,
	}
	switch typed := img.(type) {
	case *image.RGBA:
		if err := validateFourByteImageBacking(typed.Pix, typed.Stride, width, height); err != nil {
			return validatedWaitStableImage{}, fmt.Errorf("*image.RGBA backing is invalid: %w", err)
		}
		validated.format = waitStablePixelRGBA
		validated.pix = typed.Pix
		validated.stride = typed.Stride
	case *image.NRGBA:
		if err := validateFourByteImageBacking(typed.Pix, typed.Stride, width, height); err != nil {
			return validatedWaitStableImage{}, fmt.Errorf("*image.NRGBA backing is invalid: %w", err)
		}
		validated.format = waitStablePixelNRGBA
		validated.pix = typed.Pix
		validated.stride = typed.Stride
	}
	return validated, nil
}

func waitStableImageBounds(img image.Image) (bounds image.Rectangle, err error) {
	completed := false
	defer func() {
		_ = recover()
		if !completed {
			err = fmt.Errorf("reading image bounds panicked")
		}
	}()
	bounds = img.Bounds()
	completed = true
	return bounds, nil
}

// validateFourByteImageBacking proves that every row slice used by the fast
// comparison path is in range. The division-based final-row check avoids
// multiplying an attacker-controlled stride by the image height.
func validateFourByteImageBacking(pix []byte, stride, width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("dimensions %dx%d must be positive", width, height)
	}
	if width > int(^uint(0)>>1)/4 {
		return fmt.Errorf("width %d overflows the four-byte row size", width)
	}
	rowBytes := width * 4
	if stride < rowBytes {
		return fmt.Errorf("stride %d is smaller than the %d-byte row width", stride, rowBytes)
	}
	if len(pix) < rowBytes {
		return fmt.Errorf("pix length %d cannot cover the first %d-byte row", len(pix), rowBytes)
	}
	if height > 1 && height-1 > (len(pix)-rowBytes)/stride {
		return fmt.Errorf(
			"pix length %d with stride %d cannot cover %d rows of %d bytes",
			len(pix), stride, height, rowBytes)
	}
	return nil
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
	validatedPrevious, err := validateWaitStableImage(previous)
	if err != nil {
		return 0, false, fmt.Errorf("previous image is invalid: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	validatedCurrent, err := validateWaitStableImage(current)
	if err != nil {
		return 0, false, fmt.Errorf("current image is invalid: %w", err)
	}
	return changedValidatedPixelFraction(ctx, validatedPrevious, validatedCurrent)
}

func changedValidatedPixelFraction(
	ctx context.Context,
	previous validatedWaitStableImage,
	current validatedWaitStableImage,
) (fraction float64, sameResolution bool, err error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	width, height := previous.width, previous.height
	if width != current.width || height != current.height {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		return 1, false, nil
	}
	// FFmpeg's PNG output normally decodes to one of these two four-byte
	// image types. Compare their backing rows directly so the common path
	// avoids millions of interface calls and temporary color values.
	if previous.format != waitStablePixelGeneric && previous.format == current.format {
		return changedFourBytePixelFraction(
			ctx, previous.pix, previous.stride,
			current.pix, current.stride, width, height)
	}
	return changedGenericPixelFraction(ctx, previous, current)
}

func changedGenericPixelFraction(
	ctx context.Context,
	previous validatedWaitStableImage,
	current validatedWaitStableImage,
) (fraction float64, sameResolution bool, err error) {
	position := waitStableComparisonPosition{}
	completed := false
	defer func() {
		_ = recover()
		if !completed {
			fraction = 0
			sameResolution = true
			err = fmt.Errorf(
				"%s decoder image panicked during generic pixel comparison at relative pixel (%d,%d)",
				position.imageRole, position.relativeX, position.relativeY)
		}
	}()
	fraction, sameResolution, err = compareGenericPixels(ctx, previous, current, &position)
	completed = true
	return fraction, sameResolution, err
}

type waitStableComparisonPosition struct {
	imageRole            string
	relativeX, relativeY int
}

func compareGenericPixels(
	ctx context.Context,
	previous validatedWaitStableImage,
	current validatedWaitStableImage,
	position *waitStableComparisonPosition,
) (float64, bool, error) {
	var changed, compared uint64
	for y := 0; y < previous.height; y++ {
		if err := ctx.Err(); err != nil {
			return 0, true, err
		}
		for x := 0; x < previous.width; x++ {
			// Keep even unusually large or synthetic images bounded by ctx
			// without paying for a cancellation check on every pixel.
			if compared&0x3fff == 0 {
				if err := ctx.Err(); err != nil {
					return 0, true, err
				}
			}

			position.relativeX, position.relativeY = x, y
			position.imageRole = "previous"
			pr, pg, pb, pa, err := waitStablePixelColor(previous, x, y)
			if err != nil {
				return 0, true, fmt.Errorf("previous decoder image: %w", err)
			}
			position.imageRole = "current"
			cr, cg, cb, ca, err := waitStablePixelColor(current, x, y)
			if err != nil {
				return 0, true, fmt.Errorf("current decoder image: %w", err)
			}
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

func waitStablePixelColor(img validatedWaitStableImage, x, y int) (r, g, b, a uint32, err error) {
	if img.format != waitStablePixelGeneric {
		offset := y*img.stride + x*4
		pixel := img.pix[offset : offset+4]
		if img.format == waitStablePixelRGBA {
			r, g, b, a = color.RGBA{R: pixel[0], G: pixel[1], B: pixel[2], A: pixel[3]}.RGBA()
			return r, g, b, a, nil
		}
		r, g, b, a = color.NRGBA{R: pixel[0], G: pixel[1], B: pixel[2], A: pixel[3]}.RGBA()
		return r, g, b, a, nil
	}

	pixel := img.image.At(img.bounds.Min.X+x, img.bounds.Min.Y+y)
	if pixel == nil {
		return 0, 0, 0, 0, fmt.Errorf("image.At returned a nil color")
	}
	r, g, b, a = pixel.RGBA()
	return r, g, b, a, nil
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
	if err := ctx.Err(); err != nil {
		return 0, true, err
	}
	if err := validateFourByteImageBacking(previous, previousStride, width, height); err != nil {
		return 0, true, fmt.Errorf("previous four-byte image backing is invalid: %w", err)
	}
	if err := validateFourByteImageBacking(current, currentStride, width, height); err != nil {
		return 0, true, fmt.Errorf("current four-byte image backing is invalid: %w", err)
	}

	rowBytes := width * 4
	var changed, compared uint64
	for y := 0; y < height; y++ {
		if err := ctx.Err(); err != nil {
			return 0, true, err
		}
		previousRow := previous[y*previousStride : y*previousStride+rowBytes]
		currentRow := current[y*currentStride : y*currentStride+rowBytes]
		for offset := 0; offset < rowBytes; offset += 4 {
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
