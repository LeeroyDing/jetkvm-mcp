package mcpserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"io/fs"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

var screenshotFixtureCapturedAt = time.Date(2026, time.August, 20, 12, 34, 56, 789, time.UTC)

type cancelOnScreenshotCheckContext struct {
	context.Context
	cancelAt int
	checks   int
}

func (c *cancelOnScreenshotCheckContext) Err() error {
	c.checks++
	if c.checks >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

type screenshotToolMetadata struct {
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Format       string `json:"format"`
	MIMEType     string `json:"mimeType"`
	Quality      *int   `json:"quality"`
	SourceWidth  *int   `json:"sourceWidth"`
	SourceHeight *int   `json:"sourceHeight"`
	CapturedAt   string `json:"capturedAt"`
	Fresh        bool   `json:"fresh"`
}

func makeScreenshotFixture(t testing.TB, width, height int) (jetkvm.Screenshot, *image.NRGBA) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(17*x + 3*y),
				G: uint8(5*x + 29*y),
				B: uint8(41*x + 7*y),
				A: 0xff,
			})
		}
	}

	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("encoding screenshot fixture: %v", err)
	}
	return jetkvm.Screenshot{
		ScreenshotResult: jetkvm.ScreenshotResult{
			Width:      width,
			Height:     height,
			CapturedAt: screenshotFixtureCapturedAt,
			Fresh:      true,
		},
		PNG: encoded.Bytes(),
	}, img
}

// screenshotPNGWithDimensions rewrites only the IHDR dimensions and checksum.
// DecodeConfig therefore sees attacker-selected dimensions without the test
// allocating a corresponding pixel buffer or constructing a valid IDAT body.
func screenshotPNGWithDimensions(t testing.TB, source []byte, width, height uint32) []byte {
	t.Helper()
	if len(source) < 33 || !bytes.Equal(source[:8], []byte("\x89PNG\r\n\x1a\n")) ||
		!bytes.Equal(source[12:16], []byte("IHDR")) {
		t.Fatal("screenshot fixture does not start with a PNG IHDR chunk")
	}
	result := append([]byte(nil), source...)
	binary.BigEndian.PutUint32(result[16:20], width)
	binary.BigEndian.PutUint32(result[20:24], height)
	binary.BigEndian.PutUint32(result[29:33], crc32.ChecksumIEEE(result[12:29]))
	return result
}

func newScreenshotToolTestSession(t *testing.T, shot jetkvm.Screenshot) (*mcp.ClientSession, *int) {
	t.Helper()
	calls := 0
	device := &mockDevice{screenshotFunc: func(ctx context.Context) (jetkvm.Screenshot, error) {
		calls++
		if err := ctx.Err(); err != nil {
			return jetkvm.Screenshot{}, err
		}
		return shot, nil
	}}
	return newTestServerSessionForDevice(t, device, false), &calls
}

func callScreenshotTool(t *testing.T, cs *mcp.ClientSession, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	params := &mcp.CallToolParams{Name: "jetkvm_screenshot"}
	if args != nil {
		params.Arguments = args
	}
	return cs.CallTool(context.Background(), params)
}

func screenshotImageContent(t testing.TB, result *mcp.CallToolResult) *mcp.ImageContent {
	t.Helper()
	for _, content := range result.Content {
		if img, ok := content.(*mcp.ImageContent); ok {
			return img
		}
	}
	t.Fatal("screenshot result carried no image content")
	return nil
}

func screenshotMetadata(t testing.TB, result *mcp.CallToolResult) screenshotToolMetadata {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshalling screenshot metadata: %v", err)
	}
	if bytes.Equal(raw, []byte("null")) {
		t.Fatal("screenshot result carried no structured metadata")
	}
	var metadata screenshotToolMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("decoding screenshot metadata: %v", err)
	}
	return metadata
}

func requireScreenshotSuccess(t testing.TB, result *mcp.CallToolResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("screenshot returned an error result: %s", toolResultTextTB(t, result))
	}
}

func toolResultTextTB(t testing.TB, result *mcp.CallToolResult) string {
	t.Helper()
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			return text.Text
		}
	}
	t.Fatal("tool result carried no text content")
	return ""
}

func assertScreenshotMetadata(t testing.TB, result *mcp.CallToolResult, width, height int, format, mimeType string) screenshotToolMetadata {
	t.Helper()
	metadata := screenshotMetadata(t, result)
	if metadata.Width != width || metadata.Height != height {
		t.Errorf("metadata dimensions = %dx%d, want %dx%d", metadata.Width, metadata.Height, width, height)
	}
	if metadata.Format != format {
		t.Errorf("metadata format = %q, want %q", metadata.Format, format)
	}
	if metadata.MIMEType != mimeType {
		t.Errorf("metadata MIME type = %q, want %q", metadata.MIMEType, mimeType)
	}
	if metadata.CapturedAt != screenshotFixtureCapturedAt.Format(time.RFC3339Nano) {
		t.Errorf("metadata capturedAt = %q, want %q", metadata.CapturedAt, screenshotFixtureCapturedAt.Format(time.RFC3339Nano))
	}
	if !metadata.Fresh {
		t.Error("metadata fresh = false, want request-fresh capture metadata preserved")
	}

	text := toolResultTextTB(t, result)
	for _, want := range []string{
		"width=" + jsonInt(width),
		"height=" + jsonInt(height),
		"format=" + format,
		"mimeType=" + mimeType,
		"capturedAt=" + screenshotFixtureCapturedAt.Format(time.RFC3339Nano),
		"fresh=true",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("screenshot summary %q does not contain %q", text, want)
		}
	}
	return metadata
}

func jsonInt(value int) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func TestScreenshotToolDefaultPreservesPNG(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 12, 8)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{name: "omitted arguments", args: nil},
		{name: "empty object", args: map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, calls := newScreenshotToolTestSession(t, shot)
			result, err := callScreenshotTool(t, cs, tc.args)
			requireScreenshotSuccess(t, result, err)
			if *calls != 1 {
				t.Fatalf("capture calls = %d, want exactly 1", *calls)
			}

			content := screenshotImageContent(t, result)
			if content.MIMEType != "image/png" {
				t.Errorf("image MIME type = %q, want image/png", content.MIMEType)
			}
			if !bytes.Equal(content.Data, shot.PNG) {
				t.Error("default screenshot did not preserve the captured PNG bytes exactly")
			}
			metadata := assertScreenshotMetadata(t, result, 12, 8, "png", "image/png")
			if metadata.SourceWidth == nil || *metadata.SourceWidth != shot.Width || metadata.SourceHeight == nil || *metadata.SourceHeight != shot.Height {
				t.Errorf("source metadata = %v x %v, want %d x %d", metadata.SourceWidth, metadata.SourceHeight, shot.Width, shot.Height)
			}
			if metadata.Quality != nil {
				t.Errorf("default PNG metadata unexpectedly reported JPEG quality %d", *metadata.Quality)
			}
		})
	}
}

func TestRenderScreenshotRejectsUnsafeCapturedDimensionsBeforePNGParsing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		width  int
		height int
	}{
		{name: "axis limit", width: 8193, height: 1},
		{name: "total pixel limit", width: 4097, height: 4097},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shot := jetkvm.Screenshot{
				ScreenshotResult: jetkvm.ScreenshotResult{Width: tc.width, Height: tc.height},
				PNG:              []byte("not a PNG"),
			}
			_, err := renderScreenshot(context.Background(), shot, screenshotOptions{
				Format: screenshotFormatPNG,
				Scale:  1,
			})
			if err == nil {
				t.Fatal("render accepted unsafe captured dimensions")
			}
			if !strings.Contains(err.Error(), "captured screenshot dimensions") {
				t.Fatalf("unsafe-dimension error = %v, want captured metadata identified", err)
			}
			if strings.Contains(err.Error(), "configuration") {
				t.Fatalf("unsafe metadata reached PNG configuration parsing: %v", err)
			}
		})
	}
}

func TestRenderScreenshotValidatesPNGConfigBeforeDecode(t *testing.T) {
	fixture, _ := makeScreenshotFixture(t, 1, 1)
	tests := []struct {
		name      string
		metadataW int
		metadataH int
		configW   uint32
		configH   uint32
		want      string
	}{
		{
			name: "axis limit", metadataW: 1, metadataH: 1,
			configW: 8193, configH: 1, want: "captured screenshot PNG dimensions",
		},
		{
			name: "total pixel limit", metadataW: 1, metadataH: 1,
			configW: 4097, configH: 4097, want: "captured screenshot PNG dimensions",
		},
		{
			name: "metadata mismatch", metadataW: 2, metadataH: 1,
			configW: 1, configH: 1, want: "do not match PNG configuration",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shot := jetkvm.Screenshot{
				ScreenshotResult: jetkvm.ScreenshotResult{Width: tc.metadataW, Height: tc.metadataH},
				PNG:              screenshotPNGWithDimensions(t, fixture.PNG, tc.configW, tc.configH),
			}
			_, err := renderScreenshot(context.Background(), shot, screenshotOptions{
				Format: screenshotFormatPNG,
				Scale:  1,
			})
			if err == nil {
				t.Fatal("render accepted unsafe or mismatched PNG configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PNG configuration error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRenderScreenshotDefaultChecksContextAfterPNGConfig(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 12, 8)
	ctx := &cancelOnScreenshotCheckContext{
		Context:  context.Background(),
		cancelAt: 4,
	}

	rendered, err := renderScreenshot(ctx, shot, screenshotOptions{
		Format: screenshotFormatPNG,
		Scale:  1,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("default render error = %v, want context.Canceled after configuration parsing", err)
	}
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindTimeout {
		t.Fatalf("default render error kind = %q, want timeout: %v", kind, err)
	}
	if len(rendered.Data) != 0 {
		t.Fatalf("canceled default render returned %d image bytes", len(rendered.Data))
	}
}

func TestBoundedScreenshotBufferRejectsOverflow(t *testing.T) {
	encoded := newBoundedScreenshotBuffer(context.Background(), 4)
	if n, err := encoded.Write([]byte("1234")); err != nil || n != 4 {
		t.Fatalf("bounded write = %d, %v; want 4, nil", n, err)
	}
	if n, err := encoded.Write([]byte("5")); n != 0 || !errors.Is(err, errScreenshotOutputTooLarge) {
		t.Fatalf("overflow write = %d, %v; want 0 and errScreenshotOutputTooLarge", n, err)
	}
	if got := string(encoded.Bytes()); got != "1234" {
		t.Fatalf("overflow changed buffered output to %q, want %q", got, "1234")
	}
}

func TestBoundedScreenshotBufferHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	encoded := newBoundedScreenshotBuffer(ctx, 4)
	if n, err := encoded.Write([]byte("1")); n != 0 || !errors.Is(err, context.Canceled) || jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindTimeout {
		t.Fatalf("canceled write = %d, %v; want 0 and context.Canceled", n, err)
	}
}

func TestScreenshotToolOutputOptions(t *testing.T) {
	shot, source := makeScreenshotFixture(t, 12, 8)
	tests := []struct {
		name         string
		args         map[string]any
		wantWidth    int
		wantHeight   int
		wantFormat   string
		wantMIME     string
		wantExactPNG bool
		wantQuality  int
		checkPixels  bool
	}{
		{
			name: "explicit png", args: map[string]any{"format": "png"},
			wantWidth: 12, wantHeight: 8, wantFormat: "png", wantMIME: "image/png", wantExactPNG: true,
		},
		{
			name: "jpeg default quality", args: map[string]any{"format": "jpeg"},
			wantWidth: 12, wantHeight: 8, wantFormat: "jpeg", wantMIME: "image/jpeg", wantQuality: 80,
		},
		{
			name: "jpeg minimum quality", args: map[string]any{"format": "jpeg", "quality": 1},
			wantWidth: 12, wantHeight: 8, wantFormat: "jpeg", wantMIME: "image/jpeg", wantQuality: 1,
		},
		{
			name: "jpeg maximum quality", args: map[string]any{"format": "jpeg", "quality": 100},
			wantWidth: 12, wantHeight: 8, wantFormat: "jpeg", wantMIME: "image/jpeg", wantQuality: 100,
		},
		{
			name: "scale only", args: map[string]any{"scale": 0.5},
			wantWidth: 6, wantHeight: 4, wantFormat: "png", wantMIME: "image/png",
		},
		{
			name: "crop only", args: map[string]any{"region": map[string]any{"x": 2, "y": 1, "width": 8, "height": 6}},
			wantWidth: 8, wantHeight: 6, wantFormat: "png", wantMIME: "image/png", checkPixels: true,
		},
		{
			name: "crop before scale", args: map[string]any{
				"scale":  0.5,
				"region": map[string]any{"x": 2, "y": 1, "width": 8, "height": 6},
			},
			wantWidth: 4, wantHeight: 3, wantFormat: "png", wantMIME: "image/png",
		},
		{
			name: "aspect ratio", args: map[string]any{"scale": 0.25},
			wantWidth: 3, wantHeight: 2, wantFormat: "png", wantMIME: "image/png",
		},
		{
			name: "larger scale clamps without upscaling", args: map[string]any{"scale": 2.5},
			wantWidth: 12, wantHeight: 8, wantFormat: "png", wantMIME: "image/png",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs, calls := newScreenshotToolTestSession(t, shot)
			result, err := callScreenshotTool(t, cs, tc.args)
			requireScreenshotSuccess(t, result, err)
			if *calls != 1 {
				t.Fatalf("capture calls = %d, want exactly 1", *calls)
			}

			content := screenshotImageContent(t, result)
			if content.MIMEType != tc.wantMIME {
				t.Errorf("image MIME type = %q, want %q", content.MIMEType, tc.wantMIME)
			}
			if tc.wantExactPNG && !bytes.Equal(content.Data, shot.PNG) {
				t.Error("untransformed PNG output did not preserve the source bytes")
			}
			if tc.wantFormat == "png" && !bytes.HasPrefix(content.Data, []byte("\x89PNG\r\n\x1a\n")) {
				t.Error("PNG response does not carry a PNG signature")
			}
			if tc.wantFormat == "jpeg" && !bytes.HasPrefix(content.Data, []byte{0xff, 0xd8, 0xff}) {
				t.Error("JPEG response does not carry a JPEG signature")
			}
			config, format, err := image.DecodeConfig(bytes.NewReader(content.Data))
			if err != nil {
				t.Fatalf("decoding image config: %v", err)
			}
			if format != tc.wantFormat {
				t.Errorf("decoded format = %q, want %q", format, tc.wantFormat)
			}
			if config.Width != tc.wantWidth || config.Height != tc.wantHeight {
				t.Errorf("decoded dimensions = %dx%d, want %dx%d", config.Width, config.Height, tc.wantWidth, tc.wantHeight)
			}
			if tc.name == "aspect ratio" && config.Width*shot.Height != config.Height*shot.Width {
				t.Errorf("scaled aspect ratio %d:%d does not preserve source ratio %d:%d", config.Width, config.Height, shot.Width, shot.Height)
			}

			metadata := assertScreenshotMetadata(t, result, tc.wantWidth, tc.wantHeight, tc.wantFormat, tc.wantMIME)
			if tc.wantQuality != 0 {
				if metadata.Quality == nil || *metadata.Quality != tc.wantQuality {
					t.Errorf("metadata quality = %v, want %d", metadata.Quality, tc.wantQuality)
				}
				if text := toolResultTextTB(t, result); !strings.Contains(text, "quality="+jsonInt(tc.wantQuality)) {
					t.Errorf("JPEG summary %q does not report quality %d", text, tc.wantQuality)
				}
			} else if metadata.Quality != nil {
				t.Errorf("non-JPEG metadata unexpectedly reported quality %d", *metadata.Quality)
			}
			if metadata.SourceWidth == nil || *metadata.SourceWidth != shot.Width || metadata.SourceHeight == nil || *metadata.SourceHeight != shot.Height {
				t.Errorf("source metadata = %v x %v, want %d x %d", metadata.SourceWidth, metadata.SourceHeight, shot.Width, shot.Height)
			}

			if tc.checkPixels {
				got, err := png.Decode(bytes.NewReader(content.Data))
				if err != nil {
					t.Fatalf("decoding cropped PNG: %v", err)
				}
				for _, point := range []struct{ gotX, gotY, sourceX, sourceY int }{
					{0, 0, 2, 1},
					{7, 0, 9, 1},
					{0, 5, 2, 6},
					{7, 5, 9, 6},
				} {
					if gotColor, wantColor := color.NRGBAModel.Convert(got.At(point.gotX, point.gotY)), color.NRGBAModel.Convert(source.At(point.sourceX, point.sourceY)); gotColor != wantColor {
						t.Errorf("cropped pixel (%d,%d) = %v, want source (%d,%d) = %v", point.gotX, point.gotY, gotColor, point.sourceX, point.sourceY, wantColor)
					}
				}
			}
		})
	}
}

func TestScreenshotToolRejectsInvalidOutputOptionsBeforeCapture(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 12, 8)
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "unknown format", args: map[string]any{"format": "jpg"}},
		{name: "format wrong type", args: map[string]any{"format": 1}},
		{name: "quality zero", args: map[string]any{"format": "jpeg", "quality": 0}},
		{name: "quality over maximum", args: map[string]any{"format": "jpeg", "quality": 101}},
		{name: "quality non integer", args: map[string]any{"format": "jpeg", "quality": 80.5}},
		{name: "scale zero", args: map[string]any{"scale": 0}},
		{name: "scale negative", args: map[string]any{"scale": -0.1}},
		{name: "scale wrong type", args: map[string]any{"scale": "half"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs, calls := newScreenshotToolTestSession(t, shot)
			_, err := callScreenshotTool(t, cs, tc.args)
			assertInvalidScreenshotParams(t, err)
			if *calls != 0 {
				t.Errorf("invalid arguments captured %d frames, want zero", *calls)
			}
		})
	}

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{name: "quality without jpeg", args: map[string]any{"quality": 80}},
		{name: "quality with png", args: map[string]any{"format": "png", "quality": 80}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, calls := newScreenshotToolTestSession(t, shot)
			result, err := callScreenshotTool(t, cs, tc.args)
			if err != nil {
				t.Fatalf("CallTool protocol error: %v", err)
			}
			if !result.IsError {
				t.Fatal("invalid format/quality combination was reported as success")
			}
			if text := toolResultTextTB(t, result); !strings.Contains(text, "quality") || !strings.Contains(text, "jpeg") {
				t.Errorf("combination error %q does not clearly name quality and jpeg", text)
			}
			if *calls != 0 {
				t.Errorf("invalid format/quality combination captured %d frames, want zero", *calls)
			}
		})
	}
}

func TestScreenshotToolRejectsInvalidRegions(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 12, 8)
	schemaInvalid := []struct {
		name   string
		region map[string]any
	}{
		{name: "missing x", region: map[string]any{"y": 0, "width": 1, "height": 1}},
		{name: "missing y", region: map[string]any{"x": 0, "width": 1, "height": 1}},
		{name: "missing width", region: map[string]any{"x": 0, "y": 0, "height": 1}},
		{name: "missing height", region: map[string]any{"x": 0, "y": 0, "width": 1}},
		{name: "negative x", region: map[string]any{"x": -1, "y": 0, "width": 1, "height": 1}},
		{name: "negative y", region: map[string]any{"x": 0, "y": -1, "width": 1, "height": 1}},
		{name: "zero width", region: map[string]any{"x": 0, "y": 0, "width": 0, "height": 1}},
		{name: "negative width", region: map[string]any{"x": 0, "y": 0, "width": -1, "height": 1}},
		{name: "zero height", region: map[string]any{"x": 0, "y": 0, "width": 1, "height": 0}},
		{name: "negative height", region: map[string]any{"x": 0, "y": 0, "width": 1, "height": -1}},
		{name: "fractional coordinate", region: map[string]any{"x": 0.5, "y": 0, "width": 1, "height": 1}},
		{name: "x over schema cap", region: map[string]any{"x": maxScreenshotRegionValue + 1, "y": 0, "width": 1, "height": 1}},
		{name: "width over schema cap", region: map[string]any{"x": 0, "y": 0, "width": maxScreenshotRegionValue + 1, "height": 1}},
		{name: "x typed decode overflow", region: map[string]any{"x": math.MaxInt, "y": 0, "width": 1, "height": 1}},
		{name: "width typed decode overflow", region: map[string]any{"x": 1, "y": 0, "width": math.MaxInt, "height": 1}},
		{name: "y typed decode overflow", region: map[string]any{"x": 0, "y": math.MaxInt, "width": 1, "height": 1}},
		{name: "height typed decode overflow", region: map[string]any{"x": 0, "y": 1, "width": 1, "height": math.MaxInt}},
	}
	for _, tc := range schemaInvalid {
		t.Run(tc.name, func(t *testing.T) {
			cs, calls := newScreenshotToolTestSession(t, shot)
			_, err := callScreenshotTool(t, cs, map[string]any{"region": tc.region})
			assertInvalidScreenshotParams(t, err)
			if *calls != 0 {
				t.Errorf("schema-invalid region captured %d frames, want zero", *calls)
			}
		})
	}

	frameInvalid := []struct {
		name   string
		region map[string]any
	}{
		{name: "x at right edge", region: map[string]any{"x": 12, "y": 0, "width": 1, "height": 1}},
		{name: "y at bottom edge", region: map[string]any{"x": 0, "y": 8, "width": 1, "height": 1}},
		{name: "extends past right", region: map[string]any{"x": 11, "y": 0, "width": 2, "height": 1}},
		{name: "extends past bottom", region: map[string]any{"x": 0, "y": 7, "width": 1, "height": 2}},
	}
	for _, tc := range frameInvalid {
		t.Run(tc.name, func(t *testing.T) {
			cs, calls := newScreenshotToolTestSession(t, shot)
			result, err := callScreenshotTool(t, cs, map[string]any{"region": tc.region})
			if err != nil {
				t.Fatalf("CallTool protocol error: %v", err)
			}
			if !result.IsError {
				t.Fatal("out-of-bounds region was reported as success")
			}
			if text := toolResultTextTB(t, result); !strings.Contains(strings.ToLower(text), "region") {
				t.Errorf("out-of-bounds error %q does not identify the region", text)
			}
			if *calls != 1 {
				t.Errorf("frame-relative validation captured %d frames, want exactly 1", *calls)
			}
		})
	}
}

func assertInvalidScreenshotParams(t testing.TB, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("invalid screenshot arguments were accepted")
	}
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("tool rejection = %v, want JSON-RPC InvalidParams", err)
	}
	if rpcErr.Message != invalidToolArgumentsMessage {
		t.Errorf("tool rejection message = %q, want fixed message %q", rpcErr.Message, invalidToolArgumentsMessage)
	}
}

func TestScreenshotToolRemainsReadOnly(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, false)
	result, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	for _, tool := range result.Tools {
		if tool.Name != "jetkvm_screenshot" {
			continue
		}
		if tool.Annotations == nil {
			t.Fatal("jetkvm_screenshot has no annotations")
		}
		if !tool.Annotations.ReadOnlyHint {
			t.Error("jetkvm_screenshot is not marked read-only")
		}
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Error("jetkvm_screenshot is marked destructive")
		}
		return
	}
	t.Fatal("jetkvm_screenshot was not advertised")
}

func TestScreenshotOutputOptionsWriteNothing(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 12, 8)
	cs, calls := newScreenshotToolTestSession(t, shot)
	sandbox := t.TempDir()
	t.Setenv("TMPDIR", sandbox)
	t.Chdir(sandbox)
	before := screenshotTree(t, sandbox)

	for _, args := range []map[string]any{
		nil,
		{"format": "png"},
		{"format": "jpeg", "quality": 80},
		{"scale": 0.5},
		{"region": map[string]any{"x": 2, "y": 1, "width": 8, "height": 6}},
		{"scale": 0.5, "region": map[string]any{"x": 2, "y": 1, "width": 8, "height": 6}},
	} {
		result, err := callScreenshotTool(t, cs, args)
		requireScreenshotSuccess(t, result, err)
	}
	if *calls != 6 {
		t.Errorf("capture calls = %d, want 6", *calls)
	}
	after := screenshotTree(t, sandbox)
	if !reflect.DeepEqual(after, before) {
		t.Errorf("screenshot output options changed the filesystem: before=%v after=%v", before, after)
	}
}

func screenshotTree(t testing.TB, root string) []string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, rel)
		return nil
	}); err != nil {
		t.Fatalf("walking screenshot filesystem sandbox: %v", err)
	}
	return paths
}
