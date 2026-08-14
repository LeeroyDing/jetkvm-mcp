//go:build !darwin && !linux

package mcpserver

import (
	"context"
	"fmt"
)

func newFileCoordinator(string) (*fileCoordinator, error) {
	return nil, fmt.Errorf("mcpserver: cross-process device coordination is supported only on Darwin and Linux")
}

type fileCoordinator struct{}

func (*fileCoordinator) lock(context.Context) (func() error, error) {
	return nil, fmt.Errorf("mcpserver: cross-process device coordination is supported only on Darwin and Linux")
}
