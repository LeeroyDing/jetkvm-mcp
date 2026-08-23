package jetkvm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConnectFailsClosedWhenHIDReadinessIsNotConfirmed(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{RejectHIDHandshake: true})
	ctx := contextWithTimeout(t, connectTimeout(t, 15*time.Second))

	client, err := Connect(ctx, Options{
		BaseURL:      fd.baseURL(),
		AllowControl: true,
	})
	if client != nil {
		_ = client.Close(context.Background())
		t.Fatal("Connect returned a client whose HID channel never became ready")
	}
	if !errors.Is(err, ErrHIDClosed) {
		t.Fatalf("Connect error = %v, want ErrHIDClosed", err)
	}
	if got := ErrorKindOf(err); got != ErrorKindUnreachable {
		t.Fatalf("Connect error kind = %q, want %q: %v", got, ErrorKindUnreachable, err)
	}
}

func TestHIDHandshakeRejectsTerminalStateAtReadyCommit(t *testing.T) {
	transport := &fakeHIDTransport{autoDrain: true}
	hid := newHIDClient(transport)
	wantCause := errors.New("device disconnected after confirming HID readiness")
	transport.setBeforeSend(func(frame []byte) {
		// Model the real callback race in which the peer confirms readiness and
		// transitions terminal before handshake can commit ready ownership. Keep
		// the writer alive until the commit so this test reaches that boundary
		// deterministically rather than winning via writerDone.
		hid.handleMessage(frame)
		hid.stateMu.Lock()
		hid.state = hidStateClosed
		hid.closeCause = wantCause
		hid.stateMu.Unlock()
	})
	t.Cleanup(func() {
		hid.closeWith(errors.New("test cleanup"))
		select {
		case <-hid.writerDone:
		case <-time.After(time.Second):
			t.Error("HID writer did not stop during cleanup")
		}
	})

	err := hid.handshake(contextWithTimeout(t, time.Second))
	if !errors.Is(err, ErrHIDClosed) {
		t.Fatalf("handshake error = %v, want ErrHIDClosed", err)
	}
	if !strings.Contains(err.Error(), "device disconnected after confirming HID readiness") {
		t.Fatalf("handshake error lost the terminal cause: %v", err)
	}
	if got := hid.currentState(); got != hidStateClosed {
		t.Fatalf("state after terminal readiness race = %s, want closed", got)
	}
}

func TestReleaseKeyboardCancellationCannotClearOrReachDevice(t *testing.T) {
	hid, transport := newFakeHIDClient(t)
	token, err := hid.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease: %v", err)
	}
	if err := hid.sendKeyboardReport(context.Background(), token, 0x02, []byte{0x04}); err != nil {
		t.Fatalf("send held keyboard state: %v", err)
	}
	if !hid.hasHeldState() {
		t.Fatal("held keyboard report did not update the conservative state")
	}
	framesBeforeRelease := transport.count()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err = hid.releaseKeyboard(canceled, token)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("releaseKeyboard error = %v, want context.Canceled", err)
	}
	if got := transport.count(); got != framesBeforeRelease {
		t.Fatalf("canceled release reached the device: frame count %d -> %d", framesBeforeRelease, got)
	}
	if !hid.hasHeldState() {
		t.Fatal("canceled release falsely cleared conservative held state")
	}
}

func TestReleaseKeyboardTransportFailurePreservesHeldState(t *testing.T) {
	hid, transport := newFakeHIDClient(t)
	token, err := hid.beginLease(context.Background())
	if err != nil {
		t.Fatalf("beginLease: %v", err)
	}
	if err := hid.sendKeyboardReport(context.Background(), token, 0, []byte{0x04}); err != nil {
		t.Fatalf("send held keyboard state: %v", err)
	}
	framesBeforeRelease := transport.count()
	transport.setFailure(1, errors.New("synthetic keyboard-release write failure"))

	err = hid.releaseKeyboard(contextWithTimeout(t, time.Second), token)
	if got := ErrorKindOf(err); got != ErrorKindUnreachable {
		t.Fatalf("releaseKeyboard error kind = %q, want %q: %v", got, ErrorKindUnreachable, err)
	}
	if got := transport.count(); got != framesBeforeRelease {
		t.Fatalf("failed release was recorded as delivered: frame count %d -> %d", framesBeforeRelease, got)
	}
	if !hid.hasHeldState() {
		t.Fatal("failed release falsely cleared conservative held state")
	}
}
