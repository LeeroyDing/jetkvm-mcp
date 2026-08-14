package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/buildinfo"
	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func connectTestClient(t *testing.T, allowControl bool) *jetkvm.Client {
	t.Helper()
	fd := startFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL(), AllowControl: allowControl})
	if err != nil {
		t.Fatalf("jetkvm.Connect failed: %v", err)
	}
	t.Cleanup(func() { client.Close(context.Background()) })
	return client
}

func newTestServerSession(t *testing.T, client *jetkvm.Client, allowControl bool) *mcp.ClientSession {
	t.Helper()
	operations := &testClientOperations{session: &clientSession{client: client}}
	return newOperationsServerSession(t, operations, allowControl)
}

func newOperationsServerSession(t *testing.T, operations deviceOperations, allowControl bool) *mcp.ClientSession {
	t.Helper()
	server := newServer(operations, allowControl, 10*time.Second)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()

	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server.Connect failed: %v", err)
	}

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
	cs, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

type stubOperations struct {
	statusErr     error
	screenshotErr error
	keypressErr   error
	mouseMoveErr  error
	releaseAllErr error
}

func (o *stubOperations) Status(context.Context) (jetkvm.StatusResult, error) {
	return jetkvm.StatusResult{}, o.statusErr
}

func (o *stubOperations) Screenshot(context.Context) (jetkvm.Screenshot, error) {
	return jetkvm.Screenshot{}, o.screenshotErr
}

func (o *stubOperations) Keypress(context.Context, int, int) error { return o.keypressErr }
func (o *stubOperations) MouseMove(context.Context, int, int, int) error {
	return o.mouseMoveErr
}
func (o *stubOperations) ReleaseAll(context.Context) error { return o.releaseAllErr }

func TestMCPImplementationUsesAuthoritativeVersion(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)
	result := cs.InitializeResult()
	if result == nil || result.ServerInfo == nil {
		t.Fatal("MCP initialize result carried no serverInfo")
	}
	if result.ServerInfo.Version != buildinfo.Version {
		t.Fatalf("MCP version = %q, want %q", result.ServerInfo.Version, buildinfo.Version)
	}
}

type testClientOperations struct {
	session *clientSession
}

func (o *testClientOperations) Status(ctx context.Context) (jetkvm.StatusResult, error) {
	return o.session.status(ctx)
}

func (o *testClientOperations) Screenshot(ctx context.Context) (jetkvm.Screenshot, error) {
	return o.session.screenshot(ctx)
}

func (o *testClientOperations) Keypress(ctx context.Context, key, modifier int) error {
	return o.session.keypress(ctx, key, modifier)
}

func (o *testClientOperations) MouseMove(ctx context.Context, x, y, buttons int) error {
	return o.session.mouseMove(ctx, x, y, buttons)
}

func (o *testClientOperations) ReleaseAll(ctx context.Context) error {
	return o.session.releaseAll(ctx)
}

func TestWithDefaultTimeoutAddsDeadline(t *testing.T) {
	ctx, cancel := withDefaultTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("context without caller deadline did not receive the server default")
	}
	select {
	case <-ctx.Done():
		if ctx.Err() == nil {
			t.Fatal("context completed without an error")
		}
	case <-time.After(time.Second):
		t.Fatal("default MCP tool timeout did not fire")
	}
}

func TestReadOnlyToolsListedWithoutControl(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	ctx := context.Background()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"jetkvm_status", "jetkvm_screenshot"} {
		if !names[want] {
			t.Errorf("expected tool %q to be listed", want)
		}
	}
	if len(res.Tools) != 2 {
		t.Fatalf("read-only tools/list returned %d tools, want exactly 2", len(res.Tools))
	}
	for _, dangerous := range []string{"jetkvm_keypress", "jetkvm_mouse_move", "jetkvm_release_all"} {
		if names[dangerous] {
			t.Errorf("tool %q should not be listed when control is disabled", dangerous)
		}
	}
}

func TestControlToolsListedWhenEnabled(t *testing.T) {
	client := connectTestClient(t, true)
	cs := newTestServerSession(t, client, true)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"jetkvm_keypress", "jetkvm_mouse_move", "jetkvm_release_all"} {
		if !names[want] {
			t.Errorf("expected tool %q to be listed when control is enabled", want)
		}
	}
	if len(res.Tools) != 5 {
		t.Fatalf("control-enabled tools/list returned %d tools, want exactly 5", len(res.Tools))
	}
}

func TestStatusToolCall(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "jetkvm_status"})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result: %+v", res.Content)
	}
}

func TestFakeFirmwareSingleSessionHandoffClosesPreviousPeer(t *testing.T) {
	fd := startFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	first, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL()})
	if err != nil {
		t.Fatalf("first Connect failed: %v", err)
	}
	defer first.Close(context.Background())
	if _, err := first.Status(ctx); err != nil {
		t.Fatalf("first Status before handoff failed: %v", err)
	}

	second, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL()})
	if err != nil {
		t.Fatalf("second Connect failed: %v", err)
	}
	defer second.Close(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	for {
		_, closed := fd.sessionCounts()
		if closed >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake firmware did not close the previous currentSession after handoff")
		}
		time.Sleep(10 * time.Millisecond)
	}

	firstCtx, cancelFirst := context.WithTimeout(context.Background(), time.Second)
	defer cancelFirst()
	if _, err := first.Status(firstCtx); !errors.Is(err, jetkvm.ErrSessionTransport) {
		t.Fatalf("previous peer Status error = %v, want terminal session transport failure", err)
	}
	if _, err := second.Status(ctx); err != nil {
		t.Fatalf("new currentSession Status failed: %v", err)
	}
}

// TestScreenshotToolReturnsImageAndWritesNothing pins the replacement for
// the old output_path parameter: the PNG comes back in the response, and
// the server touches no filesystem path at all.
func TestScreenshotToolReturnsImageAndWritesNothing(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	workdir := t.TempDir()
	before := countFiles(t, workdir)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "jetkvm_screenshot"})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result: %+v", res.Content)
	}

	var image *mcp.ImageContent
	for _, content := range res.Content {
		if img, ok := content.(*mcp.ImageContent); ok {
			image = img
		}
	}
	if image == nil {
		t.Fatal("screenshot result carried no image content")
	}
	if image.MIMEType != "image/png" {
		t.Errorf("image MIME type = %q, want image/png", image.MIMEType)
	}
	if len(image.Data) == 0 {
		t.Fatal("screenshot image content was empty")
	}
	if !bytes.HasPrefix(image.Data, []byte("\x89PNG\r\n\x1a\n")) {
		t.Error("screenshot image content is not a PNG")
	}
	if after := countFiles(t, workdir); after != before {
		t.Errorf("screenshot tool wrote %d files; it must not touch the filesystem", after-before)
	}
}

// TestScreenshotToolRejectsCallerChosenPath is the regression test for the
// arbitrary-file-overwrite primitive the old output_path parameter handed
// to any MCP caller. The parameter must not merely be ignored - it must be
// rejected, so a caller cannot believe it took effect.
func TestScreenshotToolRejectsCallerChosenPath(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	victim := filepath.Join(t.TempDir(), "important.txt")
	if err := os.WriteFile(victim, []byte("original contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, attempt := range []map[string]any{
		{"output_path": victim},
		{"output_path": "../../etc/passwd"},
		{"path": victim},
	} {
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_screenshot",
			Arguments: attempt,
		})
		if err == nil {
			t.Fatalf("screenshot accepted a caller-chosen path %v; it must be rejected", attempt)
		}
	}

	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original contents" {
		t.Fatal("the screenshot tool overwrote a caller-nominated file")
	}
}

// TestToolSchemasRejectUnknownFields proves the strict-schema contract is
// uniform: every tool rejects unknown properties deterministically rather
// than silently ignoring them.
func TestToolSchemasRejectUnknownFields(t *testing.T) {
	client := connectTestClient(t, true)
	cs := newTestServerSession(t, client, true)

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"jetkvm_status", map[string]any{"unexpected": 1}},
		{"jetkvm_screenshot", map[string]any{"unexpected": 1}},
		{"jetkvm_release_all", map[string]any{"unexpected": 1}},
		{"jetkvm_keypress", map[string]any{"key": 4, "unexpected": 1}},
		{"jetkvm_mouse_move", map[string]any{"x": 1, "y": 1, "unexpected": 1}},
	}
	for _, tc := range cases {
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
		if err == nil {
			t.Errorf("%s accepted an unknown field; schemas must be strict", tc.tool)
		}
	}
}

// TestToolSchemasAreStrictAndStable pins the advertised schema shape, so a
// dependency bump cannot silently loosen the contract agents rely on.
func TestToolSchemasAreStrictAndStable(t *testing.T) {
	client := connectTestClient(t, true)
	cs := newTestServerSession(t, client, true)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	wantRequired := map[string][]string{
		"jetkvm_status":      nil,
		"jetkvm_screenshot":  nil,
		"jetkvm_release_all": nil,
		"jetkvm_keypress":    {"key"},
		"jetkvm_mouse_move":  {"x", "y"},
	}

	seen := map[string]bool{}
	for _, tool := range res.Tools {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling %s schema: %v", tool.Name, err)
		}
		var schema struct {
			Type                 string          `json:"type"`
			Required             []string        `json:"required"`
			AdditionalProperties json.RawMessage `json:"additionalProperties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decoding %s schema: %v", tool.Name, err)
		}
		if schema.Type != "object" {
			t.Errorf("%s input schema type = %q, want object", tool.Name, schema.Type)
		}
		if len(schema.AdditionalProperties) == 0 {
			t.Errorf("%s does not declare additionalProperties; unknown fields would be accepted", tool.Name)
		}
		want, ok := wantRequired[tool.Name]
		if !ok {
			t.Errorf("unexpected tool %q in tools/list", tool.Name)
			continue
		}
		seen[tool.Name] = true
		if len(schema.Required) != len(want) {
			t.Errorf("%s required = %v, want %v", tool.Name, schema.Required, want)
			continue
		}
		for i := range want {
			if schema.Required[i] != want[i] {
				t.Errorf("%s required = %v, want %v", tool.Name, schema.Required, want)
				break
			}
		}
	}
	for name := range wantRequired {
		if !seen[name] {
			t.Errorf("tool %q was not advertised", name)
		}
	}
}

// TestToolSchemasRejectConfusableFieldNames covers the two argument-parsing
// confusions the MCP SDK fixed in GO-2026-4569 (case-insensitive struct field
// matching) and GO-2026-4770 (NUL inside JSON strings). Go's encoding/json
// matches struct fields case-insensitively by default, so before the fix a
// caller could reach the Modifier field by sending "Modifier" - past a schema
// that never listed it. The SDK now decodes arguments case-sensitively, and
// our additionalProperties:false schemas reject the variant outright.
//
// What matters is that a confusable spelling is *rejected*, never quietly
// mapped onto a real field: that is the difference between a caller being told
// its call was malformed and a modifier bitmask arriving that no schema
// approved.
func TestToolSchemasRejectConfusableFieldNames(t *testing.T) {
	client := connectTestClient(t, true)
	cs := newTestServerSession(t, client, true)

	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"capitalized required field", "jetkvm_keypress", map[string]any{"Key": 4}},
		{"upper-case required field", "jetkvm_keypress", map[string]any{"KEY": 4}},
		{"capitalized optional field", "jetkvm_keypress", map[string]any{"key": 4, "Modifier": 255}},
		{"NUL-suffixed required field", "jetkvm_keypress", map[string]any{"key\x00": 4}},
		{"NUL-suffixed optional field", "jetkvm_keypress", map[string]any{"key": 4, "modifier\x00": 255}},
		{"capitalized coordinate", "jetkvm_mouse_move", map[string]any{"X": 1, "y": 1}},
		{"capitalized button mask", "jetkvm_mouse_move", map[string]any{"x": 1, "y": 1, "Buttons": 255}},
		{"NUL-suffixed button mask", "jetkvm_mouse_move", map[string]any{"x": 1, "y": 1, "buttons\x00": 255}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      tc.tool,
				Arguments: tc.args,
			}); err == nil {
				t.Errorf("%s accepted confusable arguments %v; a field name that is not exactly the schema's must be rejected, not folded onto a real field", tc.tool, tc.args)
			}
		})
	}
}

func TestMCPProtocolToolErrorsDoNotReflectArgumentOrNameCanaries(t *testing.T) {
	client := connectTestClient(t, true)
	cs := newTestServerSession(t, client, true)

	const canary = "short-mcp-credential-canary"
	for _, params := range []*mcp.CallToolParams{
		{Name: "jetkvm_keypress", Arguments: map[string]any{"key": canary}},
		{Name: "jetkvm_keypress", Arguments: map[string]any{canary: 4}},
		{Name: canary},
	} {
		_, err := cs.CallTool(context.Background(), params)
		if err == nil {
			t.Fatalf("CallTool(%+v) succeeded, want protocol rejection", params)
		}
		if strings.Contains(err.Error(), canary) {
			t.Errorf("protocol tool error reflected canary: %v", err)
		}
		if !strings.Contains(err.Error(), "tool call rejected") {
			t.Errorf("protocol tool error was not normalized: %v", err)
		}
	}
}

func TestMCPRawMalformedToolParamsDoNotReachWire(t *testing.T) {
	server := newServer(&stubOperations{}, false, time.Second)
	reader, writer := io.Pipe()
	captured := &syncWriteCloser{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, newRedactingIOTransport(reader, captured))
	}()
	defer func() {
		cancel()
		_ = writer.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("MCP server did not stop after raw protocol test")
		}
	}()

	writeLine := func(line string) {
		t.Helper()
		if _, err := io.WriteString(writer, line+"\n"); err != nil {
			t.Fatalf("write raw MCP message: %v", err)
		}
	}
	waitFor := func(fragment string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(captured.String(), fragment) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("MCP output did not contain %q: %s", fragment, captured.String())
	}

	writeLine(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	waitFor(`"id":1`)
	writeLine(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	const canary = "short-top-level-MCP-CREDENTIAL-CANARY"
	writeLine(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":123,"canary":"` + canary + `"}}`)
	waitFor(`"id":2`)

	out := captured.String()
	if strings.Contains(out, canary) {
		t.Fatalf("raw MCP output reflected malformed-param canary: %s", out)
	}
	if !strings.Contains(out, "request rejected") {
		t.Fatalf("raw MCP rejection was not normalized: %s", out)
	}
}

func TestMCPCurrentProtocolStillRejectsJSONRPCBatch(t *testing.T) {
	server := newServer(&stubOperations{}, false, time.Second)
	reader, writer := io.Pipe()
	captured := &syncWriteCloser{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	defer writer.Close()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, newRedactingIOTransport(reader, captured)) }()

	if _, err := io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(captured.String(), `"id":1`) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(captured.String(), `"id":1`) {
		t.Fatalf("initialize did not complete: %s", captured.String())
	}
	if _, err := io.WriteString(writer, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	batch := `[{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}},{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}]` + "\n"
	if _, err := io.WriteString(writer, batch); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "batching is not supported") {
			t.Fatalf("current-protocol batch ended server with %v, want batching rejection", err)
		}
	case <-ctx.Done():
		t.Fatal("current-protocol JSON-RPC batch was accepted or left the server running")
	}
}

// TestAdvertisedToolNamesAreSpecValid guards a silent failure mode introduced
// in go-sdk v1.4.x: Server.AddTool validates tool names against the MCP
// character set but only *logs* a violation, and the default logger discards
// everything. A tool renamed to something the spec disallows would therefore
// still be advertised, and be rejected by stricter clients rather than here.
func TestAdvertisedToolNamesAreSpecValid(t *testing.T) {
	client := connectTestClient(t, true)
	cs := newTestServerSession(t, client, true)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("no tools advertised")
	}
	for _, tool := range res.Tools {
		if tool.Name == "" || len(tool.Name) > 128 {
			t.Errorf("tool name %q has invalid length %d; MCP allows 1-128 characters", tool.Name, len(tool.Name))
		}
		for _, r := range tool.Name {
			valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.'
			if !valid {
				t.Errorf("tool name %q contains %q, outside the MCP tool-name character set", tool.Name, string(r))
			}
		}
	}
}

// syncWriteCloser serializes writes into a buffer. The SDK may write from more
// than one goroutine, which is exactly what the real stdout is exposed to.
type syncWriteCloser struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriteCloser) Close() error { return nil }

func (w *syncWriteCloser) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type shortWriteCloser struct{}

func (shortWriteCloser) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

func (shortWriteCloser) Close() error { return nil }

func TestRedactingTransportFailsClosedOnShortWrite(t *testing.T) {
	w := &redactingWriteCloser{next: shortWriteCloser{}}
	if n, err := w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")); err == nil || n != 0 {
		t.Fatalf("Write = (%d, %v), want (0, fixed protocol-output error)", n, err)
	} else if strings.Contains(err.Error(), "jsonrpc") {
		t.Fatalf("short-write error reflected protocol data: %v", err)
	}
}

func TestVersionAwareReaderEnforcesBatchPolicyBeforeSDK(t *testing.T) {
	initialize := func(version string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + version + `"}}` + "\n"
	}
	batch := `[{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}]` + "\n"

	t.Run("current protocol rejects batch", func(t *testing.T) {
		reader := newVersionAwareReadCloser(io.NopCloser(strings.NewReader(initialize("2025-06-18") + batch)))
		got, err := io.ReadAll(reader)
		if err == nil || !strings.Contains(err.Error(), "batching is not supported") {
			t.Fatalf("ReadAll error = %v, want fixed batch-policy rejection", err)
		}
		if strings.Contains(string(got), `"id":2`) {
			t.Fatalf("prohibited batch reached SDK input: %s", got)
		}
	})

	t.Run("older protocol retains legacy batch support", func(t *testing.T) {
		input := initialize("2025-03-26") + batch
		reader := newVersionAwareReadCloser(io.NopCloser(strings.NewReader(input)))
		got, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if string(got) != input {
			t.Fatalf("legacy protocol input changed: %q", got)
		}
	})

	t.Run("unsupported older-looking version rejects later batch", func(t *testing.T) {
		reader := newVersionAwareReadCloser(io.NopCloser(strings.NewReader(initialize("2025-01-01") + batch)))
		got, err := io.ReadAll(reader)
		if err == nil || !strings.Contains(err.Error(), "batching is not supported") {
			t.Fatalf("ReadAll error = %v, want negotiated-current batch rejection", err)
		}
		if strings.Contains(string(got), `"id":2`) {
			t.Fatalf("batch after unsupported initialize reached SDK input: %s", got)
		}
	})

	t.Run("unsupported initialize inside batch is rejected", func(t *testing.T) {
		input := `[{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"0000"}},{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}]` + "\n"
		reader := newVersionAwareReadCloser(io.NopCloser(strings.NewReader(input)))
		got, err := io.ReadAll(reader)
		if err == nil || !strings.Contains(err.Error(), "batching is not supported") {
			t.Fatalf("ReadAll error = %v, want initialize-batch rejection", err)
		}
		if len(got) != 0 {
			t.Fatalf("unsupported initialize batch reached SDK input: %s", got)
		}
	})
}

// teeWriteCloser forwards to the transport and copies to a capture buffer.
type teeWriteCloser struct {
	io.WriteCloser
	capture io.Writer
}

func (w teeWriteCloser) Write(p []byte) (int, error) {
	w.capture.Write(p)
	return w.WriteCloser.Write(p)
}

// TestServerWriteStreamCarriesOnlyJSONRPC is the end-to-end form of the stdout
// discipline that TestPackageNeverWritesToStdout only checks in our own
// source. Under stdio the server's write side *is* os.Stdout, so anything the
// SDK or a dependency emits there - a log line, a banner, a stray newline -
// corrupts the session for every subsequent message.
//
// go-sdk v1.4.1 added server-lifecycle logging ("server run start", "session
// initialized", ...) on a Logger we never set. It currently defaults to
// slog.DiscardHandler, so nothing escapes; this test fails loudly if a future
// bump points that default at a real writer.
func TestServerWriteStreamCarriesOnlyJSONRPC(t *testing.T) {
	client := connectTestClient(t, true)

	// Built exactly as Run builds it, so the assertion covers the shipping
	// registration path rather than a simplified stand-in.
	operations := &testClientOperations{session: &clientSession{client: client}}
	server := newServer(operations, true, 10*time.Second)

	clientToServer, clientWriter := io.Pipe()
	serverToClient, serverWriter := io.Pipe()
	captured := &syncWriteCloser{}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	serverSession, err := server.Connect(ctx, &mcp.IOTransport{
		Reader: clientToServer,
		Writer: teeWriteCloser{WriteCloser: serverWriter, capture: captured},
	}, nil)
	if err != nil {
		cancel()
		t.Fatalf("server.Connect failed: %v", err)
	}

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
	cs, err := mcpClient.Connect(ctx, &mcp.IOTransport{
		Reader: serverToClient,
		Writer: clientWriter,
	}, nil)
	if err != nil {
		cancel()
		t.Fatalf("client.Connect failed: %v", err)
	}
	defer func() {
		cs.Close()
		cancel()
		serverSession.Wait()
	}()

	// Exercise a listing, a successful call, and a rejected call, so the
	// capture spans normal traffic, tool output, and error reporting.
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "jetkvm_status"}); err != nil {
		t.Fatalf("status call failed: %v", err)
	}
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "jetkvm_keypress",
		Arguments: map[string]any{"key": 9999},
	}); err == nil {
		t.Fatal("expected the out-of-range keypress to be rejected")
	}

	out := captured.String()
	if strings.TrimSpace(out) == "" {
		t.Fatal("captured no server output; the test proved nothing")
	}
	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			t.Errorf("line %d of the server write stream is blank; the framing must be newline-delimited JSON-RPC only", i+1)
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Errorf("line %d of the server write stream is not JSON (%v); under stdio this byte would corrupt the protocol: %q", i+1, err, line)
			continue
		}
		if msg["jsonrpc"] != "2.0" {
			t.Errorf("line %d is JSON but not a JSON-RPC 2.0 message: %q", i+1, line)
		}
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestReleaseAllToolWithoutControlIsUnavailable(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "jetkvm_release_all"}); err == nil {
		t.Fatal("release_all was callable without --allow-control")
	}
}

func TestKeypressToolCallSucceedsWhenControlEnabled(t *testing.T) {
	client := connectTestClient(t, true)
	cs := newTestServerSession(t, client, true)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "jetkvm_keypress",
		Arguments: map[string]any{"key": 4, "modifier": 0},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result: %+v", res.Content)
	}
}

func TestControlToolReportsSendAndReleaseErrorsSafely(t *testing.T) {
	const sendCanary = "SEND-QUERY-CREDENTIAL-CANARY-0123456789"
	const releaseCanary = "RELEASE-PASSWORD-CANARY"
	sendErr := errors.New("sending input via http://user:pass@device.invalid/?token=" + sendCanary + " failed")
	releaseErr := errors.New("release failed password=" + releaseCanary)
	cs := newOperationsServerSession(t, &stubOperations{keypressErr: errors.Join(sendErr, releaseErr)}, true)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_keypress",
		Arguments: map[string]any{"key": 4, "modifier": 0},
	})
	if err != nil {
		t.Fatalf("CallTool protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("control cleanup failure was reported as MCP success")
	}
	var rendered strings.Builder
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			rendered.WriteString(text.Text)
		}
	}
	got := rendered.String()
	for _, want := range []string{"sending input", "release failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("joined MCP error lost %q: %q", want, got)
		}
	}
	for _, canary := range []string{sendCanary, releaseCanary, "user:pass"} {
		if strings.Contains(got, canary) {
			t.Errorf("joined MCP error leaked %q: %q", canary, got)
		}
	}
}

func TestReleaseAllFailureIsMCPError(t *testing.T) {
	cs := newOperationsServerSession(t, &stubOperations{releaseAllErr: jetkvm.ErrNeutralizeUnverified}, true)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "jetkvm_release_all"})
	if err != nil {
		t.Fatalf("CallTool protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("unverified release_all was reported as MCP success")
	}
}

// TestKeypressToolRejectsOutOfRangeKey checks the schema bound. Rejection
// happens at the protocol layer now (InvalidParams) rather than as a tool
// error result, which is the stricter and more deterministic contract.
func TestKeypressToolRejectsOutOfRangeKey(t *testing.T) {
	client := connectTestClient(t, true)
	cs := newTestServerSession(t, client, true)

	for _, args := range []map[string]any{
		{"key": 9999},
		{"key": -1},
		{"key": 4, "modifier": 512},
		{}, // key is required
	} {
		if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_keypress",
			Arguments: args,
		}); err == nil {
			t.Errorf("keypress accepted out-of-contract arguments %v", args)
		}
	}
}

func TestMouseMoveToolRejectsOutOfRangeCoordinates(t *testing.T) {
	client := connectTestClient(t, true)
	cs := newTestServerSession(t, client, true)

	for _, args := range []map[string]any{
		{"x": -1, "y": 0},
		{"x": 0, "y": 32768},
		{"x": 0}, // y is required
	} {
		if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_mouse_move",
			Arguments: args,
		}); err == nil {
			t.Errorf("mouse_move accepted out-of-contract arguments %v", args)
		}
	}
}

// TestToolErrorsCarryNoSecrets checks the redaction boundary at the MCP
// surface: whatever goes wrong underneath, a tool result must never carry
// credential material.
func TestToolErrorsCarryNoSecrets(t *testing.T) {
	const password = "sup3r-s3cret-p4ssw0rd"

	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	// Close the device connection so subsequent calls fail, then confirm the
	// resulting error text is clean.
	if err := client.Close(context.Background()); err != nil {
		t.Logf("close reported: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "jetkvm_status"})
	if err != nil {
		return // a protocol-level failure carries no tool text at all
	}
	for _, content := range res.Content {
		text, ok := content.(*mcp.TextContent)
		if !ok {
			continue
		}
		if strings.Contains(text.Text, password) {
			t.Error("tool error text leaked a credential")
		}
		for _, marker := range []string{"authToken", "Authorization", "Set-Cookie"} {
			if strings.Contains(text.Text, marker) {
				t.Errorf("tool error text mentioned %q, which risks reflecting credential material", marker)
			}
		}
	}
}

// TestPackageNeverWritesToStdout enforces the MCP stdio contract at the
// source level: stdout belongs to the JSON-RPC transport, and a single
// stray byte corrupts the protocol stream for the whole session.
func TestPackageNeverWritesToStdout(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{"fmt.Print(", "fmt.Printf(", "fmt.Println(", "println("}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, pattern := range banned {
			if strings.Contains(string(source), pattern) {
				t.Errorf("%s references %s; MCP stdout must stay protocol-clean (write diagnostics to stderr)", name, pattern)
			}
		}
		stdoutRefs := strings.Count(string(source), "os.Stdout")
		if name == "transport.go" {
			if stdoutRefs != 1 || !strings.Contains(string(source), "newRedactingIOTransport(os.Stdin, nonClosingWriter{Writer: os.Stdout})") {
				t.Errorf("transport.go must route its sole stdout reference through the redacting NDJSON transport")
			}
		} else if stdoutRefs != 0 {
			t.Errorf("%s references os.Stdout outside the sole redacting transport boundary", name)
		}
	}
}
