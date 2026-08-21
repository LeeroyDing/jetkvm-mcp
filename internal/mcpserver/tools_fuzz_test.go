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
		{jetkvm.MaxAbsoluteCoordinate, jetkvm.MaxAbsoluteCoordinate, 255, true, true, true},
		{123, 456, 0, true, true, false},
		{-1, 0, 1, true, true, true},
		{0, -1, 1, true, true, true},
		{jetkvm.MaxAbsoluteCoordinate + 1, 0, 1, true, true, true},
		{0, jetkvm.MaxAbsoluteCoordinate + 1, 1, true, true, true},
		{0, 0, -1, true, true, true},
		{0, 0, 256, true, true, true},
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
			(!includeButton || button >= 0 && button <= 255)
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
