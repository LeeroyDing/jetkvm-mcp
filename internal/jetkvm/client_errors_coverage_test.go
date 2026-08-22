package jetkvm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClassifyConnectErrorBranches(t *testing.T) {
	preclassified := fmt.Errorf("outer classification: %w", &DeviceError{
		Kind:      ErrorKindBadFrame,
		Operation: "decoding status",
	})

	tests := []struct {
		name          string
		err           error
		fallback      ErrorKind
		wantKind      ErrorKind
		wantOperation string
		wantSame      bool
	}{
		{
			name:          "preclassified error is preserved",
			err:           preclassified,
			fallback:      ErrorKindUnreachable,
			wantKind:      ErrorKindBadFrame,
			wantOperation: "decoding status",
			wantSame:      true,
		},
		{
			name:          "unauthorized API response",
			err:           &APIError{Path: "/device", StatusCode: 401, Body: "unauthorized"},
			fallback:      ErrorKindUnreachable,
			wantKind:      ErrorKindAuthFailed,
			wantOperation: "connecting",
		},
		{
			name:          "forbidden API response",
			err:           &APIError{Path: "/device", StatusCode: 403, Body: "forbidden"},
			fallback:      ErrorKindUnreachable,
			wantKind:      ErrorKindAuthFailed,
			wantOperation: "connecting",
		},
		{
			name:          "server API response",
			err:           &APIError{Path: "/device/status", StatusCode: 503, Body: "busy"},
			fallback:      ErrorKindAuthFailed,
			wantKind:      ErrorKindUnreachable,
			wantOperation: "connecting",
		},
		{
			name:          "ordinary API response uses fallback",
			err:           &APIError{Path: "/device/status", StatusCode: 418, Body: "teapot"},
			fallback:      ErrorKindTimeout,
			wantKind:      ErrorKindTimeout,
			wantOperation: "connecting",
		},
		{
			name:          "ordinary error uses fallback",
			err:           errors.New("transport refused connection"),
			fallback:      ErrorKindUnreachable,
			wantKind:      ErrorKindUnreachable,
			wantOperation: "connecting",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyConnectError("connecting", tc.err, tc.fallback)
			if kind := ErrorKindOf(got); kind != tc.wantKind {
				t.Fatalf("classifyConnectError kind = %q, want %q: %v", kind, tc.wantKind, got)
			}
			if tc.wantSame && got != tc.err {
				t.Fatalf("classifyConnectError replaced preclassified error: got %T %v, want original %T %v", got, got, tc.err, tc.err)
			}

			var deviceErr *DeviceError
			if !errors.As(got, &deviceErr) {
				t.Fatalf("classifyConnectError result = %T %v, want a DeviceError", got, got)
			}
			if deviceErr.Operation != tc.wantOperation {
				t.Errorf("DeviceError operation = %q, want %q", deviceErr.Operation, tc.wantOperation)
			}
		})
	}
}

func TestErrorKindOfBranches(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{
			name: "wrapped device error",
			err:  fmt.Errorf("outer: %w", &DeviceError{Kind: ErrorKindBadFrame}),
			want: ErrorKindBadFrame,
		},
		{
			name: "wrapped closed HID transport",
			err:  fmt.Errorf("outer: %w", ErrHIDClosed),
			want: ErrorKindUnreachable,
		},
		{
			name: "canceled context",
			err:  context.Canceled,
			want: ErrorKindTimeout,
		},
		{
			name: "wrapped deadline",
			err:  fmt.Errorf("outer: %w", context.DeadlineExceeded),
			want: ErrorKindTimeout,
		},
		{
			name: "unclassified error",
			err:  errors.New("unclassified"),
		},
		{
			name: "nil error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorKindOf(tc.err); got != tc.want {
				t.Fatalf("ErrorKindOf(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestNewDeviceErrorBranches(t *testing.T) {
	existing := &DeviceError{
		Kind:      ErrorKindBadFrame,
		Operation: "existing operation",
		Detail:    "existing detail",
	}
	wrappedExisting := fmt.Errorf("outer: %w", existing)
	ordinary := errors.New("safe dependency detail")

	tests := []struct {
		name          string
		cause         error
		wantSame      error
		wantKind      ErrorKind
		wantOperation string
		wantDetail    string
		wantFlattened bool
	}{
		{
			name:          "existing device error",
			cause:         existing,
			wantSame:      existing,
			wantKind:      ErrorKindBadFrame,
			wantOperation: "existing operation",
			wantDetail:    "existing detail",
		},
		{
			name:          "wrapped existing device error",
			cause:         wrappedExisting,
			wantSame:      wrappedExisting,
			wantKind:      ErrorKindBadFrame,
			wantOperation: "existing operation",
			wantDetail:    "existing detail",
		},
		{
			name:          "nil cause",
			wantKind:      ErrorKindUnreachable,
			wantOperation: "testing constructor",
		},
		{
			name:          "ordinary cause is flattened",
			cause:         ordinary,
			wantKind:      ErrorKindUnreachable,
			wantOperation: "testing constructor",
			wantDetail:    ordinary.Error(),
			wantFlattened: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := newDeviceError(ErrorKindUnreachable, "testing constructor", tc.cause)
			if tc.wantSame != nil && got != tc.wantSame {
				t.Fatalf("newDeviceError result = %T %v, want original %T %v", got, got, tc.wantSame, tc.wantSame)
			}
			if tc.wantFlattened && errors.Is(got, tc.cause) {
				t.Fatalf("newDeviceError retained raw cause through unwrapping: %v", got)
			}

			var deviceErr *DeviceError
			if !errors.As(got, &deviceErr) {
				t.Fatalf("newDeviceError result = %T %v, want a DeviceError", got, got)
			}
			if deviceErr.Kind != tc.wantKind {
				t.Errorf("DeviceError kind = %q, want %q", deviceErr.Kind, tc.wantKind)
			}
			if deviceErr.Operation != tc.wantOperation {
				t.Errorf("DeviceError operation = %q, want %q", deviceErr.Operation, tc.wantOperation)
			}
			if deviceErr.Detail != tc.wantDetail {
				t.Errorf("DeviceError detail = %q, want %q", deviceErr.Detail, tc.wantDetail)
			}
		})
	}
}

func TestDeviceErrorUnknownKindWording(t *testing.T) {
	tests := []struct {
		name string
		err  *DeviceError
		want string
	}{
		{
			name: "unknown kind without optional context",
			err:  &DeviceError{Kind: ErrorKind("future-kind")},
			want: "jetkvm: future-kind: device operation failed",
		},
		{
			name: "unknown kind with operation and detail",
			err: &DeviceError{
				Kind:      ErrorKind("future-kind"),
				Operation: "probing device",
				Detail:    "safe detail",
			},
			want: "jetkvm: future-kind: device operation failed during probing device: safe detail",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("DeviceError.Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestURLHelperEdgeCases(t *testing.T) {
	t.Run("loopback hosts", func(t *testing.T) {
		tests := []struct {
			name string
			host string
			want bool
		}{
			{name: "localhost", host: "localhost", want: true},
			{name: "mixed case localhost", host: "LoCaLhOsT", want: true},
			{name: "other address in IPv4 loopback block", host: "127.255.255.254", want: true},
			{name: "IPv6 loopback", host: "::1", want: true},
			{name: "IPv4 mapped loopback", host: "::ffff:127.0.0.1", want: true},
			{name: "non-loopback IP", host: "192.0.2.1"},
			{name: "DNS name", host: "jetkvm.local"},
			{name: "malformed IP", host: "127.0.0.999"},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if got := isLoopbackHost(tc.host); got != tc.want {
					t.Fatalf("isLoopbackHost(%q) = %t, want %t", tc.host, got, tc.want)
				}
			})
		}
	})

	t.Run("canonical hostnames", func(t *testing.T) {
		tests := []struct {
			name    string
			raw     string
			want    string
			wantErr bool
		}{
			{name: "IPv6 zone preserves case", raw: "fe80:0:0:0:0:0:0:1%En0", want: "fe80::1%En0"},
			{name: "empty IPv6 zone", raw: "fe80::1%", wantErr: true},
			{name: "zone on DNS name", raw: "jetkvm.local%en0", wantErr: true},
			{name: "zone on IPv4 literal", raw: "127.0.0.1%lo0", wantErr: true},
			{name: "DNS root marker", raw: "JetKVM.Local.", want: "jetkvm.local"},
			{name: "repeated DNS root marker", raw: "jetkvm.local..", wantErr: true},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				got, err := canonicalHostname(tc.raw)
				if (err != nil) != tc.wantErr {
					t.Fatalf("canonicalHostname(%q) = %q, %v; wantErr %t", tc.raw, got, err, tc.wantErr)
				}
				if err == nil && got != tc.want {
					t.Fatalf("canonicalHostname(%q) = %q, want %q", tc.raw, got, tc.want)
				}
			})
		}
	})
}

// observedLockWaitContext exposes the instant Client.lock has completed its
// fail-fast Err check. Closing done after initialCheck is observed makes the
// lock's waiting-cancellation branch deterministic without sleeps.
type observedLockWaitContext struct {
	done         chan struct{}
	initialCheck chan struct{}
	err          error
	once         sync.Once
}

func (c *observedLockWaitContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *observedLockWaitContext) Done() <-chan struct{}       { return c.done }
func (c *observedLockWaitContext) Value(any) any               { return nil }

func (c *observedLockWaitContext) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		c.once.Do(func() { close(c.initialCheck) })
		return nil
	}
}

func TestClientLockReturnsCancellationWhileWaiting(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{cmdMu: make(chan struct{}, 1)}
			client.cmdMu <- struct{}{}
			ctx := &observedLockWaitContext{
				done:         make(chan struct{}),
				initialCheck: make(chan struct{}),
				err:          tc.err,
			}

			type lockResult struct {
				unlock func()
				err    error
			}
			result := make(chan lockResult, 1)
			go func() {
				unlock, err := client.lock(ctx)
				result <- lockResult{unlock: unlock, err: err}
			}()

			<-ctx.initialCheck
			close(ctx.done)

			select {
			case got := <-result:
				if got.unlock != nil {
					got.unlock()
					t.Fatal("lock returned an unlock function after its waiting context ended")
				}
				if !errors.Is(got.err, tc.err) {
					t.Fatalf("lock error = %v, want %v", got.err, tc.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("lock did not return after its waiting context ended")
			}

			if len(client.cmdMu) != 1 {
				t.Fatalf("blocked lock changed the occupied command slot count to %d", len(client.cmdMu))
			}
			<-client.cmdMu
		})
	}
}

func TestClientStatusRPCSendFailures(t *testing.T) {
	transportFailure := errors.New("synthetic RPC transport failure")
	tests := []struct {
		name          string
		channel       func() *fakeRPCDataChannel
		wantKind      ErrorKind
		wantErr       error
		wantSent      int
		wantRawHidden error
	}{
		{
			name: "transport send failure is unreachable and ambiguous",
			channel: func() *fakeRPCDataChannel {
				return &fakeRPCDataChannel{sendErr: transportFailure}
			},
			wantKind:      ErrorKindUnreachable,
			wantErr:       errRPCAmbiguousDelivery,
			wantSent:      1,
			wantRawHidden: transportFailure,
		},
		{
			name: "buffer rejection is unambiguous",
			channel: func() *fakeRPCDataChannel {
				return &fakeRPCDataChannel{buffered: maxRPCBufferedAmount}
			},
			wantErr:  ErrRPCBufferFull,
			wantSent: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			channel := tc.channel()
			client := &Client{
				deviceID:    "coverage-device",
				firmwareVer: "coverage-firmware",
				cmdMu:       make(chan struct{}, 1),
				sess:        &session{rpc: newRPCClientWithChannel(channel)},
			}

			status, err := client.Status(context.Background())
			if err == nil {
				t.Fatal("Status succeeded after its RPC send was rejected")
			}
			if kind := ErrorKindOf(err); kind != tc.wantKind {
				t.Fatalf("Status error kind = %q, want %q: %v", kind, tc.wantKind, err)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Status error = %v, want wrapped %v", err, tc.wantErr)
			}
			if tc.wantRawHidden != nil && errors.Is(err, tc.wantRawHidden) {
				t.Fatalf("Status error retained raw transport failure: %v", err)
			}
			if status.DeviceID != client.deviceID || status.FirmwareVersion != client.firmwareVer {
				t.Fatalf("failed Status metadata = %+v, want device=%q firmware=%q", status, client.deviceID, client.firmwareVer)
			}
			if status.RPCReachable {
				t.Fatal("failed Status reported RPCReachable=true")
			}
			if len(channel.sent) != tc.wantSent {
				t.Fatalf("Status sent %d RPC frames, want %d", len(channel.sent), tc.wantSent)
			}
			if len(client.cmdMu) != 0 {
				t.Fatal("failed Status retained the command lock")
			}
		})
	}
}

func TestClientScrollFailsClosedWithoutRPCSession(t *testing.T) {
	tests := []struct {
		name string
		sess *session
	}{
		{name: "nil session"},
		{name: "session with nil RPC", sess: &session{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hid, transport := newFakeHIDClient(t)
			lease := newControlLease(hid)
			client := &Client{
				sess:         tc.sess,
				allowControl: true,
				cmdMu:        make(chan struct{}, 1),
				control:      lease,
			}
			before := transport.count()

			err := client.Scroll(context.Background(), 0, 1)
			if kind := ErrorKindOf(err); kind != ErrorKindUnreachable {
				t.Fatalf("Scroll error kind = %q, want %q: %v", kind, ErrorKindUnreachable, err)
			}
			if !strings.Contains(err.Error(), "RPC session is unavailable") {
				t.Fatalf("Scroll error does not identify the unavailable RPC session: %v", err)
			}
			if got := transport.count(); got != before+2 {
				t.Fatalf("Scroll failure wrote %d HID frames, want two terminal neutral reports", got-before)
			}
			if hid.activeGeneration() != 0 {
				t.Fatal("Scroll failure left its control generation active")
			}
			if len(lease.slot) != 0 {
				t.Fatal("Scroll failure retained the control lease slot")
			}
			if len(client.cmdMu) != 0 {
				t.Fatal("Scroll failure retained the command lock")
			}
		})
	}
}
