package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

// e2eRecordingDevice sits immediately below the production MCP handlers and
// delegates every operation to a real clientDevice. It makes the negative
// assertion precise: malformed MCP arguments must not cross the operation
// boundary, while successful calls still continue through the real JetKVM
// client and the loopback WebRTC fake.
type e2eRecordingDevice struct {
	device

	mu    sync.Mutex
	calls []string
}

func e2eCall(name string, payload any) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("marshalling e2e call %s: %v", name, err))
	}
	return name + ":" + string(raw)
}

func (d *e2eRecordingDevice) record(name string, payload any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, e2eCall(name, payload))
}

func (d *e2eRecordingDevice) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

func (d *e2eRecordingDevice) status(ctx context.Context) (jetkvm.StatusResult, error) {
	d.record("status", nil)
	return d.device.status(ctx)
}

func (d *e2eRecordingDevice) captureScreenshot(ctx context.Context) (jetkvm.Screenshot, error) {
	d.record("captureScreenshot", nil)
	return d.device.captureScreenshot(ctx)
}

func e2eWaitStablePayload(opts jetkvm.WaitStableOptions) map[string]any {
	payload := map[string]any{}
	if opts.Threshold != nil {
		payload["threshold"] = *opts.Threshold
	}
	if opts.StableFrames != nil {
		payload["stableFrames"] = *opts.StableFrames
	}
	if opts.PollInterval != nil {
		payload["pollInterval"] = opts.PollInterval.String()
	}
	return payload
}

func (d *e2eRecordingDevice) waitStable(ctx context.Context, opts jetkvm.WaitStableOptions) (jetkvm.WaitStableResult, error) {
	d.record("waitStable", e2eWaitStablePayload(opts))
	return d.device.waitStable(ctx, opts)
}

func (d *e2eRecordingDevice) releaseAll(ctx context.Context) (bool, error) {
	d.record("releaseAll", nil)
	return d.device.releaseAll(ctx)
}

func (d *e2eRecordingDevice) keypress(ctx context.Context, modifier, key byte) error {
	d.record("keypress", map[string]any{"key": key, "modifier": modifier})
	return d.device.keypress(ctx, modifier, key)
}

func (d *e2eRecordingDevice) keyCombo(ctx context.Context, modifier byte, keys []byte) error {
	d.record("keyCombo", map[string]any{
		"keys":     append([]byte(nil), keys...),
		"modifier": modifier,
	})
	return d.device.keyCombo(ctx, modifier, keys)
}

func (d *e2eRecordingDevice) holdKey(ctx context.Context, modifier byte, keys []byte, holdMS int) error {
	d.record("holdKey", map[string]any{
		"holdMS":   holdMS,
		"keys":     append([]byte(nil), keys...),
		"modifier": modifier,
	})
	return d.device.holdKey(ctx, modifier, keys, holdMS)
}

func (d *e2eRecordingDevice) mouseMove(ctx context.Context, x, y int32, buttons byte) error {
	d.record("mouseMove", map[string]any{"buttons": buttons, "x": x, "y": y})
	return d.device.mouseMove(ctx, x, y, buttons)
}

func (d *e2eRecordingDevice) mouseButton(ctx context.Context, button byte, pressed bool) error {
	d.record("mouseButton", map[string]any{"button": button, "pressed": pressed})
	return d.device.mouseButton(ctx, button, pressed)
}

func (d *e2eRecordingDevice) scroll(ctx context.Context, dx, dy int8) error {
	d.record("scroll", map[string]any{"dx": dx, "dy": dy})
	return d.device.scroll(ctx, dx, dy)
}

func (d *e2eRecordingDevice) drag(ctx context.Context, reports []jetkvm.PointerDragReport) error {
	copied := append([]jetkvm.PointerDragReport(nil), reports...)
	d.record("drag", copied)
	return d.device.drag(ctx, reports)
}

func (d *e2eRecordingDevice) close(ctx context.Context) error {
	d.record("close", nil)
	return d.device.close(ctx)
}

type e2eRig struct {
	fake      *fakeDevice
	recording *e2eRecordingDevice
	session   *mcp.ClientSession
	nextRPCID int64
}

const e2eOCRText = "JetKVM test screen READY\n"

func newE2ERig(t *testing.T, clientControl, serverControl bool) *e2eRig {
	t.Helper()
	fd := startFakeDeviceWithOptions(t, fakeDeviceOptions{CaptureWire: true})
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 15*time.Second))
	defer cancel()
	client, err := jetkvm.Connect(ctx, jetkvm.Options{
		BaseURL:      fd.baseURL(),
		AllowControl: clientControl,
	})
	if err != nil {
		t.Fatalf("jetkvm.Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	recording := &e2eRecordingDevice{device: &clientDevice{client: client}}
	ocrEngine := &recordingOCREngine{output: e2eOCRText}
	return &e2eRig{
		fake:      fd,
		recording: recording,
		session:   newReadTextToolTestSession(t, recording, ocrEngine, serverControl),
		nextRPCID: 1,
	}
}

type e2eRPCExpectation struct {
	method string
	params map[string]any
}

type e2eToolCase struct {
	validArgs   any
	invalidArgs any

	wantText        string
	validateResult  func(*testing.T, *mcp.CallToolResult)
	wantDeviceCalls []string
	wantHID         [][]byte
	wantRPC         *e2eRPCExpectation

	invalidToolErrorContains []string
	invalidToolErrorExcludes []string
	unauthorizedContains     string
}

func e2eKeyboardFrame(t *testing.T, modifier byte, keys ...byte) []byte {
	t.Helper()
	if len(keys) > 6 {
		t.Fatalf("documented keyboard report supports at most 6 keys, got %d", len(keys))
	}
	return append([]byte{0x02, modifier}, keys...)
}

func e2ePointerFrame(t *testing.T, x, y int32, buttons byte) []byte {
	t.Helper()
	if x < 0 || x > 32767 || y < 0 || y > 32767 {
		t.Fatalf("documented pointer coordinate out of range: %d,%d", x, y)
	}
	return []byte{
		0x03,
		byte(uint32(x) >> 24), byte(uint32(x) >> 16), byte(uint32(x) >> 8), byte(x),
		byte(uint32(y) >> 24), byte(uint32(y) >> 16), byte(uint32(y) >> 8), byte(y),
		buttons,
	}
}

func e2eMouseFrame(dx, dy int8, buttons byte) []byte {
	return []byte{0x06, byte(dx), byte(dy), buttons}
}

func e2eNeutralFrames(t *testing.T) [][]byte {
	t.Helper()
	return [][]byte{
		{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		{0x06, 0x00, 0x00, 0x00},
	}
}

func e2eWithNeutralization(t *testing.T, frames ...[]byte) [][]byte {
	t.Helper()
	return append(append([][]byte(nil), frames...), e2eNeutralFrames(t)...)
}

func e2eListAllTools(t *testing.T, session *mcp.ClientSession) []*mcp.Tool {
	t.Helper()
	var tools []*mcp.Tool
	cursor := ""
	seenCursors := map[string]bool{}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		cancel()
		if err != nil {
			t.Fatalf("tools/list cursor %q: %v", cursor, err)
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == "" {
			return tools
		}
		if seenCursors[result.NextCursor] {
			t.Fatalf("tools/list repeated cursor %q", result.NextCursor)
		}
		seenCursors[result.NextCursor] = true
		cursor = result.NextCursor
	}
}

func e2eCallTool(t *testing.T, session *mcp.ClientSession, name string, args any) (*mcp.CallToolResult, error) {
	t.Helper()
	params := &mcp.CallToolParams{Name: name}
	if args != nil {
		params.Arguments = args
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 15*time.Second))
	defer cancel()
	return session.CallTool(ctx, params)
}

func assertE2ETextResult(t *testing.T, result *mcp.CallToolResult, want string) {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("content blocks = %d, want exactly one text block", len(result.Content))
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T, want *mcp.TextContent", result.Content[0])
	}
	if text.Text != want {
		t.Errorf("result text = %q, want %q", text.Text, want)
	}
	if result.StructuredContent != nil {
		t.Errorf("structured content = %#v, want nil", result.StructuredContent)
	}
}

func assertE2EStatusResult(t *testing.T, result *mcp.CallToolResult) {
	t.Helper()
	const wantText = "deviceId=fake-device firmwareVersion=0.4.7+dev rpcReachable=true"
	if len(result.Content) != 1 || toolResultText(t, result) != wantText {
		t.Fatalf("status content = %+v, want exactly %q", result.Content, wantText)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshalling status structured content: %v", err)
	}
	var got jetkvm.StatusResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding status structured content: %v", err)
	}
	want := jetkvm.StatusResult{
		DeviceID:        "fake-device",
		FirmwareVersion: "0.4.7+dev",
		RPCReachable:    true,
	}
	if got != want {
		t.Errorf("status structured content = %+v, want %+v", got, want)
	}
}

func assertE2EScreenshotResult(t *testing.T, result *mcp.CallToolResult) {
	t.Helper()
	if len(result.Content) != 2 {
		t.Fatalf("screenshot content blocks = %d, want text + image", len(result.Content))
	}
	metadata := screenshotMetadata(t, result)
	if metadata.Width != 32 || metadata.Height != 32 ||
		metadata.SourceWidth == nil || *metadata.SourceWidth != 32 ||
		metadata.SourceHeight == nil || *metadata.SourceHeight != 32 ||
		metadata.Format != "png" || metadata.MIMEType != "image/png" ||
		metadata.Quality != nil || !metadata.Fresh {
		t.Errorf("screenshot structured content = %+v", metadata)
	}
	if _, err := time.Parse(time.RFC3339Nano, metadata.CapturedAt); err != nil {
		t.Errorf("capturedAt = %q, want RFC3339Nano: %v", metadata.CapturedAt, err)
	}
	wantText := fmt.Sprintf(
		"width=32 height=32 format=png mimeType=image/png sourceWidth=32 sourceHeight=32 capturedAt=%s fresh=true",
		metadata.CapturedAt,
	)
	if got := toolResultText(t, result); got != wantText {
		t.Errorf("screenshot result text = %q, want %q", got, wantText)
	}
	image := screenshotImageContent(t, result)
	if image.MIMEType != "image/png" || len(image.Data) == 0 {
		t.Fatalf("screenshot image = mime %q, %d bytes", image.MIMEType, len(image.Data))
	}
	config, err := png.DecodeConfig(bytes.NewReader(image.Data))
	if err != nil {
		t.Fatalf("decoding screenshot PNG header: %v", err)
	}
	if config.Width != 32 || config.Height != 32 {
		t.Errorf("screenshot PNG = %dx%d, want 32x32", config.Width, config.Height)
	}
}

func assertE2EReadTextResult(t *testing.T, result *mcp.CallToolResult) {
	t.Helper()
	if len(result.Content) != 1 || toolResultText(t, result) != e2eOCRText {
		t.Fatalf("read-text content = %+v, want exactly %q", result.Content, e2eOCRText)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshalling read-text structured content: %v", err)
	}
	var metadata struct {
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		SourceWidth  int    `json:"sourceWidth"`
		SourceHeight int    `json:"sourceHeight"`
		CapturedAt   string `json:"capturedAt"`
		Fresh        bool   `json:"fresh"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("decoding read-text structured content: %v", err)
	}
	if metadata.Width != 32 || metadata.Height != 32 ||
		metadata.SourceWidth != 32 || metadata.SourceHeight != 32 || !metadata.Fresh {
		t.Errorf("read-text structured content = %+v", metadata)
	}
	if _, err := time.Parse(time.RFC3339Nano, metadata.CapturedAt); err != nil {
		t.Errorf("read-text capturedAt = %q, want RFC3339Nano: %v", metadata.CapturedAt, err)
	}
}

func assertE2EWaitStableResult(t *testing.T, result *mcp.CallToolResult) {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("wait-stable content blocks = %d, want one", len(result.Content))
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshalling wait-stable structured content: %v", err)
	}
	var metadata struct {
		Settled             bool    `json:"settled"`
		FramesSampled       int     `json:"framesSampled"`
		FinalChangeFraction float64 `json:"finalChangeFraction"`
		Elapsed             string  `json:"elapsed"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("decoding wait-stable structured content: %v", err)
	}
	if !metadata.Settled || metadata.FramesSampled != 2 || metadata.FinalChangeFraction != 0 {
		t.Errorf("wait-stable structured content = %+v", metadata)
	}
	if elapsed, err := time.ParseDuration(metadata.Elapsed); err != nil || elapsed <= 0 {
		t.Errorf("wait-stable elapsed = %q, want a positive duration: %v", metadata.Elapsed, err)
	}
	wantText := fmt.Sprintf(
		"settled=true framesSampled=2 finalChangeFraction=0 elapsed=%s",
		metadata.Elapsed,
	)
	if got := toolResultText(t, result); got != wantText {
		t.Errorf("wait-stable result text = %q, want %q", got, wantText)
	}
}

func assertE2EWaitForTextResult(t *testing.T, result *mcp.CallToolResult) {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("wait-for-text content blocks = %d, want one", len(result.Content))
	}
	metadata := decodeWaitForTextMetadata(t, result)
	if !metadata.Matched || metadata.Match != "READY" || metadata.TimedOut || metadata.FrameCount != 1 {
		t.Errorf("wait-for-text structured content = %+v", metadata)
	}
	if _, err := time.ParseDuration(metadata.Elapsed); err != nil {
		t.Errorf("wait-for-text elapsed = %q, want duration: %v", metadata.Elapsed, err)
	}
	wantText := fmt.Sprintf(
		"matched=true match=%q timedOut=false elapsed=%s frameCount=1",
		"READY", metadata.Elapsed,
	)
	if got := toolResultText(t, result); got != wantText {
		t.Errorf("wait-for-text result text = %q, want %q", got, wantText)
	}
}

func assertE2ERPCFrame(t *testing.T, rig *e2eRig, want e2eRPCExpectation) {
	t.Helper()
	frame, isString := rig.fake.nextRPCWireFrame(t)
	if !isString {
		t.Error("device RPC request used a binary WebRTC message, want text")
	}
	var request struct {
		JSONRPC string         `json:"jsonrpc"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params,omitempty"`
		ID      int64          `json:"id"`
	}
	if err := json.Unmarshal(frame, &request); err != nil {
		t.Fatalf("device RPC frame is not JSON: %q: %v", frame, err)
	}
	if request.ID != rig.nextRPCID {
		t.Errorf("device RPC id = %d, want %d", request.ID, rig.nextRPCID)
	}
	wantFrame, err := json.Marshal(struct {
		JSONRPC string         `json:"jsonrpc"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params,omitempty"`
		ID      int64          `json:"id"`
	}{
		JSONRPC: "2.0",
		Method:  want.method,
		Params:  want.params,
		ID:      rig.nextRPCID,
	})
	if err != nil {
		t.Fatalf("marshalling expected RPC frame: %v", err)
	}
	if !bytes.Equal(frame, wantFrame) {
		t.Errorf("device RPC frame = %s, want exact bytes %s", frame, wantFrame)
	}
	rig.nextRPCID++
}

func assertE2EWireDelta(t *testing.T, rig *e2eRig, beforeRPC, beforeHID int, tc e2eToolCase) {
	t.Helper()
	for i, want := range tc.wantHID {
		got, isString := rig.fake.nextHIDWireFrame(t)
		if isString {
			t.Errorf("HID frame %d used a text WebRTC message, want binary", i+1)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("HID frame %d = % x, want exact bytes % x", i+1, got, want)
		}
	}
	if tc.wantRPC != nil {
		assertE2ERPCFrame(t, rig, *tc.wantRPC)
	}
	// Pion invokes the remote OnMessage callback asynchronously. Consuming all
	// expected frames synchronizes the positive path; this small quiet window
	// catches an unexpected trailing frame without racing that callback.
	timer := time.NewTimer(30 * time.Millisecond)
	<-timer.C
	afterRPC, afterHID := rig.fake.wireCounts()
	wantRPC := beforeRPC
	if tc.wantRPC != nil {
		wantRPC++
	}
	wantHID := beforeHID + len(tc.wantHID)
	if afterRPC != wantRPC || afterHID != wantHID {
		t.Errorf("device wire counts = rpc %d hid %d, want rpc %d hid %d", afterRPC, afterHID, wantRPC, wantHID)
	}
	if len(rig.fake.rpcFrames) != 0 || len(rig.fake.hidFrames) != 0 {
		t.Errorf("unconsumed device frames = rpc %d hid %d", len(rig.fake.rpcFrames), len(rig.fake.hidFrames))
	}
}

func assertE2EProtocolInvalid(t *testing.T, result *mcp.CallToolResult, err error) {
	t.Helper()
	if result != nil {
		t.Errorf("invalid call result = %+v, want nil protocol result", result)
	}
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("invalid call error = %v, want *jsonrpc.Error", err)
	}
	if rpcErr.Code != jsonrpc.CodeInvalidParams || rpcErr.Message != invalidToolArgumentsMessage {
		t.Errorf("invalid call error = code %d message %q, want InvalidParams/%q",
			rpcErr.Code, rpcErr.Message, invalidToolArgumentsMessage)
	}
}

func buildE2EToolCases(t *testing.T) (map[string]e2eToolCase, []string) {
	t.Helper()
	neutral := e2eNeutralFrames(t)
	keypress := e2eKeyboardFrame(t, 0x02, 0x04)
	typeA := e2eKeyboardFrame(t, 0, 0x04)
	typeShiftA := e2eKeyboardFrame(t, jetkvm.ModifierLeftShift, 0x04)
	combo := e2eKeyboardFrame(t,
		jetkvm.ModifierLeftControl|jetkvm.ModifierLeftAlt,
		jetkvm.KeyUsageDelete,
	)
	sequenceCtrlC := e2eKeyboardFrame(t, jetkvm.ModifierLeftControl, jetkvm.KeyUsageC)
	sequenceAltTab := e2eKeyboardFrame(t, jetkvm.ModifierLeftAlt, jetkvm.KeyUsageTab)
	pointer := e2ePointerFrame(t, 123, 456, 3)
	mouseButtonPress := e2eMouseFrame(0, 0, jetkvm.MouseButtonRight)
	clickPress := e2ePointerFrame(t, 321, 654, 2)
	clickRelease := e2ePointerFrame(t, 321, 654, 0)
	doublePress := e2ePointerFrame(t, 111, 222, 1)
	doubleRelease := e2ePointerFrame(t, 111, 222, 0)
	dragReports := []jetkvm.PointerDragReport{
		{X: 0, Y: 0, Buttons: 1},
		{X: 3, Y: 2, Buttons: 1},
		{X: 6, Y: 4, Buttons: 1},
		{X: 9, Y: 6, Buttons: 1},
		{X: 9, Y: 6, Buttons: 0},
	}
	var dragFrames [][]byte
	for _, report := range dragReports {
		dragFrames = append(dragFrames, e2ePointerFrame(t, int32(report.X), int32(report.Y), byte(report.Buttons)))
	}
	dragFrames = append(dragFrames, neutral...)

	cases := map[string]e2eToolCase{
		"jetkvm_status": {
			validArgs:       map[string]any{},
			invalidArgs:     map[string]any{"unexpected": 1},
			validateResult:  assertE2EStatusResult,
			wantDeviceCalls: []string{e2eCall("status", nil)},
			wantRPC:         &e2eRPCExpectation{method: "ping"},
		},
		"jetkvm_screenshot": {
			validArgs:       map[string]any{},
			invalidArgs:     map[string]any{"format": "gif"},
			validateResult:  assertE2EScreenshotResult,
			wantDeviceCalls: []string{e2eCall("captureScreenshot", nil)},
		},
		"jetkvm_read_text": {
			validArgs:       map[string]any{},
			invalidArgs:     map[string]any{"scale": 0},
			validateResult:  assertE2EReadTextResult,
			wantDeviceCalls: []string{e2eCall("captureScreenshot", nil)},
		},
		"jetkvm_wait_stable": {
			validArgs: map[string]any{
				"threshold": 1, "stable_frames": 1, "poll_interval_ms": 0,
			},
			invalidArgs:    map[string]any{"threshold": -1},
			validateResult: assertE2EWaitStableResult,
			wantDeviceCalls: []string{e2eCall("waitStable", e2eWaitStablePayload(jetkvm.WaitStableOptions{
				Threshold:    func() *float64 { value := 1.0; return &value }(),
				StableFrames: func() *int { value := 1; return &value }(),
				PollInterval: func() *time.Duration { value := time.Duration(0); return &value }(),
			}))},
		},
		"jetkvm_wait_for_text": {
			validArgs: map[string]any{
				"text": "READY", "interval_ms": 100, "timeout_ms": 1000,
			},
			invalidArgs:     map[string]any{"text": ""},
			validateResult:  assertE2EWaitForTextResult,
			wantDeviceCalls: []string{e2eCall("captureScreenshot", nil)},
		},
		"jetkvm_keypress": {
			validArgs:            map[string]any{"key": 4, "modifier": 2},
			invalidArgs:          map[string]any{"key": 256},
			wantText:             "sent keypress key=4 modifier=2",
			wantDeviceCalls:      []string{e2eCall("keypress", map[string]any{"key": byte(4), "modifier": byte(2)})},
			wantHID:              e2eWithNeutralization(t, keypress),
			unauthorizedContains: "control was not enabled",
		},
		"jetkvm_type": {
			validArgs:   map[string]any{"text": "aA", "delay_ms": 0},
			invalidArgs: map[string]any{"text": "\u00e9"},
			wantText:    "typed runes=2 delay_ms=0",
			wantDeviceCalls: []string{
				e2eCall("keypress", map[string]any{"key": byte(4), "modifier": byte(0)}),
				e2eCall("keypress", map[string]any{"key": byte(4), "modifier": byte(jetkvm.ModifierLeftShift)}),
			},
			wantHID: append(
				e2eWithNeutralization(t, typeA),
				e2eWithNeutralization(t, typeShiftA)...,
			),
			invalidToolErrorContains: []string{"position 1", "category: Ll", "US keyboard layout"},
			invalidToolErrorExcludes: []string{"é", "'é'", "U+00E9"},
			unauthorizedContains:     "control was not enabled",
		},
		"jetkvm_key_combo": {
			validArgs:   map[string]any{"combo": "ctrl+alt+del"},
			invalidArgs: map[string]any{"combo": "definitely-not-a-combo"},
			wantText:    "sent key combo modifier=5 keys=[76]",
			wantDeviceCalls: []string{e2eCall("keyCombo", map[string]any{
				"keys": []byte{jetkvm.KeyUsageDelete}, "modifier": byte(5),
			})},
			wantHID:                  e2eWithNeutralization(t, combo),
			invalidToolErrorContains: []string{"unknown key combo", "ctrl+alt+del"},
			unauthorizedContains:     "control was not enabled",
		},
		"jetkvm_hold_key": {
			validArgs:   map[string]any{"combo": "ctrl+c", "hold_ms": 1},
			invalidArgs: map[string]any{"combo": "ctrl+c", "hold_ms": jetkvm.MaxHoldMS + 1},
			wantText:    "held key combo modifier=1 keys=[6] hold_ms=1",
			wantDeviceCalls: []string{e2eCall("holdKey", map[string]any{
				"holdMS": 1, "keys": []byte{jetkvm.KeyUsageC}, "modifier": byte(jetkvm.ModifierLeftControl),
			})},
			wantHID:              e2eWithNeutralization(t, sequenceCtrlC),
			unauthorizedContains: "control was not enabled",
		},
		"jetkvm_key_sequence": {
			validArgs: map[string]any{
				"combos": []string{"ctrl+c", "alt+tab"}, "delay_ms": 0,
			},
			invalidArgs: map[string]any{"combos": []string{"ctrl+c", "definitely-not-a-combo"}},
			wantText:    "sent key sequence combos=2 delay_ms=0",
			wantDeviceCalls: []string{
				e2eCall("keyCombo", map[string]any{"keys": []byte{jetkvm.KeyUsageC}, "modifier": byte(jetkvm.ModifierLeftControl)}),
				e2eCall("keyCombo", map[string]any{"keys": []byte{jetkvm.KeyUsageTab}, "modifier": byte(jetkvm.ModifierLeftAlt)}),
			},
			wantHID: append(
				e2eWithNeutralization(t, sequenceCtrlC),
				e2eWithNeutralization(t, sequenceAltTab)...,
			),
			invalidToolErrorContains: []string{"combos[1]", "unknown key combo"},
			unauthorizedContains:     "control was not enabled",
		},
		"jetkvm_mouse_move": {
			validArgs:   map[string]any{"x": 123, "y": 456, "buttons": 3},
			invalidArgs: map[string]any{"x": -1, "y": 0},
			wantText:    "moved mouse to x=123 y=456 buttons=3",
			wantDeviceCalls: []string{e2eCall("mouseMove", map[string]any{
				"buttons": byte(3), "x": int32(123), "y": int32(456),
			})},
			wantHID:              e2eWithNeutralization(t, pointer),
			unauthorizedContains: "control was not enabled",
		},
		"jetkvm_mouse_button": {
			validArgs:   map[string]any{"button": "right", "action": "press"},
			invalidArgs: map[string]any{"button": "side", "action": "press"},
			wantText:    "pressed mouse button=right",
			wantDeviceCalls: []string{e2eCall("mouseButton", map[string]any{
				"button": jetkvm.MouseButtonRight, "pressed": true,
			})},
			wantHID:              [][]byte{mouseButtonPress},
			unauthorizedContains: "control was not enabled",
		},
		"jetkvm_scroll": {
			validArgs:       map[string]any{"dx": -3, "dy": 4},
			invalidArgs:     map[string]any{"dx": 0, "dy": 0},
			wantText:        "scrolled mouse dx=-3 dy=4",
			wantDeviceCalls: []string{e2eCall("scroll", map[string]any{"dx": int8(-3), "dy": int8(4)})},
			wantRPC: &e2eRPCExpectation{
				method: "wheelReport",
				params: map[string]any{"wheelX": int8(-3), "wheelY": int8(4)},
			},
			invalidToolErrorContains: []string{"nothing would be scrolled"},
			unauthorizedContains:     "control is not enabled",
		},
		"jetkvm_click": {
			validArgs:   map[string]any{"x": 321, "y": 654, "button": 2},
			invalidArgs: map[string]any{"x": 321, "y": 654, "button": jetkvm.MaxPointerButtonMask + 1},
			wantText:    "clicked mouse at x=321 y=654 button=2",
			wantDeviceCalls: []string{
				e2eCall("mouseMove", map[string]any{"buttons": byte(2), "x": int32(321), "y": int32(654)}),
				e2eCall("mouseMove", map[string]any{"buttons": byte(0), "x": int32(321), "y": int32(654)}),
			},
			wantHID: append(
				e2eWithNeutralization(t, clickPress),
				e2eWithNeutralization(t, clickRelease)...,
			),
			unauthorizedContains: "control was not enabled",
		},
		"jetkvm_drag": {
			validArgs: map[string]any{
				"x1": 0, "y1": 0, "x2": 9, "y2": 6, "button": 1, "steps": 2,
			},
			invalidArgs: map[string]any{
				"x1": 0, "y1": 0, "x2": 9, "y2": 6, "steps": jetkvm.MaxDragSteps + 1,
			},
			wantText:             "dragged mouse from x1=0 y1=0 to x2=9 y2=6 button=1 steps=2",
			wantDeviceCalls:      []string{e2eCall("drag", dragReports)},
			wantHID:              dragFrames,
			unauthorizedContains: "control was not enabled",
		},
		"jetkvm_double_click": {
			validArgs:   map[string]any{"x": 111, "y": 222, "button": 1},
			invalidArgs: map[string]any{"x": -1, "y": 222},
			wantText:    "double-clicked mouse at x=111 y=222 button=1",
			wantDeviceCalls: []string{
				e2eCall("mouseMove", map[string]any{"buttons": byte(1), "x": int32(111), "y": int32(222)}),
				e2eCall("mouseMove", map[string]any{"buttons": byte(0), "x": int32(111), "y": int32(222)}),
				e2eCall("mouseMove", map[string]any{"buttons": byte(1), "x": int32(111), "y": int32(222)}),
				e2eCall("mouseMove", map[string]any{"buttons": byte(0), "x": int32(111), "y": int32(222)}),
			},
			wantHID: append(
				append(
					e2eWithNeutralization(t, doublePress),
					e2eWithNeutralization(t, doubleRelease)...,
				),
				append(
					e2eWithNeutralization(t, doublePress),
					e2eWithNeutralization(t, doubleRelease)...,
				)...,
			),
			unauthorizedContains: "control was not enabled",
		},
		"jetkvm_release_all": {
			validArgs:            map[string]any{},
			invalidArgs:          map[string]any{"unexpected": 1},
			wantText:             "released all keys and mouse buttons (no cursor movement)",
			wantDeviceCalls:      []string{e2eCall("releaseAll", nil)},
			wantHID:              neutral,
			unauthorizedContains: "control is not available",
		},
	}

	order := []string{
		"jetkvm_status",
		"jetkvm_screenshot",
		"jetkvm_read_text",
		"jetkvm_wait_stable",
		"jetkvm_wait_for_text",
		"jetkvm_keypress",
		"jetkvm_type",
		"jetkvm_key_combo",
		"jetkvm_hold_key",
		"jetkvm_key_sequence",
		"jetkvm_mouse_move",
		"jetkvm_scroll",
		"jetkvm_click",
		"jetkvm_drag",
		"jetkvm_double_click",
		// Keep the retained press immediately before release-all: together these
		// cases prove a press is not neutralized on return and release-all clears it.
		"jetkvm_mouse_button",
		"jetkvm_release_all",
	}
	return cases, order
}

func TestMCPToolCatalogEndToEnd(t *testing.T) {
	controlRig := newE2ERig(t, true, true)
	readOnlyRig := newE2ERig(t, false, false)
	// This deliberately inconsistent server/device combination exercises the
	// clientDevice defense in depth: handlers are dispatchable, but the real
	// JetKVM client never received control authorization and cannot acquire a
	// lease or send the legacy wheel RPC.
	leaseDeniedSession := newTestServerSessionForDevice(t, readOnlyRig.recording, true)

	cases, order := buildE2EToolCases(t)
	fullCatalog := e2eListAllTools(t, controlRig.session)
	readOnlyCatalog := e2eListAllTools(t, readOnlyRig.session)

	fullByName := make(map[string]*mcp.Tool, len(fullCatalog))
	for _, tool := range fullCatalog {
		if _, duplicate := fullByName[tool.Name]; duplicate {
			t.Fatalf("control-enabled tools/list repeated %q", tool.Name)
		}
		fullByName[tool.Name] = tool
		if _, covered := cases[tool.Name]; !covered {
			t.Errorf("registered tool %q has no end-to-end case", tool.Name)
		}
	}
	for name := range cases {
		if _, advertised := fullByName[name]; !advertised {
			t.Errorf("end-to-end case %q has no registered tool", name)
		}
	}
	if len(order) != len(cases) {
		t.Fatalf("explicit execution order has %d tools, case map has %d", len(order), len(cases))
	}
	seenOrder := map[string]bool{}
	for _, name := range order {
		if seenOrder[name] {
			t.Errorf("execution order repeats %q", name)
		}
		seenOrder[name] = true
		if _, ok := cases[name]; !ok {
			t.Errorf("execution order names missing case %q", name)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	readOnlyNames := make(map[string]bool, len(readOnlyCatalog))
	for _, tool := range readOnlyCatalog {
		readOnlyNames[tool.Name] = true
		if _, present := fullByName[tool.Name]; !present {
			t.Errorf("read-only tool %q is absent from the full catalog", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("read-only catalog tool %q annotations = %+v", tool.Name, tool.Annotations)
		}
	}

	gatedNames := map[string]bool{}
	controlNames := map[string]bool{}
	for name, tool := range fullByName {
		if readOnlyNames[name] {
			continue
		}
		gatedNames[name] = true
		if tool.Annotations == nil {
			t.Errorf("gated tool %q has no annotations", name)
			continue
		}
		if tool.Annotations.ReadOnlyHint {
			if (name != "jetkvm_wait_stable" && name != "jetkvm_wait_for_text") ||
				tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint ||
				tool.Annotations.IdempotentHint {
				t.Errorf("gated read-only tool %q annotations = %+v", name, tool.Annotations)
			}
			continue
		}
		controlNames[name] = true
		if !strings.HasPrefix(tool.Description, "DANGEROUS:") ||
			tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint ||
			tool.Annotations.IdempotentHint {
			t.Errorf("dangerous control tool %q metadata = description %q annotations %+v", name, tool.Description, tool.Annotations)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	t.Run("valid calls and exact device wire", func(t *testing.T) {
		for _, name := range order {
			name := name
			t.Run(name, func(t *testing.T) {
				tc := cases[name]
				rig := controlRig
				if readOnlyNames[name] {
					rig = readOnlyRig
				}
				beforeCalls := rig.recording.snapshot()
				beforeRPC, beforeHID := rig.fake.wireCounts()
				result, err := e2eCallTool(t, rig.session, name, tc.validArgs)
				if err != nil {
					t.Fatalf("valid tools/call failed: %v", err)
				}
				if result == nil || result.IsError {
					t.Fatalf("valid tools/call result = %+v", result)
				}
				if tc.validateResult != nil {
					tc.validateResult(t, result)
				} else {
					assertE2ETextResult(t, result, tc.wantText)
				}
				afterCalls := rig.recording.snapshot()
				if got := afterCalls[len(beforeCalls):]; !reflect.DeepEqual(got, tc.wantDeviceCalls) {
					t.Errorf("device calls = %v, want %v", got, tc.wantDeviceCalls)
				}
				assertE2EWireDelta(t, rig, beforeRPC, beforeHID, tc)
			})
		}
	})

	t.Run("invalid arguments never touch device", func(t *testing.T) {
		for _, name := range order {
			name := name
			t.Run(name, func(t *testing.T) {
				tc := cases[name]
				rig := controlRig
				if readOnlyNames[name] {
					rig = readOnlyRig
				}
				beforeCalls := rig.recording.snapshot()
				beforeRPC, beforeHID := rig.fake.wireCounts()
				result, err := e2eCallTool(t, rig.session, name, tc.invalidArgs)
				if len(tc.invalidToolErrorContains) == 0 {
					assertE2EProtocolInvalid(t, result, err)
				} else {
					if err != nil {
						t.Fatalf("semantic invalid call returned protocol error: %v", err)
					}
					if result == nil || !result.IsError {
						t.Fatalf("semantic invalid call result = %+v, want MCP tool error", result)
					}
					message := toolResultText(t, result)
					for _, want := range tc.invalidToolErrorContains {
						if !strings.Contains(message, want) {
							t.Error("semantic tool error omitted expected safe context")
						}
					}
					for _, excluded := range tc.invalidToolErrorExcludes {
						if strings.Contains(message, excluded) {
							t.Error("semantic tool error reflected caller-supplied input")
						}
					}
				}
				if afterCalls := rig.recording.snapshot(); !reflect.DeepEqual(afterCalls, beforeCalls) {
					t.Errorf("invalid arguments crossed device boundary: before %v after %v", beforeCalls, afterCalls)
				}
				timer := time.NewTimer(30 * time.Millisecond)
				<-timer.C
				afterRPC, afterHID := rig.fake.wireCounts()
				if afterRPC != beforeRPC || afterHID != beforeHID {
					t.Errorf("invalid arguments wrote device wire: before rpc/hid %d/%d after %d/%d",
						beforeRPC, beforeHID, afterRPC, afterHID)
				}
			})
		}
	})

	t.Run("control authorization gates", func(t *testing.T) {
		for _, name := range order {
			if !gatedNames[name] {
				continue
			}
			name := name
			t.Run(name, func(t *testing.T) {
				tc := cases[name]
				beforeCalls := readOnlyRig.recording.snapshot()
				beforeRPC, beforeHID := readOnlyRig.fake.wireCounts()

				result, err := e2eCallTool(t, readOnlyRig.session, name, tc.validArgs)
				if result != nil {
					t.Errorf("structurally gated call result = %+v, want nil", result)
				}
				var rpcErr *jsonrpc.Error
				if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams ||
					rpcErr.Message != fmt.Sprintf("unknown tool %q", name) {
					t.Errorf("structurally gated error = %v, want InvalidParams unknown-tool error", err)
				}
				if afterCalls := readOnlyRig.recording.snapshot(); !reflect.DeepEqual(afterCalls, beforeCalls) {
					t.Errorf("--allow-control gate touched device: before %v after %v", beforeCalls, afterCalls)
				}
				afterRPC, afterHID := readOnlyRig.fake.wireCounts()
				if afterRPC != beforeRPC || afterHID != beforeHID {
					t.Errorf("structurally gated call wrote device wire: before rpc/hid %d/%d after %d/%d",
						beforeRPC, beforeHID, afterRPC, afterHID)
				}
				if !controlNames[name] {
					return
				}

				result, err = e2eCallTool(t, leaseDeniedSession, name, tc.validArgs)
				if err != nil {
					t.Fatalf("lease-denied dispatch returned protocol error: %v", err)
				}
				if result == nil || !result.IsError {
					t.Fatalf("lease-denied result = %+v, want MCP tool error", result)
				}
				if got := toolResultText(t, result); !strings.Contains(got, tc.unauthorizedContains) {
					t.Errorf("lease-denied result = %q, want %q", got, tc.unauthorizedContains)
				}
				afterRPC, afterHID = readOnlyRig.fake.wireCounts()
				if afterRPC != beforeRPC || afterHID != beforeHID {
					t.Errorf("unauthorized call wrote device wire: before rpc/hid %d/%d after %d/%d",
						beforeRPC, beforeHID, afterRPC, afterHID)
				}
			})
		}
	})
}
