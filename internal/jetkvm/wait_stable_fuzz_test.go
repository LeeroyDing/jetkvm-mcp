package jetkvm

import (
	"context"
	"image"
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

// FuzzChangedPixelFractionImageBacking exercises raw rectangle, stride, and
// backing combinations for both optimized four-byte image types. Any panic is
// a test failure; malformed layouts must instead produce an ordinary error.
func FuzzChangedPixelFractionImageBacking(f *testing.F) {
	maxInt := int(^uint(0) >> 1)
	minInt := -maxInt - 1
	for _, seed := range []struct {
		kind                           uint8
		minX, minY, maxX, maxY, stride int
		pix                            []byte
	}{
		{kind: 0, minX: 0, minY: 0, maxX: 1, maxY: 1, stride: 4, pix: make([]byte, 4)},
		{kind: 1, minX: 4, minY: 7, maxX: 5, maxY: 8, stride: maxInt, pix: make([]byte, 4)},
		{kind: 1, minX: -3, minY: -2, maxX: -1, maxY: 0, stride: 12, pix: make([]byte, 20)},
		{kind: 0, minX: 0, minY: 0, maxX: 0, maxY: 1, stride: 4},
		{kind: 1, minX: 1, minY: 0, maxX: 0, maxY: 1, stride: 4},
		{kind: 0, minX: minInt, minY: 0, maxX: maxInt, maxY: 1, stride: 4},
		{kind: 0, minX: 0, minY: 0, maxX: 2, maxY: 2, stride: 8, pix: make([]byte, 15)},
		{kind: 1, minX: 0, minY: 0, maxX: 2, maxY: 2, stride: 4, pix: make([]byte, 12)},
		{kind: 0, minX: 0, minY: 0, maxX: 2, maxY: 2, stride: -1, pix: make([]byte, 16)},
		{kind: 1, minX: 0, minY: 0, maxX: 2, maxY: 2, stride: maxInt, pix: make([]byte, 8)},
	} {
		f.Add(seed.kind, seed.minX, seed.minY, seed.maxX, seed.maxY, seed.stride, seed.pix)
	}

	f.Fuzz(func(t *testing.T, kind uint8, minX, minY, maxX, maxY, stride int, pix []byte) {
		bounds := image.Rectangle{
			Min: image.Pt(minX, minY),
			Max: image.Pt(maxX, maxY),
		}
		var img image.Image
		if kind&1 == 0 {
			img = &image.RGBA{Pix: pix, Stride: stride, Rect: bounds}
		} else {
			img = &image.NRGBA{Pix: pix, Stride: stride, Rect: bounds}
		}

		wantValid := maxX >= minX && maxY >= minY
		if wantValid {
			width64 := uint64(maxX) - uint64(minX)
			height64 := uint64(maxY) - uint64(minY)
			wantValid = width64 > 0 && height64 > 0 &&
				width64 <= uint64(maxScreenshotDimension) &&
				height64 <= uint64(maxScreenshotDimension)
			if wantValid {
				width, height := int(width64), int(height64)
				wantValid = width <= maxScreenshotPixels/height
				rowBytes := width * 4
				wantValid = wantValid && stride >= rowBytes && len(pix) >= rowBytes
				if wantValid && height > 1 {
					wantValid = height-1 <= (len(pix)-rowBytes)/stride
				}
			}
		}

		_, validationErr := validateWaitStableImage(img)
		if (validationErr == nil) != wantValid {
			t.Fatalf("validation error = %v, want valid %t (bounds=%v stride=%d pix=%d)",
				validationErr, wantValid, bounds, stride, len(pix))
		}

		safe := image.NewRGBA(image.Rect(0, 0, 1, 1))
		_, _, previousErr := changedPixelFraction(context.Background(), img, safe)
		_, _, currentErr := changedPixelFraction(context.Background(), safe, img)
		if !wantValid {
			if previousErr == nil || currentErr == nil {
				t.Fatalf("invalid image comparison errors = previous:%v current:%v, want both non-nil", previousErr, currentErr)
			}
			return
		}
		if previousErr != nil || currentErr != nil {
			t.Fatalf("valid directional comparisons failed: previous:%v current:%v", previousErr, currentErr)
		}

		fraction, sameResolution, err := changedPixelFraction(context.Background(), img, img)
		if err != nil {
			t.Fatalf("valid image self-comparison failed: bounds=%v stride=%d pix=%d: %v", bounds, stride, len(pix), err)
		}
		if !sameResolution || fraction != 0 {
			t.Fatalf("self comparison = (%v, %t), want (0, true)", fraction, sameResolution)
		}
	})
}
