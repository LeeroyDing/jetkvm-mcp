package mcpserver

import (
	"context"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

// device is the operation-level surface used by MCP tools. Keeping session
// ownership behind this interface lets the production implementation replace a
// dead JetKVM connection without re-registering tools or restarting the MCP
// process, while tests can remain entirely in-process.
type device interface {
	status(context.Context) (jetkvm.StatusResult, error)
	captureScreenshot(context.Context) (jetkvm.Screenshot, error)
	releaseAll(context.Context) (bool, error)
	keypress(context.Context, byte, byte) error
	mouseMove(context.Context, int32, int32, byte) error
	close(context.Context) error
}

// clientDevice adapts one connected jetkvm.Client to operation-level methods.
// It performs no retries; retryingDevice owns that policy and never repeats a
// state-changing operation after bytes may have reached the device.
type clientDevice struct {
	client *jetkvm.Client
}

func (d *clientDevice) status(ctx context.Context) (jetkvm.StatusResult, error) {
	return d.client.Status(ctx)
}

func (d *clientDevice) captureScreenshot(ctx context.Context) (jetkvm.Screenshot, error) {
	return d.client.CaptureScreenshot(ctx)
}

func (d *clientDevice) releaseAll(ctx context.Context) (bool, error) {
	lease, err := d.client.Control()
	if err != nil {
		return false, nil
	}
	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return true, err
	}
	return true, held.Release()
}

func (d *clientDevice) keypress(ctx context.Context, modifier, key byte) (err error) {
	lease, err := d.client.Control()
	if err != nil {
		return err
	}
	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := held.Release(); err == nil {
			err = releaseErr
		}
	}()
	return held.SendKeyboardReport(ctx, modifier, []byte{key})
}

func (d *clientDevice) mouseMove(ctx context.Context, x, y int32, buttons byte) (err error) {
	lease, err := d.client.Control()
	if err != nil {
		return err
	}
	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := held.Release(); err == nil {
			err = releaseErr
		}
	}()
	return held.SendPointerReport(ctx, x, y, buttons)
}

func (d *clientDevice) close(ctx context.Context) error {
	return d.client.Close(ctx)
}
