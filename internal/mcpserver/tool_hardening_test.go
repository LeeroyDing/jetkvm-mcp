package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func TestControlToolAdmissionRejectsEveryConcurrentControlCall(t *testing.T) {
	started := make(chan struct{})
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	t.Cleanup(func() { unblockOnce.Do(func() { close(unblock) }) })

	var (
		mu       sync.Mutex
		sentKeys []byte
	)
	device := &mockDevice{
		statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
			return jetkvm.StatusResult{DeviceID: "status-while-control-active", FirmwareVersion: "test", RPCReachable: true}, nil
		},
		waitStableFunc: func(context.Context, jetkvm.WaitStableOptions) (jetkvm.WaitStableResult, error) {
			return jetkvm.WaitStableResult{Settled: true}, nil
		},
		keypressFunc: func(_ context.Context, _ byte, key byte) error {
			mu.Lock()
			sentKeys = append(sentKeys, key)
			call := len(sentKeys)
			mu.Unlock()
			if call == 1 {
				close(started)
				<-unblock
			}
			return nil
		},
	}
	cs := newTestServerSessionForDevice(t, device, true)

	type callResult struct {
		result *mcp.CallToolResult
		err    error
	}
	typeDone := make(chan callResult, 1)
	go func() {
		result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_type",
			Arguments: map[string]any{"text": "ab"},
		})
		typeDone <- callResult{result: result, err: err}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first control call did not reach the device")
	}

	// Both always-advertised status and the opt-in-but-read-only stability gate
	// remain callable while the control admission token is occupied.
	for _, name := range []string{"jetkvm_status", "jetkvm_wait_stable"} {
		result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name})
		if err != nil || result.IsError {
			t.Fatalf("read-only %s while control active = %+v, %v", name, result, err)
		}
	}

	concurrentCalls := []struct {
		name string
		args map[string]any
	}{
		{name: "jetkvm_keypress", args: map[string]any{"key": 4}},
		{name: "jetkvm_type", args: map[string]any{"text": "c"}},
		{name: "jetkvm_key_combo", args: map[string]any{"combo": "ctrl+c"}},
		{name: "jetkvm_hold_key", args: map[string]any{"combo": "ctrl+c", "hold_ms": 1}},
		{name: "jetkvm_key_sequence", args: map[string]any{"combos": []string{"ctrl+c", "enter"}}},
		{name: "jetkvm_mouse_move", args: map[string]any{"x": 1, "y": 2}},
		{name: "jetkvm_mouse_button", args: map[string]any{"button": "left", "action": "press"}},
		{name: "jetkvm_scroll", args: map[string]any{"dy": 1}},
		{name: "jetkvm_click", args: map[string]any{"x": 1, "y": 2}},
		{name: "jetkvm_drag", args: map[string]any{"x1": 1, "y1": 2, "x2": 3, "y2": 4}},
		{name: "jetkvm_double_click", args: map[string]any{"x": 1, "y": 2}},
		{name: "jetkvm_release_all"},
	}
	for _, call := range concurrentCalls {
		t.Run(call.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			params := &mcp.CallToolParams{Name: call.name}
			if call.args != nil {
				params.Arguments = call.args
			}
			result, err := cs.CallTool(ctx, params)
			if err != nil {
				t.Fatalf("concurrent call returned protocol error: %v", err)
			}
			if !result.IsError {
				t.Fatalf("concurrent call succeeded: %+v", result.Content)
			}
			if text := toolResultText(t, result); text != jetkvm.ErrControlHeld.Error() {
				t.Fatalf("concurrent call error = %q, want %q", text, jetkvm.ErrControlHeld)
			}
		})
	}

	// Semantic validation precedes admission: a malformed operation reports
	// its own stable error even while another valid operation is active.
	invalid, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_key_combo",
		Arguments: map[string]any{"combo": "not-a-combo"},
	})
	if err != nil || !invalid.IsError {
		t.Fatalf("invalid combo while control active = %+v, %v", invalid, err)
	}
	if text := toolResultText(t, invalid); !strings.Contains(text, "unknown key combo") || strings.Contains(text, jetkvm.ErrControlHeld.Error()) {
		t.Fatalf("invalid combo error = %q, want semantic validation error", text)
	}

	unblockOnce.Do(func() { close(unblock) })
	select {
	case completed := <-typeDone:
		if completed.err != nil || completed.result.IsError {
			t.Fatalf("first control call = %+v, %v", completed.result, completed.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first control call did not finish after device release")
	}

	// The token is released when the complete composite call returns.
	result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_keypress",
		Arguments: map[string]any{"key": 6},
	})
	if err != nil || result.IsError {
		t.Fatalf("control call after release = %+v, %v", result, err)
	}
	mu.Lock()
	gotKeys := append([]byte(nil), sentKeys...)
	mu.Unlock()
	if want := []byte{4, 5, 6}; !reflect.DeepEqual(gotKeys, want) {
		t.Fatalf("sent keys = %v, want contiguous first operation followed by next call %v", gotKeys, want)
	}
}

func TestErrorResultNormalizesRawContextErrors(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		fmt.Errorf("waiting between keys: %w", context.Canceled),
	} {
		result, _, protocolErr := errorResult(err)
		if protocolErr != nil {
			t.Fatalf("errorResult(%v) protocol error: %v", err, protocolErr)
		}
		text := toolResultText(t, result)
		for _, want := range []string{"jetkvm: timeout:", "during MCP tool call", "call deadline expired"} {
			if !strings.Contains(text, want) {
				t.Errorf("normalized context error %q does not contain %q", text, want)
			}
		}
	}
}

func TestNormalizeToolErrorPreservesClassifiedAndSafetyErrors(t *testing.T) {
	existing := &jetkvm.DeviceError{
		Kind:      jetkvm.ErrorKindUnreachable,
		Operation: "existing operation",
		Detail:    "existing detail",
	}
	if got := normalizeToolError(existing); got != existing {
		t.Fatalf("existing DeviceError was replaced: got %v, want identical %v", got, existing)
	}

	joined := errors.Join(context.DeadlineExceeded, jetkvm.ErrNeutralizeUnverified)
	got := normalizeToolError(joined)
	if jetkvm.ErrorKindOf(got) != jetkvm.ErrorKindTimeout {
		t.Fatalf("joined context error kind = %q, want timeout: %v", jetkvm.ErrorKindOf(got), got)
	}
	if !errors.Is(got, jetkvm.ErrNeutralizeUnverified) {
		t.Fatalf("normalized timeout lost ErrNeutralizeUnverified: %v", got)
	}
}

func TestStatusStructuredContentUsesLowerCamelKeys(t *testing.T) {
	device := &mockDevice{statusFunc: func(context.Context) (jetkvm.StatusResult, error) {
		return jetkvm.StatusResult{DeviceID: "device-1", FirmwareVersion: "1.2.3", RPCReachable: true}, nil
	}}
	cs := newTestServerSessionForDevice(t, device, false)
	result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "jetkvm_status"})
	if err != nil || result.IsError {
		t.Fatalf("status = %+v, %v", result, err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured status: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode structured status: %v", err)
	}
	want := map[string]any{
		"deviceId":        "device-1",
		"firmwareVersion": "1.2.3",
		"rpcReachable":    true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("structured status = %#v, want exact lowerCamel keys %#v", got, want)
	}
}

func TestHardenedToolSchemasAdvertiseDefaultsAndBounds(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	type property struct {
		Default   json.RawMessage `json:"default"`
		Maximum   *float64        `json:"maximum"`
		MaxLength *int            `json:"maxLength"`
		Items     *property       `json:"items"`
	}
	type schema struct {
		Properties map[string]property `json:"properties"`
	}
	schemas := make(map[string]schema, len(listed.Tools))
	for _, tool := range listed.Tools {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", tool.Name, err)
		}
		var decoded schema
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decode %s schema: %v", tool.Name, err)
		}
		schemas[tool.Name] = decoded
	}

	zeroDefaults := []struct {
		tool     string
		property string
	}{
		{tool: "jetkvm_keypress", property: "modifier"},
		{tool: "jetkvm_type", property: "delay_ms"},
		{tool: "jetkvm_key_sequence", property: "delay_ms"},
		{tool: "jetkvm_mouse_move", property: "buttons"},
	}
	for _, want := range zeroDefaults {
		if got := string(schemas[want.tool].Properties[want.property].Default); got != "0" {
			t.Errorf("%s.%s default = %q, want 0", want.tool, want.property, got)
		}
	}

	combo := schemas["jetkvm_key_combo"].Properties["combo"]
	if combo.MaxLength == nil || *combo.MaxLength != jetkvm.MaxKeyComboNameRunes {
		t.Errorf("key_combo.combo maxLength = %v, want %d", combo.MaxLength, jetkvm.MaxKeyComboNameRunes)
	}
	holdCombo := schemas["jetkvm_hold_key"].Properties["combo"]
	if holdCombo.MaxLength == nil || *holdCombo.MaxLength != jetkvm.MaxKeyComboNameRunes {
		t.Errorf("hold_key.combo maxLength = %v, want %d", holdCombo.MaxLength, jetkvm.MaxKeyComboNameRunes)
	}
	sequence := schemas["jetkvm_key_sequence"].Properties["combos"]
	if sequence.Items == nil || sequence.Items.MaxLength == nil || *sequence.Items.MaxLength != jetkvm.MaxKeyComboNameRunes {
		t.Errorf("key_sequence.combos item maxLength = %+v, want %d", sequence.Items, jetkvm.MaxKeyComboNameRunes)
	}
	stableFrames := schemas["jetkvm_wait_stable"].Properties["stable_frames"]
	if stableFrames.Maximum == nil || *stableFrames.Maximum != float64(jetkvm.MaxWaitStableFrames) {
		t.Errorf("wait_stable.stable_frames maximum = %v, want %d", stableFrames.Maximum, jetkvm.MaxWaitStableFrames)
	}
}

func TestWaitStableSchemaRejectsTypedIntOverflowBeforeDeviceCall(t *testing.T) {
	calls := 0
	device := &mockDevice{waitStableFunc: func(context.Context, jetkvm.WaitStableOptions) (jetkvm.WaitStableResult, error) {
		calls++
		return jetkvm.WaitStableResult{}, nil
	}}
	cs := newTestServerSessionForDevice(t, device, true)
	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "jetkvm_wait_stable",
		Arguments: map[string]any{
			"stable_frames": int64(jetkvm.MaxWaitStableFrames) + 1,
		},
	})
	if err == nil {
		t.Fatal("wait_stable accepted stable_frames beyond its portable typed range")
	}
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("overflow rejection = %v, want JSON-RPC InvalidParams", err)
	}
	if calls != 0 {
		t.Fatalf("overflow reached device %d times, want 0", calls)
	}
}
