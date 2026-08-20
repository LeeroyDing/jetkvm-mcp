package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func boolPtr(b bool) *bool { return &b }

func textResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

// errorResult converts a Go error into a tool error result. Errors reaching
// here have already been through the jetkvm package's redaction (see
// internal/jetkvm/redact.go): they never carry credentials, auth response
// bodies, query strings, or inherited environment values. Returning a nil
// error alongside IsError is the MCP convention for "the tool ran and
// failed", as distinct from a protocol-level failure.
func errorResult(err error) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: jetkvm.RedactError(err)}},
	}, nil, nil
}

func withDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline && time.Until(deadline) <= timeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// noArgsSchema is the explicit, strict schema for a tool that takes no
// arguments. Declaring it (rather than letting an empty struct be
// inferred) is what makes unknown fields a deterministic, stable
// InvalidParams rejection instead of a silently ignored payload.
func noArgsSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:                 "object",
		Properties:           map[string]*jsonschema.Schema{},
		AdditionalProperties: falseSchema(),
	}
}

// falseSchema is JSON Schema's `false`, i.e. "nothing validates against
// this". {"not":{}} is how jsonschema-go itself represents a false schema,
// and its validator special-cases exactly this shape to report every
// unexpected property in one deterministic message.
func falseSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Not: &jsonschema.Schema{}}
}

// registerReadOnlyTools registers exactly the tools available without
// --allow-control: status and screenshot. No HID-capable tool is advertised
// on the read-only surface, including release-all (the v0.2.0 production
// contract from oc-q3w.5: the accepted read-only catalog is two tools).
func registerReadOnlyTools(server *mcp.Server, client device, timeout time.Duration) {
	type statusArgs struct{}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "jetkvm_status",
		Description: "Check connectivity to the JetKVM device: device ID, firmware version, and whether the control-channel RPC ping succeeds.",
		InputSchema: noArgsSchema(),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args statusArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()
		status, err := client.status(ctx)
		if err != nil {
			return errorResult(err)
		}
		return textResult(
			"deviceId=%s firmwareVersion=%s rpcReachable=%v",
			status.DeviceID, status.FirmwareVersion, status.RPCReachable,
		), status, nil
	})

	// The screenshot tool takes no arguments on purpose. An earlier version
	// accepted an output_path and wrote the PNG there, which handed any MCP
	// caller an arbitrary-file-overwrite primitive (plus traversal and
	// symlink-following) on the machine running this server. The image is
	// returned in the response instead, so the server never writes to a
	// caller-chosen location at all.
	type screenshotArgs struct{}
	mcp.AddTool(server, &mcp.Tool{
		Name: "jetkvm_screenshot",
		Description: "Capture one request-fresh screenshot of the attached computer's display via the JetKVM's video feed and return it as a PNG image. " +
			"Success requires a frame captured after this call begins; if no newer frame arrives before the deadline, the call fails instead of returning a cached frame. " +
			"This tool never writes to the filesystem.",
		InputSchema: noArgsSchema(),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args screenshotArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()
		shot, err := client.captureScreenshot(ctx)
		if err != nil {
			return errorResult(err)
		}
		meta := map[string]any{
			"width":      shot.Width,
			"height":     shot.Height,
			"capturedAt": shot.CapturedAt.Format(time.RFC3339Nano),
			"fresh":      shot.Fresh,
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf(
					"width=%d height=%d capturedAt=%s fresh=%v",
					shot.Width, shot.Height, shot.CapturedAt.Format(time.RFC3339Nano), shot.Fresh,
				)},
				&mcp.ImageContent{Data: shot.PNG, MIMEType: "image/png"},
			},
		}, meta, nil
	})

}

// registerControlTools registers keyboard/mouse tools. Only called when
// the server was started with --allow-control, so these tools are
// structurally absent from tools/list otherwise - an agent talking to a
// server started without that flag cannot even discover them.
func registerControlTools(server *mcp.Server, client device, timeout time.Duration) {
	dangerous := &mcp.ToolAnnotations{
		ReadOnlyHint:    false,
		DestructiveHint: boolPtr(true),
		IdempotentHint:  false,
	}

	type keypressArgs struct {
		Key      int `json:"key"`
		Modifier int `json:"modifier,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "jetkvm_keypress",
		Description: "DANGEROUS: sends one live key press to the computer attached to the JetKVM. Requires the server to have been started with --allow-control.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"key": {
					Type:        "integer",
					Description: "USB HID keyboard usage code",
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(255),
				},
				"modifier": {
					Type:        "integer",
					Description: "modifier bitmask (ctrl/shift/alt/meta)",
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(255),
				},
			},
			Required:             []string{"key"},
			AdditionalProperties: falseSchema(),
		},
		Annotations: dangerous,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args keypressArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()
		// Belt and braces: the schema already rejects out-of-range values,
		// but the handler must not depend on the validator to stay safe.
		// CLI and MCP share this exact function, so neither surface can
		// narrow an unvalidated int into a wire byte.
		if err := jetkvm.ValidateKeypress(args.Key, args.Modifier); err != nil {
			return errorResult(err)
		}
		if err := client.keypress(ctx, byte(args.Modifier), byte(args.Key)); err != nil {
			return errorResult(err)
		}
		return textResult("sent keypress key=%d modifier=%d", args.Key, args.Modifier), nil, nil
	})

	type typeArgs struct {
		Text    string `json:"text"`
		DelayMS int    `json:"delay_ms,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name: "jetkvm_type",
		Description: "DANGEROUS: types a UTF-8 string into the computer attached to the JetKVM using a US keyboard layout. " +
			"Supports printable ASCII, newline, and tab; requires --allow-control.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"text": {
					Type:        "string",
					Description: fmt.Sprintf("text to type using a US keyboard layout (maximum %d runes)", jetkvm.MaxTypeStringRunes),
					MaxLength:   intPtr(jetkvm.MaxTypeStringRunes),
				},
				"delay_ms": {
					Type:        "integer",
					Description: fmt.Sprintf("delay between keypresses in milliseconds (default %d)", jetkvm.DefaultTypeDelayMS),
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(jetkvm.MaxTypeDelayMS),
				},
			},
			Required:             []string{"text"},
			AdditionalProperties: falseSchema(),
		},
		Annotations: dangerous,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args typeArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()

		if err := jetkvm.ValidateTypeDelay(args.DelayMS); err != nil {
			return errorResult(err)
		}
		keypresses, err := jetkvm.MapTypeString(args.Text)
		if err != nil {
			return errorResult(err)
		}
		runes := []rune(args.Text)

		// Validate the complete mapped sequence before the first HID call. The
		// mapper currently emits only in-range values, but keeping the shared
		// validator at this adapter boundary prevents a future mapping change
		// from silently narrowing an invalid integer into a wire byte.
		for i, keypress := range keypresses {
			if err := jetkvm.ValidateKeypress(keypress.HIDUsageCode, keypress.Modifier); err != nil {
				return errorResult(fmt.Errorf("invalid mapped keypress for character %d %q: %w", i+1, runes[i], err))
			}
		}

		for i, keypress := range keypresses {
			if err := client.keypress(ctx, byte(keypress.Modifier), byte(keypress.HIDUsageCode)); err != nil {
				return errorResult(fmt.Errorf("%w (typing character %d %q)", err, i+1, runes[i]))
			}
			if i+1 < len(keypresses) && args.DelayMS > 0 {
				if err := waitInterKeyDelay(ctx, time.Duration(args.DelayMS)*time.Millisecond); err != nil {
					return errorResult(fmt.Errorf("%w (before typing character %d %q)", err, i+2, runes[i+1]))
				}
			}
		}

		return textResult("typed runes=%d delay_ms=%d", len(keypresses), args.DelayMS), nil, nil
	})

	type mouseMoveArgs struct {
		X       int `json:"x"`
		Y       int `json:"y"`
		Buttons int `json:"buttons,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "jetkvm_mouse_move",
		Description: "DANGEROUS: moves the mouse to an absolute position (and optionally sets button state) on the computer attached to the JetKVM. Requires --allow-control.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"x": {
					Type:        "integer",
					Description: "absolute X position",
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(jetkvm.MaxAbsoluteCoordinate),
				},
				"y": {
					Type:        "integer",
					Description: "absolute Y position",
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(jetkvm.MaxAbsoluteCoordinate),
				},
				"buttons": {
					Type:        "integer",
					Description: "mouse button bitmask",
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(255),
				},
			},
			Required:             []string{"x", "y"},
			AdditionalProperties: falseSchema(),
		},
		Annotations: dangerous,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args mouseMoveArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()
		if err := jetkvm.ValidatePointer(args.X, args.Y, args.Buttons); err != nil {
			return errorResult(err)
		}
		if err := client.mouseMove(ctx, int32(args.X), int32(args.Y), byte(args.Buttons)); err != nil {
			return errorResult(err)
		}
		return textResult("moved mouse to x=%d y=%d buttons=%d", args.X, args.Y, args.Buttons), nil, nil
	})

	type releaseAllArgs struct{}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "jetkvm_release_all",
		Description: "Release every held key and mouse button immediately, without moving the mouse cursor. Requires --allow-control.",
		InputSchema: noArgsSchema(),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args releaseAllArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()
		released, err := client.releaseAll(ctx)
		if err != nil {
			return errorResult(err)
		}
		if !released {
			// Structurally this tool only exists with --allow-control, so a
			// device session without control available is a failed release,
			// never a quiet success.
			return errorResult(fmt.Errorf("jetkvm: control is not available for this session; nothing was released"))
		}
		return textResult("released all keys and mouse buttons (no cursor movement)"), nil, nil
	})
}

func float64Ptr(v float64) *float64 { return &v }
func intPtr(v int) *int             { return &v }

func waitInterKeyDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
