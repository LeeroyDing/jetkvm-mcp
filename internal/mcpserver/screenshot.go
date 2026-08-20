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

type screenshotRegionArgs struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

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

type renderedScreenshot struct {
	Data     []byte
	Width    int
	Height   int
	Format   string
	MIMEType string
	Quality  int
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
	if shot.Width <= 0 || shot.Height <= 0 {
		return renderedScreenshot{}, errors.New("captured screenshot has invalid dimensions")
	}
	if err := validateScreenshotOptions(opts); err != nil {
		return renderedScreenshot{}, err
	}
	opts.Scale = min(opts.Scale, 1)
	if err := validateScreenshotRegion(opts.Region, shot.Width, shot.Height); err != nil {
		return renderedScreenshot{}, err
	}

	// Preserve the existing secure default exactly: an unmodified PNG is the
	// byte slice produced by CaptureScreenshot, not a decode/re-encode of it.
	if opts.Format == screenshotFormatPNG && opts.Region == nil && opts.Scale == 1 {
		return renderedScreenshot{
			Data:     shot.PNG,
			Width:    shot.Width,
			Height:   shot.Height,
			Format:   screenshotFormatPNG,
			MIMEType: screenshotMIMETypePNG,
		}, nil
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
		scaled := image.NewRGBA(image.Rect(0, 0, width, height))
		xdraw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), working, working.Bounds(), xdraw.Src, nil)
		working = scaled
		if err := screenshotTransformContextError(ctx); err != nil {
			return renderedScreenshot{}, err
		}
	}

	var encoded bytes.Buffer
	mimeType := screenshotMIMETypePNG
	switch opts.Format {
	case screenshotFormatPNG:
		err = png.Encode(&encoded, working)
	case screenshotFormatJPEG:
		mimeType = screenshotMIMETypeJPEG
		err = jpeg.Encode(&encoded, working, &jpeg.Options{Quality: opts.Quality})
	}
	if err != nil {
		return renderedScreenshot{}, fmt.Errorf("encoding %s screenshot: %w", opts.Format, err)
	}
	if err := screenshotTransformContextError(ctx); err != nil {
		return renderedScreenshot{}, err
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
		return fmt.Errorf("screenshot transform canceled: %w", err)
	}
	return nil
}
