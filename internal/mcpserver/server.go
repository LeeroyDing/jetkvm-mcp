// Package mcpserver exposes a jetkvm.Client over the Model Context
// Protocol (stdio transport), so an agent can inspect and (opt-in) control
// a JetKVM device through standard MCP tool calls instead of the CLI.
package mcpserver

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "jetkvm",
		Version: "0.3.0",
	}, nil)

	registerReadOnlyTools(server, client, timeout)
	if opts.AllowControl {
		registerControlTools(server, client, timeout)
	}

	return server.Run(ctx, &mcp.StdioTransport{})
}
