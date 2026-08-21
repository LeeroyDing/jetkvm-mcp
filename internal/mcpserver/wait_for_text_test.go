package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

type scriptedWaitForTextOCREngine struct {
	mu sync.Mutex

	outputs  []string
	checkErr error
	readErr  error

	checkCalls int
	readCalls  int
}

func (e *scriptedWaitForTextOCREngine) CheckAvailable(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkCalls++
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.checkErr
}

func (e *scriptedWaitForTextOCREngine) ReadText(ctx context.Context, _ []byte) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.readCalls++
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if e.readErr != nil {
		return "", e.readErr
	}
	if len(e.outputs) == 0 {
		return "", nil
	}
	index := e.readCalls - 1
	if index >= len(e.outputs) {
		index = len(e.outputs) - 1
	}
	return e.outputs[index], nil
}

func (e *scriptedWaitForTextOCREngine) counts() (check, read int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.checkCalls, e.readCalls
}

func newWaitForTextTestSession(t testing.TB, client device, allowControl bool, timeout time.Duration, engine jetkvm.OCREngine) *mcp.ClientSession {
	t.Helper()
	server := newServerWithOCREngine(client, allowControl, timeout, engine)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server.Connect failed: %v", err)
	}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "wait-for-text-test-client"}, nil)
	cs, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callWaitForTextTool(t testing.TB, cs *mcp.ClientSession, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	params := &mcp.CallToolParams{Name: "jetkvm_wait_for_text"}
	if args != nil {
		params.Arguments = args
	}
	return cs.CallTool(context.Background(), params)
}

type waitForTextMetadata struct {
	Matched    bool   `json:"matched"`
	Match      string `json:"match"`
	TimedOut   bool   `json:"timedOut"`
	Elapsed    string `json:"elapsed"`
	FrameCount int    `json:"frameCount"`
}

func decodeWaitForTextMetadata(t testing.TB, result *mcp.CallToolResult) waitForTextMetadata {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshalling wait-for-text structured content: %v", err)
	}
	var metadata waitForTextMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("decoding wait-for-text structured content: %v", err)
	}
	return metadata
}

func TestWaitForTextToolLiteralMatch(t *testing.T) {
	captures := 0
	dev := &mockDevice{screenshotFunc: func(ctx context.Context) (jetkvm.Screenshot, error) {
		captures++
		if err := ctx.Err(); err != nil {
			return jetkvm.Screenshot{}, err
		}
		return jetkvm.Screenshot{PNG: []byte("frame")}, nil
	}}
	engine := &scriptedWaitForTextOCREngine{outputs: []string{"Boot complete: READY for login"}}
	cs := newWaitForTextTestSession(t, dev, true, time.Second, engine)

	result, err := callWaitForTextTool(t, cs, map[string]any{"text": "READY"})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("literal match result = %+v, want success", result)
	}
	metadata := decodeWaitForTextMetadata(t, result)
	if !metadata.Matched || metadata.Match != "READY" || metadata.TimedOut || metadata.FrameCount != 1 {
		t.Errorf("literal match metadata = %+v", metadata)
	}
	if _, err := time.ParseDuration(metadata.Elapsed); err != nil {
		t.Errorf("elapsed = %q, want duration: %v", metadata.Elapsed, err)
	}
	wantText := "matched=true match=\"READY\" timedOut=false elapsed=" + metadata.Elapsed + " frameCount=1"
	if got := toolResultTextTB(t, result); got != wantText {
		t.Errorf("literal match summary = %q, want %q", got, wantText)
	}
	checkCalls, readCalls := engine.counts()
	if captures != 1 || checkCalls != 1 || readCalls != 1 {
		t.Errorf("capture/check/read calls = %d/%d/%d, want 1/1/1", captures, checkCalls, readCalls)
	}
}

func TestWaitForTextToolRegexMatch(t *testing.T) {
	dev := &mockDevice{screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
		return jetkvm.Screenshot{PNG: []byte("frame")}, nil
	}}
	engine := &scriptedWaitForTextOCREngine{outputs: []string{"kernel 6.12.7 started"}}
	cs := newWaitForTextTestSession(t, dev, true, time.Second, engine)

	result, err := callWaitForTextTool(t, cs, map[string]any{
		"text":  `kernel [0-9]+\.[0-9]+\.[0-9]+`,
		"regex": true,
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("regex match result = %+v, want success", result)
	}
	metadata := decodeWaitForTextMetadata(t, result)
	if !metadata.Matched || metadata.Match != "kernel 6.12.7" || metadata.TimedOut || metadata.FrameCount != 1 {
		t.Errorf("regex match metadata = %+v", metadata)
	}
}

func TestWaitForTextToolTimeoutIsStructuredSuccess(t *testing.T) {
	captures := 0
	dev := &mockDevice{screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
		captures++
		return jetkvm.Screenshot{PNG: []byte("frame")}, nil
	}}
	engine := &scriptedWaitForTextOCREngine{outputs: []string{"still booting"}}
	cs := newWaitForTextTestSession(t, dev, true, time.Second, engine)

	result, err := callWaitForTextTool(t, cs, map[string]any{
		"text":        "READY",
		"interval_ms": 100,
		"timeout_ms":  100,
	})
	if err != nil {
		t.Fatalf("timeout returned protocol error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("timeout result = %+v, want non-error result", result)
	}
	metadata := decodeWaitForTextMetadata(t, result)
	if metadata.Matched || metadata.Match != "" || !metadata.TimedOut || metadata.FrameCount < 1 {
		t.Errorf("timeout metadata = %+v", metadata)
	}
	elapsed, err := time.ParseDuration(metadata.Elapsed)
	if err != nil || elapsed < jetkvm.MinWaitForTextTimeout {
		t.Errorf("timeout elapsed = %q, want at least %s: %v", metadata.Elapsed, jetkvm.MinWaitForTextTimeout, err)
	}
	_, readCalls := engine.counts()
	if captures != metadata.FrameCount || readCalls != metadata.FrameCount {
		t.Errorf("capture/read calls = %d/%d, frameCount=%d", captures, readCalls, metadata.FrameCount)
	}
}

func TestWaitForTextToolHonorsServerContextCap(t *testing.T) {
	dev := &mockDevice{screenshotFunc: func(ctx context.Context) (jetkvm.Screenshot, error) {
		<-ctx.Done()
		return jetkvm.Screenshot{}, ctx.Err()
	}}
	engine := &scriptedWaitForTextOCREngine{}
	cs := newWaitForTextTestSession(t, dev, true, 25*time.Millisecond, engine)

	started := time.Now()
	result, err := callWaitForTextTool(t, cs, map[string]any{
		"text":        "READY",
		"interval_ms": 100,
		"timeout_ms":  1000,
	})
	if err != nil {
		t.Fatalf("server-capped timeout returned protocol error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("server-capped timeout result = %+v, want structured success", result)
	}
	metadata := decodeWaitForTextMetadata(t, result)
	if metadata.Matched || !metadata.TimedOut || metadata.FrameCount != 0 {
		t.Errorf("server-capped timeout metadata = %+v", metadata)
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Errorf("server context cap returned after %s, want well before caller timeout", elapsed)
	}
}

func TestWaitForTextToolNilEngineFailsBeforeCapture(t *testing.T) {
	captures := 0
	dev := &mockDevice{screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
		captures++
		return jetkvm.Screenshot{}, errors.New("unexpected capture")
	}}
	cs := newWaitForTextTestSession(t, dev, true, time.Second, nil)

	result, err := callWaitForTextTool(t, cs, map[string]any{"text": "READY"})
	if err != nil {
		t.Fatalf("nil OCR engine returned protocol error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("nil OCR engine result = %+v, want tool error", result)
	}
	message := strings.ToLower(toolResultTextTB(t, result))
	if !strings.Contains(message, "ocr") || !strings.Contains(message, "unavailable") {
		t.Errorf("nil OCR engine result = %q, want actionable OCR unavailable error", message)
	}
	if captures != 0 {
		t.Errorf("nil OCR engine captured %d frames, want zero", captures)
	}
}

func TestWaitForTextToolRejectsInvalidParametersBeforeWork(t *testing.T) {
	captures := 0
	dev := &mockDevice{screenshotFunc: func(context.Context) (jetkvm.Screenshot, error) {
		captures++
		return jetkvm.Screenshot{PNG: []byte("frame")}, nil
	}}
	engine := &scriptedWaitForTextOCREngine{outputs: []string{"unused"}}
	cs := newWaitForTextTestSession(t, dev, true, time.Second, engine)

	schemaInvalid := []struct {
		name string
		args map[string]any
	}{
		{name: "missing text", args: map[string]any{}},
		{name: "empty text", args: map[string]any{"text": ""}},
		{name: "oversized text", args: map[string]any{"text": strings.Repeat("x", jetkvm.MaxWaitForTextTextRunes+1)}},
		{name: "wrong regex type", args: map[string]any{"text": "ready", "regex": "yes"}},
		{name: "interval below minimum", args: map[string]any{"text": "ready", "interval_ms": jetkvm.MinWaitForTextInterval.Milliseconds() - 1}},
		{name: "interval above maximum", args: map[string]any{"text": "ready", "interval_ms": jetkvm.MaxWaitForTextInterval.Milliseconds() + 1}},
		{name: "timeout below minimum", args: map[string]any{"text": "ready", "timeout_ms": jetkvm.MinWaitForTextTimeout.Milliseconds() - 1}},
		{name: "timeout above maximum", args: map[string]any{"text": "ready", "timeout_ms": jetkvm.MaxWaitForTextTimeout.Milliseconds() + 1}},
		{name: "unknown property", args: map[string]any{"text": "ready", "extra": true}},
	}
	for _, tc := range schemaInvalid {
		t.Run(tc.name, func(t *testing.T) {
			result, err := callWaitForTextTool(t, cs, tc.args)
			if result != nil {
				t.Errorf("invalid call result = %+v, want nil", result)
			}
			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams || rpcErr.Message != invalidToolArgumentsMessage {
				t.Fatalf("invalid parameter rejection = %v, want fixed InvalidParams", err)
			}
		})
	}

	semanticInvalid := []struct {
		name string
		args map[string]any
	}{
		{name: "invalid regular expression", args: map[string]any{"text": "(", "regex": true}},
		{name: "interval exceeds timeout", args: map[string]any{"text": "ready", "interval_ms": 200, "timeout_ms": 100}},
	}
	for _, tc := range semanticInvalid {
		t.Run(tc.name, func(t *testing.T) {
			result, err := callWaitForTextTool(t, cs, tc.args)
			if err != nil {
				t.Fatalf("semantic validation returned protocol error: %v", err)
			}
			if result == nil || !result.IsError {
				t.Fatalf("semantic validation result = %+v, want tool error", result)
			}
		})
	}

	checkCalls, readCalls := engine.counts()
	if captures != 0 || checkCalls != 0 || readCalls != 0 {
		t.Errorf("invalid parameters capture/check/read = %d/%d/%d, want 0/0/0", captures, checkCalls, readCalls)
	}
}

func TestWaitForTextArgumentAdapterRejectsUnsafeDurations(t *testing.T) {
	valid := waitForTextArgs{
		Text:       "ready",
		IntervalMS: jetkvm.DefaultWaitForTextInterval.Milliseconds(),
		TimeoutMS:  jetkvm.DefaultWaitForTextTimeout.Milliseconds(),
	}
	for _, tc := range []struct {
		name string
		args waitForTextArgs
		want string
	}{
		{name: "zero interval", args: waitForTextArgs{Text: valid.Text, IntervalMS: 0, TimeoutMS: valid.TimeoutMS}, want: "interval"},
		{name: "negative interval", args: waitForTextArgs{Text: valid.Text, IntervalMS: -1, TimeoutMS: valid.TimeoutMS}, want: "interval"},
		{name: "overflowing interval", args: waitForTextArgs{Text: valid.Text, IntervalMS: maxWaitForTextDurationMS + 1, TimeoutMS: valid.TimeoutMS}, want: "interval"},
		{name: "zero timeout", args: waitForTextArgs{Text: valid.Text, IntervalMS: valid.IntervalMS, TimeoutMS: 0}, want: "timeout"},
		{name: "negative timeout", args: waitForTextArgs{Text: valid.Text, IntervalMS: valid.IntervalMS, TimeoutMS: -1}, want: "timeout"},
		{name: "overflowing timeout", args: waitForTextArgs{Text: valid.Text, IntervalMS: valid.IntervalMS, TimeoutMS: maxWaitForTextDurationMS + 1}, want: "timeout"},
		{name: "interval over timeout", args: waitForTextArgs{Text: valid.Text, IntervalMS: 200, TimeoutMS: 100}, want: "must not exceed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := waitForTextOptionsFromArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("adapter error = %v, want field text %q", err, tc.want)
			}
		})
	}
}

func TestWaitForTextToolSchemaAdvertisesStrictDefaultsAndBounds(t *testing.T) {
	cs := newWaitForTextTestSession(t, &mockDevice{}, true, time.Second, &scriptedWaitForTextOCREngine{})
	result, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	for _, tool := range result.Tools {
		if tool.Name != "jetkvm_wait_for_text" {
			continue
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint ||
			tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint ||
			tool.Annotations.IdempotentHint {
			t.Errorf("wait-for-text annotations = %+v, want wait-stable-identical read-only annotations", tool.Annotations)
		}

		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling schema: %v", err)
		}
		var schema struct {
			Properties map[string]struct {
				Type      string          `json:"type"`
				Default   json.RawMessage `json:"default"`
				Minimum   *float64        `json:"minimum"`
				Maximum   *float64        `json:"maximum"`
				MinLength *int            `json:"minLength"`
				MaxLength *int            `json:"maxLength"`
			} `json:"properties"`
			Required             []string        `json:"required"`
			AdditionalProperties json.RawMessage `json:"additionalProperties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decoding schema: %v", err)
		}
		if len(schema.Properties) != 4 || len(schema.Required) != 1 || schema.Required[0] != "text" || len(schema.AdditionalProperties) == 0 {
			t.Errorf("schema shape = properties %d required %v additionalProperties %s", len(schema.Properties), schema.Required, schema.AdditionalProperties)
		}
		textProperty := schema.Properties["text"]
		if textProperty.Type != "string" || textProperty.MinLength == nil || *textProperty.MinLength != 1 ||
			textProperty.MaxLength == nil || *textProperty.MaxLength != jetkvm.MaxWaitForTextTextRunes {
			t.Errorf("text schema = %+v", textProperty)
		}
		regexProperty := schema.Properties["regex"]
		if regexProperty.Type != "boolean" || string(regexProperty.Default) != "false" {
			t.Errorf("regex schema = %+v", regexProperty)
		}
		intervalProperty := schema.Properties["interval_ms"]
		if intervalProperty.Type != "integer" || string(intervalProperty.Default) != "500" ||
			intervalProperty.Minimum == nil || *intervalProperty.Minimum != float64(jetkvm.MinWaitForTextInterval.Milliseconds()) ||
			intervalProperty.Maximum == nil || *intervalProperty.Maximum != float64(jetkvm.MaxWaitForTextInterval.Milliseconds()) {
			t.Errorf("interval_ms schema = %+v", intervalProperty)
		}
		timeoutProperty := schema.Properties["timeout_ms"]
		if timeoutProperty.Type != "integer" || string(timeoutProperty.Default) != "10000" ||
			timeoutProperty.Minimum == nil || *timeoutProperty.Minimum != float64(jetkvm.MinWaitForTextTimeout.Milliseconds()) ||
			timeoutProperty.Maximum == nil || *timeoutProperty.Maximum != float64(jetkvm.MaxWaitForTextTimeout.Milliseconds()) {
			t.Errorf("timeout_ms schema = %+v", timeoutProperty)
		}
		return
	}
	t.Fatal("jetkvm_wait_for_text was not advertised")
}

func FuzzWaitForTextTextAndRegexArgs(f *testing.F) {
	for _, seed := range []struct {
		text  string
		regex bool
	}{
		{text: "ready"},
		{text: `ready|login`, regex: true},
		{text: "(", regex: true},
		{text: ""},
		{text: "\x00"},
		{text: strings.Repeat("x", jetkvm.MaxWaitForTextTextRunes)},
		{text: strings.Repeat("x", jetkvm.MaxWaitForTextTextRunes+1)},
	} {
		f.Add(seed.text, seed.regex)
	}

	f.Fuzz(func(t *testing.T, text string, regexMode bool) {
		args := waitForTextArgs{
			Text:       text,
			Regex:      regexMode,
			IntervalMS: jetkvm.DefaultWaitForTextInterval.Milliseconds(),
			TimeoutMS:  jetkvm.DefaultWaitForTextTimeout.Milliseconds(),
		}
		opts, err := waitForTextOptionsFromArgs(args)

		valid := utf8.ValidString(text) && utf8.RuneCountInString(text) >= 1 &&
			utf8.RuneCountInString(text) <= jetkvm.MaxWaitForTextTextRunes
		if valid && regexMode {
			_, compileErr := regexp.Compile(text)
			valid = compileErr == nil
		}
		if valid != (err == nil) {
			t.Fatalf("adapter validity for text length %d regex=%v: err=%v", utf8.RuneCountInString(text), regexMode, err)
		}
		if err != nil {
			return
		}
		if opts.Text != text || opts.Regex != regexMode || opts.Interval == nil || opts.Timeout == nil ||
			*opts.Interval != jetkvm.DefaultWaitForTextInterval || *opts.Timeout != jetkvm.DefaultWaitForTextTimeout {
			t.Fatalf("adapter changed validated arguments: %+v", opts)
		}
		if err := jetkvm.ValidateWaitForTextOptions(opts); err != nil {
			t.Fatalf("adapter returned options rejected by core: %v", err)
		}
	})
}
