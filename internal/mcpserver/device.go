package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

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
	holdKey(context.Context, byte, []byte, int) error
	mouseMove(context.Context, int32, int32, byte) error
	mouseButton(context.Context, byte, bool) error
	scroll(context.Context, int8, int8) error
	drag(context.Context, []jetkvm.PointerDragReport) error
	close(context.Context) error
}

// clientDevice adapts one connected jetkvm.Client to operation-level methods.
// It performs no retries; retryingDevice owns that policy and never repeats a
// state-changing operation after bytes may have reached the device.
type clientDevice struct {
	client *jetkvm.Client

	// controlMu serializes direct users of clientDevice and protects the
	// session-scoped holder used by discrete mouse-button actions. Production
	// MCP calls are also serialized by retryingDevice, but keeping the state
	// local and locked here makes reconnect/discard and direct tests safe too.
	controlMu   sync.Mutex
	buttonLease *jetkvm.Held
	heldButtons byte
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

	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	if held, _ := d.liveButtonLeaseLocked(); held != nil {
		return true, d.releaseButtonLeaseLocked(held)
	}

	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return true, err
	}
	return true, held.Release()
}

// sendSingleReport preserves both halves of a failed control operation: the
// report may have reached the device even when its send failed, and a failed
// release means that input cannot be assumed to have been neutralized.
func sendSingleReport(send, release func() error) (err error) {
	defer func() {
		err = errors.Join(err, release())
	}()
	return send()
}

func (d *clientDevice) keypress(ctx context.Context, modifier, key byte) (err error) {
	lease, err := d.client.Control()
	if err != nil {
		return err
	}

	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	if held, _ := d.liveButtonLeaseLocked(); held != nil {
		if err := held.SendKeyboardReport(ctx, modifier, []byte{key}); err != nil {
			return d.failButtonLeaseLocked(held, err)
		}
		if err := held.ReleaseKeyboard(ctx); err != nil {
			return d.failButtonLeaseLocked(held, err)
		}
		return nil
	}

	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	return sendSingleReport(
		func() error { return held.SendKeyboardReport(ctx, modifier, []byte{key}) },
		held.Release,
	)
}

func (d *clientDevice) keyCombo(ctx context.Context, modifier byte, keys []byte) (err error) {
	lease, err := d.client.Control()
	if err != nil {
		return err
	}

	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	if held, _ := d.liveButtonLeaseLocked(); held != nil {
		if err := held.SendKeyboardReport(ctx, modifier, keys); err != nil {
			return d.failButtonLeaseLocked(held, err)
		}
		if err := held.ReleaseKeyboard(ctx); err != nil {
			return d.failButtonLeaseLocked(held, err)
		}
		return nil
	}

	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	return sendSingleReport(
		func() error { return held.SendKeyboardReport(ctx, modifier, keys) },
		held.Release,
	)
}

func validateHoldKey(modifier byte, keys []byte, holdMS int) error {
	resolvedKeys := make([]int, len(keys))
	for i, key := range keys {
		resolvedKeys[i] = int(key)
	}
	if err := jetkvm.ValidateKeyCombo(int(modifier), resolvedKeys); err != nil {
		return err
	}
	return jetkvm.ValidateHoldMS(holdMS)
}

func (d *clientDevice) holdKey(ctx context.Context, modifier byte, keys []byte, holdMS int) (err error) {
	// Keep this operation boundary independently safe: validate the complete
	// report and duration before acquiring a lease or making any HID call.
	if err := validateHoldKey(modifier, keys, holdMS); err != nil {
		return err
	}

	lease, err := d.client.Control()
	if err != nil {
		return err
	}

	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	if held, _ := d.liveButtonLeaseLocked(); held != nil {
		if err := held.SendKeyboardReport(ctx, modifier, keys); err != nil {
			return d.failButtonLeaseLocked(held, err)
		}
		if err := waitHold(ctx, holdMS); err != nil {
			// The caller context may already be canceled. End the retained
			// generation through Held.Release's independent cleanup context so
			// both keys and sticky buttons are safely neutralized.
			return d.failButtonLeaseLocked(held, err)
		}
		if err := held.ReleaseKeyboard(ctx); err != nil {
			return d.failButtonLeaseLocked(held, err)
		}
		return nil
	}

	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	// Held.Release uses its own bounded background context, so this cleanup
	// remains effective after ctx cancellation or timeout. Preserve an
	// independent neutralization failure alongside the primary error.
	defer func() { err = errors.Join(err, held.Release()) }()

	if err := held.SendKeyboardReport(ctx, modifier, keys); err != nil {
		return err
	}
	return waitHold(ctx, holdMS)
}

func waitHold(ctx context.Context, holdMS int) error {
	timer := time.NewTimer(time.Duration(holdMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("holding key combo: %w", ctx.Err())
	}
}

func (d *clientDevice) mouseMove(ctx context.Context, x, y int32, buttons byte) (err error) {
	lease, err := d.client.Control()
	if err != nil {
		return err
	}

	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	if held, sticky := d.liveButtonLeaseLocked(); held != nil {
		combined := buttons | sticky
		if err := held.SendPointerReport(ctx, x, y, combined); err != nil {
			return d.failButtonLeaseLocked(held, err)
		}
		// The legacy buttons argument is an operation-local state. Preserve the
		// explicitly held named buttons after any additional buttons it supplied.
		if buttons&^sticky != 0 {
			if err := held.SendMouseReport(ctx, 0, 0, sticky); err != nil {
				return d.failButtonLeaseLocked(held, err)
			}
		}
		return nil
	}

	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	return sendSingleReport(
		func() error { return held.SendPointerReport(ctx, x, y, buttons) },
		held.Release,
	)
}

// mouseButton applies one named button transition without moving the cursor.
// A non-zero aggregate mask retains the lease across MCP calls so later pointer
// operations can compose a drag. The independent lease watchdog remains the
// final safety bound if no matching release or release-all arrives.
func (d *clientDevice) mouseButton(ctx context.Context, button byte, pressed bool) error {
	if err := jetkvm.ValidateMouseButton(button); err != nil {
		return err
	}
	lease, err := d.client.Control()
	if err != nil {
		return err
	}

	d.controlMu.Lock()
	defer d.controlMu.Unlock()

	held, current := d.liveButtonLeaseLocked()
	newLease := held == nil
	if held == nil {
		held, err = lease.AcquirePersistent(ctx, jetkvm.DefaultControlLeaseTimeout)
		if err != nil {
			return err
		}
	}

	next := current
	if pressed {
		next |= button
	} else {
		next &^= button
	}
	if err := held.SendMouseReport(ctx, 0, 0, next); err != nil {
		return d.failButtonLeaseLocked(held, err)
	}

	if next == 0 {
		return d.releaseButtonLeaseLocked(held)
	}
	d.buttonLease = held
	d.heldButtons = next
	if newLease {
		d.watchButtonLease(held)
	}
	return nil
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

	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	if held, sticky := d.liveButtonLeaseLocked(); held != nil {
		var finalButtons byte = sticky
		for _, report := range reports {
			finalButtons = byte(report.Buttons) | sticky
			if err := held.SendPointerReport(ctx, int32(report.X), int32(report.Y), finalButtons); err != nil {
				return d.failButtonLeaseLocked(held, err)
			}
		}
		if finalButtons != sticky {
			if err := held.SendMouseReport(ctx, 0, 0, sticky); err != nil {
				return d.failButtonLeaseLocked(held, err)
			}
		}
		return nil
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
	d.controlMu.Lock()
	defer d.controlMu.Unlock()

	// Client.Close owns the canonical session-end neutralization and performs
	// it before transport teardown. Release the retained handle afterward to
	// free its lease/watchdog without sending duplicate neutral reports.
	closeErr := d.client.Close(ctx)
	if held, _ := d.liveButtonLeaseLocked(); held != nil {
		return errors.Join(closeErr, d.releaseButtonLeaseLocked(held))
	}
	return closeErr
}

// liveButtonLeaseLocked returns the current retained holder, clearing
// bookkeeping if its watchdog has already released it. d.controlMu must be
// held by the caller.
func (d *clientDevice) liveButtonLeaseLocked() (*jetkvm.Held, byte) {
	if d.buttonLease == nil {
		return nil, 0
	}
	select {
	case <-d.buttonLease.Done():
		d.buttonLease = nil
		d.heldButtons = 0
		return nil, 0
	default:
		return d.buttonLease, d.heldButtons
	}
}

// watchButtonLease makes watchdog expiry visible to the adapter even when no
// later device call arrives. Exactly one watcher is started per retained
// holder.
func (d *clientDevice) watchButtonLease(held *jetkvm.Held) {
	go func() {
		<-held.Done()
		d.controlMu.Lock()
		defer d.controlMu.Unlock()
		if d.buttonLease == held {
			d.buttonLease = nil
			d.heldButtons = 0
		}
	}()
}

// releaseButtonLeaseLocked clears adapter state and performs the holder's
// independent-context neutralization. d.controlMu must be held by the caller.
func (d *clientDevice) releaseButtonLeaseLocked(held *jetkvm.Held) error {
	err := held.Release()
	if d.buttonLease == held {
		d.buttonLease = nil
		d.heldButtons = 0
	}
	return err
}

func (d *clientDevice) failButtonLeaseLocked(held *jetkvm.Held, operationErr error) error {
	return errors.Join(operationErr, d.releaseButtonLeaseLocked(held))
}
