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
	if _, hasDeadline := ctx.Deadline(); hasDeadline || timeout <= 0 {
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

// registerReadOnlyTools registers the tools available regardless of
// --allow-control: status, screenshot, and release-all (a safety valve
// that's harmless to expose even when control was never enabled - if
// there's no HID channel, there's nothing to release, and it says so).
func registerReadOnlyTools(server *mcp.Server, client *jetkvm.Client, timeout time.Duration) {
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
		status, err := client.Status(ctx)
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
		Description: "Capture one fresh screenshot of the attached computer's display via the JetKVM's video feed and return it as a PNG image. " +
			"Always check the returned capturedAt/fresh fields before acting on the image - a stale frame is reported, not hidden. " +
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
		shot, err := client.CaptureScreenshot(ctx)
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

	type releaseAllArgs struct{}
	mcp.AddTool(server, &mcp.Tool{
		Name: "jetkvm_release_all",
		Description: "Release every held key and mouse button immediately, without moving the mouse cursor. " +
			"Safe to call at any time, including when control was never enabled (it then reports there is nothing to release).",
		InputSchema: noArgsSchema(),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args releaseAllArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()
		lease, err := client.Control()
		if err != nil {
			return textResult("control was not enabled for this session; nothing to release"), nil, nil
		}
		held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
		if err != nil {
			return errorResult(err)
		}
		// Release performs the neutralization; a non-nil error here means it
		// could not be confirmed on the wire, which must be reported rather
		// than smoothed over.
		if err := held.Release(); err != nil {
			return errorResult(err)
		}
		return textResult("released all keys and mouse buttons (no cursor movement)"), nil, nil
	})
}

// registerControlTools registers keyboard/mouse tools. Only called when
// the server was started with --allow-control, so these tools are
// structurally absent from tools/list otherwise - an agent talking to a
// server started without that flag cannot even discover them.
func registerControlTools(server *mcp.Server, client *jetkvm.Client, timeout time.Duration) {
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
		if args.Key < 0 || args.Key > 255 {
			return errorResult(fmt.Errorf("key must be in [0,255], got %d", args.Key))
		}
		if args.Modifier < 0 || args.Modifier > 255 {
			return errorResult(fmt.Errorf("modifier must be in [0,255], got %d", args.Modifier))
		}
		lease, err := client.Control()
		if err != nil {
			return errorResult(err)
		}
		held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
		if err != nil {
			return errorResult(err)
		}
		defer held.Release()
		if err := held.SendKeyboardReport(ctx, byte(args.Modifier), []byte{byte(args.Key)}); err != nil {
			return errorResult(err)
		}
		return textResult("sent keypress key=%d modifier=%d", args.Key, args.Modifier), nil, nil
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
					Maximum:     float64Ptr(jetkvmMaxCoordinate),
				},
				"y": {
					Type:        "integer",
					Description: "absolute Y position",
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(jetkvmMaxCoordinate),
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
		if args.X < 0 || args.X > jetkvmMaxCoordinate || args.Y < 0 || args.Y > jetkvmMaxCoordinate {
			return errorResult(fmt.Errorf("x and y must be in [0,%d]", jetkvmMaxCoordinate))
		}
		if args.Buttons < 0 || args.Buttons > 255 {
			return errorResult(fmt.Errorf("buttons must be in [0,255], got %d", args.Buttons))
		}
		lease, err := client.Control()
		if err != nil {
			return errorResult(err)
		}
		held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
		if err != nil {
			return errorResult(err)
		}
		defer held.Release()
		if err := held.SendPointerReport(ctx, int32(args.X), int32(args.Y), byte(args.Buttons)); err != nil {
			return errorResult(err)
		}
		return textResult("moved mouse to x=%d y=%d buttons=%d", args.X, args.Y, args.Buttons), nil, nil
	})
}

func float64Ptr(v float64) *float64 { return &v }

const jetkvmMaxCoordinate = 32767
