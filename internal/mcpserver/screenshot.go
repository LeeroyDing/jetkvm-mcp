package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"

	xdraw "golang.org/x/image/draw"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

const (
	screenshotFormatPNG          = "png"
	screenshotFormatJPEG         = "jpeg"
	defaultScreenshotJPEGQuality = 80
	screenshotMIMETypePNG        = "image/png"
	screenshotMIMETypeJPEG       = "image/jpeg"
	maxScreenshotRegionValue     = 1<<31 - 1
)

var errScreenshotOutputTooLarge = errors.New("encoded screenshot exceeds the maximum allowed size")

// ScreenshotRegion is a rectangular crop in source-image pixels. It is
// shared by the MCP screenshot/read-text tools and the CLI read-text adapter
// so every surface uses the same validation and crop-before-scale behavior.
type ScreenshotRegion struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Keep the existing internal name at the MCP argument boundary while making
// the geometry type available to jetkvmctl without duplicating it.
type screenshotRegionArgs = ScreenshotRegion

type screenshotArgs struct {
	Format  *string               `json:"format,omitempty"`
	Quality *int                  `json:"quality,omitempty"`
	Scale   *float64              `json:"scale,omitempty"`
	Region  *screenshotRegionArgs `json:"region,omitempty"`
}

// screenshotOptions is the normalized, fully validated form of the MCP
// arguments. A scale larger than one is normalized to one so rendering can
// never enlarge the captured frame.
type screenshotOptions struct {
	Format  string
	Quality int
	Scale   float64
	Region  *screenshotRegionArgs
}

// ScreenshotTransformOptions selects the geometry applied to a captured
// frame before OCR. Scale is clamped to one, Region is validated against the
// fresh source frame, and cropping happens before scaling.
type ScreenshotTransformOptions struct {
	Scale  *float64
	Region *ScreenshotRegion
}

// ScreenshotTransformResult is an in-memory rendered frame. Read-text uses
// PNG, while the additional format fields keep this type useful to the
// existing screenshot response path too.
type ScreenshotTransformResult struct {
	Data     []byte
	Width    int
	Height   int
	Format   string
	MIMEType string
	Quality  int
}

type renderedScreenshot = ScreenshotTransformResult

// boundedScreenshotBuffer keeps transformed image output within the same
// encoded-byte ceiling as the capture pipeline. Checking ctx on every write
// also lets PNG/JPEG encoders stop at their next output chunk when a request
// deadline expires.
type boundedScreenshotBuffer struct {
	data  []byte
	ctx   context.Context
	limit int
}

func newBoundedScreenshotBuffer(ctx context.Context, limit int) *boundedScreenshotBuffer {
	return &boundedScreenshotBuffer{ctx: ctx, limit: limit}
}

func (b *boundedScreenshotBuffer) Write(p []byte) (int, error) {
	if err := screenshotTransformContextError(b.ctx); err != nil {
		return 0, err
	}
	if b.limit < len(b.data) || len(p) > b.limit-len(b.data) {
		return 0, fmt.Errorf("%w (%d-byte limit)", errScreenshotOutputTooLarge, b.limit)
	}
	required := len(b.data) + len(p)
	if required > cap(b.data) {
		capacity := max(required, cap(b.data)*2)
		capacity = min(capacity, b.limit)
		grown := make([]byte, len(b.data), capacity)
		copy(grown, b.data)
		b.data = grown
	}
	start := len(b.data)
	b.data = b.data[:required]
	copy(b.data[start:], p)
	return len(p), nil
}

func (b *boundedScreenshotBuffer) Bytes() []byte {
	return b.data
}

// RenderScreenshotForText applies the screenshot tool's exact geometry
// contract and returns an in-memory PNG suitable for an OCR engine. Keeping
// this as a thin adapter over normalizeScreenshotOptions and renderScreenshot
// prevents the CLI and MCP read-text surfaces from drifting apart.
func RenderScreenshotForText(ctx context.Context, shot jetkvm.Screenshot, opts ScreenshotTransformOptions) (ScreenshotTransformResult, error) {
	normalized, err := normalizeScreenshotOptions(screenshotArgs{
		Scale:  opts.Scale,
		Region: opts.Region,
	})
	if err != nil {
		return ScreenshotTransformResult{}, err
	}
	return renderScreenshot(ctx, shot, normalized)
}

func normalizeScreenshotOptions(args screenshotArgs) (screenshotOptions, error) {
	opts := screenshotOptions{
		Format: screenshotFormatPNG,
		Scale:  1,
	}

	if args.Format != nil {
		opts.Format = *args.Format
	}
	if opts.Format != screenshotFormatPNG && opts.Format != screenshotFormatJPEG {
		// Do not quote the caller-provided value: tool errors must not reflect
		// arbitrary argument strings that could contain secrets.
		return screenshotOptions{}, errors.New("format must be png or jpeg")
	}

	if args.Quality != nil && opts.Format != screenshotFormatJPEG {
		return screenshotOptions{}, errors.New("quality is only valid when format is jpeg")
	}
	if opts.Format == screenshotFormatJPEG {
		opts.Quality = defaultScreenshotJPEGQuality
		if args.Quality != nil {
			if *args.Quality < 1 || *args.Quality > 100 {
				return screenshotOptions{}, errors.New("quality must be between 1 and 100")
			}
			opts.Quality = *args.Quality
		}
	}

	if args.Scale != nil {
		if math.IsNaN(*args.Scale) || math.IsInf(*args.Scale, 0) || *args.Scale <= 0 {
			return screenshotOptions{}, errors.New("scale must be a positive finite number")
		}
		opts.Scale = min(*args.Scale, 1)
	}

	if args.Region != nil {
		if args.Region.X < 0 || args.Region.Y < 0 {
			return screenshotOptions{}, errors.New("region x and y must be non-negative")
		}
		if args.Region.X > maxScreenshotRegionValue || args.Region.Y > maxScreenshotRegionValue {
			return screenshotOptions{}, errors.New("region x and y exceed the supported range")
		}
		if args.Region.Width <= 0 || args.Region.Height <= 0 {
			return screenshotOptions{}, errors.New("region width and height must be positive")
		}
		if args.Region.Width > maxScreenshotRegionValue || args.Region.Height > maxScreenshotRegionValue {
			return screenshotOptions{}, errors.New("region width and height exceed the supported range")
		}
		region := *args.Region
		opts.Region = &region
	}

	return opts, nil
}

func renderScreenshot(ctx context.Context, shot jetkvm.Screenshot, opts screenshotOptions) (renderedScreenshot, error) {
	if err := screenshotTransformContextError(ctx); err != nil {
		return renderedScreenshot{}, err
	}
	if err := jetkvm.ValidateScreenshotDimensions(shot.Width, shot.Height); err != nil {
		return renderedScreenshot{}, fmt.Errorf("captured screenshot dimensions: %w", err)
	}
	if len(shot.PNG) > jetkvm.MaxScreenshotEncodedBytes {
		return renderedScreenshot{}, fmt.Errorf(
			"captured screenshot PNG exceeds the %d-byte limit",
			jetkvm.MaxScreenshotEncodedBytes,
		)
	}
	if err := validateScreenshotOptions(opts); err != nil {
		return renderedScreenshot{}, err
	}
	opts.Scale = min(opts.Scale, 1)
	if err := validateScreenshotRegion(opts.Region, shot.Width, shot.Height); err != nil {
		return renderedScreenshot{}, err
	}
	if err := screenshotTransformContextError(ctx); err != nil {
		return renderedScreenshot{}, err
	}

	config, err := png.DecodeConfig(bytes.NewReader(shot.PNG))
	if err != nil {
		return renderedScreenshot{}, fmt.Errorf("decoding captured screenshot PNG configuration: %w", err)
	}
	if err := screenshotTransformContextError(ctx); err != nil {
		return renderedScreenshot{}, err
	}
	if err := jetkvm.ValidateScreenshotDimensions(config.Width, config.Height); err != nil {
		return renderedScreenshot{}, fmt.Errorf("captured screenshot PNG dimensions: %w", err)
	}
	if config.Width != shot.Width || config.Height != shot.Height {
		return renderedScreenshot{}, errors.New("captured screenshot dimensions do not match PNG configuration")
	}

	// Preserve the existing secure default exactly: an unmodified PNG is the
	// byte slice produced by CaptureScreenshot, not a decode/re-encode of it.
	if opts.Format == screenshotFormatPNG && opts.Region == nil && opts.Scale == 1 {
		if err := screenshotTransformContextError(ctx); err != nil {
			return renderedScreenshot{}, err
		}
		return renderedScreenshot{
			Data:     shot.PNG,
			Width:    shot.Width,
			Height:   shot.Height,
			Format:   screenshotFormatPNG,
			MIMEType: screenshotMIMETypePNG,
		}, nil
	}

	if err := screenshotTransformContextError(ctx); err != nil {
		return renderedScreenshot{}, err
	}
	decoded, err := png.Decode(bytes.NewReader(shot.PNG))
	if err != nil {
		return renderedScreenshot{}, fmt.Errorf("decoding captured screenshot PNG: %w", err)
	}
	if err := screenshotTransformContextError(ctx); err != nil {
		return renderedScreenshot{}, err
	}
	if decoded.Bounds().Dx() != shot.Width || decoded.Bounds().Dy() != shot.Height {
		return renderedScreenshot{}, errors.New("captured screenshot dimensions do not match decoded PNG")
	}

	working := decoded
	workingWidth, workingHeight := shot.Width, shot.Height
	if opts.Region != nil {
		if err := screenshotTransformContextError(ctx); err != nil {
			return renderedScreenshot{}, err
		}
		workingWidth, workingHeight = opts.Region.Width, opts.Region.Height
		cropped := image.NewRGBA(image.Rect(0, 0, workingWidth, workingHeight))
		sourcePoint := image.Pt(
			decoded.Bounds().Min.X+opts.Region.X,
			decoded.Bounds().Min.Y+opts.Region.Y,
		)
		draw.Draw(cropped, cropped.Bounds(), decoded, sourcePoint, draw.Src)
		working = cropped
		if err := screenshotTransformContextError(ctx); err != nil {
			return renderedScreenshot{}, err
		}
	}

	width, height := scaledScreenshotDimensions(workingWidth, workingHeight, opts.Scale)
	if width != workingWidth || height != workingHeight {
		if err := screenshotTransformContextError(ctx); err != nil {
			return renderedScreenshot{}, err
		}
		scaled := image.NewRGBA(image.Rect(0, 0, width, height))
		xdraw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), working, working.Bounds(), xdraw.Src, nil)
		working = scaled
		if err := screenshotTransformContextError(ctx); err != nil {
			return renderedScreenshot{}, err
		}
	}

	if err := screenshotTransformContextError(ctx); err != nil {
		return renderedScreenshot{}, err
	}
	encoded := newBoundedScreenshotBuffer(ctx, jetkvm.MaxScreenshotEncodedBytes)
	mimeType := screenshotMIMETypePNG
	switch opts.Format {
	case screenshotFormatPNG:
		err = png.Encode(encoded, working)
	case screenshotFormatJPEG:
		mimeType = screenshotMIMETypeJPEG
		err = jpeg.Encode(encoded, working, &jpeg.Options{Quality: opts.Quality})
	}
	if ctxErr := screenshotTransformContextError(ctx); ctxErr != nil {
		return renderedScreenshot{}, ctxErr
	}
	if err != nil {
		return renderedScreenshot{}, fmt.Errorf("encoding %s screenshot: %w", opts.Format, err)
	}

	return renderedScreenshot{
		Data:     encoded.Bytes(),
		Width:    width,
		Height:   height,
		Format:   opts.Format,
		MIMEType: mimeType,
		Quality:  opts.Quality,
	}, nil
}

func validateScreenshotOptions(opts screenshotOptions) error {
	if opts.Format != screenshotFormatPNG && opts.Format != screenshotFormatJPEG {
		return errors.New("format must be png or jpeg")
	}
	if math.IsNaN(opts.Scale) || math.IsInf(opts.Scale, 0) || opts.Scale <= 0 {
		return errors.New("scale must be a positive finite number")
	}
	if opts.Format == screenshotFormatJPEG {
		if opts.Quality < 1 || opts.Quality > 100 {
			return errors.New("quality must be between 1 and 100")
		}
	} else if opts.Quality != 0 {
		return errors.New("quality is only valid when format is jpeg")
	}
	if opts.Region != nil {
		if opts.Region.X < 0 || opts.Region.Y < 0 {
			return errors.New("region x and y must be non-negative")
		}
		if opts.Region.X > maxScreenshotRegionValue || opts.Region.Y > maxScreenshotRegionValue {
			return errors.New("region x and y exceed the supported range")
		}
		if opts.Region.Width <= 0 || opts.Region.Height <= 0 {
			return errors.New("region width and height must be positive")
		}
		if opts.Region.Width > maxScreenshotRegionValue || opts.Region.Height > maxScreenshotRegionValue {
			return errors.New("region width and height exceed the supported range")
		}
	}
	return nil
}

func validateScreenshotRegion(region *screenshotRegionArgs, sourceWidth, sourceHeight int) error {
	if region == nil {
		return nil
	}
	// Subtraction after the non-negative checks avoids overflowing on
	// attacker-controlled x+width or y+height additions.
	if region.X > sourceWidth || region.Width > sourceWidth-region.X ||
		region.Y > sourceHeight || region.Height > sourceHeight-region.Y {
		return errors.New("region must lie within the captured frame")
	}
	return nil
}

func scaledScreenshotDimensions(width, height int, scale float64) (int, int) {
	scale = min(scale, 1)
	scaledWidth := max(1, int(math.Round(float64(width)*scale)))
	scaledHeight := max(1, int(math.Round(float64(height)*scale)))
	return min(width, scaledWidth), min(height, scaledHeight)
}

func screenshotTransformContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf(
			"%w: %w",
			callTimeoutError("transforming screenshot", "call deadline expired"),
			err,
		)
	}
	return nil
}
