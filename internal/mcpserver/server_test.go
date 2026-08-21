package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/buildinfo"
	"github.com/leeroyding/jetkvm-mcp/internal/hidproto"
	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func connectTestClient(t *testing.T, allowControl bool) *jetkvm.Client {
	t.Helper()
	fd := startFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 15*time.Second))
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
	return newTestServerSessionForDevice(t, &clientDevice{client: client}, allowControl)
}

func newTestServerSessionForDevice(t *testing.T, client device, allowControl bool) *mcp.ClientSession {
	t.Helper()
	// The production constructor, so test sessions carry the same server
	// identity (name/version) and catalog the deployed binary would.
	server := newServer(client, allowControl, 10*time.Second)

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

func TestWithDefaultTimeoutKeepsShorterCallerDeadline(t *testing.T) {
	callerCtx, callerCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer callerCancel()
	want, _ := callerCtx.Deadline()

	ctx, cancel := withDefaultTimeout(callerCtx, time.Second)
	defer cancel()
	got, ok := ctx.Deadline()
	if !ok {
		t.Fatal("bounded caller context lost its deadline")
	}
	if !got.Equal(want) {
		t.Fatalf("deadline = %v, want caller's earlier deadline %v", got, want)
	}
}

func TestWithDefaultTimeoutCapsLongerCallerDeadline(t *testing.T) {
	callerCtx, callerCancel := context.WithTimeout(context.Background(), time.Hour)
	defer callerCancel()

	start := time.Now()
	ctx, cancel := withDefaultTimeout(callerCtx, 50*time.Millisecond)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("server timeout did not add a deadline")
	}
	remaining := deadline.Sub(start)
	if remaining <= 0 || remaining > 250*time.Millisecond {
		t.Fatalf("server deadline remaining = %v, want about 50ms", remaining)
	}
}

// TestMCPImplementationUsesAuthoritativeVersion pins serverInfo to the
// single version source. Every surface that reports a version (MCP
// serverInfo, --version, the app bundle plist checked by doctor) derives
// from buildinfo.Version, so a release bump is one source edit.
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
	if result.ServerInfo.Name != "jetkvm" {
		t.Fatalf("MCP server name = %q, want jetkvm", result.ServerInfo.Name)
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
	for _, want := range []string{"jetkvm_status", "jetkvm_screenshot", "jetkvm_read_text"} {
		if !names[want] {
			t.Errorf("expected tool %q to be listed", want)
		}
	}
	if len(res.Tools) != 3 {
		t.Fatalf("read-only tools/list returned %d tools, want exactly 3", len(res.Tools))
	}
	for _, gated := range []string{"jetkvm_wait_stable", "jetkvm_keypress", "jetkvm_type", "jetkvm_key_combo", "jetkvm_key_sequence", "jetkvm_mouse_move", "jetkvm_mouse_button", "jetkvm_scroll", "jetkvm_click", "jetkvm_double_click", "jetkvm_drag", "jetkvm_release_all"} {
		if names[gated] {
			t.Errorf("tool %q should not be listed when control is disabled", gated)
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
	for _, want := range []string{"jetkvm_status", "jetkvm_screenshot", "jetkvm_read_text", "jetkvm_wait_stable", "jetkvm_keypress", "jetkvm_type", "jetkvm_key_combo", "jetkvm_key_sequence", "jetkvm_mouse_move", "jetkvm_mouse_button", "jetkvm_scroll", "jetkvm_click", "jetkvm_double_click", "jetkvm_drag", "jetkvm_release_all"} {
		if !names[want] {
			t.Errorf("expected tool %q to be listed when control is enabled", want)
		}
	}
	if len(res.Tools) != 15 {
		t.Fatalf("control-enabled tools/list returned %d tools, want exactly 15", len(res.Tools))
	}
}

func TestKeyComboToolIsMarkedDangerous(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "jetkvm_key_combo" {
			continue
		}
		if !strings.HasPrefix(tool.Description, "DANGEROUS:") ||
			!strings.Contains(tool.Description, "--allow-control") {
			t.Errorf("key-combo description does not carry the required warning and gate: %q", tool.Description)
		}
		if tool.Annotations == nil ||
			tool.Annotations.ReadOnlyHint ||
			tool.Annotations.DestructiveHint == nil ||
			!*tool.Annotations.DestructiveHint ||
			tool.Annotations.IdempotentHint {
			t.Errorf("key-combo annotations = %+v, want the shared dangerous control annotations", tool.Annotations)
		}
		return
	}
	t.Fatal("jetkvm_key_combo was not advertised")
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

// TestScreenshotToolReturnsImageAndWritesNothing pins the replacement for
// the old output_path parameter: the PNG comes back in the response, and
// the server touches no filesystem path at all.
func TestScreenshotToolReturnsImageAndWritesNothing(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	workdir := t.TempDir()
	t.Setenv("TMPDIR", workdir)
	t.Chdir(workdir)
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
		{"format": "jpeg", "quality": 80, "output_path": victim},
		{"scale": 0.5, "path": victim},
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

func TestWaitStableToolDefaultsAndCustomOptions(t *testing.T) {
	type observedOptions struct {
		threshold    float64
		stableFrames int
		pollInterval time.Duration
	}

	for _, tc := range []struct {
		name string
		args map[string]any
		want observedOptions
	}{
		{
			name: "schema defaults",
			want: observedOptions{
				threshold:    jetkvm.DefaultWaitStableThreshold,
				stableFrames: jetkvm.DefaultWaitStableFrames,
				pollInterval: jetkvm.DefaultWaitStablePollInterval,
			},
		},
		{
			name: "explicit zero threshold and poll interval",
			args: map[string]any{
				"threshold":        0.0,
				"stable_frames":    1,
				"poll_interval_ms": 0,
			},
			want: observedOptions{threshold: 0, stableFrames: 1, pollInterval: 0},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got observedOptions
			device := &mockDevice{waitStableFunc: func(_ context.Context, opts jetkvm.WaitStableOptions) (jetkvm.WaitStableResult, error) {
				if opts.Threshold == nil || opts.StableFrames == nil || opts.PollInterval == nil {
					return jetkvm.WaitStableResult{}, errors.New("wait-stable handler passed unresolved options")
				}
				got = observedOptions{
					threshold:    *opts.Threshold,
					stableFrames: *opts.StableFrames,
					pollInterval: *opts.PollInterval,
				}
				return jetkvm.WaitStableResult{
					Settled:             true,
					FramesSampled:       3,
					FinalChangeFraction: 0.0025,
					Elapsed:             1250 * time.Millisecond,
				}, nil
			}}
			cs := newTestServerSessionForDevice(t, device, true)

			params := &mcp.CallToolParams{Name: "jetkvm_wait_stable"}
			if tc.args != nil {
				params.Arguments = tc.args
			}
			res, err := cs.CallTool(context.Background(), params)
			if err != nil {
				t.Fatalf("CallTool failed: %v", err)
			}
			if res.IsError {
				t.Fatalf("expected success, got error result: %+v", res.Content)
			}
			if got != tc.want {
				t.Errorf("options = %+v, want %+v", got, tc.want)
			}

			if len(res.Content) != 1 {
				t.Fatalf("content blocks = %d, want 1", len(res.Content))
			}
			text, ok := res.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("content type = %T, want text", res.Content[0])
			}
			const wantText = "settled=true framesSampled=3 finalChangeFraction=0.0025 elapsed=1.25s"
			if text.Text != wantText {
				t.Errorf("text = %q, want %q", text.Text, wantText)
			}

			raw, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatalf("marshalling structured content: %v", err)
			}
			var meta struct {
				Settled             bool    `json:"settled"`
				FramesSampled       int     `json:"framesSampled"`
				FinalChangeFraction float64 `json:"finalChangeFraction"`
				Elapsed             string  `json:"elapsed"`
			}
			if err := json.Unmarshal(raw, &meta); err != nil {
				t.Fatalf("decoding structured content: %v", err)
			}
			if !meta.Settled || meta.FramesSampled != 3 || meta.FinalChangeFraction != 0.0025 || meta.Elapsed != "1.25s" {
				t.Errorf("structured content = %+v", meta)
			}
		})
	}
}

func TestWaitStableToolTimeoutReportsPartialObservations(t *testing.T) {
	want := jetkvm.WaitStableResult{
		Settled:             false,
		FramesSampled:       5,
		FinalChangeFraction: 0.75,
		Elapsed:             750 * time.Millisecond,
	}
	connector := func(context.Context) (device, error) {
		return &mockDevice{waitStableFunc: func(context.Context, jetkvm.WaitStableOptions) (jetkvm.WaitStableResult, error) {
			return want, deviceFailure(jetkvm.ErrorKindTimeout, "waiting for screen stability")
		}}, nil
	}
	// Exercise the production retry wrapper: it normalizes the timeout text,
	// but must leave the partial result available to the MCP handler.
	client := newRetryingDeviceWithConnector(true, connector, immediateRetryPolicy(1, nil))
	client.decoderPreflight = func(context.Context) error { return nil }
	cs := newTestServerSessionForDevice(t, client, true)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "jetkvm_wait_stable"})
	if err != nil {
		t.Fatalf("CallTool protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("timed-out wait-stable call was reported as success")
	}
	text := toolResultText(t, res)
	for _, part := range []string{
		"jetkvm: timeout:",
		"settled=false",
		"framesSampled=5",
		"finalChangeFraction=0.75",
		"elapsed=750ms",
	} {
		if !strings.Contains(text, part) {
			t.Errorf("timeout result %q does not contain %q", text, part)
		}
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshalling structured content: %v", err)
	}
	var meta struct {
		Settled             bool    `json:"settled"`
		FramesSampled       int     `json:"framesSampled"`
		FinalChangeFraction float64 `json:"finalChangeFraction"`
		Elapsed             string  `json:"elapsed"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decoding structured content: %v", err)
	}
	if meta.Settled || meta.FramesSampled != 5 || meta.FinalChangeFraction != 0.75 || meta.Elapsed != "750ms" {
		t.Errorf("structured timeout observations = %+v, want %+v", meta, want)
	}
}

func TestWaitStableToolRejectsInvalidOptionsBeforeDeviceCall(t *testing.T) {
	calls := 0
	device := &mockDevice{waitStableFunc: func(context.Context, jetkvm.WaitStableOptions) (jetkvm.WaitStableResult, error) {
		calls++
		return jetkvm.WaitStableResult{}, nil
	}}
	cs := newTestServerSessionForDevice(t, device, true)

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"negative threshold", map[string]any{"threshold": -0.01}},
		{"threshold above one", map[string]any{"threshold": 1.01}},
		{"zero stable frames", map[string]any{"stable_frames": 0}},
		{"negative poll interval", map[string]any{"poll_interval_ms": -1}},
		{"poll interval over duration range", map[string]any{"poll_interval_ms": maxWaitStablePollIntervalMS + 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "jetkvm_wait_stable",
				Arguments: tc.args,
			}); err == nil {
				t.Fatalf("tool accepted invalid options %v", tc.args)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("invalid options reached device %d times, want 0", calls)
	}
}

func TestWaitStableArgumentValidationNamesFields(t *testing.T) {
	valid := waitStableArgs{
		Threshold:      jetkvm.DefaultWaitStableThreshold,
		StableFrames:   jetkvm.DefaultWaitStableFrames,
		PollIntervalMS: jetkvm.DefaultWaitStablePollInterval.Milliseconds(),
	}
	for _, tc := range []struct {
		name string
		args waitStableArgs
		want string
	}{
		{"threshold", waitStableArgs{Threshold: -1, StableFrames: valid.StableFrames, PollIntervalMS: valid.PollIntervalMS}, "Threshold"},
		{"stable frames", waitStableArgs{Threshold: valid.Threshold, StableFrames: 0, PollIntervalMS: valid.PollIntervalMS}, "StableFrames"},
		{"negative poll", waitStableArgs{Threshold: valid.Threshold, StableFrames: valid.StableFrames, PollIntervalMS: -1}, "PollInterval"},
		{"overflowing poll", waitStableArgs{Threshold: valid.Threshold, StableFrames: valid.StableFrames, PollIntervalMS: maxWaitStablePollIntervalMS + 1}, "PollInterval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := waitStableOptionsFromArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation error = %v, want field %q", err, tc.want)
			}
		})
	}
}

func TestWaitStableToolSchemaAdvertisesDefaultsAndBounds(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	for _, tool := range res.Tools {
		if tool.Name != "jetkvm_wait_stable" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling schema: %v", err)
		}
		var schema struct {
			Properties map[string]struct {
				Type    string          `json:"type"`
				Default json.RawMessage `json:"default"`
				Minimum *float64        `json:"minimum"`
				Maximum *float64        `json:"maximum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decoding schema: %v", err)
		}

		threshold := schema.Properties["threshold"]
		if threshold.Type != "number" || string(threshold.Default) != "0.01" ||
			threshold.Minimum == nil || *threshold.Minimum != 0 ||
			threshold.Maximum == nil || *threshold.Maximum != 1 {
			t.Errorf("threshold schema = %+v", threshold)
		}
		stableFrames := schema.Properties["stable_frames"]
		if stableFrames.Type != "integer" || string(stableFrames.Default) != "2" ||
			stableFrames.Minimum == nil || *stableFrames.Minimum != 1 {
			t.Errorf("stable_frames schema = %+v", stableFrames)
		}
		pollInterval := schema.Properties["poll_interval_ms"]
		if pollInterval.Type != "integer" || string(pollInterval.Default) != "250" ||
			pollInterval.Minimum == nil || *pollInterval.Minimum != 0 ||
			pollInterval.Maximum == nil || *pollInterval.Maximum != float64(maxWaitStablePollIntervalMS) {
			t.Errorf("poll_interval_ms schema = %+v", pollInterval)
		}
		return
	}
	t.Fatal("jetkvm_wait_stable was not advertised")
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
		{"jetkvm_screenshot", map[string]any{"region": map[string]any{
			"x": 0, "y": 0, "width": 1, "height": 1, "unexpected": 1,
		}}},
		{"jetkvm_read_text", map[string]any{"unexpected": 1}},
		{"jetkvm_read_text", map[string]any{"region": map[string]any{
			"x": 0, "y": 0, "width": 1, "height": 1, "unexpected": 1,
		}}},
		{"jetkvm_wait_stable", map[string]any{"unexpected": 1}},
		{"jetkvm_release_all", map[string]any{"unexpected": 1}},
		{"jetkvm_keypress", map[string]any{"key": 4, "unexpected": 1}},
		{"jetkvm_type", map[string]any{"text": "a", "unexpected": 1}},
		{"jetkvm_key_combo", map[string]any{"combo": "ctrl+c", "unexpected": 1}},
		{"jetkvm_key_sequence", map[string]any{"combos": []string{"ctrl+c"}, "unexpected": 1}},
		{"jetkvm_mouse_move", map[string]any{"x": 1, "y": 1, "unexpected": 1}},
		{"jetkvm_mouse_button", map[string]any{"button": "left", "action": "press", "unexpected": 1}},
		{"jetkvm_scroll", map[string]any{"dy": 1, "unexpected": 1}},
		{"jetkvm_click", map[string]any{"x": 1, "y": 1, "unexpected": 1}},
		{"jetkvm_drag", map[string]any{"x1": 1, "y1": 1, "x2": 2, "y2": 2, "unexpected": 1}},
		{"jetkvm_double_click", map[string]any{"x": 1, "y": 1, "unexpected": 1}},
	}
	for _, tc := range cases {
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
		if err == nil {
			t.Errorf("%s accepted an unknown field; schemas must be strict", tc.tool)
		}
	}
}

func TestToolArgumentErrorsDoNotReflectCallerInput(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)
	const valueCanary = "PASSWORD-CANARY-WRONG-TYPE"
	const propertyCanary = "TOKEN-CANARY-UNKNOWN-PROPERTY"

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"jetkvm_keypress", map[string]any{"key": valueCanary}},
		{"jetkvm_keypress", map[string]any{"key": 4, propertyCanary: true}},
		{"jetkvm_mouse_button", map[string]any{"button": valueCanary, "action": "press"}},
		{"jetkvm_mouse_button", map[string]any{"button": "left", "action": "press", propertyCanary: true}},
		{"jetkvm_key_sequence", map[string]any{"combos": []string{"ctrl+c"}, "delay_ms": valueCanary}},
		{"jetkvm_key_sequence", map[string]any{"combos": []string{"ctrl+c"}, propertyCanary: true}},
		{"jetkvm_screenshot", map[string]any{"format": valueCanary}},
		{"jetkvm_screenshot", map[string]any{"region": map[string]any{
			"x": 0, "y": 0, "width": 1, "height": 1, propertyCanary: true,
		}}},
		{"jetkvm_read_text", map[string]any{"scale": valueCanary}},
		{"jetkvm_read_text", map[string]any{propertyCanary: true}},
	} {
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      tc.tool,
			Arguments: tc.args,
		})
		if err == nil {
			t.Fatalf("tool accepted invalid caller input: %v", tc.args)
		}
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
			t.Fatalf("tool rejection = %v, want JSON-RPC InvalidParams", err)
		}
		if rpcErr.Message != invalidToolArgumentsMessage {
			t.Errorf("tool rejection message = %q, want fixed message", rpcErr.Message)
		}
		for _, canary := range []string{valueCanary, propertyCanary} {
			if strings.Contains(err.Error(), canary) {
				t.Errorf("tool rejection reflected caller canary %q: %v", canary, err)
			}
		}
	}
}

func TestToolSchemaDefaultsRejectNullArgumentsWithoutPanicking(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)

	for _, args := range []any{
		json.RawMessage("null"),
		map[string]any(nil),
	} {
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_wait_stable",
			Arguments: args,
		})
		if err == nil {
			t.Fatalf("wait-stable accepted null arguments (%T)", args)
		}
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
			t.Fatalf("null-argument rejection = %v, want JSON-RPC InvalidParams", err)
		}
		if rpcErr.Message != invalidToolArgumentsMessage {
			t.Errorf("null-argument message = %q, want fixed message", rpcErr.Message)
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
		"jetkvm_status":       nil,
		"jetkvm_screenshot":   nil,
		"jetkvm_read_text":    nil,
		"jetkvm_wait_stable":  nil,
		"jetkvm_release_all":  nil,
		"jetkvm_keypress":     {"key"},
		"jetkvm_type":         {"text"},
		"jetkvm_key_combo":    {"combo"},
		"jetkvm_key_sequence": {"combos"},
		"jetkvm_mouse_move":   {"x", "y"},
		"jetkvm_mouse_button": {"button", "action"},
		"jetkvm_scroll":       {"dy"},
		"jetkvm_click":        {"x", "y"},
		"jetkvm_double_click": {"x", "y"},
		"jetkvm_drag":         {"x1", "y1", "x2", "y2"},
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

func TestMouseButtonToolSchemaAdvertisesExactEnums(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	for _, tool := range res.Tools {
		if tool.Name != "jetkvm_mouse_button" {
			continue
		}
		if !strings.HasPrefix(tool.Description, "DANGEROUS:") ||
			!strings.Contains(tool.Description, "--allow-control") {
			t.Errorf("mouse-button description does not carry the required warning and gate: %q", tool.Description)
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling mouse-button schema: %v", err)
		}
		var schema struct {
			Properties map[string]struct {
				Type string   `json:"type"`
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decoding mouse-button schema: %v", err)
		}

		for name, want := range map[string][]string{
			"button": {"left", "right", "middle"},
			"action": {"press", "release"},
		} {
			property, ok := schema.Properties[name]
			if !ok {
				t.Errorf("mouse-button schema has no %q property", name)
				continue
			}
			if property.Type != "string" || !reflect.DeepEqual(property.Enum, want) {
				t.Errorf("mouse-button %s schema = type %q enum %v, want string/%v", name, property.Type, property.Enum, want)
			}
		}
		return
	}
	t.Fatal("jetkvm_mouse_button was not advertised")
}

func TestKeySequenceToolSchemaAdvertisesItemAndDelayBounds(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	for _, tool := range res.Tools {
		if tool.Name != "jetkvm_key_sequence" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling key-sequence schema: %v", err)
		}
		var schema struct {
			Properties map[string]struct {
				Type  string `json:"type"`
				Items *struct {
					Type string `json:"type"`
				} `json:"items"`
				MinItems *int     `json:"minItems"`
				MaxItems *int     `json:"maxItems"`
				Minimum  *float64 `json:"minimum"`
				Maximum  *float64 `json:"maximum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decoding key-sequence schema: %v", err)
		}
		combos := schema.Properties["combos"]
		if combos.Type != "array" || combos.Items == nil || combos.Items.Type != "string" {
			t.Errorf("combos schema = %+v, want an array of strings", combos)
		}
		if combos.MinItems == nil || *combos.MinItems != 1 || combos.MaxItems == nil || *combos.MaxItems != jetkvm.MaxKeySequenceLength {
			t.Errorf("combos item bounds = min %v max %v, want 1..%d", combos.MinItems, combos.MaxItems, jetkvm.MaxKeySequenceLength)
		}
		delay := schema.Properties["delay_ms"]
		if delay.Type != "integer" || delay.Minimum == nil || *delay.Minimum != 0 || delay.Maximum == nil || *delay.Maximum != float64(jetkvm.MaxTypeDelayMS) {
			t.Errorf("delay_ms schema = %+v, want integer bounds 0..%d", delay, jetkvm.MaxTypeDelayMS)
		}
		return
	}
	t.Fatal("jetkvm_key_sequence was not advertised")
}

func TestClickToolSchemaAdvertisesDefaultButton(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	for _, tool := range res.Tools {
		if tool.Name != "jetkvm_click" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling click schema: %v", err)
		}
		var schema struct {
			Properties map[string]struct {
				Default json.RawMessage `json:"default"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decoding click schema: %v", err)
		}
		button, ok := schema.Properties["button"]
		if !ok {
			t.Fatal("click schema has no button property")
		}
		var got int
		if err := json.Unmarshal(button.Default, &got); err != nil {
			t.Fatalf("decoding click button default: %v", err)
		}
		if got != 1 {
			t.Errorf("click button default = %d, want 1", got)
		}
		return
	}
	t.Fatal("jetkvm_click was not advertised")
}

func TestDoubleClickToolSchemaAdvertisesDefaultButton(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	for _, tool := range res.Tools {
		if tool.Name != "jetkvm_double_click" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling double-click schema: %v", err)
		}
		var schema struct {
			Properties map[string]struct {
				Default json.RawMessage `json:"default"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decoding double-click schema: %v", err)
		}
		button, ok := schema.Properties["button"]
		if !ok {
			t.Fatal("double-click schema has no button property")
		}
		var got int
		if err := json.Unmarshal(button.Default, &got); err != nil {
			t.Fatalf("decoding double-click button default: %v", err)
		}
		if got != 1 {
			t.Errorf("double-click button default = %d, want 1", got)
		}
		return
	}
	t.Fatal("jetkvm_double_click was not advertised")
}

func TestScrollToolSchemaAdvertisesDefaultAndBounds(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	for _, tool := range res.Tools {
		if tool.Name != "jetkvm_scroll" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling scroll schema: %v", err)
		}
		var schema struct {
			Properties map[string]struct {
				Description string          `json:"description"`
				Default     json.RawMessage `json:"default"`
				Minimum     *float64        `json:"minimum"`
				Maximum     *float64        `json:"maximum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decoding scroll schema: %v", err)
		}

		dx, ok := schema.Properties["dx"]
		if !ok {
			t.Fatal("scroll schema has no dx property")
		}
		var defaultDX int
		if err := json.Unmarshal(dx.Default, &defaultDX); err != nil {
			t.Fatalf("decoding scroll dx default: %v", err)
		}
		if defaultDX != 0 {
			t.Errorf("scroll dx default = %d, want 0", defaultDX)
		}

		for name, property := range schema.Properties {
			if property.Minimum == nil || *property.Minimum != -float64(jetkvm.MaxScrollDelta) {
				t.Errorf("scroll %s minimum = %v, want %d", name, property.Minimum, -jetkvm.MaxScrollDelta)
			}
			if property.Maximum == nil || *property.Maximum != float64(jetkvm.MaxScrollDelta) {
				t.Errorf("scroll %s maximum = %v, want %d", name, property.Maximum, jetkvm.MaxScrollDelta)
			}
			if !strings.Contains(property.Description, "positive") {
				t.Errorf("scroll %s description = %q, want positive-direction documentation", name, property.Description)
			}
		}
		return
	}
	t.Fatal("jetkvm_scroll was not advertised")
}

func TestDragToolSchemaAdvertisesDefaultsAndStepBounds(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	for _, tool := range res.Tools {
		if tool.Name != "jetkvm_drag" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling drag schema: %v", err)
		}
		var schema struct {
			Properties map[string]struct {
				Default json.RawMessage `json:"default"`
				Minimum *float64        `json:"minimum"`
				Maximum *float64        `json:"maximum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decoding drag schema: %v", err)
		}
		for name, want := range map[string]int{"button": 1, "steps": 0} {
			property, ok := schema.Properties[name]
			if !ok {
				t.Fatalf("drag schema has no %s property", name)
			}
			var got int
			if err := json.Unmarshal(property.Default, &got); err != nil {
				t.Fatalf("decoding drag %s default: %v", name, err)
			}
			if got != want {
				t.Errorf("drag %s default = %d, want %d", name, got, want)
			}
		}
		steps := schema.Properties["steps"]
		if steps.Minimum == nil || *steps.Minimum != 0 {
			t.Errorf("drag steps minimum = %v, want 0", steps.Minimum)
		}
		if steps.Maximum == nil || *steps.Maximum != float64(jetkvm.MaxDragSteps) {
			t.Errorf("drag steps maximum = %v, want %d", steps.Maximum, jetkvm.MaxDragSteps)
		}
		return
	}
	t.Fatal("jetkvm_drag was not advertised")
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
		{"capitalized type text", "jetkvm_type", map[string]any{"Text": "a"}},
		{"capitalized type delay", "jetkvm_type", map[string]any{"text": "a", "Delay_ms": 1}},
		{"NUL-suffixed type text", "jetkvm_type", map[string]any{"text\x00": "a"}},
		{"NUL-suffixed type delay", "jetkvm_type", map[string]any{"text": "a", "delay_ms\x00": 1}},
		{"capitalized combo", "jetkvm_key_combo", map[string]any{"Combo": "ctrl+c"}},
		{"NUL-suffixed combo", "jetkvm_key_combo", map[string]any{"combo\x00": "ctrl+c"}},
		{"capitalized sequence combos", "jetkvm_key_sequence", map[string]any{"Combos": []string{"ctrl+c"}}},
		{"capitalized sequence delay", "jetkvm_key_sequence", map[string]any{"combos": []string{"ctrl+c"}, "Delay_ms": 1}},
		{"NUL-suffixed sequence combos", "jetkvm_key_sequence", map[string]any{"combos\x00": []string{"ctrl+c"}}},
		{"NUL-suffixed sequence delay", "jetkvm_key_sequence", map[string]any{"combos": []string{"ctrl+c"}, "delay_ms\x00": 1}},
		{"capitalized screenshot format", "jetkvm_screenshot", map[string]any{"Format": "jpeg"}},
		{"NUL-suffixed screenshot scale", "jetkvm_screenshot", map[string]any{"scale\x00": 0.5}},
		{"capitalized screenshot region width", "jetkvm_screenshot", map[string]any{"region": map[string]any{
			"x": 0, "y": 0, "Width": 1, "height": 1,
		}}},
		{"NUL-suffixed screenshot region coordinate", "jetkvm_screenshot", map[string]any{"region": map[string]any{
			"x\x00": 0, "y": 0, "width": 1, "height": 1,
		}}},
		{"capitalized read-text scale", "jetkvm_read_text", map[string]any{"Scale": 0.5}},
		{"NUL-suffixed read-text scale", "jetkvm_read_text", map[string]any{"scale\x00": 0.5}},
		{"capitalized read-text region width", "jetkvm_read_text", map[string]any{"region": map[string]any{
			"x": 0, "y": 0, "Width": 1, "height": 1,
		}}},
		{"NUL-suffixed read-text region width", "jetkvm_read_text", map[string]any{"region": map[string]any{
			"x": 0, "y": 0, "width\x00": 1, "height": 1,
		}}},
		{"capitalized wait threshold", "jetkvm_wait_stable", map[string]any{"Threshold": 0.01}},
		{"capitalized stable frames", "jetkvm_wait_stable", map[string]any{"Stable_frames": 2}},
		{"NUL-suffixed poll interval", "jetkvm_wait_stable", map[string]any{"poll_interval_ms\x00": 250}},
		{"capitalized coordinate", "jetkvm_mouse_move", map[string]any{"X": 1, "y": 1}},
		{"capitalized button mask", "jetkvm_mouse_move", map[string]any{"x": 1, "y": 1, "Buttons": 255}},
		{"NUL-suffixed button mask", "jetkvm_mouse_move", map[string]any{"x": 1, "y": 1, "buttons\x00": 255}},
		{"capitalized mouse button", "jetkvm_mouse_button", map[string]any{"Button": "left", "action": "press"}},
		{"capitalized mouse action", "jetkvm_mouse_button", map[string]any{"button": "left", "Action": "press"}},
		{"NUL-suffixed mouse button", "jetkvm_mouse_button", map[string]any{"button\x00": "left", "action": "press"}},
		{"NUL-suffixed mouse action", "jetkvm_mouse_button", map[string]any{"button": "left", "action\x00": "press"}},
		{"capitalized scroll delta", "jetkvm_scroll", map[string]any{"DY": 1}},
		{"capitalized horizontal scroll delta", "jetkvm_scroll", map[string]any{"dy": 1, "DX": 1}},
		{"NUL-suffixed scroll delta", "jetkvm_scroll", map[string]any{"dy\x00": 1}},
		{"NUL-suffixed horizontal scroll delta", "jetkvm_scroll", map[string]any{"dy": 1, "dx\x00": 1}},
		{"capitalized click coordinate", "jetkvm_click", map[string]any{"X": 1, "y": 1}},
		{"capitalized click button", "jetkvm_click", map[string]any{"x": 1, "y": 1, "Button": 1}},
		{"NUL-suffixed click button", "jetkvm_click", map[string]any{"x": 1, "y": 1, "button\x00": 1}},
		{"capitalized drag coordinate", "jetkvm_drag", map[string]any{"X1": 1, "y1": 1, "x2": 2, "y2": 2}},
		{"capitalized drag button", "jetkvm_drag", map[string]any{"x1": 1, "y1": 1, "x2": 2, "y2": 2, "Button": 1}},
		{"NUL-suffixed drag steps", "jetkvm_drag", map[string]any{"x1": 1, "y1": 1, "x2": 2, "y2": 2, "steps\x00": 1}},
		{"capitalized double-click coordinate", "jetkvm_double_click", map[string]any{"X": 1, "y": 1}},
		{"capitalized double-click button", "jetkvm_double_click", map[string]any{"x": 1, "y": 1, "Button": 1}},
		{"NUL-suffixed double-click button", "jetkvm_double_click", map[string]any{"x": 1, "y": 1, "button\x00": 1}},
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
	server := newServer(&clientDevice{client: client}, true, 10*time.Second)

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

func TestClickToolWithoutControlIsUnavailable(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_click",
		Arguments: map[string]any{"x": 123, "y": 456},
	}); err == nil {
		t.Fatal("click was callable without --allow-control")
	}
}

func TestMouseButtonToolWithoutControlIsUnavailable(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_mouse_button",
		Arguments: map[string]any{"button": "right", "action": "press"},
	}); err == nil {
		t.Fatal("mouse_button was callable without --allow-control")
	}
}

func TestKeyComboToolWithoutControlIsUnavailable(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_key_combo",
		Arguments: map[string]any{"combo": "ctrl+c"},
	}); err == nil {
		t.Fatal("key_combo was callable without --allow-control")
	}
}

func TestKeySequenceToolWithoutControlIsUnavailable(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_key_sequence",
		Arguments: map[string]any{"combos": []string{"ctrl+c", "enter"}},
	}); err == nil {
		t.Fatal("key_sequence was callable without --allow-control")
	}
}

func TestKeyComboToolRejectsUnknownCombo(t *testing.T) {
	called := false
	cs := newTestServerSessionForDevice(t, &mockDevice{
		keyComboFunc: func(context.Context, byte, []byte) error {
			called = true
			return nil
		},
	}, true)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_key_combo",
		Arguments: map[string]any{"combo": "definitely-not-a-combo"},
	})
	if err != nil {
		t.Fatalf("CallTool protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("unknown key combo was reported as MCP success")
	}
	if called {
		t.Fatal("unknown key combo reached the device")
	}

	var message string
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			message += text.Text
		}
	}
	if !strings.Contains(message, "unknown key combo") || !strings.Contains(message, "ctrl+alt+del") {
		t.Fatalf("unknown-combo error = %q, want a clear error with a valid-name hint", message)
	}
}

func TestKeySequenceToolSendsResolvedCombosInOrder(t *testing.T) {
	type sentCombo struct {
		modifier byte
		keys     []byte
	}
	var got []sentCombo
	cs := newTestServerSessionForDevice(t, &mockDevice{
		keyComboFunc: func(_ context.Context, modifier byte, keys []byte) error {
			got = append(got, sentCombo{modifier: modifier, keys: append([]byte(nil), keys...)})
			return nil
		},
	}, true)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "jetkvm_key_sequence",
		Arguments: map[string]any{
			"combos": []string{"cmd+space", "t", "e", "r", "m", "enter"},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result: %+v", res.Content)
	}

	want := []sentCombo{
		{modifier: jetkvm.ModifierLeftMeta, keys: []byte{jetkvm.KeyUsageSpace}},
		{keys: []byte{jetkvm.KeyUsageT}},
		{keys: []byte{jetkvm.KeyUsageE}},
		{keys: []byte{jetkvm.KeyUsageR}},
		{keys: []byte{jetkvm.KeyUsageM}},
		{keys: []byte{jetkvm.KeyUsageEnter}},
	}
	if len(got) != len(want) {
		t.Fatalf("key-combo calls = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].modifier != want[i].modifier || !bytes.Equal(got[i].keys, want[i].keys) {
			t.Errorf("key-combo call %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if text := toolResultText(t, res); !strings.Contains(text, "combos=6") || !strings.Contains(text, "delay_ms=0") {
		t.Errorf("key-sequence result = %q, want count and delay", text)
	}
}

func TestKeySequenceToolRejectsBadEntryBeforeAnySendWithoutReflection(t *testing.T) {
	const canary = "seq-canary"
	calls := 0
	cs := newTestServerSessionForDevice(t, &mockDevice{
		keyComboFunc: func(context.Context, byte, []byte) error {
			calls++
			return nil
		},
	}, true)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_key_sequence",
		Arguments: map[string]any{"combos": []string{"ctrl+c", canary, "enter"}},
	})
	if err != nil {
		t.Fatalf("CallTool protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("invalid sequence was reported as success")
	}
	if calls != 0 {
		t.Fatalf("invalid sequence sent %d key combos, want zero", calls)
	}
	message := toolResultText(t, res)
	if !strings.Contains(message, "combos[1]") || !strings.Contains(message, "unknown key combo") {
		t.Errorf("invalid sequence error does not identify the bad index: %q", message)
	}
	if strings.Contains(message, canary) {
		t.Errorf("invalid sequence error reflected caller input: %q", message)
	}
}

func TestKeySequenceToolValidatesDelayAndLengthBeforeSending(t *testing.T) {
	calls := 0
	cs := newTestServerSessionForDevice(t, &mockDevice{
		keyComboFunc: func(context.Context, byte, []byte) error {
			calls++
			return nil
		},
	}, true)

	tooLong := make([]string, jetkvm.MaxKeySequenceLength+1)
	for i := range tooLong {
		tooLong[i] = "enter"
	}
	for name, args := range map[string]map[string]any{
		"missing combos":  {},
		"empty sequence":  {"combos": []string{}},
		"too many combos": {"combos": tooLong},
		"negative delay":  {"combos": []string{"enter"}, "delay_ms": -1},
		"oversized delay": {"combos": []string{"enter"}, "delay_ms": jetkvm.MaxTypeDelayMS + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "jetkvm_key_sequence",
				Arguments: args,
			}); err == nil {
				t.Fatal("out-of-contract key sequence was accepted")
			}
		})
	}
	if calls != 0 {
		t.Fatalf("invalid key sequences sent %d key combos, want zero", calls)
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "jetkvm_key_sequence",
		Arguments: map[string]any{
			"combos":   []string{"enter"},
			"delay_ms": jetkvm.MaxTypeDelayMS,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("inclusive maximum delay was rejected: result=%+v err=%v", res, err)
	}
	if calls != 1 {
		t.Fatalf("maximum-delay sequence sent %d combos, want 1", calls)
	}
}

func TestKeySequenceToolWaitsBetweenCombos(t *testing.T) {
	const delayMS = 25
	var sentAt []time.Time
	cs := newTestServerSessionForDevice(t, &mockDevice{
		keyComboFunc: func(context.Context, byte, []byte) error {
			sentAt = append(sentAt, time.Now())
			return nil
		},
	}, true)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "jetkvm_key_sequence",
		Arguments: map[string]any{
			"combos":   []string{"ctrl+c", "enter"},
			"delay_ms": delayMS,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("key sequence with delay failed: result=%+v err=%v", res, err)
	}
	if len(sentAt) != 2 {
		t.Fatalf("key-combo calls = %d, want 2", len(sentAt))
	}
	if elapsed := sentAt[1].Sub(sentAt[0]); elapsed < 20*time.Millisecond {
		t.Errorf("inter-combo delay = %v, want at least 20ms for delay_ms=%d", elapsed, delayMS)
	}
}

func TestScrollToolWithoutControlIsUnavailable(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_scroll",
		Arguments: map[string]any{"dy": 1},
	}); err == nil {
		t.Fatal("scroll was callable without --allow-control")
	}
}

func TestDragToolWithoutControlIsUnavailable(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_drag",
		Arguments: map[string]any{"x1": 123, "y1": 456, "x2": 321, "y2": 654},
	}); err == nil {
		t.Fatal("drag was callable without --allow-control")
	}
}

func TestDoubleClickToolWithoutControlIsUnavailable(t *testing.T) {
	client := connectTestClient(t, false)
	cs := newTestServerSession(t, client, false)

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_double_click",
		Arguments: map[string]any{"x": 123, "y": 456},
	}); err == nil {
		t.Fatal("double-click was callable without --allow-control")
	}
}

// TestReleaseAllFailureIsMCPError pins the ported v0.2.0 contract that a
// release which did not actually release input is a tool error, never a
// quiet success - whether the device layer reports an explicit error or
// simply that control was unavailable.
func TestReleaseAllFailureIsMCPError(t *testing.T) {
	for name, stub := range map[string]*mockDevice{
		"control unavailable": {},
		"release error": {releaseAllFunc: func(context.Context) (bool, error) {
			return true, deviceFailure(jetkvm.ErrorKindAuthFailed, "release all input")
		}},
	} {
		cs := newTestServerSessionForDevice(t, stub, true)
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "jetkvm_release_all"})
		if err != nil {
			t.Fatalf("%s: CallTool protocol error: %v", name, err)
		}
		if !res.IsError {
			t.Fatalf("%s: failed release_all was reported as MCP success", name)
		}
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

func TestClientDeviceScrollUsesLegacyRPCWithoutHIDWheelFrame(t *testing.T) {
	fd := startFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 15*time.Second))
	defer cancel()
	client, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL(), AllowControl: true})
	if err != nil {
		t.Fatalf("jetkvm.Connect: %v", err)
	}
	defer client.Close(context.Background())

	device := &clientDevice{client: client}
	if err := device.scroll(ctx, -3, 4); err != nil {
		t.Fatalf("clientDevice.scroll: %v", err)
	}
	_, _, _, rpcRequests := fd.counts()
	if rpcRequests != 1 {
		t.Fatalf("scroll RPC requests = %d, want 1", rpcRequests)
	}
	select {
	case frame := <-fd.hidFrames:
		t.Fatalf("scroll incorrectly sent a hidrpc frame: % x", frame)
	default:
	}
}

func TestDangerousToolsAreAdvertisedAsDangerous(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	want := map[string]bool{
		"jetkvm_keypress":     false,
		"jetkvm_type":         false,
		"jetkvm_key_combo":    false,
		"jetkvm_key_sequence": false,
		"jetkvm_mouse_move":   false,
		"jetkvm_mouse_button": false,
		"jetkvm_scroll":       false,
		"jetkvm_click":        false,
		"jetkvm_double_click": false,
		"jetkvm_drag":         false,
		"jetkvm_release_all":  false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; !ok {
			continue
		}
		want[tool.Name] = true
		if !strings.HasPrefix(tool.Description, "DANGEROUS:") {
			t.Errorf("%s description = %q, want DANGEROUS prefix", tool.Name, tool.Description)
		}
		if tool.Annotations == nil {
			t.Errorf("%s has no annotations", tool.Name)
			continue
		}
		if tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is incorrectly marked read-only", tool.Name)
		}
		if tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
			t.Errorf("%s is not marked destructive", tool.Name)
		}
		if tool.Annotations.IdempotentHint {
			t.Errorf("%s is incorrectly marked idempotent", tool.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("%s was not advertised", name)
		}
	}
}

func TestMouseButtonToolForwardsEveryNamedAction(t *testing.T) {
	tests := []struct {
		button      string
		action      string
		wantMask    byte
		wantPressed bool
		wantText    string
	}{
		{"left", "press", jetkvm.MouseButtonLeft, true, "pressed mouse button=left"},
		{"left", "release", jetkvm.MouseButtonLeft, false, "released mouse button=left"},
		{"right", "press", jetkvm.MouseButtonRight, true, "pressed mouse button=right"},
		{"right", "release", jetkvm.MouseButtonRight, false, "released mouse button=right"},
		{"middle", "press", jetkvm.MouseButtonMiddle, true, "pressed mouse button=middle"},
		{"middle", "release", jetkvm.MouseButtonMiddle, false, "released mouse button=middle"},
	}

	for _, tc := range tests {
		t.Run(tc.button+"/"+tc.action, func(t *testing.T) {
			calls := 0
			device := &mockDevice{mouseButtonFunc: func(_ context.Context, mask byte, pressed bool) error {
				calls++
				if mask != tc.wantMask || pressed != tc.wantPressed {
					t.Errorf("mouseButton arguments = mask %d pressed %v, want %d/%v", mask, pressed, tc.wantMask, tc.wantPressed)
				}
				return nil
			}}
			cs := newTestServerSessionForDevice(t, device, true)

			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "jetkvm_mouse_button",
				Arguments: map[string]any{
					"button": tc.button,
					"action": tc.action,
				},
			})
			if err != nil {
				t.Fatalf("CallTool failed: %v", err)
			}
			if res.IsError {
				t.Fatalf("expected success, got error result: %+v", res.Content)
			}
			if calls != 1 {
				t.Fatalf("mouseButton calls = %d, want 1", calls)
			}
			if text := toolResultText(t, res); text != tc.wantText {
				t.Errorf("mouse-button result = %q, want %q", text, tc.wantText)
			}
		})
	}
}

func TestMouseButtonToolRejectsOutOfContractArgumentsWithoutSending(t *testing.T) {
	calls := 0
	device := &mockDevice{mouseButtonFunc: func(context.Context, byte, bool) error {
		calls++
		return nil
	}}
	cs := newTestServerSessionForDevice(t, device, true)

	for _, args := range []map[string]any{
		{},
		{"action": "press"},
		{"button": "left"},
		{"button": "LEFT", "action": "press"},
		{"button": " left", "action": "press"},
		{"button": "side", "action": "press"},
		{"button": "left", "action": "PRESS"},
		{"button": "left", "action": "click"},
		{"button": 1, "action": "press"},
		{"button": "left", "action": true},
	} {
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_mouse_button",
			Arguments: args,
		})
		if err == nil {
			t.Errorf("mouse-button accepted out-of-contract arguments %v", args)
			continue
		}
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("mouse-button rejection for %v = %v, want JSON-RPC InvalidParams", args, err)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid mouse-button calls reached the device %d times, want zero", calls)
	}
}

func TestMouseButtonToolReportsDeviceFailure(t *testing.T) {
	calls := 0
	device := &mockDevice{mouseButtonFunc: func(_ context.Context, mask byte, pressed bool) error {
		calls++
		if mask != jetkvm.MouseButtonRight || !pressed {
			t.Errorf("mouseButton arguments = mask %d pressed %v, want right/pressed", mask, pressed)
		}
		return errors.New("synthetic mouse-button failure")
	}}
	cs := newTestServerSessionForDevice(t, device, true)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_mouse_button",
		Arguments: map[string]any{"button": "right", "action": "press"},
	})
	if err != nil {
		t.Fatalf("CallTool protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("mouse-button failure was reported as success")
	}
	if calls != 1 {
		t.Fatalf("mouseButton calls = %d, want 1", calls)
	}
	if text := toolResultText(t, res); !strings.Contains(text, "synthetic mouse-button failure") {
		t.Errorf("mouse-button error result = %q, want underlying failure", text)
	}
}

func TestScrollToolForwardsValidatedDeltas(t *testing.T) {
	tests := []struct {
		name     string
		args     map[string]any
		wantDX   int8
		wantDY   int8
		wantText string
	}{
		{
			name:     "default horizontal delta",
			args:     map[string]any{"dy": 7},
			wantDY:   7,
			wantText: "scrolled mouse dx=0 dy=7",
		},
		{
			name:     "signed bounds",
			args:     map[string]any{"dx": -jetkvm.MaxScrollDelta, "dy": jetkvm.MaxScrollDelta},
			wantDX:   -int8(jetkvm.MaxScrollDelta),
			wantDY:   int8(jetkvm.MaxScrollDelta),
			wantText: "scrolled mouse dx=-127 dy=127",
		},
		{
			name:     "opposite signed bounds",
			args:     map[string]any{"dx": jetkvm.MaxScrollDelta, "dy": -jetkvm.MaxScrollDelta},
			wantDX:   int8(jetkvm.MaxScrollDelta),
			wantDY:   -int8(jetkvm.MaxScrollDelta),
			wantText: "scrolled mouse dx=127 dy=-127",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			device := &mockDevice{scrollFunc: func(_ context.Context, dx, dy int8) error {
				calls++
				if dx != tc.wantDX || dy != tc.wantDY {
					t.Errorf("scroll arguments = %d/%d, want %d/%d", dx, dy, tc.wantDX, tc.wantDY)
				}
				return nil
			}}
			cs := newTestServerSessionForDevice(t, device, true)

			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "jetkvm_scroll",
				Arguments: tc.args,
			})
			if err != nil {
				t.Fatalf("CallTool failed: %v", err)
			}
			if res.IsError {
				t.Fatalf("expected success, got error result: %+v", res.Content)
			}
			if calls != 1 {
				t.Fatalf("scroll calls = %d, want 1", calls)
			}
			if text := toolResultText(t, res); text != tc.wantText {
				t.Errorf("scroll result = %q, want %q", text, tc.wantText)
			}
		})
	}
}

func TestScrollToolReportsDeviceFailure(t *testing.T) {
	calls := 0
	device := &mockDevice{scrollFunc: func(context.Context, int8, int8) error {
		calls++
		return errors.New("synthetic scroll failure")
	}}
	cs := newTestServerSessionForDevice(t, device, true)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_scroll",
		Arguments: map[string]any{"dx": -5, "dy": 7},
	})
	if err != nil {
		t.Fatalf("CallTool protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("scroll failure was reported as success")
	}
	if calls != 1 {
		t.Fatalf("scroll calls = %d, want 1", calls)
	}
	if text := toolResultText(t, res); !strings.Contains(text, "synthetic scroll failure") {
		t.Errorf("scroll error result = %q, want underlying failure", text)
	}
}

func TestScrollToolRejectsOutOfContractArgumentsWithoutSending(t *testing.T) {
	calls := 0
	device := &mockDevice{scrollFunc: func(context.Context, int8, int8) error {
		calls++
		return nil
	}}
	cs := newTestServerSessionForDevice(t, device, true)

	for _, args := range []map[string]any{
		{},
		{"dy": -jetkvm.MaxScrollDelta - 1},
		{"dy": jetkvm.MaxScrollDelta + 1},
		{"dx": -jetkvm.MaxScrollDelta - 1, "dy": 0},
		{"dx": jetkvm.MaxScrollDelta + 1, "dy": 0},
		{"dy": "1"},
		{"dx": "1", "dy": 1},
	} {
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_scroll",
			Arguments: args,
		})
		if err == nil {
			t.Errorf("scroll accepted out-of-contract arguments %v", args)
			continue
		}
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("scroll rejection for %v = %v, want JSON-RPC InvalidParams", args, err)
		}
	}
	// Zero is individually in range for both axes, so this reaches the
	// handler's belt-and-braces validator instead of being rejected by JSON
	// Schema. It must still fail loudly because the firmware would no-op it.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_scroll",
		Arguments: map[string]any{"dx": 0, "dy": 0},
	})
	if err != nil {
		t.Fatalf("zero scroll returned a protocol error: %v", err)
	}
	if !res.IsError || !strings.Contains(toolResultText(t, res), "nothing would be scrolled") {
		t.Fatalf("zero scroll result = %+v, want actionable MCP tool error", res)
	}
	if calls != 0 {
		t.Fatalf("invalid scroll calls sent %d wheel reports, want zero", calls)
	}
}

func TestClickToolMovesPressesAndReleasesInOrder(t *testing.T) {
	type pointerCall struct {
		x, y    int32
		buttons byte
	}
	tests := []struct {
		name       string
		args       map[string]any
		wantButton byte
		wantText   string
	}{
		{
			name:       "default left button",
			args:       map[string]any{"x": 123, "y": 456},
			wantButton: 1,
			wantText:   "clicked mouse at x=123 y=456 button=1",
		},
		{
			name:       "explicit maximum button mask and coordinates",
			args:       map[string]any{"x": jetkvm.MaxAbsoluteCoordinate, "y": jetkvm.MaxAbsoluteCoordinate, "button": 255},
			wantButton: 255,
			wantText:   "clicked mouse at x=32767 y=32767 button=255",
		},
		{
			name:       "explicit zero is not replaced by default",
			args:       map[string]any{"x": 0, "y": 0, "button": 0},
			wantButton: 0,
			wantText:   "clicked mouse at x=0 y=0 button=0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []pointerCall
			device := &mockDevice{mouseMoveFunc: func(_ context.Context, x, y int32, buttons byte) error {
				got = append(got, pointerCall{x: x, y: y, buttons: buttons})
				return nil
			}}
			cs := newTestServerSessionForDevice(t, device, true)

			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "jetkvm_click",
				Arguments: tc.args,
			})
			if err != nil {
				t.Fatalf("CallTool failed: %v", err)
			}
			if res.IsError {
				t.Fatalf("expected success, got error result: %+v", res.Content)
			}

			x := int32(tc.args["x"].(int))
			y := int32(tc.args["y"].(int))
			want := []pointerCall{
				{x: x, y: y, buttons: tc.wantButton},
				{x: x, y: y, buttons: 0},
			}
			if len(got) != len(want) {
				t.Fatalf("mouseMove calls = %+v, want %+v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("mouseMove call %d = %+v, want %+v", i+1, got[i], want[i])
				}
			}
			if text := toolResultText(t, res); text != tc.wantText {
				t.Errorf("click result = %q, want %q", text, tc.wantText)
			}
		})
	}
}

func TestClickToolStopsAndReportsMouseMoveFailures(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(map[int]string{1: "press", 2: "release"}[failAt], func(t *testing.T) {
			calls := 0
			device := &mockDevice{mouseMoveFunc: func(_ context.Context, x, y int32, buttons byte) error {
				calls++
				if x != 123 || y != 456 {
					t.Errorf("mouseMove coordinates = %d/%d, want 123/456", x, y)
				}
				wantButtons := byte(1)
				if calls == 2 {
					wantButtons = 0
				}
				if buttons != wantButtons {
					t.Errorf("mouseMove call %d buttons = %d, want %d", calls, buttons, wantButtons)
				}
				if calls == failAt {
					return errors.New("synthetic click failure")
				}
				return nil
			}}
			cs := newTestServerSessionForDevice(t, device, true)

			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "jetkvm_click",
				Arguments: map[string]any{"x": 123, "y": 456},
			})
			if err != nil {
				t.Fatalf("CallTool protocol error: %v", err)
			}
			if !res.IsError {
				t.Fatal("mouseMove failure was reported as success")
			}
			if calls != failAt {
				t.Fatalf("mouseMove calls = %d, want %d", calls, failAt)
			}
			if text := toolResultText(t, res); !strings.Contains(text, "synthetic click failure") {
				t.Errorf("click error result = %q, want underlying failure", text)
			}
		})
	}
}

func TestDoubleClickToolMovesPressesAndReleasesTwiceInOrder(t *testing.T) {
	type pointerCall struct {
		x, y    int32
		buttons byte
	}
	tests := []struct {
		name       string
		args       map[string]any
		wantButton byte
		wantText   string
	}{
		{
			name:       "default left button",
			args:       map[string]any{"x": 123, "y": 456},
			wantButton: 1,
			wantText:   "double-clicked mouse at x=123 y=456 button=1",
		},
		{
			name:       "explicit maximum button mask and coordinates",
			args:       map[string]any{"x": jetkvm.MaxAbsoluteCoordinate, "y": jetkvm.MaxAbsoluteCoordinate, "button": 255},
			wantButton: 255,
			wantText:   "double-clicked mouse at x=32767 y=32767 button=255",
		},
		{
			name:       "explicit zero is not replaced by default",
			args:       map[string]any{"x": 0, "y": 0, "button": 0},
			wantButton: 0,
			wantText:   "double-clicked mouse at x=0 y=0 button=0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []pointerCall
			device := &mockDevice{mouseMoveFunc: func(_ context.Context, x, y int32, buttons byte) error {
				got = append(got, pointerCall{x: x, y: y, buttons: buttons})
				return nil
			}}
			cs := newTestServerSessionForDevice(t, device, true)

			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "jetkvm_double_click",
				Arguments: tc.args,
			})
			if err != nil {
				t.Fatalf("CallTool failed: %v", err)
			}
			if res.IsError {
				t.Fatalf("expected success, got error result: %+v", res.Content)
			}

			x := int32(tc.args["x"].(int))
			y := int32(tc.args["y"].(int))
			want := []pointerCall{
				{x: x, y: y, buttons: tc.wantButton},
				{x: x, y: y, buttons: 0},
				{x: x, y: y, buttons: tc.wantButton},
				{x: x, y: y, buttons: 0},
			}
			if len(got) != len(want) {
				t.Fatalf("mouseMove calls = %+v, want %+v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("mouseMove call %d = %+v, want %+v", i+1, got[i], want[i])
				}
			}
			if text := toolResultText(t, res); text != tc.wantText {
				t.Errorf("double-click result = %q, want %q", text, tc.wantText)
			}
		})
	}
}

func TestDoubleClickToolStopsAndReportsMouseMoveFailures(t *testing.T) {
	for _, failAt := range []int{1, 2, 3, 4} {
		t.Run(map[int]string{1: "first press", 2: "first release", 3: "second press", 4: "second release"}[failAt], func(t *testing.T) {
			calls := 0
			device := &mockDevice{mouseMoveFunc: func(_ context.Context, x, y int32, buttons byte) error {
				calls++
				if x != 123 || y != 456 {
					t.Errorf("mouseMove coordinates = %d/%d, want 123/456", x, y)
				}
				wantButtons := byte(1)
				if calls%2 == 0 {
					wantButtons = 0
				}
				if buttons != wantButtons {
					t.Errorf("mouseMove call %d buttons = %d, want %d", calls, buttons, wantButtons)
				}
				if calls == failAt {
					return errors.New("synthetic double-click failure")
				}
				return nil
			}}
			cs := newTestServerSessionForDevice(t, device, true)

			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "jetkvm_double_click",
				Arguments: map[string]any{"x": 123, "y": 456},
			})
			if err != nil {
				t.Fatalf("CallTool protocol error: %v", err)
			}
			if !res.IsError {
				t.Fatal("mouseMove failure was reported as success")
			}
			if calls != failAt {
				t.Fatalf("mouseMove calls = %d, want %d", calls, failAt)
			}
			if text := toolResultText(t, res); !strings.Contains(text, "synthetic double-click failure") {
				t.Errorf("double-click error result = %q, want underlying failure", text)
			}
		})
	}
}

func TestWaitStableToolIsAdvertisedAsReadOnlyNonIdempotent(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	for _, tool := range res.Tools {
		if tool.Name != "jetkvm_wait_stable" {
			continue
		}
		if tool.Annotations == nil {
			t.Fatal("jetkvm_wait_stable has no annotations")
		}
		if !tool.Annotations.ReadOnlyHint {
			t.Error("jetkvm_wait_stable is not marked read-only")
		}
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Error("jetkvm_wait_stable is not explicitly marked non-destructive")
		}
		if tool.Annotations.IdempotentHint {
			t.Error("jetkvm_wait_stable is incorrectly marked idempotent")
		}
		return
	}
	t.Fatal("jetkvm_wait_stable was not advertised")
}

func TestDragToolPressesMovesAndReleasesInOrder(t *testing.T) {
	tests := []struct {
		name     string
		args     map[string]any
		want     []jetkvm.PointerDragReport
		wantText string
	}{
		{
			name: "direct move with defaults",
			args: map[string]any{"x1": 123, "y1": 456, "x2": 321, "y2": 654},
			want: []jetkvm.PointerDragReport{
				{X: 123, Y: 456, Buttons: 1},
				{X: 321, Y: 654, Buttons: 1},
				{X: 321, Y: 654, Buttons: 0},
			},
			wantText: "dragged mouse from x1=123 y1=456 to x2=321 y2=654 button=1 steps=0",
		},
		{
			name: "interpolated held-button motion",
			args: map[string]any{"x1": 0, "y1": 0, "x2": 9, "y2": 6, "button": 3, "steps": 2},
			want: []jetkvm.PointerDragReport{
				{X: 0, Y: 0, Buttons: 3},
				{X: 3, Y: 2, Buttons: 3},
				{X: 6, Y: 4, Buttons: 3},
				{X: 9, Y: 6, Buttons: 3},
				{X: 9, Y: 6, Buttons: 0},
			},
			wantText: "dragged mouse from x1=0 y1=0 to x2=9 y2=6 button=3 steps=2",
		},
		{
			name: "maximum endpoint and button bounds",
			args: map[string]any{"x1": jetkvm.MaxAbsoluteCoordinate, "y1": 0, "x2": 0, "y2": jetkvm.MaxAbsoluteCoordinate, "button": 255},
			want: []jetkvm.PointerDragReport{
				{X: jetkvm.MaxAbsoluteCoordinate, Y: 0, Buttons: 255},
				{X: 0, Y: jetkvm.MaxAbsoluteCoordinate, Buttons: 255},
				{X: 0, Y: jetkvm.MaxAbsoluteCoordinate, Buttons: 0},
			},
			wantText: "dragged mouse from x1=32767 y1=0 to x2=0 y2=32767 button=255 steps=0",
		},
		{
			name: "explicit zero button is preserved",
			args: map[string]any{"x1": 1, "y1": 2, "x2": 3, "y2": 4, "button": 0},
			want: []jetkvm.PointerDragReport{
				{X: 1, Y: 2, Buttons: 0},
				{X: 3, Y: 4, Buttons: 0},
				{X: 3, Y: 4, Buttons: 0},
			},
			wantText: "dragged mouse from x1=1 y1=2 to x2=3 y2=4 button=0 steps=0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			device := &mockDevice{dragFunc: func(_ context.Context, reports []jetkvm.PointerDragReport) error {
				calls++
				if len(reports) != len(tc.want) {
					t.Fatalf("drag reports = %+v, want %+v", reports, tc.want)
				}
				for i := range tc.want {
					if reports[i] != tc.want[i] {
						t.Errorf("drag report %d = %+v, want %+v", i+1, reports[i], tc.want[i])
					}
				}
				return nil
			}}
			cs := newTestServerSessionForDevice(t, device, true)

			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "jetkvm_drag",
				Arguments: tc.args,
			})
			if err != nil {
				t.Fatalf("CallTool failed: %v", err)
			}
			if res.IsError {
				t.Fatalf("expected success, got error result: %+v", res.Content)
			}
			if calls != 1 {
				t.Fatalf("drag calls = %d, want 1", calls)
			}
			if text := toolResultText(t, res); text != tc.wantText {
				t.Errorf("drag result = %q, want %q", text, tc.wantText)
			}
		})
	}
}

func TestDragToolRejectsOutOfContractArgumentsWithoutSending(t *testing.T) {
	calls := 0
	device := &mockDevice{dragFunc: func(context.Context, []jetkvm.PointerDragReport) error {
		calls++
		return nil
	}}
	cs := newTestServerSessionForDevice(t, device, true)

	cases := []map[string]any{
		{"y1": 2, "x2": 3, "y2": 4},
		{"x1": 1, "x2": 3, "y2": 4},
		{"x1": 1, "y1": 2, "y2": 4},
		{"x1": 1, "y1": 2, "x2": 3},
		{"x1": -1, "y1": 2, "x2": 3, "y2": 4},
		{"x1": 1, "y1": jetkvm.MaxAbsoluteCoordinate + 1, "x2": 3, "y2": 4},
		{"x1": 1, "y1": 2, "x2": -1, "y2": 4},
		{"x1": 1, "y1": 2, "x2": 3, "y2": jetkvm.MaxAbsoluteCoordinate + 1},
		{"x1": 1, "y1": 2, "x2": 3, "y2": 4, "button": -1},
		{"x1": 1, "y1": 2, "x2": 3, "y2": 4, "button": 256},
		{"x1": 1, "y1": 2, "x2": 3, "y2": 4, "button": "1"},
		{"x1": 1, "y1": 2, "x2": 3, "y2": 4, "steps": -1},
		{"x1": 1, "y1": 2, "x2": 3, "y2": 4, "steps": jetkvm.MaxDragSteps + 1},
		{"x1": "1", "y1": 2, "x2": 3, "y2": 4},
		{"x1": 1, "y1": 2, "x2": 3, "y2": 4, "steps": "1"},
	}
	for _, args := range cases {
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_drag",
			Arguments: args,
		})
		if err == nil {
			t.Errorf("drag accepted out-of-contract arguments %v", args)
			continue
		}
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("drag rejection for %v = %v, want JSON-RPC InvalidParams", args, err)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid drag calls started %d device operations, want zero", calls)
	}
}

func TestDragToolReportsDeviceFailure(t *testing.T) {
	calls := 0
	device := &mockDevice{dragFunc: func(context.Context, []jetkvm.PointerDragReport) error {
		calls++
		return errors.New("synthetic drag failure")
	}}
	cs := newTestServerSessionForDevice(t, device, true)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_drag",
		Arguments: map[string]any{"x1": 1, "y1": 2, "x2": 3, "y2": 4},
	})
	if err != nil {
		t.Fatalf("CallTool protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("drag device failure was reported as success")
	}
	if calls != 1 {
		t.Fatalf("drag calls = %d, want 1", calls)
	}
	if text := toolResultText(t, res); !strings.Contains(text, "synthetic drag failure") {
		t.Errorf("drag error result = %q, want underlying failure", text)
	}
}

func TestTypeToolMapsAndSendsEveryCharacterInOrder(t *testing.T) {
	type sentKeypress struct {
		modifier byte
		key      byte
	}
	var got []sentKeypress
	device := &mockDevice{keypressFunc: func(_ context.Context, modifier, key byte) error {
		got = append(got, sentKeypress{modifier: modifier, key: key})
		return nil
	}}
	cs := newTestServerSessionForDevice(t, device, true)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_type",
		Arguments: map[string]any{"text": "aA1!\n\t ", "delay_ms": 0},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result: %+v", res.Content)
	}

	want := []sentKeypress{
		{modifier: 0, key: 0x04},
		{modifier: 0x02, key: 0x04},
		{modifier: 0, key: 0x1e},
		{modifier: 0x02, key: 0x1e},
		{modifier: 0, key: 0x28},
		{modifier: 0, key: 0x2b},
		{modifier: 0, key: 0x2c},
	}
	if len(got) != len(want) {
		t.Fatalf("keypress calls = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("keypress call %d = %+v, want %+v", i+1, got[i], want[i])
		}
	}
}

func TestTypeToolRejectsUnsupportedRuneBeforeSending(t *testing.T) {
	calls := 0
	device := &mockDevice{keypressFunc: func(context.Context, byte, byte) error {
		calls++
		return nil
	}}
	cs := newTestServerSessionForDevice(t, device, true)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_type",
		Arguments: map[string]any{"text": "aéz"},
	})
	if err != nil {
		t.Fatalf("CallTool protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("unsupported rune was reported as success")
	}
	if calls != 0 {
		t.Fatalf("unsupported text sent %d keypresses, want zero", calls)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "position 2") {
		t.Error("type tool error omitted the one-based character position")
	}
	if !strings.Contains(text, "category: Ll") {
		t.Error("type tool error omitted the Unicode category")
	}
	for _, reflected := range []string{"é", "'é'", "U+00E9"} {
		if strings.Contains(text, reflected) {
			t.Error("type tool error reflected the caller-supplied character")
		}
	}
}

func TestTypeToolStopsAfterFirstKeypressFailure(t *testing.T) {
	calls := 0
	device := &mockDevice{keypressFunc: func(context.Context, byte, byte) error {
		calls++
		if calls == 2 {
			return errors.New("synthetic keypress failure")
		}
		return nil
	}}
	cs := newTestServerSessionForDevice(t, device, true)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_type",
		Arguments: map[string]any{"text": "a~c"},
	})
	if err != nil {
		t.Fatalf("CallTool protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("keypress failure was reported as success")
	}
	if calls != 2 {
		t.Fatalf("keypress calls = %d, want exactly 2 and no send after the failure", calls)
	}
	text := toolResultText(t, res)
	for _, want := range []string{"synthetic keypress failure", "position 2", "category: Sm"} {
		if !strings.Contains(text, want) {
			t.Error("type tool send error omitted safe failure context")
		}
	}
	for _, reflected := range []string{"~", "'~'", "U+007E"} {
		if strings.Contains(text, reflected) {
			t.Error("type tool send error reflected the caller-supplied character")
		}
	}
}

func TestTypeToolDelayFailureDoesNotReflectNextCharacter(t *testing.T) {
	keypresses, err := jetkvm.MapTypeString("a~")
	if err != nil {
		t.Fatal("test fixture did not map")
	}
	sendCalls := 0
	waitCalls := 0
	typeErr := sendTypeKeypresses(
		context.Background(),
		keypresses,
		[]rune("a~"),
		time.Millisecond,
		func(context.Context, byte, byte) error {
			sendCalls++
			return nil
		},
		func(context.Context, time.Duration) error {
			waitCalls++
			return errors.New("synthetic type delay failure")
		},
	)
	if typeErr == nil {
		t.Fatal("type delay failure was reported as success")
	}
	if sendCalls != 1 || waitCalls != 1 {
		t.Fatal("type delay failure did not stop before the second keypress")
	}
	res, _, _ := errorResult(typeErr)
	text := toolResultText(t, res)
	for _, want := range []string{"synthetic type delay failure", "position 2", "category: Sm"} {
		if !strings.Contains(text, want) {
			t.Error("type tool delay error omitted safe failure context")
		}
	}
	for _, reflected := range []string{"~", "'~'", "U+007E"} {
		if strings.Contains(text, reflected) {
			t.Error("type tool delay error reflected the caller-supplied character")
		}
	}
}

func TestTypeToolRejectsOutOfContractArgumentsWithoutSending(t *testing.T) {
	calls := 0
	device := &mockDevice{keypressFunc: func(context.Context, byte, byte) error {
		calls++
		return nil
	}}
	cs := newTestServerSessionForDevice(t, device, true)

	for _, args := range []map[string]any{
		{},
		{"text": "a", "delay_ms": -1},
		{"text": "a", "delay_ms": jetkvm.MaxTypeDelayMS + 1},
		{"text": strings.Repeat("a", jetkvm.MaxTypeStringRunes+1)},
	} {
		if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_type",
			Arguments: args,
		}); err == nil {
			t.Errorf("jetkvm_type accepted out-of-contract arguments")
		}
	}
	if calls != 0 {
		t.Fatalf("invalid calls sent %d keypresses, want zero", calls)
	}
}

func TestTypeToolPressesAndNeutralizesEveryKey(t *testing.T) {
	fd := startFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 15*time.Second))
	defer cancel()
	client, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL(), AllowControl: true})
	if err != nil {
		t.Fatalf("jetkvm.Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	cs := newTestServerSession(t, client, true)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "jetkvm_type",
		Arguments: map[string]any{"text": "aA"},
	})
	if err != nil || res.IsError {
		t.Fatalf("jetkvm_type = %+v, %v", res, err)
	}

	pressA, _ := hidproto.EncodeKeyboardReport(0, []byte{0x04})
	pressShiftA, _ := hidproto.EncodeKeyboardReport(0x02, []byte{0x04})
	releaseKeyboard, _ := hidproto.ReleaseAllKeyboardReport()
	releaseMouse, _ := hidproto.ReleaseAllMouseReport()
	want := [][]byte{
		pressA, releaseKeyboard, releaseMouse,
		pressShiftA, releaseKeyboard, releaseMouse,
	}
	for i, expected := range want {
		if got := fd.nextHIDFrame(t); !bytes.Equal(got, expected) {
			t.Fatalf("HID frame %d = % x, want % x", i, got, expected)
		}
	}
}

func TestKeyComboToolReachesHIDTransport(t *testing.T) {
	fd := startFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 15*time.Second))
	defer cancel()
	client, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL(), AllowControl: true})
	if err != nil {
		t.Fatalf("jetkvm.Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	cs := newTestServerSession(t, client, true)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "jetkvm_key_combo",
		Arguments: map[string]any{"combo": "Ctrl-Alt-Del"},
	})
	if err != nil || res.IsError {
		t.Fatalf("key_combo = %+v, %v", res, err)
	}

	combo, _ := hidproto.EncodeKeyboardReport(
		jetkvm.ModifierLeftControl|jetkvm.ModifierLeftAlt,
		[]byte{jetkvm.KeyUsageDelete},
	)
	releaseKeyboard, _ := hidproto.ReleaseAllKeyboardReport()
	releaseMouse, _ := hidproto.ReleaseAllMouseReport()
	for i, expected := range [][]byte{combo, releaseKeyboard, releaseMouse} {
		if got := fd.nextHIDFrame(t); !bytes.Equal(got, expected) {
			t.Fatalf("HID frame %d = % x, want % x", i, got, expected)
		}
	}
}

func TestMouseButtonToolPressAndReleaseReachHIDTransport(t *testing.T) {
	fd := startFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 15*time.Second))
	defer cancel()
	client, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL(), AllowControl: true})
	if err != nil {
		t.Fatalf("jetkvm.Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	cs := newTestServerSession(t, client, true)

	for _, action := range []string{"press", "release"} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "jetkvm_mouse_button",
			Arguments: map[string]any{"button": "right", "action": action},
		})
		if err != nil || res.IsError {
			t.Fatalf("mouse_button %s = %+v, %v", action, res, err)
		}
	}

	press, _ := hidproto.EncodeMouseReport(0, 0, jetkvm.MouseButtonRight)
	release, _ := hidproto.EncodeMouseReport(0, 0, 0)
	releaseKeyboard, _ := hidproto.ReleaseAllKeyboardReport()
	releaseMouse, _ := hidproto.ReleaseAllMouseReport()
	for i, expected := range [][]byte{press, release, releaseKeyboard, releaseMouse} {
		if got := fd.nextHIDFrame(t); !bytes.Equal(got, expected) {
			t.Fatalf("mouse-button HID frame %d = % x, want % x", i, got, expected)
		}
	}
}

func TestClientDeviceHeldButtonsComposeWithMoveAndReleaseAll(t *testing.T) {
	fd := startFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 15*time.Second))
	defer cancel()
	client, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL(), AllowControl: true})
	if err != nil {
		t.Fatalf("jetkvm.Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	device := &clientDevice{client: client}

	if err := device.mouseButton(ctx, jetkvm.MouseButtonLeft, true); err != nil {
		t.Fatalf("press left: %v", err)
	}
	if err := device.mouseButton(ctx, jetkvm.MouseButtonRight, true); err != nil {
		t.Fatalf("press right: %v", err)
	}
	if err := device.mouseMove(ctx, 123, 456, 0); err != nil {
		t.Fatalf("move with held buttons: %v", err)
	}
	if err := device.mouseButton(ctx, jetkvm.MouseButtonLeft, false); err != nil {
		t.Fatalf("release left: %v", err)
	}
	released, err := device.releaseAll(ctx)
	if err != nil || !released {
		t.Fatalf("releaseAll = released %v error %v, want true/nil", released, err)
	}

	left, _ := hidproto.EncodeMouseReport(0, 0, jetkvm.MouseButtonLeft)
	leftRight, _ := hidproto.EncodeMouseReport(0, 0, jetkvm.MouseButtonLeft|jetkvm.MouseButtonRight)
	moveHeld, _ := hidproto.EncodePointerReport(123, 456, jetkvm.MouseButtonLeft|jetkvm.MouseButtonRight)
	right, _ := hidproto.EncodeMouseReport(0, 0, jetkvm.MouseButtonRight)
	releaseKeyboard, _ := hidproto.ReleaseAllKeyboardReport()
	releaseMouse, _ := hidproto.ReleaseAllMouseReport()
	want := [][]byte{left, leftRight, moveHeld, right, releaseKeyboard, releaseMouse}
	for i, expected := range want {
		if got := fd.nextHIDFrame(t); !bytes.Equal(got, expected) {
			t.Fatalf("composed HID frame %d = % x, want % x", i, got, expected)
		}
	}

	device.controlMu.Lock()
	leaseRetained := device.buttonLease != nil
	heldButtons := device.heldButtons
	device.controlMu.Unlock()
	if leaseRetained || heldButtons != 0 {
		t.Fatalf("releaseAll left adapter state lease=%v buttons=%#02x, want nil/0", leaseRetained, heldButtons)
	}
}

func TestClientDeviceCloseClearsRetainedMouseButtonLease(t *testing.T) {
	fd := startFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 15*time.Second))
	defer cancel()
	client, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL(), AllowControl: true})
	if err != nil {
		t.Fatalf("jetkvm.Connect: %v", err)
	}
	device := &clientDevice{client: client}

	if err := device.mouseButton(ctx, jetkvm.MouseButtonMiddle, true); err != nil {
		t.Fatalf("press middle: %v", err)
	}
	press, _ := hidproto.EncodeMouseReport(0, 0, jetkvm.MouseButtonMiddle)
	if got := fd.nextHIDFrame(t); !bytes.Equal(got, press) {
		t.Fatalf("mouse-button press = % x, want % x", got, press)
	}

	device.controlMu.Lock()
	held := device.buttonLease
	heldButtons := device.heldButtons
	device.controlMu.Unlock()
	if held == nil || heldButtons != jetkvm.MouseButtonMiddle {
		t.Fatalf("retained adapter state = lease %v buttons %#02x, want non-nil/%#02x",
			held != nil, heldButtons, jetkvm.MouseButtonMiddle)
	}

	// Client.Close deliberately uses a fresh neutralization context, so even a
	// canceled session-end caller cannot strand the retained button.
	canceled, cancelClose := context.WithCancel(context.Background())
	cancelClose()
	if err := device.close(canceled); err != nil {
		t.Fatalf("clientDevice.close: %v", err)
	}

	// Exact close neutralization bytes are pinned without WebRTC scheduling in
	// jetkvm.TestClientCloseNeutralizesHeldMouseButtonWithFreshContext. This
	// adapter-level assertion deliberately stops at synchronous lifecycle state:
	// a Pion Send can return just before session teardown wins the race with the
	// fake peer's receive callback.
	select {
	case <-held.Done():
	default:
		t.Fatal("clientDevice.close did not close the retained Held")
	}
	assertClientDeviceButtonsCleared(t, device)
}

func TestClientDeviceWatchButtonLeaseClearsWatchdogExpiry(t *testing.T) {
	fd := startFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 15*time.Second))
	defer cancel()
	client, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL(), AllowControl: true})
	if err != nil {
		t.Fatalf("jetkvm.Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	lease, err := client.Control()
	if err != nil {
		t.Fatalf("Control: %v", err)
	}
	// Install a short-lived holder directly so this test exercises the same
	// watcher as a production press without waiting for the 30-second default.
	held, err := lease.AcquirePersistent(ctx, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("AcquirePersistent: %v", err)
	}
	if err := held.SendMouseReport(ctx, 0, 0, jetkvm.MouseButtonRight); err != nil {
		t.Fatalf("SendMouseReport: %v", err)
	}

	device := &clientDevice{client: client}
	device.controlMu.Lock()
	device.buttonLease = held
	device.heldButtons = jetkvm.MouseButtonRight
	device.watchButtonLease(held)
	device.controlMu.Unlock()

	press, _ := hidproto.EncodeMouseReport(0, 0, jetkvm.MouseButtonRight)
	if got := fd.nextHIDFrame(t); !bytes.Equal(got, press) {
		t.Fatalf("mouse-button press = % x, want % x", got, press)
	}
	select {
	case <-held.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("persistent holder watchdog did not expire")
	}
	waitForClientDeviceButtonsCleared(t, device)

	releaseKeyboard, _ := hidproto.ReleaseAllKeyboardReport()
	releaseMouse, _ := hidproto.ReleaseAllMouseReport()
	for i, expected := range [][]byte{releaseKeyboard, releaseMouse} {
		if got := fd.nextHIDFrame(t); !bytes.Equal(got, expected) {
			t.Fatalf("watchdog neutralization frame %d = % x, want % x", i, got, expected)
		}
	}
}

func assertClientDeviceButtonsCleared(t *testing.T, device *clientDevice) {
	t.Helper()
	device.controlMu.Lock()
	defer device.controlMu.Unlock()
	if device.buttonLease != nil || device.heldButtons != 0 {
		t.Fatalf("adapter state = lease %v buttons %#02x, want nil/0",
			device.buttonLease != nil, device.heldButtons)
	}
}

func waitForClientDeviceButtonsCleared(t *testing.T, device *clientDevice) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		device.controlMu.Lock()
		cleared := device.buttonLease == nil && device.heldButtons == 0
		device.controlMu.Unlock()
		if cleared {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			assertClientDeviceButtonsCleared(t, device)
			return
		}
	}
}

func toolResultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			return text.Text
		}
	}
	t.Fatal("tool result carried no text content")
	return ""
}

func TestMouseMoveClickDragAndReleaseAllToolsReachHIDTransport(t *testing.T) {
	fd := startFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 15*time.Second))
	defer cancel()
	client, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL(), AllowControl: true})
	if err != nil {
		t.Fatalf("jetkvm.Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	cs := newTestServerSession(t, client, true)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "jetkvm_mouse_move",
		Arguments: map[string]any{"x": 123, "y": 456, "buttons": 3},
	})
	if err != nil || res.IsError {
		t.Fatalf("mouse_move = %+v, %v", res, err)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "jetkvm_click",
		Arguments: map[string]any{"x": 321, "y": 654, "button": 2},
	})
	if err != nil || res.IsError {
		t.Fatalf("click = %+v, %v", res, err)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "jetkvm_drag",
		Arguments: map[string]any{
			"x1": 0, "y1": 0, "x2": 9, "y2": 6, "button": 1, "steps": 2,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("drag = %+v, %v", res, err)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "jetkvm_release_all"})
	if err != nil || res.IsError {
		t.Fatalf("release_all = %+v, %v", res, err)
	}

	pointer, _ := hidproto.EncodePointerReport(123, 456, 3)
	clickPress, _ := hidproto.EncodePointerReport(321, 654, 2)
	clickRelease, _ := hidproto.EncodePointerReport(321, 654, 0)
	dragStart, _ := hidproto.EncodePointerReport(0, 0, 1)
	dragStep1, _ := hidproto.EncodePointerReport(3, 2, 1)
	dragStep2, _ := hidproto.EncodePointerReport(6, 4, 1)
	dragEnd, _ := hidproto.EncodePointerReport(9, 6, 1)
	dragRelease, _ := hidproto.EncodePointerReport(9, 6, 0)
	releaseKeyboard, _ := hidproto.ReleaseAllKeyboardReport()
	releaseMouse, _ := hidproto.ReleaseAllMouseReport()
	want := [][]byte{
		pointer, releaseKeyboard, releaseMouse,
		clickPress, releaseKeyboard, releaseMouse,
		clickRelease, releaseKeyboard, releaseMouse,
		dragStart, dragStep1, dragStep2, dragEnd, dragRelease,
		releaseKeyboard, releaseMouse,
		releaseKeyboard, releaseMouse,
	}
	for i, expected := range want {
		if got := fd.nextHIDFrame(t); !bytes.Equal(got, expected) {
			t.Fatalf("HID frame %d = % x, want % x", i, got, expected)
		}
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
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_keypress",
			Arguments: args,
		})
		if err == nil {
			t.Errorf("keypress accepted out-of-contract arguments %v", args)
			continue
		}
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("keypress rejection for %v = %v, want JSON-RPC InvalidParams", args, err)
		}
	}
}

func TestMouseMoveToolRejectsOutOfRangeCoordinates(t *testing.T) {
	client := connectTestClient(t, true)
	cs := newTestServerSession(t, client, true)

	for _, args := range []map[string]any{
		{"x": -1, "y": 0},
		{"x": 0, "y": 32768},
		{"x": 0, "y": 0, "buttons": 256},
		{"x": 0, "y": 0, "buttons": -1},
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

func TestClickToolRejectsOutOfContractArgumentsWithoutSending(t *testing.T) {
	calls := 0
	device := &mockDevice{mouseMoveFunc: func(context.Context, int32, int32, byte) error {
		calls++
		return nil
	}}
	cs := newTestServerSessionForDevice(t, device, true)

	for _, args := range []map[string]any{
		{"x": -1, "y": 0},
		{"x": 0, "y": -1},
		{"x": jetkvm.MaxAbsoluteCoordinate + 1, "y": 0},
		{"x": 0, "y": jetkvm.MaxAbsoluteCoordinate + 1},
		{"x": 0, "y": 0, "button": 256},
		{"x": 0, "y": 0, "button": -1},
		{"x": 0, "y": 0, "button": "1"},
		{"y": 0},
		{"x": 0},
	} {
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_click",
			Arguments: args,
		})
		if err == nil {
			t.Errorf("click accepted out-of-contract arguments %v", args)
			continue
		}
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("click rejection for %v = %v, want JSON-RPC InvalidParams", args, err)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid click calls sent %d mouse moves, want zero", calls)
	}
}

func TestDoubleClickToolRejectsOutOfContractArgumentsWithoutSending(t *testing.T) {
	calls := 0
	device := &mockDevice{mouseMoveFunc: func(context.Context, int32, int32, byte) error {
		calls++
		return nil
	}}
	cs := newTestServerSessionForDevice(t, device, true)

	for _, args := range []map[string]any{
		{"x": -1, "y": 0},
		{"x": 0, "y": -1},
		{"x": jetkvm.MaxAbsoluteCoordinate + 1, "y": 0},
		{"x": 0, "y": jetkvm.MaxAbsoluteCoordinate + 1},
		{"x": 0, "y": 0, "button": 256},
		{"x": 0, "y": 0, "button": -1},
		{"x": 0, "y": 0, "button": "1"},
		{"y": 0},
		{"x": 0},
	} {
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_double_click",
			Arguments: args,
		})
		if err == nil {
			t.Errorf("double-click accepted out-of-contract arguments %v", args)
			continue
		}
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("double-click rejection for %v = %v, want JSON-RPC InvalidParams", args, err)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid double-click calls sent %d mouse moves, want zero", calls)
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
	banned := []string{"os.Stdout", "fmt.Print(", "fmt.Printf(", "fmt.Println(", "println("}
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
	}
}
