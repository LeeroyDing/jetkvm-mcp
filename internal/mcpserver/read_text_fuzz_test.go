package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"math"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
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

// FuzzReadTextToolArgumentValidation drives the complete MCP argument path
// for the read-only OCR tool. It distinguishes transport-unrepresentable
// floats, strict-schema failures, source-frame crop failures, and successful
// crop-then-scale transforms while requiring rejected calls to stop before
// the corresponding OCR or capture side effect.
func FuzzReadTextToolArgumentValidation(f *testing.F) {
	shot, _ := makeScreenshotFixture(f, 12, 8)
	add := func(
		scale float64,
		includeScale, includeRegion bool,
		x, y, width, height int,
		regionMask uint8,
		extra bool,
	) {
		f.Add(
			math.Float64bits(scale), includeScale, includeRegion,
			x, y, width, height, regionMask, extra,
		)
	}
	add(0, false, false, 0, 0, 0, 0, 0, false)
	add(0.5, true, true, 2, 1, 8, 6, 0b1111, false)
	add(2.5, true, false, 0, 0, 0, 0, 0, false)
	add(math.SmallestNonzeroFloat64, true, false, 0, 0, 0, 0, 0, false)
	add(0, true, false, 0, 0, 0, 0, 0, false)
	add(-1, true, false, 0, 0, 0, 0, 0, false)
	add(math.NaN(), true, false, 0, 0, 0, 0, 0, false)
	add(math.Inf(1), true, false, 0, 0, 0, 0, 0, false)
	add(1, false, true, 2, 1, 8, 6, 0b1110, false)
	add(1, false, true, -1, 0, 1, 1, 0b1111, false)
	add(1, false, true, 11, 7, 2, 1, 0b1111, false)
	add(1, false, true, maxScreenshotRegionValue, 0, 1, 1, 0b1111, false)
	add(1, false, false, math.MinInt, math.MaxInt, math.MaxInt, math.MinInt, 0, false)
	add(1, false, false, 0, 0, 0, 0, 0, true)

	f.Fuzz(func(
		t *testing.T,
		scaleBits uint64,
		includeScale, includeRegion bool,
		x, y, width, height int,
		regionMask uint8,
		extra bool,
	) {
		scale := math.Float64frombits(scaleBits)
		args := map[string]any{}
		if includeScale {
			args["scale"] = scale
		}
		if includeRegion {
			region := map[string]any{}
			for _, field := range []struct {
				name  string
				bit   uint8
				value int
			}{
				{name: "x", bit: 1 << 0, value: x},
				{name: "y", bit: 1 << 1, value: y},
				{name: "width", bit: 1 << 2, value: width},
				{name: "height", bit: 1 << 3, value: height},
			} {
				if regionMask&field.bit != 0 {
					region[field.name] = field.value
				}
			}
			args["region"] = region
		}
		if extra {
			args["unexpected"] = true
		}

		captures := 0
		device := &mockDevice{screenshotFunc: func(ctx context.Context) (jetkvm.Screenshot, error) {
			captures++
			if err := ctx.Err(); err != nil {
				return jetkvm.Screenshot{}, err
			}
			return shot, nil
		}}
		engine := &recordingOCREngine{output: "fuzz OCR text"}
		cs := newReadTextToolTestSession(t, device, engine, false)
		assertCalls := func(wantCaptures, wantChecks, wantReads int) {
			t.Helper()
			if captures != wantCaptures || engine.checkCalls != wantChecks || engine.readCalls != wantReads {
				t.Fatalf(
					"capture/check/read calls = %d/%d/%d, want %d/%d/%d",
					captures, engine.checkCalls, engine.readCalls,
					wantCaptures, wantChecks, wantReads,
				)
			}
		}

		res, err := callReadTextTool(t, cs, args)
		if includeScale && (math.IsNaN(scale) || math.IsInf(scale, 0)) {
			// encoding/json cannot put these values on the MCP transport. They
			// must fail locally without reaching schema validation or tool work.
			if err == nil {
				t.Fatalf("read-text transported non-finite scale %v: result=%+v", scale, res)
			}
			assertCalls(0, 0, 0)
			return
		}

		schemaValid := !extra && (!includeScale || scale > 0)
		if includeRegion {
			schemaValid = schemaValid && regionMask&0b1111 == 0b1111 &&
				x >= 0 && x <= maxScreenshotRegionValue &&
				y >= 0 && y <= maxScreenshotRegionValue &&
				width >= 1 && width <= maxScreenshotRegionValue &&
				height >= 1 && height <= maxScreenshotRegionValue
		}
		if !schemaValid {
			if err == nil {
				t.Fatalf("read-text accepted schema-invalid arguments %v", args)
			}
			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("read-text rejection for %v = %v, want JSON-RPC InvalidParams", args, err)
			}
			assertCalls(0, 0, 0)
			return
		}

		frameValid := !includeRegion ||
			(x <= shot.Width && width <= shot.Width-x &&
				y <= shot.Height && height <= shot.Height-y)
		if !frameValid {
			if err != nil {
				t.Fatalf("frame-invalid read-text arguments returned protocol error: %v", err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("frame-invalid read-text arguments returned success: %+v", res)
			}
			assertCalls(1, 1, 0)
			return
		}

		if err != nil {
			t.Fatalf("read-text rejected valid arguments %v: %v", args, err)
		}
		if res == nil || res.IsError {
			t.Fatalf("read-text returned a tool error for valid arguments %v: %+v", args, res)
		}
		assertCalls(1, 1, 1)
		if got := toolResultTextTB(t, res); got != engine.output {
			t.Fatalf("read-text output = %q, want exact OCR output %q", got, engine.output)
		}
		if len(engine.images) != 1 {
			t.Fatalf("OCR image count = %d, want 1", len(engine.images))
		}

		workingWidth, workingHeight := shot.Width, shot.Height
		if includeRegion {
			workingWidth, workingHeight = width, height
		}
		wantScale := 1.0
		if includeScale {
			wantScale = min(scale, 1)
		}
		wantWidth := max(1, int(math.Round(float64(workingWidth)*wantScale)))
		wantHeight := max(1, int(math.Round(float64(workingHeight)*wantScale)))
		wantWidth = min(workingWidth, wantWidth)
		wantHeight = min(workingHeight, wantHeight)

		config, format, decodeErr := image.DecodeConfig(bytes.NewReader(engine.images[0]))
		if decodeErr != nil {
			t.Fatalf("decoding OCR input: %v", decodeErr)
		}
		if format != "png" || config.Width != wantWidth || config.Height != wantHeight {
			t.Fatalf("OCR input = %s %dx%d, want png %dx%d", format, config.Width, config.Height, wantWidth, wantHeight)
		}
		if config.Width < 1 || config.Height < 1 || config.Width > workingWidth || config.Height > workingHeight {
			t.Fatalf("OCR input dimensions %dx%d escape no-upscale bounds for %dx%d source", config.Width, config.Height, workingWidth, workingHeight)
		}
		if !includeRegion && wantScale == 1 && !bytes.Equal(engine.images[0], shot.PNG) {
			t.Fatal("unmodified read-text path did not preserve captured PNG bytes")
		}

		rawMeta, marshalErr := json.Marshal(res.StructuredContent)
		if marshalErr != nil {
			t.Fatalf("marshalling read-text metadata: %v", marshalErr)
		}
		var meta struct {
			Width        int `json:"width"`
			Height       int `json:"height"`
			SourceWidth  int `json:"sourceWidth"`
			SourceHeight int `json:"sourceHeight"`
		}
		if unmarshalErr := json.Unmarshal(rawMeta, &meta); unmarshalErr != nil {
			t.Fatalf("decoding read-text metadata: %v", unmarshalErr)
		}
		if meta.Width != wantWidth || meta.Height != wantHeight ||
			meta.SourceWidth != shot.Width || meta.SourceHeight != shot.Height {
			t.Fatalf("read-text metadata = %+v, want output %dx%d source %dx%d", meta, wantWidth, wantHeight, shot.Width, shot.Height)
		}
	})
}
