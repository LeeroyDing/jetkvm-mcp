package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func boolPtr(b bool) *bool { return &b }

const maxWaitStablePollIntervalMS = int64(math.MaxInt64) / int64(time.Millisecond)

type waitStableArgs struct {
	Threshold      float64 `json:"threshold,omitempty"`
	StableFrames   int     `json:"stable_frames,omitempty"`
	PollIntervalMS int64   `json:"poll_interval_ms,omitempty"`
}

func jsonDefault(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("marshalling static JSON Schema default: %v", err))
	}
	return data
}

func waitStableOptionsFromArgs(args waitStableArgs) (jetkvm.WaitStableOptions, error) {
	// Check milliseconds before multiplication: sufficiently large positive
	// values can wrap a time.Duration into a small, apparently valid value.
	if args.PollIntervalMS < 0 || args.PollIntervalMS > maxWaitStablePollIntervalMS {
		return jetkvm.WaitStableOptions{}, fmt.Errorf(
			"PollInterval (poll_interval_ms) must be in [0,%d], got %d",
			maxWaitStablePollIntervalMS, args.PollIntervalMS)
	}

	threshold := args.Threshold
	stableFrames := args.StableFrames
	pollInterval := time.Duration(args.PollIntervalMS) * time.Millisecond
	opts := jetkvm.WaitStableOptions{
		Threshold:    &threshold,
		StableFrames: &stableFrames,
		PollInterval: &pollInterval,
	}
	if err := jetkvm.ValidateWaitStableOptions(opts); err != nil {
		return jetkvm.WaitStableOptions{}, err
	}
	return opts, nil
}

func textResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

func waitStableResult(result jetkvm.WaitStableResult, err error) (*mcp.CallToolResult, any, error) {
	elapsed := result.Elapsed.String()
	summary := fmt.Sprintf(
		"settled=%v framesSampled=%d finalChangeFraction=%g elapsed=%s",
		result.Settled, result.FramesSampled, result.FinalChangeFraction, elapsed,
	)
	meta := map[string]any{
		"settled":             result.Settled,
		"framesSampled":       result.FramesSampled,
		"finalChangeFraction": result.FinalChangeFraction,
		"elapsed":             elapsed,
	}
	if err == nil {
		return textResult("%s", summary), meta, nil
	}

	// Preserve errorResult's redacted, taxonomy-first rendering while still
	// returning WaitStable's partial observations. In particular, the retry
	// layer normalizes timeouts but deliberately leaves the partial result in
	// place, so readiness failures remain actionable instead of collapsing to
	// a bare "call deadline expired".
	callResult, _, _ := errorResult(err)
	errorText := callResult.Content[0].(*mcp.TextContent)
	errorText.Text += "; " + summary
	return callResult, meta, nil
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

func screenshotScaleSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:             "number",
		Description:      "positive image scale factor; values above 1 are clamped to 1 so the image is never enlarged (default 1)",
		ExclusiveMinimum: float64Ptr(0),
		Default:          json.RawMessage(`1`),
	}
}

func screenshotRegionSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "object",
		Description: "rectangular crop in source-image pixels, applied before scaling",
		Properties: map[string]*jsonschema.Schema{
			"x": {
				Type:        "integer",
				Description: "left edge in source pixels",
				Minimum:     float64Ptr(0),
				Maximum:     float64Ptr(maxScreenshotRegionValue),
			},
			"y": {
				Type:        "integer",
				Description: "top edge in source pixels",
				Minimum:     float64Ptr(0),
				Maximum:     float64Ptr(maxScreenshotRegionValue),
			},
			"width": {
				Type:        "integer",
				Description: "crop width in source pixels",
				Minimum:     float64Ptr(1),
				Maximum:     float64Ptr(maxScreenshotRegionValue),
			},
			"height": {
				Type:        "integer",
				Description: "crop height in source pixels",
				Minimum:     float64Ptr(1),
				Maximum:     float64Ptr(maxScreenshotRegionValue),
			},
		},
		Required:             []string{"x", "y", "width", "height"},
		AdditionalProperties: falseSchema(),
	}
}

func screenshotInputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"format": {
				Type:        "string",
				Description: "output image format (default png)",
				Enum:        []any{"png", "jpeg"},
				Default:     json.RawMessage(`"png"`),
			},
			"quality": {
				Type:        "integer",
				Description: "JPEG quality from 1 through 100 (JPEG only; default 80)",
				Minimum:     float64Ptr(1),
				Maximum:     float64Ptr(100),
			},
			"scale":  screenshotScaleSchema(),
			"region": screenshotRegionSchema(),
		},
		AdditionalProperties: falseSchema(),
	}
}

type readTextArgs struct {
	Scale  *float64              `json:"scale,omitempty"`
	Region *screenshotRegionArgs `json:"region,omitempty"`
}

func readTextInputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"scale":  screenshotScaleSchema(),
			"region": screenshotRegionSchema(),
		},
		AdditionalProperties: falseSchema(),
	}
}

// registerReadOnlyTools registers exactly the tools available without
// --allow-control: status, screenshot, and OCR text. No opt-in tool is advertised on the
// read-only surface, including wait-stable, release-all, or the legacy-RPC
// scroll path (the accepted read-only catalog is three tools).
func registerReadOnlyTools(server *mcp.Server, client device, timeout time.Duration, ocrEngine jetkvm.OCREngine) {
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

	// Screenshot output controls deliberately stop at in-memory image
	// transformations. An earlier version accepted an output_path and handed
	// any MCP caller an arbitrary-file-overwrite primitive (plus traversal and
	// symlink-following) on the machine running this server. No path-like
	// argument is accepted, and the image is returned in the response only.
	mcp.AddTool(server, &mcp.Tool{
		Name: "jetkvm_screenshot",
		Description: "Capture one request-fresh screenshot of the attached computer's display via the JetKVM's video feed and return it as an in-memory PNG (default) or JPEG image. " +
			"Optional source-pixel cropping happens before down-scaling; output is never up-scaled. " +
			"Success requires a frame captured after this call begins; if no newer frame arrives before the deadline, the call fails instead of returning a cached frame. " +
			"This tool never writes to the filesystem.",
		InputSchema: screenshotInputSchema(),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args screenshotArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()
		options, err := normalizeScreenshotOptions(args)
		if err != nil {
			return errorResult(err)
		}
		shot, err := client.captureScreenshot(ctx)
		if err != nil {
			return errorResult(err)
		}
		rendered, err := renderScreenshot(ctx, shot, options)
		if err != nil {
			return errorResult(err)
		}
		meta := map[string]any{
			"width":        rendered.Width,
			"height":       rendered.Height,
			"format":       rendered.Format,
			"mimeType":     rendered.MIMEType,
			"sourceWidth":  shot.Width,
			"sourceHeight": shot.Height,
			"capturedAt":   shot.CapturedAt.Format(time.RFC3339Nano),
			"fresh":        shot.Fresh,
		}
		if rendered.Quality != 0 {
			meta["quality"] = rendered.Quality
		}
		summary := fmt.Sprintf(
			"width=%d height=%d format=%s mimeType=%s sourceWidth=%d sourceHeight=%d capturedAt=%s fresh=%v",
			rendered.Width, rendered.Height, rendered.Format, rendered.MIMEType,
			shot.Width, shot.Height, shot.CapturedAt.Format(time.RFC3339Nano), shot.Fresh,
		)
		if rendered.Quality != 0 {
			summary += fmt.Sprintf(" quality=%d", rendered.Quality)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: summary},
				&mcp.ImageContent{Data: rendered.Data, MIMEType: rendered.MIMEType},
			},
		}, meta, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "jetkvm_read_text",
		Description: "Capture one request-fresh frame from the attached computer's display and return OCR text without returning the image. " +
			"Optional source-pixel cropping happens before down-scaling; OCR input is never up-scaled. " +
			"This read-only tool does not require --allow-control or a control lease. " +
			"It fails clearly when no OCR engine is available.",
		InputSchema: readTextInputSchema(),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args readTextArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()

		options, err := normalizeScreenshotOptions(screenshotArgs{
			Scale:  args.Scale,
			Region: args.Region,
		})
		if err != nil {
			return errorResult(err)
		}
		if ocrEngine == nil {
			return errorResult(&jetkvm.OCRUnavailableError{})
		}
		if err := ocrEngine.CheckAvailable(ctx); err != nil {
			return errorResult(err)
		}

		shot, err := client.captureScreenshot(ctx)
		if err != nil {
			return errorResult(err)
		}
		rendered, err := renderScreenshot(ctx, shot, options)
		if err != nil {
			return errorResult(err)
		}
		text, err := ocrEngine.ReadText(ctx, rendered.Data)
		if err != nil {
			return errorResult(err)
		}

		meta := map[string]any{
			"width":        rendered.Width,
			"height":       rendered.Height,
			"sourceWidth":  shot.Width,
			"sourceHeight": shot.Height,
			"capturedAt":   shot.CapturedAt.Format(time.RFC3339Nano),
			"fresh":        shot.Fresh,
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, meta, nil
	})
}

// registerWaitStableTool registers the read-only readiness gate. Its MCP
// exposure is opt-in even though the operation itself sends no control input,
// so newServer only calls this function when --allow-control is enabled.
func registerWaitStableTool(server *mcp.Server, client device, timeout time.Duration) {

	mcp.AddTool(server, &mcp.Tool{
		Name: "jetkvm_wait_stable",
		Description: "Poll successive request-fresh video frames until the attached computer's display settles. " +
			"A comparison is stable when the changed-pixel fraction is at or below threshold for stable_frames consecutive comparisons. " +
			"This is a read-only readiness gate and never sends keyboard or mouse input.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"threshold": {
					Type:        "number",
					Description: "maximum fraction of changed pixels for a stable comparison (default 0.01)",
					Default:     jsonDefault(jetkvm.DefaultWaitStableThreshold),
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(1),
				},
				"stable_frames": {
					Type:        "integer",
					Description: "consecutive stable comparisons required before returning (default 2)",
					Default:     jsonDefault(jetkvm.DefaultWaitStableFrames),
					Minimum:     float64Ptr(1),
				},
				"poll_interval_ms": {
					Type:        "integer",
					Description: "minimum gap between fresh-frame polls in milliseconds (default 250)",
					Default:     jsonDefault(jetkvm.DefaultWaitStablePollInterval.Milliseconds()),
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(float64(maxWaitStablePollIntervalMS)),
				},
			},
			AdditionalProperties: falseSchema(),
		},
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args waitStableArgs) (*mcp.CallToolResult, any, error) {
		opts, err := waitStableOptionsFromArgs(args)
		if err != nil {
			return errorResult(err)
		}

		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()
		result, err := client.waitStable(ctx, opts)
		return waitStableResult(result, err)
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
				return errorResult(fmt.Errorf("invalid mapped keypress for %s: %w", jetkvm.TypeCharacterContext(i+1, runes[i]), err))
			}
		}

		if err := sendTypeKeypresses(
			ctx,
			keypresses,
			runes,
			time.Duration(args.DelayMS)*time.Millisecond,
			client.keypress,
			waitInterKeyDelay,
		); err != nil {
			return errorResult(err)
		}

		return textResult("typed runes=%d delay_ms=%d", len(keypresses), args.DelayMS), nil, nil
	})

	type keyComboArgs struct {
		Combo string `json:"combo"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "jetkvm_key_combo",
		Description: "DANGEROUS: sends one named keyboard chord (for example ctrl+alt+del, cmd+space, alt+tab, or ctrl+c) to the computer attached to the JetKVM. Requires the server to have been started with --allow-control.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"combo": {
					Type:        "string",
					Description: "named keyboard chord",
				},
			},
			Required:             []string{"combo"},
			AdditionalProperties: falseSchema(),
		},
		Annotations: dangerous,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args keyComboArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()
		modifier, keys, err := jetkvm.ResolveKeyCombo(args.Combo)
		if err != nil {
			return errorResult(err)
		}
		resolvedKeys := make([]int, len(keys))
		for i, key := range keys {
			resolvedKeys[i] = int(key)
		}
		if err := jetkvm.ValidateKeyCombo(int(modifier), resolvedKeys); err != nil {
			return errorResult(err)
		}
		if err := client.keyCombo(ctx, modifier, keys); err != nil {
			return errorResult(err)
		}
		return textResult("sent key combo modifier=%d keys=%v", modifier, keys), nil, nil
	})

	type keySequenceArgs struct {
		Combos  []string `json:"combos"`
		DelayMS int      `json:"delay_ms,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name: "jetkvm_key_sequence",
		Description: "DANGEROUS: sends an ordered sequence of named keyboard chords to the computer attached to the JetKVM. " +
			"Every chord is validated before any input is sent; requires --allow-control.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"combos": {
					Type:        "array",
					Description: fmt.Sprintf("ordered named keyboard chords (maximum %d)", jetkvm.MaxKeySequenceLength),
					Items:       &jsonschema.Schema{Type: "string"},
					MinItems:    intPtr(1),
					MaxItems:    intPtr(jetkvm.MaxKeySequenceLength),
				},
				"delay_ms": {
					Type:        "integer",
					Description: fmt.Sprintf("delay between key combos in milliseconds (default %d)", jetkvm.DefaultTypeDelayMS),
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(jetkvm.MaxTypeDelayMS),
				},
			},
			Required:             []string{"combos"},
			AdditionalProperties: falseSchema(),
		},
		Annotations: dangerous,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args keySequenceArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()

		if err := jetkvm.ValidateTypeDelay(args.DelayMS); err != nil {
			return errorResult(err)
		}
		resolved, err := jetkvm.ResolveKeySequence(args.Combos)
		if err != nil {
			return errorResult(err)
		}

		for i, combo := range resolved {
			if err := client.keyCombo(ctx, combo.Modifier, combo.Keys); err != nil {
				return errorResult(fmt.Errorf("sending key sequence combo at index %d: %w", i, err))
			}
			if i+1 < len(resolved) && args.DelayMS > 0 {
				if err := waitInterKeyDelay(ctx, time.Duration(args.DelayMS)*time.Millisecond); err != nil {
					return errorResult(fmt.Errorf("waiting before key sequence combo at index %d: %w", i+1, err))
				}
			}
		}

		return textResult("sent key sequence combos=%d delay_ms=%d", len(resolved), args.DelayMS), nil, nil
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

	type scrollArgs struct {
		DY int `json:"dy"`
		DX int `json:"dx,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name: "jetkvm_scroll",
		Description: "DANGEROUS: sends a mouse-wheel scroll event to the computer attached to the JetKVM. " +
			"Positive dy scrolls up and positive dx scrolls right. Requires --allow-control.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"dy": {
					Type:        "integer",
					Description: "vertical wheel delta (positive scrolls up)",
					Minimum:     float64Ptr(-jetkvm.MaxScrollDelta),
					Maximum:     float64Ptr(jetkvm.MaxScrollDelta),
				},
				"dx": {
					Type:        "integer",
					Description: "horizontal wheel delta (default 0; positive scrolls right)",
					Default:     json.RawMessage("0"),
					Minimum:     float64Ptr(-jetkvm.MaxScrollDelta),
					Maximum:     float64Ptr(jetkvm.MaxScrollDelta),
				},
			},
			Required:             []string{"dy"},
			AdditionalProperties: falseSchema(),
		},
		Annotations: dangerous,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args scrollArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()
		// Belt and braces: validate adapter ints independently of the schema
		// before narrowing them to the wheel report's signed-byte domain.
		if err := jetkvm.ValidateScroll(args.DX, args.DY); err != nil {
			return errorResult(err)
		}
		if err := client.scroll(ctx, int8(args.DX), int8(args.DY)); err != nil {
			return errorResult(err)
		}
		return textResult("scrolled mouse dx=%d dy=%d", args.DX, args.DY), nil, nil
	})

	type clickArgs struct {
		X      int `json:"x"`
		Y      int `json:"y"`
		Button int `json:"button,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "jetkvm_click",
		Description: "DANGEROUS: moves the mouse to an absolute position, presses the requested button bitmask, then releases it on the computer attached to the JetKVM. Requires --allow-control.",
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
				"button": {
					Type:        "integer",
					Description: "mouse button bitmask (default 1 = left)",
					Default:     json.RawMessage("1"),
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(255),
				},
			},
			Required:             []string{"x", "y"},
			AdditionalProperties: falseSchema(),
		},
		Annotations: dangerous,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args clickArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()
		// Belt and braces: the schema already rejects out-of-range values,
		// but the handler must not depend on the validator to stay safe.
		// Validate before narrowing adapter ints into HID wire values.
		if err := jetkvm.ValidatePointer(args.X, args.Y, args.Button); err != nil {
			return errorResult(err)
		}
		if err := client.mouseMove(ctx, int32(args.X), int32(args.Y), byte(args.Button)); err != nil {
			return errorResult(err)
		}
		if err := client.mouseMove(ctx, int32(args.X), int32(args.Y), 0); err != nil {
			return errorResult(err)
		}
		return textResult("clicked mouse at x=%d y=%d button=%d", args.X, args.Y, args.Button), nil, nil
	})

	type dragArgs struct {
		X1     int `json:"x1"`
		Y1     int `json:"y1"`
		X2     int `json:"x2"`
		Y2     int `json:"y2"`
		Button int `json:"button,omitempty"`
		Steps  int `json:"steps,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "jetkvm_drag",
		Description: "DANGEROUS: presses a mouse button at one absolute position, moves to another position while holding it, then releases it on the computer attached to the JetKVM. Requires --allow-control.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"x1": {
					Type:        "integer",
					Description: "absolute starting X position",
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(jetkvm.MaxAbsoluteCoordinate),
				},
				"y1": {
					Type:        "integer",
					Description: "absolute starting Y position",
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(jetkvm.MaxAbsoluteCoordinate),
				},
				"x2": {
					Type:        "integer",
					Description: "absolute destination X position",
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(jetkvm.MaxAbsoluteCoordinate),
				},
				"y2": {
					Type:        "integer",
					Description: "absolute destination Y position",
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(jetkvm.MaxAbsoluteCoordinate),
				},
				"button": {
					Type:        "integer",
					Description: "mouse button bitmask (default 1 = left)",
					Default:     json.RawMessage("1"),
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(255),
				},
				"steps": {
					Type:        "integer",
					Description: fmt.Sprintf("intermediate held-button moves for smoother motion (default 0, maximum %d)", jetkvm.MaxDragSteps),
					Default:     json.RawMessage("0"),
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(jetkvm.MaxDragSteps),
				},
			},
			Required:             []string{"x1", "y1", "x2", "y2"},
			AdditionalProperties: falseSchema(),
		},
		Annotations: dangerous,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args dragArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()
		// Validate both caller-supplied endpoints before narrowing any values.
		// BuildPointerDragReports then validates every generated position too.
		if err := jetkvm.ValidatePointer(args.X1, args.Y1, args.Button); err != nil {
			return errorResult(fmt.Errorf("drag start: %w", err))
		}
		if err := jetkvm.ValidatePointer(args.X2, args.Y2, args.Button); err != nil {
			return errorResult(fmt.Errorf("drag destination: %w", err))
		}
		reports, err := jetkvm.BuildPointerDragReports(args.X1, args.Y1, args.X2, args.Y2, args.Button, args.Steps)
		if err != nil {
			return errorResult(err)
		}
		if err := client.drag(ctx, reports); err != nil {
			return errorResult(err)
		}
		return textResult("dragged mouse from x1=%d y1=%d to x2=%d y2=%d button=%d steps=%d", args.X1, args.Y1, args.X2, args.Y2, args.Button, args.Steps), nil, nil
	})

	type doubleClickArgs struct {
		X      int `json:"x"`
		Y      int `json:"y"`
		Button int `json:"button,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "jetkvm_double_click",
		Description: "DANGEROUS: moves the mouse to an absolute position, presses and releases the requested button bitmask twice on the computer attached to the JetKVM. Requires --allow-control.",
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
				"button": {
					Type:        "integer",
					Description: "mouse button bitmask (default 1 = left)",
					Default:     json.RawMessage("1"),
					Minimum:     float64Ptr(0),
					Maximum:     float64Ptr(255),
				},
			},
			Required:             []string{"x", "y"},
			AdditionalProperties: falseSchema(),
		},
		Annotations: dangerous,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args doubleClickArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withDefaultTimeout(ctx, timeout)
		defer cancel()
		// Belt and braces: the schema already rejects out-of-range values,
		// but the handler must not depend on the validator to stay safe.
		// Validate before narrowing adapter ints into HID wire values.
		if err := jetkvm.ValidatePointer(args.X, args.Y, args.Button); err != nil {
			return errorResult(err)
		}
		if err := client.mouseMove(ctx, int32(args.X), int32(args.Y), byte(args.Button)); err != nil {
			return errorResult(err)
		}
		if err := client.mouseMove(ctx, int32(args.X), int32(args.Y), 0); err != nil {
			return errorResult(err)
		}
		if err := client.mouseMove(ctx, int32(args.X), int32(args.Y), byte(args.Button)); err != nil {
			return errorResult(err)
		}
		if err := client.mouseMove(ctx, int32(args.X), int32(args.Y), 0); err != nil {
			return errorResult(err)
		}
		return textResult("double-clicked mouse at x=%d y=%d button=%d", args.X, args.Y, args.Button), nil, nil
	})

	type releaseAllArgs struct{}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "jetkvm_release_all",
		Description: "DANGEROUS: releases every held key and mouse button immediately, without moving the mouse cursor. Requires --allow-control.",
		InputSchema: noArgsSchema(),
		Annotations: dangerous,
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

func sendTypeKeypresses(
	ctx context.Context,
	keypresses []jetkvm.TypeKeypress,
	runes []rune,
	delay time.Duration,
	send func(context.Context, byte, byte) error,
	wait func(context.Context, time.Duration) error,
) error {
	if len(keypresses) != len(runes) {
		return fmt.Errorf("mapped keypress count %d does not match character count %d", len(keypresses), len(runes))
	}
	for i, keypress := range keypresses {
		if err := send(ctx, byte(keypress.Modifier), byte(keypress.HIDUsageCode)); err != nil {
			return fmt.Errorf("%w while typing %s", err, jetkvm.TypeCharacterContext(i+1, runes[i]))
		}
		if i+1 < len(keypresses) && delay > 0 {
			if err := wait(ctx, delay); err != nil {
				return fmt.Errorf("%w before typing %s", err, jetkvm.TypeCharacterContext(i+2, runes[i+1]))
			}
		}
	}
	return nil
}

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
