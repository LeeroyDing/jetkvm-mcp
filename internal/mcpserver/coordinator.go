package mcpserver

import (
	"context"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

type sessionCoordinator interface {
	lock(ctx context.Context) (func() error, error)
}

type closeableCoordinator interface {
	close() error
}

type noopCoordinator struct{}

func (noopCoordinator) lock(context.Context) (func() error, error) {
	return func() error { return nil }, nil
}

// canonicalDeviceIdentity creates a credential-free lock identity for the
// supported direct-device URL shape. Query strings, fragments, userinfo, and
// non-root paths are rejected rather than folded into a lock name: accepting
// them would make aliases bypass coordination and invites credential-bearing
// URLs into configuration.
func canonicalDeviceIdentity(raw string) (string, error) {
	return jetkvm.CanonicalBaseURL(raw)
}
