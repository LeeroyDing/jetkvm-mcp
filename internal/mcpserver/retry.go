package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

const (
	defaultRetryAttempts = 3
	defaultRetryBase     = 75 * time.Millisecond
	defaultRetryCap      = 300 * time.Millisecond
)

type deviceConnector func(context.Context) (device, error)

type retryPolicy struct {
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration
	jitter      func(time.Duration) time.Duration
	sleep       func(context.Context, time.Duration) error
}

func defaultRetryPolicy() retryPolicy {
	return retryPolicy{
		maxAttempts: defaultRetryAttempts,
		baseDelay:   defaultRetryBase,
		maxDelay:    defaultRetryCap,
		jitter:      jitterDelay,
		sleep:       sleepContext,
	}
}

// jitterDelay spreads a delay uniformly across 75%-125%. The result is capped
// again by retryDelay, so jitter can never defeat the policy's hard maximum.
func jitterDelay(delay time.Duration) time.Duration {
	if delay <= 0 {
		return 0
	}
	spread := delay / 4
	if spread == 0 {
		return delay
	}
	return delay - spread + time.Duration(rand.Int64N(int64(2*spread)+1))
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryingDevice owns at most one connected device session. Its one-slot gate
// serializes MCP calls across connection replacement as well as device I/O.
// Every retry loop is bounded by both maxAttempts and the caller's context.
type retryingDevice struct {
	gate             chan struct{}
	current          device
	cleanup          *cleanupBarrier
	connect          deviceConnector
	policy           retryPolicy
	allowControl     bool
	decoderPreflight func(context.Context) error
}

// cleanupBarrier represents every asynchronous discarded-session close up to
// and including this one. The close goroutines start immediately, but a later
// barrier does not become done until its predecessors are also done. That lets
// read-only recovery continue while ensuring no old neutralization frame can
// arrive after a replacement session starts another control mutation.
//
// retryingDevice.gate protects the cleanup pointer. Closing done publishes err
// to waiters.
type cleanupBarrier struct {
	done chan struct{}
	err  error
}

func newRetryingDevice(opts Options) *retryingDevice {
	connector := func(ctx context.Context) (device, error) {
		client, err := jetkvm.Connect(ctx, jetkvm.Options{
			BaseURL:      opts.BaseURL,
			Credentials:  opts.Credentials,
			AllowControl: opts.AllowControl,
			HTTPTimeout:  opts.HTTPTimeout,
		})
		if err != nil {
			return nil, err
		}
		return &clientDevice{client: client}, nil
	}
	client := newRetryingDeviceWithConnector(opts.AllowControl, connector, defaultRetryPolicy())
	client.decoderPreflight = func(ctx context.Context) error {
		return (&jetkvm.FFmpegDecoder{}).CheckAvailable(ctx)
	}
	return client
}

func newRetryingDeviceWithConnector(allowControl bool, connector deviceConnector, policy retryPolicy) *retryingDevice {
	if policy.maxAttempts <= 0 {
		policy.maxAttempts = 1
	}
	if policy.baseDelay < 0 {
		policy.baseDelay = 0
	}
	if policy.maxDelay <= 0 {
		policy.maxDelay = policy.baseDelay
	}
	if policy.jitter == nil {
		policy.jitter = func(delay time.Duration) time.Duration { return delay }
	}
	if policy.sleep == nil {
		policy.sleep = sleepContext
	}
	return &retryingDevice{
		gate:         make(chan struct{}, 1),
		connect:      connector,
		policy:       policy,
		allowControl: allowControl,
	}
}

func (d *retryingDevice) acquire(ctx context.Context) (func(), error) {
	select {
	case d.gate <- struct{}{}:
		return func() { <-d.gate }, nil
	case <-ctx.Done():
		return nil, &jetkvm.DeviceError{
			Kind:      jetkvm.ErrorKindTimeout,
			Operation: "waiting for another MCP device call",
			Detail:    "call deadline expired",
		}
	}
}

// do runs one MCP operation. Connection attempts are always safe to retry
// because no operation bytes have been sent. Once an operation starts, only
// read-only callers pass retryOperation=true; keyboard, pointer, scroll, and
// release calls are never repeated after an ambiguous transport failure.
func (d *retryingDevice) do(
	ctx context.Context,
	operation string,
	retryOperation bool,
	waitForCleanup bool,
	call func(device) error,
) error {
	release, err := d.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if waitForCleanup {
		if _, err := d.awaitCleanup(ctx, operation); err != nil {
			return err
		}
	}

	for attempt := 1; attempt <= d.policy.maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return callTimeoutError(operation, "call deadline expired")
		}

		client := d.current
		operationStarted := false
		if client == nil {
			client, err = d.connect(ctx)
			if err == nil {
				d.current = client
			}
		}
		if err == nil {
			operationStarted = true
			err = call(client)
		}
		if err == nil {
			return nil
		}

		kind := jetkvm.ErrorKindOf(err)
		if kind == jetkvm.ErrorKindUnreachable || kind == jetkvm.ErrorKindBadFrame {
			d.discard(client)
		}
		canRetry := kind == jetkvm.ErrorKindUnreachable && (!operationStarted || retryOperation)
		if !canRetry {
			if kind == jetkvm.ErrorKindTimeout || ctx.Err() != nil {
				return callTimeoutErrorPreservingSafety(operation, "call deadline expired", err)
			}
			return err
		}
		if attempt == d.policy.maxAttempts {
			break
		}

		delay := d.retryDelay(attempt)
		if deadline, ok := ctx.Deadline(); ok && delay >= time.Until(deadline) {
			return callTimeoutError(operation, "retry backoff would exceed call deadline")
		}
		if err := d.policy.sleep(ctx, delay); err != nil {
			return callTimeoutError(operation, "call deadline expired during retry backoff")
		}
		err = nil
	}

	// Exhaustion is a normal classified failure, not a process invariant. Keep
	// the server fail-safe if the loop's internal return paths change later:
	// long-lived MCP library code must never crash the host process here.
	return &jetkvm.DeviceError{
		Kind:      jetkvm.ErrorKindUnreachable,
		Operation: fmt.Sprintf("%s after %d attempts", operation, d.policy.maxAttempts),
		Detail:    "bounded retry limit reached",
	}
}

func (d *retryingDevice) retryDelay(failedAttempt int) time.Duration {
	delay := d.policy.baseDelay
	for i := 1; i < failedAttempt && delay < d.policy.maxDelay; i++ {
		if delay > d.policy.maxDelay/2 {
			delay = d.policy.maxDelay
			break
		}
		delay *= 2
	}
	if delay > d.policy.maxDelay {
		delay = d.policy.maxDelay
	}
	delay = d.policy.jitter(delay)
	if delay < 0 {
		return 0
	}
	if delay > d.policy.maxDelay {
		return d.policy.maxDelay
	}
	return delay
}

func callTimeoutError(operation, detail string) error {
	return &jetkvm.DeviceError{Kind: jetkvm.ErrorKindTimeout, Operation: operation, Detail: detail}
}

// callTimeoutErrorPreservingSafety keeps the stable timeout classification
// while retaining the one additional fact callers must act on: terminal HID
// neutralization could not be confirmed, so input may still be held.
func callTimeoutErrorPreservingSafety(operation, detail string, cause error) error {
	timeoutErr := callTimeoutError(operation, detail)
	if errors.Is(cause, jetkvm.ErrNeutralizeUnverified) {
		return errors.Join(timeoutErr, jetkvm.ErrNeutralizeUnverified)
	}
	return timeoutErr
}

func (d *retryingDevice) discard(client device) {
	if client == nil || d.current != client {
		return
	}
	d.current = nil
	if d.allowControl {
		// Close performs safety neutralization and is itself bounded, but its
		// fresh safety context can outlive an MCP deadline. Run it separately so
		// retry/error delivery never exceeds the caller's timeout budget. Later
		// control mutations wait on the aggregate barrier before sending input,
		// so this old release cannot race behind a replacement session's report.
		previous := d.cleanup
		next := &cleanupBarrier{done: make(chan struct{})}
		d.cleanup = next
		go func() {
			closeErr := client.close(context.Background())
			if previous != nil {
				<-previous.done
				closeErr = errors.Join(previous.err, closeErr)
			}
			next.err = closeErr
			close(next.done)
		}()
		return
	}
	_ = client.close(context.Background())
}

// awaitCleanup waits for every discarded control session known when the
// caller acquired gate. It returns the accumulated close error separately:
// mutations need the ordering guarantee and may proceed after a failed old
// close, while final shutdown reports the safety failure truthfully.
func (d *retryingDevice) awaitCleanup(ctx context.Context, operation string) (error, error) {
	pending := d.cleanup
	if pending == nil {
		return nil, nil
	}
	select {
	case <-pending.done:
		return pending.err, nil
	case <-ctx.Done():
		return nil, callTimeoutError(operation, "call deadline expired waiting for prior device cleanup")
	}
}

func (d *retryingDevice) status(ctx context.Context) (result jetkvm.StatusResult, err error) {
	err = d.do(ctx, "status", true, false, func(client device) error {
		result, err = client.status(ctx)
		return err
	})
	return result, err
}

func (d *retryingDevice) captureScreenshot(ctx context.Context) (shot jetkvm.Screenshot, err error) {
	if d.decoderPreflight != nil {
		if err := d.decoderPreflight(ctx); err != nil {
			return jetkvm.Screenshot{}, err
		}
	}
	err = d.do(ctx, "screenshot", true, false, func(client device) error {
		shot, err = client.captureScreenshot(ctx)
		return err
	})
	return shot, err
}

func (d *retryingDevice) waitStable(ctx context.Context, opts jetkvm.WaitStableOptions) (result jetkvm.WaitStableResult, err error) {
	// Validate before the decoder preflight or a connection attempt. MCP and
	// CLI validate at their own boundaries too, but this adapter is also used
	// directly by tests and must preserve the no-work-on-invalid-input rule.
	if err := jetkvm.ValidateWaitStableOptions(opts); err != nil {
		return jetkvm.WaitStableResult{}, err
	}
	if d.decoderPreflight != nil {
		if err := d.decoderPreflight(ctx); err != nil {
			return jetkvm.WaitStableResult{}, err
		}
	}
	err = d.do(ctx, "wait for screen stability", true, false, func(client device) error {
		result, err = client.waitStable(ctx, opts)
		return err
	})
	return result, err
}

func (d *retryingDevice) releaseAll(ctx context.Context) (released bool, err error) {
	if !d.allowControl {
		return false, nil
	}
	// Emergency neutralization may bypass an older session's pending cleanup:
	// both operations send the same zero state, so they commute. Later presses
	// still wait for the cleanup barrier before sending non-zero input.
	err = d.do(ctx, "release all input", false, false, func(client device) error {
		released, err = client.releaseAll(ctx)
		return err
	})
	return released, err
}

func (d *retryingDevice) keypress(ctx context.Context, modifier, key byte) error {
	return d.do(ctx, "keypress", false, true, func(client device) error {
		return client.keypress(ctx, modifier, key)
	})
}

func (d *retryingDevice) keyCombo(ctx context.Context, modifier byte, keys []byte) error {
	return d.do(ctx, "key combo", false, true, func(client device) error {
		return client.keyCombo(ctx, modifier, keys)
	})
}

func (d *retryingDevice) mouseMove(ctx context.Context, x, y int32, buttons byte) error {
	return d.do(ctx, "mouse move", false, true, func(client device) error {
		return client.mouseMove(ctx, x, y, buttons)
	})
}

func (d *retryingDevice) mouseButton(ctx context.Context, button byte, pressed bool) error {
	// Validate before d.do can establish a device session. Like every control
	// mutation, a button transition is never retried after operation start
	// because delivery may be ambiguous.
	if err := jetkvm.ValidateMouseButton(button); err != nil {
		return err
	}
	return d.do(ctx, "mouse button", false, true, func(client device) error {
		return client.mouseButton(ctx, button, pressed)
	})
}

func (d *retryingDevice) scroll(ctx context.Context, dx, dy int8) error {
	if !d.allowControl {
		// Unlike keyboard and pointer reports, scrolling uses the legacy RPC
		// channel, which is present on read-only sessions too. Keep the adapter's
		// control gate explicit so that transport availability cannot bypass the
		// operator's --allow-control choice.
		return jetkvm.ErrControlDisabled
	}
	return d.do(ctx, "scroll", false, true, func(client device) error {
		return client.scroll(ctx, dx, dy)
	})
}

func (d *retryingDevice) drag(ctx context.Context, reports []jetkvm.PointerDragReport) error {
	return d.do(ctx, "drag", false, true, func(client device) error {
		return client.drag(ctx, reports)
	})
}

func (d *retryingDevice) close(ctx context.Context) error {
	release, err := d.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	cleanupErr, err := d.awaitCleanup(ctx, "closing device")
	if err != nil {
		return err
	}
	client := d.current
	d.current = nil
	if client == nil {
		return cleanupErr
	}
	return errors.Join(cleanupErr, client.close(ctx))
}
