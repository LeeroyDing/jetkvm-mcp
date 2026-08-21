package mcpserver

import (
	"bytes"
	"context"
	"math"
	"testing"
)

// FuzzRenderScreenshotForText pins the exported CLI/MCP adapter to the
// screenshot tool's existing validation and transform implementation. Any
// future change must keep both surfaces identical for non-finite scales,
// integer boundaries, out-of-frame crops, crop-before-scale ordering, and
// PNG output.
func FuzzRenderScreenshotForText(f *testing.F) {
	shot, _ := makeScreenshotFixture(f, 12, 8)
	add := func(hasScale bool, scale float64, hasRegion bool, x, y, width, height int) {
		f.Add(hasScale, math.Float64bits(scale), hasRegion, x, y, width, height)
	}
	add(false, 0, false, 0, 0, 0, 0)
	add(true, 1, false, 0, 0, 0, 0)
	add(true, 0.5, true, 2, 1, 8, 6)
	add(true, 2.5, true, 0, 0, 12, 8)
	add(true, 0, false, 0, 0, 0, 0)
	add(true, -1, false, 0, 0, 0, 0)
	add(true, math.NaN(), false, 0, 0, 0, 0)
	add(true, math.Inf(1), false, 0, 0, 0, 0)
	add(false, 0, true, -1, 0, 1, 1)
	add(false, 0, true, 0, 0, 0, 1)
	add(false, 0, true, 11, 7, 1, 1)
	add(false, 0, true, 11, 7, 2, 2)
	add(false, 0, true, maxScreenshotRegionValue, 0, 1, 1)
	add(false, 0, true, maxScreenshotRegionValue+1, 0, 1, 1)
	add(false, 0, true, math.MaxInt, 0, 1, 1)
	add(false, 0, true, 0, 0, math.MaxInt, 1)

	f.Fuzz(func(t *testing.T, hasScale bool, scaleBits uint64, hasRegion bool, x, y, width, height int) {
		scale := math.Float64frombits(scaleBits)
		publicOpts := ScreenshotTransformOptions{}
		internalArgs := screenshotArgs{}
		if hasScale {
			publicOpts.Scale = &scale
			internalArgs.Scale = &scale
		}
		if hasRegion {
			region := &ScreenshotRegion{X: x, Y: y, Width: width, Height: height}
			publicOpts.Region = region
			internalArgs.Region = region
		}

		got, gotErr := RenderScreenshotForText(context.Background(), shot, publicOpts)
		normalized, normalizeErr := normalizeScreenshotOptions(internalArgs)
		var want renderedScreenshot
		wantErr := normalizeErr
		if normalizeErr == nil {
			want, wantErr = renderScreenshot(context.Background(), shot, normalized)
		}
		if (gotErr != nil) != (wantErr != nil) {
			t.Fatalf("public/internal error mismatch: got %v want %v", gotErr, wantErr)
		}
		if gotErr != nil {
			return
		}
		if got.Width != want.Width || got.Height != want.Height ||
			got.Format != screenshotFormatPNG || got.MIMEType != screenshotMIMETypePNG ||
			got.Quality != 0 || !bytes.Equal(got.Data, want.Data) {
			t.Fatalf("public transform = %s %dx%d quality=%d (%d bytes), internal = %s %dx%d quality=%d (%d bytes)",
				got.Format, got.Width, got.Height, got.Quality, len(got.Data),
				want.Format, want.Width, want.Height, want.Quality, len(want.Data))
		}
	})
}
