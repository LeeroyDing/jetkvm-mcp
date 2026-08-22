package jetkvm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

func TestDiagnosticsRecordRejectedTrack(t *testing.T) {
	diag := newVideoDiagnostics()
	diag.trackStarted(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9},
		PayloadType:        98,
	})
	diag.trackRejected("non-h264-codec")

	snapshot := diag.snapshot(nil)
	if snapshot.TrackRejection != "non-h264-codec" {
		t.Fatalf("TrackRejection = %q, want non-h264-codec", snapshot.TrackRejection)
	}
	if snapshot.TrackMimeType != "video/VP9" {
		t.Fatalf("TrackMimeType = %q, want video/VP9", snapshot.TrackMimeType)
	}
	if snapshot.FailureBoundary != BoundaryTrackRejected {
		t.Fatalf("FailureBoundary = %q, want %q", snapshot.FailureBoundary, BoundaryTrackRejected)
	}
}

func TestSanitizeMimeTypeRemainingKnownCodecs(t *testing.T) {
	tests := map[string]string{
		webrtc.MimeTypeVP9: "video/VP9",
		webrtc.MimeTypeAV1: "video/AV1",
	}
	for input, want := range tests {
		if got := sanitizeMimeType(input); got != want {
			t.Errorf("sanitizeMimeType(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAddH264RecvonlyTransceiverRejectsClosedPeerConnection(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = addH264RecvonlyTransceiver(pc)
	if err == nil {
		t.Fatal("addH264RecvonlyTransceiver succeeded on a closed peer connection")
	}
	if !strings.Contains(err.Error(), "adding video transceiver") {
		t.Fatalf("closed-peer error = %v, want video transceiver context", err)
	}
}

func TestEstablishSessionRejectsClosedSignalingConnection(t *testing.T) {
	sig := dialTestSignaler(t, func(context.Context, *websocket.Conn) {})
	if err := sig.close(); err != nil {
		t.Fatalf("closing signaling connection: %v", err)
	}

	ctx := contextWithTimeout(t, 2*time.Second)
	session, err := establishSession(ctx, sig, dialOptions{loopbackOnlyICE: true})
	if err == nil {
		if session != nil {
			session.close()
		}
		t.Fatal("establishSession succeeded with a closed signaling connection")
	}
	if session != nil {
		session.close()
		t.Fatal("establishSession returned a session after signaling failed")
	}
	if !strings.Contains(err.Error(), "sending offer") {
		t.Fatalf("closed-signaling error = %v, want sending offer context", err)
	}
}

func TestEstablishSessionReturnsWhenContextCanceledAfterOffer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sig := dialTestSignaler(t, func(serverCtx context.Context, conn *websocket.Conn) {
		for {
			_, data, err := conn.Read(serverCtx)
			if err != nil {
				return
			}
			var message signalingMessage
			if json.Unmarshal(data, &message) == nil && message.Type == "new-ice-candidate" {
				// A candidate is only sent after sendOffer has returned and the
				// offer has been marked as on the wire. Canceling here therefore
				// drives the connection-wait branch without a timing race.
				cancel()
				return
			}
		}
	})

	session, err := establishSession(ctx, sig, dialOptions{loopbackOnlyICE: true})
	if err == nil {
		if session != nil {
			session.close()
		}
		t.Fatal("establishSession succeeded after its context was canceled")
	}
	if session != nil {
		session.close()
		t.Fatal("establishSession returned a session after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("establishSession error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "waiting for connection") {
		t.Fatalf("canceled establishSession error = %v, want connection-wait context", err)
	}
}

func TestRequestKeyframesRetriesWriteFailureUntilCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatalf("NewPeerConnection: %v", err)
		}
		if err := pc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		diag := newVideoDiagnostics()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(750*time.Millisecond, cancel)

		requestKeyframes(ctx, pc, &webrtc.TrackRemote{}, diag)

		snapshot := diag.snapshot(nil)
		if snapshot.KeyframeRequestsFail != 2 {
			t.Fatalf("failed keyframe requests = %d, want the initial and safety-net attempts", snapshot.KeyframeRequestsFail)
		}
		if snapshot.KeyframeRequestsSent != 0 {
			t.Fatalf("successful keyframe requests = %d on a closed peer connection, want 0", snapshot.KeyframeRequestsSent)
		}
	})
}
