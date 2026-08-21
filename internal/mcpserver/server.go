// Package mcpserver exposes a jetkvm.Client over the Model Context
// Protocol (stdio transport), so an agent can inspect and (opt-in) control
// a JetKVM device through standard MCP tool calls instead of the CLI.
package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/buildinfo"
	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

// Options configures the MCP server's connection to the device. Readiness
// gates, keyboard, and mouse tools are only registered at all when
// AllowControl is true - an agent talking to a server started without it
// structurally cannot discover or call them, not merely be refused at call
// time.
type Options struct {
	BaseURL      string
	Credentials  jetkvm.Credentials
	AllowControl bool
	HTTPTimeout  time.Duration
}

// Run serves MCP tools over stdio until ctx is canceled (e.g. by
// SIGINT/SIGTERM) or the transport closes. Device connection is lazy: the
// first tool call establishes it, and a bounded in-call retry may replace it
// after a transient unreachable failure. The MCP process is never respawned.
//
// Stdout discipline: stdout belongs exclusively to the MCP JSON-RPC
// transport. A single stray byte written there corrupts the protocol
// stream, so nothing in this package writes to stdout, and the standard
// logger is pinned to stderr here in case a dependency logs. Diagnostics
// go to stderr and are redacted before they get there.
func Run(ctx context.Context, opts Options) error {
	log.SetOutput(os.Stderr)

	timeout := opts.HTTPTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := newRetryingDevice(Options{
		BaseURL:      opts.BaseURL,
		Credentials:  opts.Credentials,
		AllowControl: opts.AllowControl,
		HTTPTimeout:  timeout,
	})
	// Closing neutralizes any held input before tearing the current session
	// down. Give safety cleanup its own small bound after transport shutdown.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := client.close(closeCtx); err != nil {
			fmt.Fprintf(os.Stderr, "mcpserver: %s\n", jetkvm.RedactError(err))
		}
	}()

	server := newServer(client, opts.AllowControl, timeout)

	return server.Run(ctx, &mcp.StdioTransport{})
}

// newServer builds the MCP server with its authoritative identity and tool
// catalog. buildinfo.Version is the single version source: MCP serverInfo,
// the CLI --version output, and the app bundle's Info.plist (checked by
// `jetkvmctl doctor`) can only disagree by a stale build.
func newServer(client device, allowControl bool, timeout time.Duration) *mcp.Server {
	return newServerWithOCREngine(client, allowControl, timeout, &jetkvm.TesseractOCREngine{})
}

// newServerWithOCREngine keeps OCR dependency selection explicit for tests
// while newServer remains the production constructor with runtime Tesseract
// detection and graceful unavailability.
func newServerWithOCREngine(client device, allowControl bool, timeout time.Duration, ocrEngine jetkvm.OCREngine) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "jetkvm",
		Version: buildinfo.Version,
	}, nil)
	server.AddReceivingMiddleware(invalidToolArgumentsAsProtocolErrors)

	registerReadOnlyTools(server, client, timeout, ocrEngine)
	if allowControl {
		registerWaitStableTool(server, client, timeout)
		registerWaitForTextTool(server, client, timeout, ocrEngine)
		registerControlTools(server, client, timeout)
	}
	return server
}

const sdkArgumentValidationPrefix = `validating "arguments":`
const invalidToolArgumentsMessage = "invalid tool arguments"

// invalidToolArgumentsAsProtocolErrors preserves this server's strict input
// contract across go-sdk v1.7. The SDK now represents schema-validation
// failures as tool-error results; JetKVM has always rejected malformed tool
// calls at the protocol boundary with InvalidParams. GetError is server-only,
// so this conversion happens before the result reaches the transport. The
// prefix check deliberately leaves real handler/tool failures untouched.
// The SDK validation error is never returned verbatim: schema validators can
// quote wrong-typed values and unknown property names, both of which are
// caller-controlled and may contain credentials.
func invalidToolArgumentsAsProtocolErrors(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		// go-sdk applies JSON Schema defaults before validating the input.
		// jsonschema-go cannot assign defaults into a typed nil map, which is
		// what an explicit JSON null becomes, and panics instead of returning a
		// validation error. Every JetKVM tool has an object-rooted schema, so
		// reject null at the protocol boundary before default application.
		if method == "tools/call" {
			if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok &&
				bytes.Equal(bytes.TrimSpace(params.Arguments), []byte("null")) {
				return nil, &jsonrpc.Error{
					Code:    jsonrpc.CodeInvalidParams,
					Message: invalidToolArgumentsMessage,
				}
			}
		}

		result, err := next(ctx, method, req)
		if err != nil || method != "tools/call" {
			return result, err
		}

		toolResult, ok := result.(*mcp.CallToolResult)
		if !ok {
			return result, nil
		}
		validationErr := toolResult.GetError()
		if validationErr == nil || !strings.HasPrefix(validationErr.Error(), sdkArgumentValidationPrefix) {
			return result, nil
		}
		return nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInvalidParams,
			Message: invalidToolArgumentsMessage,
		}
	}
}
