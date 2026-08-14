// Package mcpserver exposes a jetkvm.Client over the Model Context
// Protocol (stdio transport), so an agent can inspect and (opt-in) control
// a JetKVM device through standard MCP tool calls instead of the CLI.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/buildinfo"
	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

// Options configures the MCP server's connection to the device. Keyboard
// and mouse tools are only registered at all when AllowControl is true -
// an agent talking to a server started without it structurally cannot
// discover or call them, not merely be refused at call time.
type Options struct {
	BaseURL      string
	Credentials  jetkvm.Credentials
	AllowControl bool
	HTTPTimeout  time.Duration
}

// Run serves MCP tools over stdio until ctx is canceled (e.g. by
// SIGINT/SIGTERM) or the transport closes. It never holds an idle device
// session: each tool call obtains an exclusive fresh connection and closes it
// before returning, matching firmware that supports only one current WebRTC
// session globally.
//
// Stdout discipline: stdout belongs exclusively to the MCP JSON-RPC
// transport. A single stray byte written there corrupts the protocol
// stream, so nothing in this package writes to stdout, and the standard
// logger is pinned to stderr here in case a dependency logs. Diagnostics
// go to stderr and are redacted before they get there.
func Run(ctx context.Context, opts Options) error {
	timeout := opts.HTTPTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	manager, err := newSessionManager(opts)
	if err != nil {
		return fmt.Errorf("mcpserver: session coordination: %s", jetkvm.RedactError(err))
	}
	if coordinator, ok := manager.coordinator.(closeableCoordinator); ok {
		defer coordinator.close()
	}

	server := newServer(manager, opts.AllowControl, timeout)

	err = server.Run(ctx, newStdioTransport())
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// Protocol/transport errors can contain attacker-supplied request bytes in
	// the SDK error chain. Keep the top-level CLI boundary fixed and non-reflective.
	return fmt.Errorf("mcpserver: protocol session ended")
}

func newServer(operations deviceOperations, allowControl bool, timeout time.Duration) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "jetkvm",
		Version: buildinfo.Version,
	}, nil)
	// Typed schema validation and unknown-tool rejection happen inside the MCP
	// SDK, before a tool handler can reach errorResult. Normalize every
	// protocol-level tools/call failure here so attacker-supplied argument
	// values or tool names can never be reflected around the central redaction
	// boundary. Tool execution failures remain IsError results and are redacted
	// by errorResult.
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)
			if err == nil || method != "tools/call" {
				return result, err
			}
			return nil, &jsonrpc.Error{
				Code:    jsonrpc.CodeInvalidParams,
				Message: "tool call rejected: invalid name or arguments",
			}
		}
	})

	registerReadOnlyTools(server, operations, timeout)
	if allowControl {
		registerControlTools(server, operations, timeout)
	}
	return server
}
