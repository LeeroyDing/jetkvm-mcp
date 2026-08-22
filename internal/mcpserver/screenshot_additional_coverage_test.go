package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"math"
	"strings"
	"testing"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func requireScreenshotTimeoutWithoutData(t testing.TB, rendered renderedScreenshot, err error) {
	t.Helper()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("render error = %v, want context.Canceled", err)
	}
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindTimeout {
		t.Fatalf("render error kind = %q, want timeout: %v", kind, err)
	}
	if len(rendered.Data) != 0 {
		t.Fatalf("canceled render returned %d image bytes, want none", len(rendered.Data))
	}
}

func TestRenderScreenshotCancellationCheckpoints(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 12, 8)
	defaultOptions := screenshotOptions{
		Format: screenshotFormatPNG,
		Scale:  1,
	}
	cropAndScaleOptions := screenshotOptions{
		Format: screenshotFormatPNG,
		Scale:  0.5,
		Region: &screenshotRegionArgs{X: 2, Y: 1, Width: 8, Height: 6},
	}

	tests := []struct {
		name     string
		cancelAt int
		options  screenshotOptions
	}{
		{name: "before captured metadata validation", cancelAt: 1, options: defaultOptions},
		{name: "before PNG configuration decode", cancelAt: 2, options: defaultOptions},
		{name: "after PNG configuration decode", cancelAt: 3, options: defaultOptions},
		{name: "before full PNG decode", cancelAt: 4, options: cropAndScaleOptions},
		{name: "after full PNG decode", cancelAt: 5, options: cropAndScaleOptions},
		{name: "before crop", cancelAt: 6, options: cropAndScaleOptions},
		{name: "after crop", cancelAt: 7, options: cropAndScaleOptions},
		{name: "before scale", cancelAt: 8, options: cropAndScaleOptions},
		{name: "after scale", cancelAt: 9, options: cropAndScaleOptions},
		{name: "before encode", cancelAt: 10, options: cropAndScaleOptions},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &cancelOnScreenshotCheckContext{
				Context:  context.Background(),
				cancelAt: tc.cancelAt,
			}
			rendered, err := renderScreenshot(ctx, shot, tc.options)
			requireScreenshotTimeoutWithoutData(t, rendered, err)
			if ctx.checks != tc.cancelAt {
				t.Fatalf("context checks = %d, want cancellation at check %d", ctx.checks, tc.cancelAt)
			}
		})
	}
}

func TestValidateScreenshotOptionsRejectsInvalidNormalizedStates(t *testing.T) {
	tests := []struct {
		name    string
		options screenshotOptions
		want    string
	}{
		{
			name:    "unknown format",
			options: screenshotOptions{Format: "gif", Scale: 1},
			want:    "format must be png or jpeg",
		},
		{
			name:    "zero scale",
			options: screenshotOptions{Format: screenshotFormatPNG, Scale: 0},
			want:    "scale must be a positive finite number",
		},
		{
			name:    "NaN scale",
			options: screenshotOptions{Format: screenshotFormatPNG, Scale: math.NaN()},
			want:    "scale must be a positive finite number",
		},
		{
			name:    "infinite scale",
			options: screenshotOptions{Format: screenshotFormatPNG, Scale: math.Inf(1)},
			want:    "scale must be a positive finite number",
		},
		{
			name:    "JPEG quality below range",
			options: screenshotOptions{Format: screenshotFormatJPEG, Quality: 0, Scale: 1},
			want:    "quality must be between 1 and 100",
		},
		{
			name:    "JPEG quality above range",
			options: screenshotOptions{Format: screenshotFormatJPEG, Quality: 101, Scale: 1},
			want:    "quality must be between 1 and 100",
		},
		{
			name:    "PNG quality",
			options: screenshotOptions{Format: screenshotFormatPNG, Quality: 80, Scale: 1},
			want:    "quality is only valid when format is jpeg",
		},
		{
			name: "negative region origin",
			options: screenshotOptions{
				Format: screenshotFormatPNG,
				Scale:  1,
				Region: &screenshotRegionArgs{X: -1, Y: 0, Width: 1, Height: 1},
			},
			want: "region x and y must be non-negative",
		},
		{
			name: "region origin above supported range",
			options: screenshotOptions{
				Format: screenshotFormatPNG,
				Scale:  1,
				Region: &screenshotRegionArgs{X: maxScreenshotRegionValue + 1, Y: 0, Width: 1, Height: 1},
			},
			want: "region x and y exceed the supported range",
		},
		{
			name: "nonpositive region size",
			options: screenshotOptions{
				Format: screenshotFormatPNG,
				Scale:  1,
				Region: &screenshotRegionArgs{X: 0, Y: 0, Width: 0, Height: 1},
			},
			want: "region width and height must be positive",
		},
		{
			name: "region size above supported range",
			options: screenshotOptions{
				Format: screenshotFormatPNG,
				Scale:  1,
				Region: &screenshotRegionArgs{X: 0, Y: 0, Width: maxScreenshotRegionValue + 1, Height: 1},
			},
			want: "region width and height exceed the supported range",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateScreenshotOptions(tc.options)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("validation error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRenderScreenshotRejectsInvalidNormalizedOptions(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 4, 3)
	rendered, err := renderScreenshot(context.Background(), shot, screenshotOptions{
		Format: "gif",
		Scale:  1,
	})
	if err == nil || err.Error() != "format must be png or jpeg" {
		t.Fatalf("render error = %v, want invalid format rejection", err)
	}
	if len(rendered.Data) != 0 {
		t.Fatalf("invalid render returned %d image bytes, want none", len(rendered.Data))
	}
}

func TestRenderScreenshotRejectsPNGThatFailsFullDecode(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 4, 3)
	shot.PNG = append([]byte(nil), shot.PNG[:33]...)
	if _, err := png.DecodeConfig(bytes.NewReader(shot.PNG)); err != nil {
		t.Fatalf("truncated fixture must retain a valid PNG configuration: %v", err)
	}

	rendered, err := renderScreenshot(context.Background(), shot, screenshotOptions{
		Format:  screenshotFormatJPEG,
		Quality: defaultScreenshotJPEGQuality,
		Scale:   1,
	})
	if err == nil || !strings.Contains(err.Error(), "decoding captured screenshot PNG:") {
		t.Fatalf("render error = %v, want full PNG decode failure", err)
	}
	if strings.Contains(err.Error(), "configuration") {
		t.Fatalf("full PNG decode failure was misclassified as a configuration failure: %v", err)
	}
	if len(rendered.Data) != 0 {
		t.Fatalf("failed decode returned %d image bytes, want none", len(rendered.Data))
	}
}

func TestRenderScreenshotChecksContextAfterSuccessfulEncode(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 12, 8)
	options := screenshotOptions{
		Format: screenshotFormatPNG,
		Scale:  0.5,
		Region: &screenshotRegionArgs{X: 2, Y: 1, Width: 8, Height: 6},
	}

	countingCtx := &cancelOnScreenshotCheckContext{
		Context:  context.Background(),
		cancelAt: math.MaxInt,
	}
	probe, err := renderScreenshot(countingCtx, shot, options)
	if err != nil || len(probe.Data) == 0 {
		t.Fatalf("probe render = %d bytes, %v; want successful encoded image", len(probe.Data), err)
	}
	if countingCtx.checks <= 10 {
		t.Fatalf("probe context checks = %d, want encoder writes plus final check", countingCtx.checks)
	}

	cancelAtFinalCheck := &cancelOnScreenshotCheckContext{
		Context:  context.Background(),
		cancelAt: countingCtx.checks,
	}
	rendered, err := renderScreenshot(cancelAtFinalCheck, shot, options)
	requireScreenshotTimeoutWithoutData(t, rendered, err)
	if cancelAtFinalCheck.checks != countingCtx.checks {
		t.Fatalf(
			"canceled render made %d context checks, want final post-encode check %d",
			cancelAtFinalCheck.checks,
			countingCtx.checks,
		)
	}
}
