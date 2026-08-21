package mcpserver

import (
	"context"
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
	gate                chan struct{}
	current             device
	connect             deviceConnector
	policy              retryPolicy
	allowControl        bool
	screenshotPreflight func(context.Context) error
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
	client.screenshotPreflight = func(ctx context.Context) error {
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
	call func(device) error,
) error {
	release, err := d.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()

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
				return callTimeoutError(operation, "call deadline expired")
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

func (d *retryingDevice) discard(client device) {
	if client == nil || d.current != client {
		return
	}
	d.current = nil
	if d.allowControl {
		// Close performs safety neutralization and is itself bounded, but its
		// fresh safety context can outlive an MCP deadline. Run it separately so
		// retry/error delivery never exceeds the caller's timeout budget.
		go client.close(context.Background())
		return
	}
	_ = client.close(context.Background())
}

func (d *retryingDevice) status(ctx context.Context) (result jetkvm.StatusResult, err error) {
	err = d.do(ctx, "status", true, func(client device) error {
		result, err = client.status(ctx)
		return err
	})
	return result, err
}

func (d *retryingDevice) captureScreenshot(ctx context.Context) (shot jetkvm.Screenshot, err error) {
	if d.screenshotPreflight != nil {
		if err := d.screenshotPreflight(ctx); err != nil {
			return jetkvm.Screenshot{}, err
		}
	}
	err = d.do(ctx, "screenshot", true, func(client device) error {
		shot, err = client.captureScreenshot(ctx)
		return err
	})
	return shot, err
}

func (d *retryingDevice) releaseAll(ctx context.Context) (released bool, err error) {
	if !d.allowControl {
		return false, nil
	}
	err = d.do(ctx, "release all input", false, func(client device) error {
		released, err = client.releaseAll(ctx)
		return err
	})
	return released, err
}

func (d *retryingDevice) keypress(ctx context.Context, modifier, key byte) error {
	return d.do(ctx, "keypress", false, func(client device) error {
		return client.keypress(ctx, modifier, key)
	})
}

func (d *retryingDevice) keyCombo(ctx context.Context, modifier byte, keys []byte) error {
	return d.do(ctx, "key combo", false, func(client device) error {
		return client.keyCombo(ctx, modifier, keys)
	})
}

func (d *retryingDevice) mouseMove(ctx context.Context, x, y int32, buttons byte) error {
	return d.do(ctx, "mouse move", false, func(client device) error {
		return client.mouseMove(ctx, x, y, buttons)
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
	return d.do(ctx, "scroll", false, func(client device) error {
		return client.scroll(ctx, dx, dy)
	})
}

func (d *retryingDevice) close(ctx context.Context) error {
	release, err := d.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	client := d.current
	d.current = nil
	if client == nil {
		return nil
	}
	return client.close(ctx)
}
