package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

// FuzzDoubleClickToolArgumentValidation pins the MCP adapter boundary before
// coordinates and the button bitmask are narrowed to HID wire types. Invalid
// or incomplete arguments must be rejected before the first mouse report;
// valid arguments must produce exactly two press-release pairs. An explicit
// zero mask is invalid; omitting the field still selects the left-button
// default.
func FuzzDoubleClickToolArgumentValidation(f *testing.F) {
	for _, seed := range []struct {
		x, y, button                   int
		includeX, includeY, includeBtn bool
	}{
		{0, 0, 0, true, true, true},
		{jetkvm.MaxAbsoluteCoordinate, jetkvm.MaxAbsoluteCoordinate, jetkvm.MaxPointerButtonMask, true, true, true},
		{123, 456, 0, true, true, false},
		{-1, 0, 1, true, true, true},
		{0, -1, 1, true, true, true},
		{jetkvm.MaxAbsoluteCoordinate + 1, 0, 1, true, true, true},
		{0, jetkvm.MaxAbsoluteCoordinate + 1, 1, true, true, true},
		{0, 0, -1, true, true, true},
		{0, 0, jetkvm.MaxPointerButtonMask + 1, true, true, true},
		{0, 0, 1, false, true, true},
		{0, 0, 1, true, false, true},
		{math.MinInt, math.MaxInt, math.MaxInt, true, true, true},
	} {
		f.Add(seed.x, seed.y, seed.button, seed.includeX, seed.includeY, seed.includeBtn)
	}

	f.Fuzz(func(t *testing.T, x, y, button int, includeX, includeY, includeButton bool) {
		type pointerCall struct {
			x, y    int32
			buttons byte
		}
		var got []pointerCall
		device := &mockDevice{mouseMoveFunc: func(_ context.Context, x, y int32, buttons byte) error {
			got = append(got, pointerCall{x: x, y: y, buttons: buttons})
			return nil
		}}
		cs := newTestServerSessionForDevice(t, device, true)

		args := map[string]any{}
		if includeX {
			args["x"] = x
		}
		if includeY {
			args["y"] = y
		}
		if includeButton {
			args["button"] = button
		}

		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_double_click",
			Arguments: args,
		})
		wantValid := includeX && includeY &&
			x >= 0 && x <= jetkvm.MaxAbsoluteCoordinate &&
			y >= 0 && y <= jetkvm.MaxAbsoluteCoordinate &&
			(!includeButton || button >= 1 && button <= jetkvm.MaxPointerButtonMask)
		if !wantValid {
			if err == nil {
				t.Fatalf("double-click accepted invalid arguments %v", args)
			}
			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("double-click rejection for %v = %v, want JSON-RPC InvalidParams", args, err)
			}
			if len(got) != 0 {
				t.Fatalf("invalid arguments %v sent mouseMove calls %+v", args, got)
			}
			return
		}

		if err != nil {
			t.Fatalf("double-click rejected valid arguments %v: %v", args, err)
		}
		if res.IsError {
			t.Fatalf("double-click returned a tool error for valid arguments %v: %+v", args, res.Content)
		}
		wantButton := byte(button)
		if !includeButton {
			wantButton = 1
		}
		want := []pointerCall{
			{x: int32(x), y: int32(y), buttons: wantButton},
			{x: int32(x), y: int32(y), buttons: 0},
			{x: int32(x), y: int32(y), buttons: wantButton},
			{x: int32(x), y: int32(y), buttons: 0},
		}
		if len(got) != len(want) {
			t.Fatalf("mouseMove calls = %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("mouseMove call %d = %+v, want %+v", i+1, got[i], want[i])
			}
		}
	})
}

// FuzzMouseButtonToolArgumentValidation drives both required enum fields and
// the strict-object boundary with arbitrary strings. Only the six documented
// button/action pairs may cross the MCP adapter; every other shape must be a
// protocol-level InvalidParams rejection before the device is touched.
func FuzzMouseButtonToolArgumentValidation(f *testing.F) {
	for _, seed := range []struct {
		button, action                      string
		includeButton, includeAction, extra bool
	}{
		{"left", "press", true, true, false},
		{"right", "release", true, true, false},
		{"middle", "press", true, true, false},
		{"LEFT", "press", true, true, false},
		{"left", "PRESS", true, true, false},
		{"side", "click", true, true, false},
		{"left", "press", false, true, false},
		{"left", "press", true, false, false},
		{"left", "press", true, true, true},
		{"\x00left", "release\x00", true, true, false},
		{"\xff\xfe", "\xff", true, true, false},
	} {
		f.Add(seed.button, seed.action, seed.includeButton, seed.includeAction, seed.extra)
	}

	f.Fuzz(func(t *testing.T, button, action string, includeButton, includeAction, extra bool) {
		type mouseButtonCall struct {
			button  byte
			pressed bool
		}
		var got []mouseButtonCall
		device := &mockDevice{mouseButtonFunc: func(_ context.Context, button byte, pressed bool) error {
			got = append(got, mouseButtonCall{button: button, pressed: pressed})
			return nil
		}}
		cs := newTestServerSessionForDevice(t, device, true)

		args := map[string]any{}
		if includeButton {
			args["button"] = button
		}
		if includeAction {
			args["action"] = action
		}
		if extra {
			args["unexpected"] = true
		}

		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_mouse_button",
			Arguments: args,
		})
		validButton := button == "left" || button == "right" || button == "middle"
		validAction := action == "press" || action == "release"
		wantValid := includeButton && includeAction && !extra && validButton && validAction
		if !wantValid {
			if err == nil {
				t.Fatalf("mouse-button accepted invalid arguments %v", args)
			}
			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("mouse-button rejection for %v = %v, want JSON-RPC InvalidParams", args, err)
			}
			if len(got) != 0 {
				t.Fatalf("invalid arguments %v sent mouseButton calls %+v", args, got)
			}
			return
		}

		if err != nil {
			t.Fatalf("mouse-button rejected valid arguments %v: %v", args, err)
		}
		if res.IsError {
			t.Fatalf("mouse-button returned a tool error for valid arguments %v: %+v", args, res.Content)
		}
		if len(got) != 1 {
			t.Fatalf("mouseButton calls = %+v, want exactly one", got)
		}
		wantButton := map[string]byte{
			"left":   jetkvm.MouseButtonLeft,
			"right":  jetkvm.MouseButtonRight,
			"middle": jetkvm.MouseButtonMiddle,
		}[button]
		if got[0].button != wantButton || got[0].pressed != (action == "press") {
			t.Fatalf("mouseButton call = %+v, want button %d pressed %v", got[0], wantButton, action == "press")
		}
	})
}

// FuzzHoldKeyToolArgumentValidation covers the strict MCP argument surface.
// Only a complete, in-range, known chord may cross the adapter unchanged.
func FuzzHoldKeyToolArgumentValidation(f *testing.F) {
	for _, seed := range []struct {
		combo                     string
		holdMS                    int
		includeCombo, includeHold bool
		extra                     bool
	}{
		{"ctrl+c", 1, true, true, false},
		{"CTRL-C", jetkvm.MaxHoldMS, true, true, false},
		{"ctrl+c", jetkvm.MaxHoldMS + 1, true, true, false},
		{"ctrl+c", 0, true, true, false},
		{"ctrl+c", -1, true, true, false},
		{"ctrl+c", math.MaxInt, true, true, false},
		{"ctrl+c", 100, false, true, false},
		{"ctrl+c", 100, true, false, false},
		{"definitely-not-a-combo", 100, true, true, false},
		{"ctrl\uFF0Bc", 100, true, true, false},
		{"ctrl+c", 100, true, true, true},
		{strings.Repeat("0", jetkvm.MaxKeyComboNameRunes+1), 100, true, true, false},
	} {
		f.Add(seed.combo, seed.holdMS, seed.includeCombo, seed.includeHold, seed.extra)
	}

	f.Fuzz(func(t *testing.T, combo string, holdMS int, includeCombo, includeHold, extra bool) {
		type holdCall struct {
			modifier byte
			keys     []byte
			holdMS   int
		}
		var got []holdCall
		device := &mockDevice{holdKeyFunc: func(_ context.Context, modifier byte, keys []byte, holdMS int) error {
			got = append(got, holdCall{
				modifier: modifier,
				keys:     append([]byte(nil), keys...),
				holdMS:   holdMS,
			})
			return nil
		}}
		cs := newTestServerSessionForDevice(t, device, true)

		args := map[string]any{}
		if includeCombo {
			args["combo"] = combo
		}
		if includeHold {
			args["hold_ms"] = holdMS
		}
		if extra {
			args["unexpected"] = true
		}

		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_hold_key",
			Arguments: args,
		})
		schemaValid := includeCombo && includeHold && !extra &&
			utf8.RuneCountInString(combo) <= jetkvm.MaxKeyComboNameRunes &&
			holdMS >= 1 && holdMS <= jetkvm.MaxHoldMS
		if !schemaValid {
			if err == nil {
				t.Fatalf("hold-key accepted invalid arguments %v", args)
			}
			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("hold-key rejection for %v = %v, want JSON-RPC InvalidParams", args, err)
			}
			if len(got) != 0 {
				t.Fatalf("invalid arguments %v sent holdKey calls %+v", args, got)
			}
			return
		}

		modifier, keys, comboErr := jetkvm.ResolveKeyCombo(combo)
		if comboErr != nil {
			if err != nil {
				t.Fatalf("unknown combo %q returned protocol error: %v", combo, err)
			}
			if !res.IsError {
				t.Fatalf("unknown combo %q returned MCP success", combo)
			}
			if len(got) != 0 {
				t.Fatalf("unknown combo %q sent holdKey calls %+v", combo, got)
			}
			return
		}

		if err != nil {
			t.Fatalf("hold-key rejected valid arguments %v: %v", args, err)
		}
		if res.IsError {
			t.Fatalf("hold-key returned a tool error for valid arguments %v: %+v", args, res.Content)
		}
		if len(got) != 1 {
			t.Fatalf("holdKey calls = %+v, want exactly one", got)
		}
		if got[0].modifier != modifier || !reflect.DeepEqual(got[0].keys, keys) || got[0].holdMS != holdMS {
			t.Fatalf("holdKey call = %+v, want modifier %d keys %v holdMS %d", got[0], modifier, keys, holdMS)
		}
	})
}

// FuzzScrollToolArgumentValidation pins the MCP schema and semantic
// validation boundaries before wheel deltas are narrowed to signed wire
// bytes. Schema-invalid calls and the validly shaped zero/zero no-op must not
// touch the device; every accepted call must preserve both deltas exactly.
func FuzzScrollToolArgumentValidation(f *testing.F) {
	for _, seed := range []struct {
		dx, dy                      int
		includeDX, includeDY, extra bool
	}{
		{0, 1, false, true, false},
		{-jetkvm.MaxScrollDelta, jetkvm.MaxScrollDelta, true, true, false},
		{jetkvm.MaxScrollDelta, -jetkvm.MaxScrollDelta, true, true, false},
		{0, 0, true, true, false},
		{0, 0, false, true, false},
		{-jetkvm.MaxScrollDelta - 1, 1, true, true, false},
		{jetkvm.MaxScrollDelta + 1, 1, true, true, false},
		{1, -jetkvm.MaxScrollDelta - 1, true, true, false},
		{1, jetkvm.MaxScrollDelta + 1, true, true, false},
		{1, 1, true, false, false},
		{1, 1, true, true, true},
		{math.MinInt, math.MaxInt, true, true, false},
	} {
		f.Add(seed.dx, seed.dy, seed.includeDX, seed.includeDY, seed.extra)
	}

	f.Fuzz(func(t *testing.T, dx, dy int, includeDX, includeDY, extra bool) {
		type scrollCall struct {
			dx, dy int8
		}
		var got []scrollCall
		device := &mockDevice{scrollFunc: func(_ context.Context, dx, dy int8) error {
			got = append(got, scrollCall{dx: dx, dy: dy})
			return nil
		}}
		cs := newTestServerSessionForDevice(t, device, true)

		args := map[string]any{}
		if includeDX {
			args["dx"] = dx
		}
		if includeDY {
			args["dy"] = dy
		}
		if extra {
			args["unexpected"] = true
		}

		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_scroll",
			Arguments: args,
		})
		schemaValid := includeDY && !extra &&
			dy >= -jetkvm.MaxScrollDelta && dy <= jetkvm.MaxScrollDelta &&
			(!includeDX || dx >= -jetkvm.MaxScrollDelta && dx <= jetkvm.MaxScrollDelta)
		if !schemaValid {
			if err == nil {
				t.Fatalf("scroll accepted invalid arguments %v", args)
			}
			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("scroll rejection for %v = %v, want JSON-RPC InvalidParams", args, err)
			}
			if len(got) != 0 {
				t.Fatalf("invalid arguments %v sent scroll calls %+v", args, got)
			}
			return
		}

		effectiveDX := dx
		if !includeDX {
			effectiveDX = 0
		}
		if effectiveDX == 0 && dy == 0 {
			if err != nil {
				t.Fatalf("zero scroll returned protocol error: %v", err)
			}
			if !res.IsError {
				t.Fatal("zero scroll returned MCP success")
			}
			if len(got) != 0 {
				t.Fatalf("zero scroll sent device calls %+v", got)
			}
			return
		}

		if err != nil {
			t.Fatalf("scroll rejected valid arguments %v: %v", args, err)
		}
		if res.IsError {
			t.Fatalf("scroll returned a tool error for valid arguments %v: %+v", args, res.Content)
		}
		want := scrollCall{dx: int8(effectiveDX), dy: int8(dy)}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("scroll calls = %+v, want exactly %+v", got, want)
		}
	})
}

// FuzzDragToolArgumentValidation covers the complete MCP drag surface: four
// required coordinates, strict optional bounds, and schema-applied defaults.
// Invalid input must be rejected before the device is touched; valid input
// must forward the exact full-width report sequence built by the shared core.
func FuzzDragToolArgumentValidation(f *testing.F) {
	for _, seed := range []struct {
		x1, y1, x2, y2, button, steps int
		requiredMask                  uint8
		includeButton, includeSteps   bool
		extra                         bool
	}{
		{0, 0, 9, 6, 0, 0, 0x0f, false, false, false},
		{0, 0, 9, 6, 1, 2, 0x0f, true, true, false},
		{jetkvm.MaxAbsoluteCoordinate, jetkvm.MaxAbsoluteCoordinate, 0, 0, jetkvm.MaxPointerButtonMask, jetkvm.MaxDragSteps, 0x0f, true, true, false},
		{-1, 0, 0, 0, 1, 0, 0x0f, true, true, false},
		{0, -1, 0, 0, 1, 0, 0x0f, true, true, false},
		{0, 0, jetkvm.MaxAbsoluteCoordinate + 1, 0, 1, 0, 0x0f, true, true, false},
		{0, 0, 0, jetkvm.MaxAbsoluteCoordinate + 1, 1, 0, 0x0f, true, true, false},
		{0, 0, 0, 0, 0, 0, 0x0f, true, true, false},
		{0, 0, 0, 0, jetkvm.MaxPointerButtonMask + 1, 0, 0x0f, true, true, false},
		{0, 0, 0, 0, 1, -1, 0x0f, true, true, false},
		{0, 0, 0, 0, 1, jetkvm.MaxDragSteps + 1, 0x0f, true, true, false},
		{0, 0, 0, 0, 1, 0, 0x0e, true, true, false},
		{0, 0, 0, 0, 1, 0, 0x00, true, true, false},
		{0, 0, 0, 0, 1, 0, 0x0f, true, true, true},
		{math.MinInt, math.MaxInt, math.MaxInt, math.MinInt, math.MaxInt, math.MinInt, 0x0f, true, true, false},
	} {
		f.Add(
			seed.x1, seed.y1, seed.x2, seed.y2, seed.button, seed.steps,
			seed.requiredMask, seed.includeButton, seed.includeSteps, seed.extra,
		)
	}

	f.Fuzz(func(
		t *testing.T,
		x1, y1, x2, y2, button, steps int,
		requiredMask uint8,
		includeButton, includeSteps, extra bool,
	) {
		var got []jetkvm.PointerDragReport
		dragCalls := 0
		device := &mockDevice{dragFunc: func(_ context.Context, reports []jetkvm.PointerDragReport) error {
			dragCalls++
			got = append([]jetkvm.PointerDragReport(nil), reports...)
			return nil
		}}
		cs := newTestServerSessionForDevice(t, device, true)

		args := map[string]any{}
		for _, field := range []struct {
			name  string
			bit   uint8
			value int
		}{
			{name: "x1", bit: 1 << 0, value: x1},
			{name: "y1", bit: 1 << 1, value: y1},
			{name: "x2", bit: 1 << 2, value: x2},
			{name: "y2", bit: 1 << 3, value: y2},
		} {
			if requiredMask&field.bit != 0 {
				args[field.name] = field.value
			}
		}
		if includeButton {
			args["button"] = button
		}
		if includeSteps {
			args["steps"] = steps
		}
		if extra {
			args["unexpected"] = true
		}

		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_drag",
			Arguments: args,
		})
		schemaValid := requiredMask&0x0f == 0x0f && !extra &&
			x1 >= 0 && x1 <= jetkvm.MaxAbsoluteCoordinate &&
			y1 >= 0 && y1 <= jetkvm.MaxAbsoluteCoordinate &&
			x2 >= 0 && x2 <= jetkvm.MaxAbsoluteCoordinate &&
			y2 >= 0 && y2 <= jetkvm.MaxAbsoluteCoordinate &&
			(!includeButton || button >= 1 && button <= jetkvm.MaxPointerButtonMask) &&
			(!includeSteps || steps >= 0 && steps <= jetkvm.MaxDragSteps)
		if !schemaValid {
			if err == nil {
				t.Fatalf("drag accepted invalid arguments %v", args)
			}
			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("drag rejection for %v = %v, want JSON-RPC InvalidParams", args, err)
			}
			if dragCalls != 0 {
				t.Fatalf("invalid arguments %v sent %d drag calls with reports %+v", args, dragCalls, got)
			}
			return
		}

		effectiveButton := button
		if !includeButton {
			effectiveButton = 1
		}
		effectiveSteps := steps
		if !includeSteps {
			effectiveSteps = 0
		}
		want, buildErr := jetkvm.BuildPointerDragReports(x1, y1, x2, y2, effectiveButton, effectiveSteps)
		if buildErr != nil {
			t.Fatalf("shared drag builder rejected schema-valid arguments %v: %v", args, buildErr)
		}
		if err != nil {
			t.Fatalf("drag rejected valid arguments %v: %v", args, err)
		}
		if res.IsError {
			t.Fatalf("drag returned a tool error for valid arguments %v: %+v", args, res.Content)
		}
		if dragCalls != 1 || !reflect.DeepEqual(got, want) {
			t.Fatalf("drag calls = %d reports %+v, want one call with %+v", dragCalls, got, want)
		}
	})
}

// FuzzKeySequenceToolArgumentValidation exercises the MCP array, per-item,
// delay, and strict-object checks around the shared sequence resolver. The
// complete sequence must validate before the first device call, and valid
// reports must retain their order and exact HID values.
func FuzzKeySequenceToolArgumentValidation(f *testing.F) {
	for _, seed := range []struct {
		first, second                      string
		delay                              int
		shape                              uint8
		includeCombos, includeDelay, extra bool
	}{
		{"ctrl+c", "alt+tab", 0, 2, true, true, false},
		{"CTRL-C", "cmd space", jetkvm.MaxTypeDelayMS, 1, true, true, false},
		{"ctrl+c", "definitely-not-a-combo", 0, 2, true, true, false},
		{"enter", "cmd", 0, 3, true, true, false},
		{"enter", "enter", 0, 4, true, true, false},
		{"ctrl+c", "alt+tab", 0, 0, true, true, false},
		{"ctrl+c", "alt+tab", 0, 5, true, true, false},
		{strings.Repeat("0", jetkvm.MaxKeyComboNameRunes), "enter", 0, 1, true, true, false},
		{strings.Repeat("0", jetkvm.MaxKeyComboNameRunes+1), "enter", 0, 1, true, true, false},
		{"ctrl+c", "alt+tab", -1, 1, true, true, false},
		{"ctrl+c", "alt+tab", jetkvm.MaxTypeDelayMS + 1, 1, true, true, false},
		{"ctrl+c", "alt+tab", math.MinInt, 1, true, true, false},
		{"ctrl+c", "alt+tab", math.MaxInt, 1, true, true, false},
		{"ctrl+c", "alt+tab", 0, 1, false, true, false},
		{"ctrl+c", "alt+tab", 0, 1, true, false, false},
		{"ctrl+c", "alt+tab", 0, 1, true, true, true},
		{"\xff\xfe", "\x00enter", 0, 2, true, true, false},
	} {
		f.Add(
			seed.first, seed.second, seed.delay, seed.shape,
			seed.includeCombos, seed.includeDelay, seed.extra,
		)
	}

	f.Fuzz(func(
		t *testing.T,
		first, second string,
		delayMS int,
		shape uint8,
		includeCombos, includeDelay, extra bool,
	) {
		mode := shape % 6
		var combos []string
		forceZeroDelay := false
		switch mode {
		case 0:
			combos = []string{}
		case 1:
			// A single report never waits, so this shape can safely drive the
			// complete delay domain including the default and positive values.
			combos = []string{first}
		case 2:
			combos = []string{first, second}
			forceZeroDelay = true
		case 3:
			combos = make([]string, jetkvm.MaxKeySequenceLength)
			for i := range combos {
				if i%2 == 0 {
					combos[i] = first
				} else {
					combos[i] = second
				}
			}
			forceZeroDelay = true
		case 4:
			combos = make([]string, jetkvm.MaxKeySequenceLength+1)
			for i := range combos {
				combos[i] = first
			}
		case 5:
			// A present nil slice crosses the transport as JSON null, not as
			// an empty array, and must be rejected by the array schema.
			combos = nil
		}

		effectiveDelay := delayMS
		sendDelay := includeDelay
		if forceZeroDelay {
			effectiveDelay = 0
			sendDelay = true
		} else if !sendDelay {
			effectiveDelay = jetkvm.DefaultTypeDelayMS
		}

		type keyComboCall struct {
			modifier byte
			keys     []byte
		}
		var got []keyComboCall
		device := &mockDevice{keyComboFunc: func(_ context.Context, modifier byte, keys []byte) error {
			got = append(got, keyComboCall{
				modifier: modifier,
				keys:     append([]byte(nil), keys...),
			})
			return nil
		}}
		cs := newTestServerSessionForDevice(t, device, true)

		args := map[string]any{}
		if includeCombos {
			args["combos"] = combos
		}
		if sendDelay {
			args["delay_ms"] = effectiveDelay
		}
		if extra {
			args["unexpected"] = true
		}

		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_key_sequence",
			Arguments: args,
		})
		schemaValid := includeCombos && combos != nil && !extra &&
			len(combos) >= 1 && len(combos) <= jetkvm.MaxKeySequenceLength &&
			effectiveDelay >= 0 && effectiveDelay <= jetkvm.MaxTypeDelayMS
		for _, combo := range combos {
			if utf8.RuneCountInString(combo) > jetkvm.MaxKeyComboNameRunes {
				schemaValid = false
			}
		}
		if !schemaValid {
			if err == nil {
				t.Fatalf("key-sequence accepted invalid arguments %v", args)
			}
			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("key-sequence rejection for %v = %v, want JSON-RPC InvalidParams", args, err)
			}
			if len(got) != 0 {
				t.Fatalf("invalid arguments %v sent keyCombo calls %+v", args, got)
			}
			return
		}

		resolved, resolveErr := jetkvm.ResolveKeySequence(combos)
		if resolveErr != nil {
			if err != nil {
				t.Fatalf("unresolvable sequence %v returned protocol error: %v", combos, err)
			}
			if !res.IsError {
				t.Fatalf("unresolvable sequence %v returned MCP success", combos)
			}
			if len(got) != 0 {
				t.Fatalf("unresolvable sequence %v sent partial keyCombo calls %+v", combos, got)
			}
			return
		}

		if err != nil {
			t.Fatalf("key-sequence rejected valid arguments %v: %v", args, err)
		}
		if res.IsError {
			t.Fatalf("key-sequence returned a tool error for valid arguments %v: %+v", args, res.Content)
		}
		wantText := fmt.Sprintf("sent key sequence combos=%d delay_ms=%d", len(resolved), effectiveDelay)
		if gotText := toolResultText(t, res); gotText != wantText {
			t.Fatalf("key-sequence result text = %q, want %q", gotText, wantText)
		}
		want := make([]keyComboCall, len(resolved))
		for i, combo := range resolved {
			want[i] = keyComboCall{
				modifier: combo.Modifier,
				keys:     append([]byte(nil), combo.Keys...),
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("keyCombo calls = %+v, want %+v", got, want)
		}
	})
}
