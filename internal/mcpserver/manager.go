package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

const (
	readOnlyMaxAttempts = 2
	retryBackoff        = 50 * time.Millisecond
	sessionCloseTimeout = 3 * time.Second
)

// deviceOperations is the complete adapter-facing surface. Implementations
// own connection, control-release, and cleanup semantics; tool handlers never
// receive a Client they could retain past one MCP request.
type deviceOperations interface {
	Status(context.Context) (jetkvm.StatusResult, error)
	Screenshot(context.Context) (jetkvm.Screenshot, error)
	Keypress(context.Context, int, int) error
	MouseMove(context.Context, int, int, int) error
	ReleaseAll(context.Context) error
}

type deviceSession interface {
	status(context.Context) (jetkvm.StatusResult, error)
	screenshot(context.Context) (jetkvm.Screenshot, error)
	keypress(context.Context, int, int) error
	mouseMove(context.Context, int, int, int) error
	releaseAll(context.Context) error
	close(context.Context) error
}

type sessionFactory func(context.Context, bool) (deviceSession, error)

// sessionManager holds configuration and coordination only. It deliberately
// never stores a live Client: every operation connects once, executes once,
// and closes before returning. A read-only terminal handoff/transport failure
// may receive one bounded fresh-session retry; control is never replayed.
type sessionManager struct {
	gate        chan struct{}
	coordinator sessionCoordinator
	connect     sessionFactory
	preflight   func(context.Context) error
}

func newSessionManager(opts Options) (*sessionManager, error) {
	coordinator, err := newFileCoordinator(opts.BaseURL)
	if err != nil {
		return nil, err
	}

	factory := func(ctx context.Context, allowControl bool) (deviceSession, error) {
		client, err := jetkvm.Connect(ctx, jetkvm.Options{
			BaseURL:      opts.BaseURL,
			Credentials:  opts.Credentials,
			AllowControl: allowControl,
			HTTPTimeout:  opts.HTTPTimeout,
		})
		if err != nil {
			return nil, err
		}
		return &clientSession{client: client}, nil
	}

	return newSessionManagerWith(factory, coordinator, func(ctx context.Context) error {
		return (&jetkvm.FFmpegDecoder{}).CheckAvailable(ctx)
	}), nil
}

func newSessionManagerWith(factory sessionFactory, coordinator sessionCoordinator, preflight func(context.Context) error) *sessionManager {
	if coordinator == nil {
		coordinator = noopCoordinator{}
	}
	if preflight == nil {
		preflight = func(context.Context) error { return nil }
	}
	return &sessionManager{
		gate:        make(chan struct{}, 1),
		coordinator: coordinator,
		connect:     factory,
		preflight:   preflight,
	}
}

func (m *sessionManager) Status(ctx context.Context) (jetkvm.StatusResult, error) {
	var result jetkvm.StatusResult
	err := m.run(ctx, false, true, func(session deviceSession) error {
		var err error
		result, err = session.status(ctx)
		return err
	})
	return result, err
}

func (m *sessionManager) Screenshot(ctx context.Context) (jetkvm.Screenshot, error) {
	if err := m.preflight(ctx); err != nil {
		return jetkvm.Screenshot{}, err
	}
	var result jetkvm.Screenshot
	err := m.run(ctx, false, true, func(session deviceSession) error {
		var err error
		result, err = session.screenshot(ctx)
		return err
	})
	return result, err
}

func (m *sessionManager) Keypress(ctx context.Context, key, modifier int) error {
	if err := jetkvm.ValidateKeypress(key, modifier); err != nil {
		return err
	}
	return m.run(ctx, true, false, func(session deviceSession) error {
		return session.keypress(ctx, key, modifier)
	})
}

func (m *sessionManager) MouseMove(ctx context.Context, x, y, buttons int) error {
	if err := jetkvm.ValidatePointer(x, y, buttons); err != nil {
		return err
	}
	return m.run(ctx, true, false, func(session deviceSession) error {
		return session.mouseMove(ctx, x, y, buttons)
	})
}

func (m *sessionManager) ReleaseAll(ctx context.Context) error {
	return m.run(ctx, true, false, func(session deviceSession) error {
		return session.releaseAll(ctx)
	})
}

func (m *sessionManager) run(ctx context.Context, allowControl, retryReadOnly bool, operation func(deviceSession) error) (err error) {
	select {
	case m.gate <- struct{}{}:
		defer func() { <-m.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}

	releaseCoordination, err := m.coordinator.lock(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, releaseCoordination()) }()

	maxAttempts := 1
	if retryReadOnly {
		maxAttempts = readOnlyMaxAttempts
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		session, connectErr := m.connect(ctx, allowControl)
		if connectErr != nil {
			if attempt < maxAttempts && jetkvm.IsRetryableReadOnly(connectErr) {
				if err := waitRetry(ctx); err != nil {
					return errors.Join(connectErr, err)
				}
				continue
			}
			return connectErr
		}

		operationErr := operation(session)
		closeCtx, cancel := context.WithTimeout(context.Background(), sessionCloseTimeout)
		closeErr := session.close(closeCtx)
		cancel()

		if operationErr == nil && closeErr == nil {
			return nil
		}
		if operationErr != nil && closeErr == nil && attempt < maxAttempts && jetkvm.IsRetryableReadOnly(operationErr) {
			if err := waitRetry(ctx); err != nil {
				return errors.Join(operationErr, err)
			}
			continue
		}
		return errors.Join(operationErr, closeErr)
	}
	return fmt.Errorf("mcpserver: read-only retry bound exhausted")
}

func waitRetry(ctx context.Context) error {
	timer := time.NewTimer(retryBackoff)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type clientSession struct {
	client *jetkvm.Client
}

func (s *clientSession) status(ctx context.Context) (jetkvm.StatusResult, error) {
	return s.client.Status(ctx)
}

func (s *clientSession) screenshot(ctx context.Context) (jetkvm.Screenshot, error) {
	return s.client.CaptureScreenshot(ctx)
}

func (s *clientSession) keypress(ctx context.Context, key, modifier int) error {
	if err := jetkvm.ValidateKeypress(key, modifier); err != nil {
		return err
	}
	lease, err := s.client.Control()
	if err != nil {
		return err
	}
	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	sendErr := held.SendKeyboardReport(ctx, byte(modifier), []byte{byte(key)})
	releaseErr := held.Release()
	return errors.Join(sendErr, releaseErr)
}

func (s *clientSession) mouseMove(ctx context.Context, x, y, buttons int) error {
	if err := jetkvm.ValidatePointer(x, y, buttons); err != nil {
		return err
	}
	lease, err := s.client.Control()
	if err != nil {
		return err
	}
	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	sendErr := held.SendPointerReport(ctx, int32(x), int32(y), byte(buttons))
	releaseErr := held.Release()
	return errors.Join(sendErr, releaseErr)
}

func (s *clientSession) releaseAll(ctx context.Context) error {
	lease, err := s.client.Control()
	if err != nil {
		return err
	}
	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	return held.Release()
}

func (s *clientSession) close(ctx context.Context) error {
	return s.client.Close(ctx)
}
