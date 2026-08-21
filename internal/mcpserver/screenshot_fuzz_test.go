package mcpserver

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"math"
	"testing"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

// FuzzScreenshotOptionsAndRender exercises the caller-controlled validation
// and in-memory transform surface without involving WebRTC. Properties pinned:
//
//   - malformed formats, quality combinations, non-finite/non-positive scales,
//     invalid dimensions, and out-of-frame rectangles fail closed;
//   - arbitrary integer coordinates cannot overflow the bounds checks or drive
//     an allocation before validation;
//   - every successful result is decodable in its stated format and MIME type;
//   - output dimensions match crop-then-scale ordering, remain positive, and
//     never exceed the cropped source, including for scales above one;
//   - the default PNG path preserves the request-fresh capture bytes exactly.
//
// Seeds cover the documented examples and important IEEE-754/integer
// boundaries. Under ordinary `go test`, only these seeds run.
func FuzzScreenshotOptionsAndRender(f *testing.F) {
	shot, _ := makeScreenshotFixture(f, 12, 8)
	add := func(
		hasFormat bool, format string,
		hasQuality bool, quality int,
		hasScale bool, scale float64,
		hasRegion bool, x, y, width, height int,
	) {
		f.Add(hasFormat, format, hasQuality, quality, hasScale, math.Float64bits(scale), hasRegion, x, y, width, height)
	}

	add(false, "", false, 0, false, 0, false, 0, 0, 0, 0)
	add(true, "png", false, 0, true, 1, false, 0, 0, 0, 0)
	add(true, "jpeg", false, 0, false, 0, false, 0, 0, 0, 0)
	add(true, "jpeg", true, 1, true, 0.5, false, 0, 0, 0, 0)
	add(true, "jpeg", true, 80, true, 2.5, false, 0, 0, 0, 0)
	add(true, "jpeg", true, 100, false, 0, true, 2, 1, 8, 6)
	add(true, "png", true, 80, false, 0, false, 0, 0, 0, 0)
	add(true, "jpg", false, 0, false, 0, false, 0, 0, 0, 0)
	add(true, "", false, 0, false, 0, false, 0, 0, 0, 0)
	add(true, "jpeg", true, 0, false, 0, false, 0, 0, 0, 0)
	add(true, "jpeg", true, 101, false, 0, false, 0, 0, 0, 0)
	add(false, "", false, 0, true, math.SmallestNonzeroFloat64, false, 0, 0, 0, 0)
	add(false, "", false, 0, true, 0, false, 0, 0, 0, 0)
	add(false, "", false, 0, true, math.Copysign(0, -1), false, 0, 0, 0, 0)
	add(false, "", false, 0, true, -1, false, 0, 0, 0, 0)
	add(false, "", false, 0, true, math.NaN(), false, 0, 0, 0, 0)
	add(false, "", false, 0, true, math.Inf(1), false, 0, 0, 0, 0)
	add(false, "", false, 0, true, math.Inf(-1), false, 0, 0, 0, 0)
	add(false, "", false, 0, true, 0.5, true, 2, 1, 8, 6)
	add(false, "", false, 0, false, 0, true, 0, 0, 12, 8)
	add(false, "", false, 0, false, 0, true, 11, 7, 1, 1)
	add(false, "", false, 0, false, 0, true, -1, 0, 1, 1)
	add(false, "", false, 0, false, 0, true, 0, -1, 1, 1)
	add(false, "", false, 0, false, 0, true, 0, 0, 0, 1)
	add(false, "", false, 0, false, 0, true, 0, 0, 1, 0)
	add(false, "", false, 0, false, 0, true, 12, 0, 1, 1)
	add(false, "", false, 0, false, 0, true, 0, 8, 1, 1)
	add(false, "", false, 0, false, 0, true, 11, 0, 2, 1)
	add(false, "", false, 0, false, 0, true, 0, 7, 1, 2)
	add(false, "", false, 0, false, 0, true, maxScreenshotRegionValue, 0, 1, 1)
	add(false, "", false, 0, false, 0, true, maxScreenshotRegionValue+1, 0, 1, 1)
	add(false, "", false, 0, false, 0, true, math.MaxInt, 0, 1, 1)
	add(false, "", false, 0, false, 0, true, 1, 0, math.MaxInt, 1)
	add(false, "", false, 0, false, 0, true, 0, math.MaxInt, 1, 1)
	add(false, "", false, 0, false, 0, true, 0, 1, 1, math.MaxInt)

	f.Fuzz(func(
		t *testing.T,
		hasFormat bool, format string,
		hasQuality bool, quality int,
		hasScale bool, scaleBits uint64,
		hasRegion bool, x, y, width, height int,
	) {
		scale := math.Float64frombits(scaleBits)
		args := screenshotArgs{}
		if hasFormat {
			args.Format = &format
		}
		if hasQuality {
			args.Quality = &quality
		}
		if hasScale {
			args.Scale = &scale
		}
		if hasRegion {
			args.Region = &screenshotRegionArgs{X: x, Y: y, Width: width, Height: height}
		}

		wantFormat := screenshotFormatPNG
		if hasFormat {
			wantFormat = format
		}
		normalizeValid := wantFormat == screenshotFormatPNG || wantFormat == screenshotFormatJPEG
		if hasQuality {
			normalizeValid = normalizeValid && wantFormat == screenshotFormatJPEG && quality >= 1 && quality <= 100
		}
		if hasScale {
			normalizeValid = normalizeValid && !math.IsNaN(scale) && !math.IsInf(scale, 0) && scale > 0
		}
		if hasRegion {
			normalizeValid = normalizeValid &&
				x >= 0 && x <= maxScreenshotRegionValue &&
				y >= 0 && y <= maxScreenshotRegionValue &&
				width > 0 && width <= maxScreenshotRegionValue &&
				height > 0 && height <= maxScreenshotRegionValue
		}

		opts, err := normalizeScreenshotOptions(args)
		if !normalizeValid {
			if err == nil {
				t.Fatalf("normalize accepted invalid args: %+v", args)
			}
			return
		}
		if err != nil {
			t.Fatalf("normalize rejected valid args %+v: %v", args, err)
		}

		wantScale := 1.0
		if hasScale {
			wantScale = min(scale, 1)
		}
		if opts.Format != wantFormat || opts.Scale != wantScale {
			t.Fatalf("normalized format/scale = %q/%g, want %q/%g", opts.Format, opts.Scale, wantFormat, wantScale)
		}
		wantQuality := 0
		if wantFormat == screenshotFormatJPEG {
			wantQuality = defaultScreenshotJPEGQuality
			if hasQuality {
				wantQuality = quality
			}
		}
		if opts.Quality != wantQuality {
			t.Fatalf("normalized quality = %d, want %d", opts.Quality, wantQuality)
		}

		frameValid := !hasRegion ||
			x <= shot.Width && width <= shot.Width-x &&
				y <= shot.Height && height <= shot.Height-y
		sourceBytes := append([]byte(nil), shot.PNG...)
		rendered, err := renderScreenshot(context.Background(), shot, opts)
		if !frameValid {
			if err == nil {
				t.Fatalf("render accepted out-of-frame region %+v for %dx%d source", opts.Region, shot.Width, shot.Height)
			}
			return
		}
		if err != nil {
			t.Fatalf("render rejected valid options %+v: %v", opts, err)
		}
		if !bytes.Equal(shot.PNG, sourceBytes) {
			t.Fatal("render mutated the captured PNG bytes")
		}

		workingWidth, workingHeight := shot.Width, shot.Height
		if hasRegion {
			workingWidth, workingHeight = width, height
		}
		wantWidth := max(1, int(math.Round(float64(workingWidth)*wantScale)))
		wantHeight := max(1, int(math.Round(float64(workingHeight)*wantScale)))
		wantWidth = min(workingWidth, wantWidth)
		wantHeight = min(workingHeight, wantHeight)
		if rendered.Width != wantWidth || rendered.Height != wantHeight {
			t.Fatalf("rendered dimensions = %dx%d, want %dx%d", rendered.Width, rendered.Height, wantWidth, wantHeight)
		}
		if rendered.Width < 1 || rendered.Height < 1 || rendered.Width > workingWidth || rendered.Height > workingHeight {
			t.Fatalf("rendered dimensions %dx%d escape positive no-upscale bounds for %dx%d input", rendered.Width, rendered.Height, workingWidth, workingHeight)
		}
		if rendered.Format != wantFormat || rendered.Quality != wantQuality {
			t.Fatalf("rendered format/quality = %q/%d, want %q/%d", rendered.Format, rendered.Quality, wantFormat, wantQuality)
		}

		wantMIME := screenshotMIMETypePNG
		wantMagic := []byte("\x89PNG\r\n\x1a\n")
		if wantFormat == screenshotFormatJPEG {
			wantMIME = screenshotMIMETypeJPEG
			wantMagic = []byte{0xff, 0xd8, 0xff}
		}
		if rendered.MIMEType != wantMIME {
			t.Fatalf("rendered MIME type = %q, want %q", rendered.MIMEType, wantMIME)
		}
		if !bytes.HasPrefix(rendered.Data, wantMagic) {
			t.Fatalf("rendered %s data has the wrong signature", wantFormat)
		}
		config, decodedFormat, err := image.DecodeConfig(bytes.NewReader(rendered.Data))
		if err != nil {
			t.Fatalf("decoding rendered image: %v", err)
		}
		if decodedFormat != wantFormat || config.Width != wantWidth || config.Height != wantHeight {
			t.Fatalf("decoded image = %s %dx%d, want %s %dx%d", decodedFormat, config.Width, config.Height, wantFormat, wantWidth, wantHeight)
		}
		if wantFormat == screenshotFormatPNG && !hasRegion && wantScale == 1 && !bytes.Equal(rendered.Data, shot.PNG) {
			t.Fatal("unmodified PNG render did not preserve the captured bytes")
		}
	})
}

// FuzzScreenshotCapturedPNGConfig covers the untrusted boundary between
// capture metadata and PNG IHDR dimensions. In particular, no arbitrary
// positive dimensions may reach a full pixel decode merely because the two
// sources agree.
func FuzzScreenshotCapturedPNGConfig(f *testing.F) {
	fixture, _ := makeScreenshotFixture(f, 12, 8)
	f.Add(fixture.PNG, 12, 8)
	f.Add(fixture.PNG, 11, 8)
	f.Add([]byte("not a PNG"), 12, 8)
	f.Add(screenshotPNGWithDimensions(f, fixture.PNG, 8193, 1), 12, 8)
	f.Add(screenshotPNGWithDimensions(f, fixture.PNG, 4097, 4097), 12, 8)

	f.Fuzz(func(t *testing.T, pngBytes []byte, metadataWidth, metadataHeight int) {
		shot := fixture
		shot.PNG = pngBytes
		shot.Width = metadataWidth
		shot.Height = metadataHeight

		rendered, err := renderScreenshot(context.Background(), shot, screenshotOptions{
			Format: screenshotFormatPNG,
			Scale:  1,
		})
		if err != nil {
			return
		}

		if len(pngBytes) > jetkvm.MaxScreenshotEncodedBytes {
			t.Fatalf("render accepted %d encoded bytes over the %d-byte limit", len(pngBytes), jetkvm.MaxScreenshotEncodedBytes)
		}
		if err := jetkvm.ValidateScreenshotDimensions(metadataWidth, metadataHeight); err != nil {
			t.Fatalf("render accepted unsafe metadata dimensions %dx%d: %v", metadataWidth, metadataHeight, err)
		}
		config, err := png.DecodeConfig(bytes.NewReader(pngBytes))
		if err != nil {
			t.Fatalf("render accepted an invalid PNG configuration: %v", err)
		}
		if err := jetkvm.ValidateScreenshotDimensions(config.Width, config.Height); err != nil {
			t.Fatalf("render accepted unsafe PNG dimensions %dx%d: %v", config.Width, config.Height, err)
		}
		if config.Width != metadataWidth || config.Height != metadataHeight {
			t.Fatalf(
				"render accepted metadata %dx%d that mismatches PNG configuration %dx%d",
				metadataWidth, metadataHeight, config.Width, config.Height,
			)
		}
		if rendered.Width != metadataWidth || rendered.Height != metadataHeight {
			t.Fatalf(
				"rendered dimensions = %dx%d, want validated source dimensions %dx%d",
				rendered.Width, rendered.Height, metadataWidth, metadataHeight,
			)
		}
		if !bytes.Equal(rendered.Data, pngBytes) {
			t.Fatal("validated default render did not preserve the PNG bytes")
		}
	})
}
