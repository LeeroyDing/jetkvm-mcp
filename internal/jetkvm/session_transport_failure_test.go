package jetkvm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEstablishSessionFailsClosedOnDTLSFingerprintMismatch(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{InvalidDTLSFingerprint: true})
	ctx := contextWithTimeout(t, connectTimeout(t, 10*time.Second))
	sig, _, err := dialSignaling(ctx, fd.baseURL(), nil)
	if err != nil {
		t.Fatalf("dialSignaling: %v", err)
	}
	defer sig.close()

	sess, err := establishSession(ctx, sig, dialOptions{loopbackOnlyICE: true})
	if sess != nil {
		sess.close()
		t.Fatal("establishSession returned a session after DTLS identity verification failed")
	}
	if err == nil {
		t.Fatal("establishSession accepted an answer with a mismatched DTLS fingerprint")
	}
	if ctx.Err() != nil {
		t.Fatalf("DTLS identity failure was reported only after the test deadline: %v", err)
	}
	if got, want := err.Error(), "jetkvm: peer connection closed before becoming connected"; got != want {
		t.Fatalf("establishSession error = %q, want %q", got, want)
	}
}

func TestEstablishSessionCancellationWhileRPCMediaIsRejected(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{RejectDataChannels: true})
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout(t, 10*time.Second))
	defer cancel()
	sig, _, err := dialSignaling(ctx, fd.baseURL(), nil)
	if err != nil {
		t.Fatalf("dialSignaling: %v", err)
	}
	defer sig.close()

	type result struct {
		sess *session
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		sess, establishErr := establishSession(ctx, sig, dialOptions{loopbackOnlyICE: true})
		resultCh <- result{sess: sess, err: establishErr}
	}()

	select {
	case <-fd.peerConnected:
		// The H.264 transport still connects even though the answer rejected its
		// application media section. Give the local Connected callback time to
		// release the first readiness wait before canceling the RPC wait.
	case got := <-resultCh:
		if got.sess != nil {
			got.sess.close()
		}
		t.Fatalf("establishSession returned before the media transport connected: %v", got.err)
	case <-ctx.Done():
		t.Fatalf("fake peer did not connect before the test deadline: %v", ctx.Err())
	}

	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case got := <-resultCh:
		if got.sess != nil {
			got.sess.close()
		}
		t.Fatalf("establishSession returned before cancellation despite the rejected RPC transport: %v", got.err)
	case <-timer.C:
	}
	cancel()

	var got result
	select {
	case got = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("establishSession did not return after cancellation")
	}
	if got.sess != nil {
		got.sess.close()
		t.Fatal("establishSession returned a session without an open RPC channel")
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("establishSession error = %v, want context.Canceled", got.err)
	}
	if !strings.Contains(got.err.Error(), "waiting for rpc data channel to open") {
		t.Fatalf("establishSession error = %q, want RPC-readiness context", got.err)
	}
}
