package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

// device is the operation-level surface used by MCP tools. Keeping session
// ownership behind this interface lets the production implementation replace a
// dead JetKVM connection without re-registering tools or restarting the MCP
// process, while tests can remain entirely in-process.
type device interface {
	status(context.Context) (jetkvm.StatusResult, error)
	captureScreenshot(context.Context) (jetkvm.Screenshot, error)
	waitStable(context.Context, jetkvm.WaitStableOptions) (jetkvm.WaitStableResult, error)
	releaseAll(context.Context) (bool, error)
	keypress(context.Context, byte, byte) error
	keyCombo(context.Context, byte, []byte) error
	mouseMove(context.Context, int32, int32, byte) error
	scroll(context.Context, int8, int8) error
	drag(context.Context, []jetkvm.PointerDragReport) error
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

func (d *clientDevice) waitStable(ctx context.Context, opts jetkvm.WaitStableOptions) (jetkvm.WaitStableResult, error) {
	return d.client.WaitStable(ctx, opts)
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

func (d *clientDevice) keyCombo(ctx context.Context, modifier byte, keys []byte) (err error) {
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
	return held.SendKeyboardReport(ctx, modifier, keys)
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

func (d *clientDevice) scroll(ctx context.Context, dx, dy int8) error {
	return d.client.Scroll(ctx, dx, dy)
}

func (d *clientDevice) drag(ctx context.Context, reports []jetkvm.PointerDragReport) (err error) {
	// Validate the complete gesture before acquiring a lease or narrowing any
	// values. Tool handlers build these reports through BuildPointerDragReports,
	// but this operation boundary must remain safe independently.
	for i, report := range reports {
		if validateErr := jetkvm.ValidatePointer(report.X, report.Y, report.Buttons); validateErr != nil {
			return fmt.Errorf("drag report %d: %w", i+1, validateErr)
		}
	}

	lease, err := d.client.Control()
	if err != nil {
		return err
	}
	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, held.Release())
	}()
	for _, report := range reports {
		if err := held.SendPointerReport(ctx, int32(report.X), int32(report.Y), byte(report.Buttons)); err != nil {
			return err
		}
	}
	return nil
}

func (d *clientDevice) close(ctx context.Context) error {
	return d.client.Close(ctx)
}
