package mcpserver

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

// FuzzDoubleClickToolArgumentValidation pins the MCP adapter boundary before
// coordinates and the button bitmask are narrowed to HID wire types. Invalid
// or incomplete arguments must be rejected before the first mouse report;
// valid arguments must produce exactly two press-release pairs.
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
			(!includeButton || button >= 0 && button <= jetkvm.MaxPointerButtonMask)
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
