package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

type fakeSession struct {
	statusFn      func(context.Context) (jetkvm.StatusResult, error)
	screenshotFn  func(context.Context) (jetkvm.Screenshot, error)
	keypressFn    func(context.Context, int, int) error
	mouseMoveFn   func(context.Context, int, int, int) error
	releaseAllFn  func(context.Context) error
	closeFn       func(context.Context) error
	closeCalls    atomic.Int32
	operationDone chan struct{}
}

func (s *fakeSession) status(ctx context.Context) (jetkvm.StatusResult, error) {
	if s.statusFn != nil {
		return s.statusFn(ctx)
	}
	return jetkvm.StatusResult{DeviceID: "fake", RPCReachable: true}, nil
}

func (s *fakeSession) screenshot(ctx context.Context) (jetkvm.Screenshot, error) {
	if s.screenshotFn != nil {
		return s.screenshotFn(ctx)
	}
	return jetkvm.Screenshot{}, nil
}

func (s *fakeSession) keypress(ctx context.Context, key, modifier int) error {
	if s.keypressFn != nil {
		return s.keypressFn(ctx, key, modifier)
	}
	return nil
}

func (s *fakeSession) mouseMove(ctx context.Context, x, y, buttons int) error {
	if s.mouseMoveFn != nil {
		return s.mouseMoveFn(ctx, x, y, buttons)
	}
	return nil
}

func (s *fakeSession) releaseAll(ctx context.Context) error {
	if s.releaseAllFn != nil {
		return s.releaseAllFn(ctx)
	}
	return nil
}

func (s *fakeSession) close(ctx context.Context) error {
	s.closeCalls.Add(1)
	if s.closeFn != nil {
		return s.closeFn(ctx)
	}
	return nil
}

type scriptedFactory struct {
	mu           sync.Mutex
	sessions     []deviceSession
	connectErrs  []error
	connectCalls int
	controlFlags []bool
	active       int
	maxActive    int
}

func (f *scriptedFactory) connect(ctx context.Context, allowControl bool) (deviceSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.connectCalls
	f.connectCalls++
	f.controlFlags = append(f.controlFlags, allowControl)
	if index < len(f.connectErrs) && f.connectErrs[index] != nil {
		return nil, f.connectErrs[index]
	}
	if index >= len(f.sessions) {
		return nil, errors.New("test factory exhausted")
	}
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	original := f.sessions[index]
	return &activeTrackingSession{deviceSession: original, onClose: func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}}, nil
}

type activeTrackingSession struct {
	deviceSession
	once    sync.Once
	onClose func()
}

func (s *activeTrackingSession) close(ctx context.Context) error {
	err := s.deviceSession.close(ctx)
	s.once.Do(s.onClose)
	return err
}

func (f *scriptedFactory) snapshot() (calls, maxActive int, control []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connectCalls, f.maxActive, append([]bool(nil), f.controlFlags...)
}

func TestSessionManagerSerializesSimultaneousCallsAndClosesEach(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	first := &fakeSession{statusFn: func(context.Context) (jetkvm.StatusResult, error) {
		close(firstStarted)
		<-releaseFirst
		return jetkvm.StatusResult{DeviceID: "one", RPCReachable: true}, nil
	}}
	second := &fakeSession{}
	factory := &scriptedFactory{sessions: []deviceSession{first, second}}
	manager := newSessionManagerWith(factory.connect, nil, nil)

	results := make(chan error, 2)
	go func() { _, err := manager.Status(context.Background()); results <- err }()
	<-firstStarted
	go func() { _, err := manager.Status(context.Background()); results <- err }()

	time.Sleep(50 * time.Millisecond)
	if calls, _, _ := factory.snapshot(); calls != 1 {
		t.Fatalf("second simultaneous call connected before first closed: calls=%d", calls)
	}
	close(releaseFirst)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("Status failed: %v", err)
		}
	}
	calls, maxActive, _ := factory.snapshot()
	if calls != 2 || maxActive != 1 {
		t.Fatalf("calls=%d maxActive=%d, want 2 and 1", calls, maxActive)
	}
	if first.closeCalls.Load() != 1 || second.closeCalls.Load() != 1 {
		t.Fatalf("close calls = (%d,%d), want (1,1)", first.closeCalls.Load(), second.closeCalls.Load())
	}
}

func TestSessionManagerUsesFreshClosedRealSessionsPerCall(t *testing.T) {
	fd := startFakeDevice(t)
	factory := func(ctx context.Context, allowControl bool) (deviceSession, error) {
		client, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL(), AllowControl: allowControl})
		if err != nil {
			return nil, err
		}
		return &clientSession{client: client}, nil
	}
	manager := newSessionManagerWith(factory, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for range 2 {
		status, err := manager.Status(ctx)
		if err != nil {
			t.Fatalf("fresh-session Status failed: %v", err)
		}
		if !status.RPCReachable {
			t.Fatal("fresh-session Status did not reach RPC")
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		opened, closed := fd.sessionCounts()
		if opened == 2 && closed >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fresh sessions were not deterministically closed: opened=%d closed=%d", opened, closed)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSessionManagerQueuedCancellationNeverConnects(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	first := &fakeSession{statusFn: func(context.Context) (jetkvm.StatusResult, error) {
		close(firstStarted)
		<-releaseFirst
		return jetkvm.StatusResult{}, nil
	}}
	factory := &scriptedFactory{sessions: []deviceSession{first}}
	manager := newSessionManagerWith(factory.connect, nil, nil)
	done := make(chan error, 1)
	go func() { _, err := manager.Status(context.Background()); done <- err }()
	<-firstStarted

	queuedCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := manager.Status(queuedCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued Status error = %v, want deadline exceeded", err)
	}
	if calls, _, _ := factory.snapshot(); calls != 1 {
		t.Fatalf("canceled queued call connected: calls=%d", calls)
	}
	close(releaseFirst)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSessionManagerCancellationDuringConnectIsBounded(t *testing.T) {
	var calls atomic.Int32
	factory := func(ctx context.Context, allowControl bool) (deviceSession, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	manager := newSessionManagerWith(factory, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := manager.Status(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Status error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("canceled connect took %v", elapsed)
	}
	if calls.Load() != 1 {
		t.Fatalf("canceled connect calls=%d, want 1", calls.Load())
	}
}

func TestSessionManagerRetriesReadOnlyTransportFailureExactlyOnce(t *testing.T) {
	dropped := fmt.Errorf("peer dropped: %w", jetkvm.ErrSessionTransport)
	first := &fakeSession{statusFn: func(context.Context) (jetkvm.StatusResult, error) {
		return jetkvm.StatusResult{}, dropped
	}}
	second := &fakeSession{}
	factory := &scriptedFactory{sessions: []deviceSession{first, second}}
	manager := newSessionManagerWith(factory.connect, nil, nil)

	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatalf("Status failed after bounded retry: %v", err)
	}
	if !status.RPCReachable {
		t.Fatal("retry result was not returned")
	}
	if calls, _, flags := factory.snapshot(); calls != 2 || flags[0] || flags[1] {
		t.Fatalf("connect calls=%d control flags=%v, want two read-only sessions", calls, flags)
	}
	if first.closeCalls.Load() != 1 || second.closeCalls.Load() != 1 {
		t.Fatalf("sessions not closed exactly once: first=%d second=%d", first.closeCalls.Load(), second.closeCalls.Load())
	}
}

func TestSessionManagerRetryBoundStopsAfterSecondFailure(t *testing.T) {
	dropped := fmt.Errorf("peer dropped: %w", jetkvm.ErrSessionTransport)
	factory := &scriptedFactory{sessions: []deviceSession{
		&fakeSession{statusFn: func(context.Context) (jetkvm.StatusResult, error) { return jetkvm.StatusResult{}, dropped }},
		&fakeSession{statusFn: func(context.Context) (jetkvm.StatusResult, error) { return jetkvm.StatusResult{}, dropped }},
		&fakeSession{},
	}}
	manager := newSessionManagerWith(factory.connect, nil, nil)

	if _, err := manager.Status(context.Background()); !errors.Is(err, jetkvm.ErrSessionTransport) {
		t.Fatalf("Status error = %v, want transport failure", err)
	}
	if calls, _, _ := factory.snapshot(); calls != readOnlyMaxAttempts {
		t.Fatalf("connect calls=%d, want retry bound %d", calls, readOnlyMaxAttempts)
	}
}

func TestSessionManagerDoesNotRetryNonTransportReadOnlyFailure(t *testing.T) {
	decodeErr := errors.New("decoder rejected frame")
	first := &fakeSession{screenshotFn: func(context.Context) (jetkvm.Screenshot, error) {
		return jetkvm.Screenshot{}, decodeErr
	}}
	factory := &scriptedFactory{sessions: []deviceSession{first, &fakeSession{}}}
	manager := newSessionManagerWith(factory.connect, nil, nil)
	if _, err := manager.Screenshot(context.Background()); !errors.Is(err, decodeErr) {
		t.Fatalf("Screenshot error = %v, want decoder failure", err)
	}
	if calls, _, _ := factory.snapshot(); calls != 1 {
		t.Fatalf("non-transport screenshot failure was retried: calls=%d", calls)
	}
}

func TestSessionManagerNeverRetriesControl(t *testing.T) {
	dropped := fmt.Errorf("control transport dropped: %w", jetkvm.ErrSessionTransport)
	first := &fakeSession{keypressFn: func(context.Context, int, int) error { return dropped }}
	factory := &scriptedFactory{sessions: []deviceSession{first, &fakeSession{}}}
	manager := newSessionManagerWith(factory.connect, nil, nil)

	if err := manager.Keypress(context.Background(), 4, 0); !errors.Is(err, jetkvm.ErrSessionTransport) {
		t.Fatalf("Keypress error = %v, want transport failure", err)
	}
	if calls, _, flags := factory.snapshot(); calls != 1 || len(flags) != 1 || !flags[0] {
		t.Fatalf("control connect calls=%d flags=%v, want one control session", calls, flags)
	}
	if first.closeCalls.Load() != 1 {
		t.Fatalf("failed control session close calls=%d, want 1", first.closeCalls.Load())
	}
}

func TestSessionManagerPreservesOperationAndCleanupErrorsWithoutRetry(t *testing.T) {
	dropped := fmt.Errorf("send failed: %w", jetkvm.ErrSessionTransport)
	cleanup := errors.New("release-all unverified")
	session := &fakeSession{
		statusFn: func(context.Context) (jetkvm.StatusResult, error) { return jetkvm.StatusResult{}, dropped },
		closeFn:  func(context.Context) error { return cleanup },
	}
	factory := &scriptedFactory{sessions: []deviceSession{session, &fakeSession{}}}
	manager := newSessionManagerWith(factory.connect, nil, nil)

	_, err := manager.Status(context.Background())
	if !errors.Is(err, jetkvm.ErrSessionTransport) || !errors.Is(err, cleanup) {
		t.Fatalf("joined error = %v, want operation and cleanup errors", err)
	}
	if calls, _, _ := factory.snapshot(); calls != 1 {
		t.Fatalf("cleanup failure was retried: calls=%d", calls)
	}
}

func TestSessionManagerSuppliesFreshBoundedCleanupContext(t *testing.T) {
	var cleanupDeadline time.Time
	session := &fakeSession{
		statusFn: func(context.Context) (jetkvm.StatusResult, error) {
			return jetkvm.StatusResult{}, context.DeadlineExceeded
		},
		closeFn: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("cleanup began canceled: %w", err)
			}
			var ok bool
			cleanupDeadline, ok = ctx.Deadline()
			if !ok {
				return errors.New("cleanup context had no deadline")
			}
			return nil
		},
	}
	factory := &scriptedFactory{sessions: []deviceSession{session}}
	manager := newSessionManagerWith(factory.connect, nil, nil)

	// A request canceled before acquiring the gate never connects; use an
	// operation-owned deadline error to model expiry after connection.
	_, err := manager.Status(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Status error = %v, want operation deadline", err)
	}
	if cleanupDeadline.IsZero() || time.Until(cleanupDeadline) <= 0 || time.Until(cleanupDeadline) > sessionCloseTimeout {
		t.Fatalf("cleanup deadline = %v, want a fresh %v bound", cleanupDeadline, sessionCloseTimeout)
	}
}

func TestSessionManagerScreenshotPreflightAvoidsDeviceSession(t *testing.T) {
	canary := errors.New("ffmpeg unavailable")
	factory := &scriptedFactory{sessions: []deviceSession{&fakeSession{}}}
	manager := newSessionManagerWith(factory.connect, nil, func(context.Context) error { return canary })

	if _, err := manager.Screenshot(context.Background()); !errors.Is(err, canary) {
		t.Fatalf("Screenshot error = %v, want preflight error", err)
	}
	if calls, _, _ := factory.snapshot(); calls != 0 {
		t.Fatalf("preflight failure opened %d device sessions, want 0", calls)
	}
	if _, err := manager.Status(context.Background()); err != nil {
		t.Fatalf("status must remain usable without FFmpeg: %v", err)
	}
}

func TestSessionManagerValidatesControlBeforeConnecting(t *testing.T) {
	factory := &scriptedFactory{}
	manager := newSessionManagerWith(factory.connect, nil, nil)
	for _, err := range []error{
		manager.Keypress(context.Background(), 4, 256),
		manager.MouseMove(context.Background(), -1, 0, 0),
		manager.MouseMove(context.Background(), 0, 0, 256),
	} {
		if err == nil {
			t.Fatal("invalid control input was accepted")
		}
	}
	if calls, _, _ := factory.snapshot(); calls != 0 {
		t.Fatalf("invalid control input opened %d sessions", calls)
	}
}

func TestCanonicalDeviceIdentity(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
	}{
		{"http://JETKVM.local/", "http://jetkvm.local"},
		{"http://jetkvm.local:80", "http://jetkvm.local"},
		{"http://jetkvm.local:00080", "http://jetkvm.local"},
		{"https://jetkvm.local:443", "https://jetkvm.local"},
		{"http://[2001:db8::1]:8080", "http://[2001:db8::1]:8080"},
		{"http://[2001:0db8::1]:8080", "http://[2001:db8::1]:8080"},
		{"http://[fe80::1%25En0]", "http://[fe80::1%25En0]"},
	} {
		got, err := canonicalDeviceIdentity(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("canonicalDeviceIdentity(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}

	const canary = "URL-CREDENTIAL-CANARY"
	for _, raw := range []string{
		"http://user:" + canary + "@jetkvm.local",
		"http://jetkvm.local/?token=" + canary,
		"http://jetkvm.local/#" + canary,
		"http://jetkvm.local/" + canary,
	} {
		_, err := canonicalDeviceIdentity(raw)
		if err == nil {
			t.Errorf("accepted non-canonical URL %q", raw)
		} else if strings.Contains(err.Error(), canary) {
			t.Errorf("validation error leaked URL canary: %v", err)
		}
	}
}
