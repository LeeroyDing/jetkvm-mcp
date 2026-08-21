package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

type recordingOCREngine struct {
	checkErr error
	readErr  error
	output   string

	checkCalls int
	readCalls  int
	images     [][]byte
}

func (e *recordingOCREngine) CheckAvailable(ctx context.Context) error {
	e.checkCalls++
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.checkErr
}

func (e *recordingOCREngine) ReadText(ctx context.Context, pngData []byte) (string, error) {
	e.readCalls++
	e.images = append(e.images, append([]byte(nil), pngData...))
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if e.readErr != nil {
		return "", e.readErr
	}
	return e.output, nil
}

func newReadTextToolTestSession(t *testing.T, device device, engine jetkvm.OCREngine, allowControl bool) *mcp.ClientSession {
	t.Helper()
	server := newServerWithOCREngine(device, allowControl, 10*time.Second, engine)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server.Connect failed: %v", err)
	}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "read-text-test-client"}, nil)
	cs, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callReadTextTool(t testing.TB, cs *mcp.ClientSession, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	params := &mcp.CallToolParams{Name: "jetkvm_read_text"}
	if args != nil {
		params.Arguments = args
	}
	return cs.CallTool(context.Background(), params)
}

func TestReadTextToolPlumbsDefaultAndGeometryToOCR(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 12, 8)
	for _, tc := range []struct {
		name       string
		args       map[string]any
		wantWidth  int
		wantHeight int
	}{
		{name: "default full frame", wantWidth: 12, wantHeight: 8},
		{
			name: "crop before scale",
			args: map[string]any{
				"scale": 0.5,
				"region": map[string]any{
					"x": 2, "y": 1, "width": 8, "height": 6,
				},
			},
			wantWidth: 4, wantHeight: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captures := 0
			dev := &mockDevice{screenshotFunc: func(ctx context.Context) (jetkvm.Screenshot, error) {
				captures++
				if err := ctx.Err(); err != nil {
					return jetkvm.Screenshot{}, err
				}
				return shot, nil
			}}
			engine := &recordingOCREngine{output: "Sign in\nPassword"}
			cs := newReadTextToolTestSession(t, dev, engine, false)

			result, err := callReadTextTool(t, cs, tc.args)
			if err != nil {
				t.Fatalf("CallTool failed: %v", err)
			}
			if result.IsError {
				t.Fatalf("read-text returned an error result: %s", toolResultTextTB(t, result))
			}
			if captures != 1 || engine.checkCalls != 1 || engine.readCalls != 1 {
				t.Fatalf("capture/check/read calls = %d/%d/%d, want 1/1/1", captures, engine.checkCalls, engine.readCalls)
			}
			if len(result.Content) != 1 {
				t.Fatalf("content blocks = %d, want exactly one text block", len(result.Content))
			}
			if got := toolResultTextTB(t, result); got != engine.output {
				t.Errorf("OCR text = %q, want exact engine output %q", got, engine.output)
			}
			if len(engine.images) != 1 || !bytes.HasPrefix(engine.images[0], []byte("\x89PNG\r\n\x1a\n")) {
				t.Fatal("OCR engine did not receive one in-memory PNG")
			}
			config, format, err := image.DecodeConfig(bytes.NewReader(engine.images[0]))
			if err != nil {
				t.Fatalf("decoding OCR input: %v", err)
			}
			if format != "png" || config.Width != tc.wantWidth || config.Height != tc.wantHeight {
				t.Errorf("OCR input = %s %dx%d, want png %dx%d", format, config.Width, config.Height, tc.wantWidth, tc.wantHeight)
			}
			if tc.args == nil && !bytes.Equal(engine.images[0], shot.PNG) {
				t.Error("default read-text path did not preserve the captured PNG bytes")
			}

			raw, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatalf("marshalling metadata: %v", err)
			}
			var meta struct {
				Width        int    `json:"width"`
				Height       int    `json:"height"`
				SourceWidth  int    `json:"sourceWidth"`
				SourceHeight int    `json:"sourceHeight"`
				CapturedAt   string `json:"capturedAt"`
				Fresh        bool   `json:"fresh"`
			}
			if err := json.Unmarshal(raw, &meta); err != nil {
				t.Fatalf("decoding metadata: %v", err)
			}
			if meta.Width != tc.wantWidth || meta.Height != tc.wantHeight ||
				meta.SourceWidth != shot.Width || meta.SourceHeight != shot.Height ||
				meta.CapturedAt != shot.CapturedAt.Format(time.RFC3339Nano) || !meta.Fresh {
				t.Errorf("read-text metadata = %+v", meta)
			}
		})
	}
}

func TestReadTextToolUnavailableFailsBeforeCapture(t *testing.T) {
	captures := 0
	dev := &mockDevice{screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
		captures++
		return jetkvm.Screenshot{}, errors.New("unexpected capture")
	}}
	engine := &recordingOCREngine{checkErr: jetkvm.ErrOCRUnavailable}
	cs := newReadTextToolTestSession(t, dev, engine, false)

	result, err := callReadTextTool(t, cs, nil)
	if err != nil {
		t.Fatalf("unavailable OCR returned a protocol error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("unavailable OCR result = %+v, want a tool error", result)
	}
	message := strings.ToLower(toolResultTextTB(t, result))
	if !strings.Contains(message, "ocr") || !strings.Contains(message, "unavailable") {
		t.Errorf("unavailable OCR error = %q, want clear OCR unavailable text", message)
	}
	if captures != 0 || engine.checkCalls != 1 || engine.readCalls != 0 {
		t.Errorf("capture/check/read calls = %d/%d/%d, want 0/1/0", captures, engine.checkCalls, engine.readCalls)
	}
}

func TestReadTextToolNilEngineFailsWithoutPanickingOrCapturing(t *testing.T) {
	captures := 0
	dev := &mockDevice{screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
		captures++
		return jetkvm.Screenshot{}, errors.New("unexpected capture")
	}}
	cs := newReadTextToolTestSession(t, dev, nil, false)

	result, err := callReadTextTool(t, cs, nil)
	if err != nil {
		t.Fatalf("nil OCR engine returned a protocol error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("nil OCR engine result = %+v, want a tool error", result)
	}
	message := strings.ToLower(toolResultTextTB(t, result))
	if !strings.Contains(message, "ocr") || !strings.Contains(message, "unavailable") {
		t.Errorf("nil OCR engine error = %q, want clear OCR unavailable text", message)
	}
	if captures != 0 {
		t.Errorf("nil OCR engine captured %d frames, want zero", captures)
	}
}

func TestReadTextToolRejectsInvalidArgumentsBeforeWork(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 12, 8)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{name: "zero scale", args: map[string]any{"scale": 0}},
		{name: "wrong scale type", args: map[string]any{"scale": "half"}},
		{name: "missing region field", args: map[string]any{"region": map[string]any{"x": 0, "y": 0, "width": 1}}},
		{name: "unknown root field", args: map[string]any{"format": "png"}},
		{name: "unknown nested field", args: map[string]any{"region": map[string]any{
			"x": 0, "y": 0, "width": 1, "height": 1, "output_path": "ignored",
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captures := 0
			dev := &mockDevice{screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
				captures++
				return shot, nil
			}}
			engine := &recordingOCREngine{output: "unused"}
			cs := newReadTextToolTestSession(t, dev, engine, false)
			result, err := callReadTextTool(t, cs, tc.args)
			if result != nil {
				t.Errorf("schema-invalid call returned result %+v, want nil", result)
			}
			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams || rpcErr.Message != invalidToolArgumentsMessage {
				t.Fatalf("schema-invalid rejection = %v, want fixed InvalidParams", err)
			}
			if captures != 0 || engine.checkCalls != 0 || engine.readCalls != 0 {
				t.Errorf("invalid call capture/check/read = %d/%d/%d, want 0/0/0", captures, engine.checkCalls, engine.readCalls)
			}
		})
	}
}

func TestReadTextToolRejectsFrameRelativeRegionBeforeOCR(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 12, 8)
	captures := 0
	dev := &mockDevice{screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
		captures++
		return shot, nil
	}}
	engine := &recordingOCREngine{output: "unused"}
	cs := newReadTextToolTestSession(t, dev, engine, false)

	result, err := callReadTextTool(t, cs, map[string]any{
		"region": map[string]any{"x": 11, "y": 0, "width": 2, "height": 1},
	})
	if err != nil {
		t.Fatalf("frame-relative validation returned protocol error: %v", err)
	}
	if result == nil || !result.IsError || !strings.Contains(strings.ToLower(toolResultTextTB(t, result)), "region") {
		t.Fatalf("frame-relative region result = %+v, want clear tool error", result)
	}
	if captures != 1 || engine.checkCalls != 1 || engine.readCalls != 0 {
		t.Errorf("capture/check/read calls = %d/%d/%d, want 1/1/0", captures, engine.checkCalls, engine.readCalls)
	}
}

func TestReadTextToolIsAdvertisedReadOnlyWithoutControl(t *testing.T) {
	engine := &recordingOCREngine{}
	cs := newReadTextToolTestSession(t, &mockDevice{}, engine, false)
	result, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	for _, tool := range result.Tools {
		if tool.Name != "jetkvm_read_text" {
			continue
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint ||
			tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint ||
			tool.Annotations.IdempotentHint {
			t.Errorf("read-text annotations = %+v, want read-only/non-destructive/non-idempotent", tool.Annotations)
		}

		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling read-text schema: %v", err)
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decoding read-text schema: %v", err)
		}
		if len(schema.Properties) != 2 || schema.Properties["scale"] == nil || schema.Properties["region"] == nil {
			t.Errorf("read-text properties = %v, want exactly scale and region", schema.Properties)
		}
		if len(schema.Required) != 0 {
			t.Errorf("read-text required fields = %v, want none", schema.Required)
		}
		return
	}
	t.Fatal("jetkvm_read_text was not advertised without control")
}
