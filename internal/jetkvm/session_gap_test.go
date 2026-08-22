package jetkvm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

func TestAddH264RecvonlyTransceiverRejectsClosedPeer(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = addH264RecvonlyTransceiver(pc)
	if err == nil {
		t.Fatal("adding a transceiver to a closed peer unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "adding video transceiver") {
		t.Fatalf("error = %q, want video-transceiver context", err)
	}
}

func TestAddH264RecvonlyTransceiverRequiresRegisteredCodecs(t *testing.T) {
	mediaEngine := &webrtc.MediaEngine{}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	defer pc.Close()

	err = addH264RecvonlyTransceiver(pc)
	if err == nil {
		t.Fatal("configuring H.264 preferences without registered codecs unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "constraining video negotiation to H.264") {
		t.Fatalf("error = %q, want codec-preference context", err)
	}
}

func TestEstablishSessionRejectsClosedSignalingTransport(t *testing.T) {
	sig := dialTestSignaler(t, func(context.Context, *websocket.Conn) {})
	if err := sig.close(); err != nil {
		t.Fatalf("closing signaling transport: %v", err)
	}

	ctx := contextWithTimeout(t, 5*time.Second)
	sess, err := establishSession(ctx, sig, dialOptions{loopbackOnlyICE: true})
	if sess != nil {
		sess.close()
		t.Fatal("establishSession returned a session after its signaling transport was closed")
	}
	if err == nil {
		t.Fatal("establishSession unexpectedly succeeded with a closed signaling transport")
	}
	if !strings.Contains(err.Error(), "sending offer") {
		t.Fatalf("error = %q, want sending-offer context", err)
	}
	if got := ErrorKindOf(err); got != ErrorKindUnreachable {
		t.Fatalf("error kind = %q, want %q", got, ErrorKindUnreachable)
	}
}

func TestEstablishSessionHonorsDeadlineWhileWaitingForConnection(t *testing.T) {
	// The server reads the offer but deliberately sends no answer. This puts
	// establishment past signaling writes and into its bounded connection wait.
	sig := dialTestSignaler(t, func(context.Context, *websocket.Conn) {})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sess, err := establishSession(ctx, sig, dialOptions{loopbackOnlyICE: true})
	if sess != nil {
		sess.close()
		t.Fatal("establishSession returned a session without an SDP answer")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if !strings.Contains(err.Error(), "waiting for connection") {
		t.Fatalf("error = %q, want connection-wait context", err)
	}
}

func TestEstablishSessionRequiresConfirmedHIDHandshake(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{RejectHIDHandshake: true})
	ctx := contextWithTimeout(t, 5*time.Second)
	sig, _, err := dialSignaling(ctx, fd.baseURL(), nil)
	if err != nil {
		t.Fatalf("dialSignaling: %v", err)
	}
	defer sig.close()

	sess, err := establishSession(ctx, sig, dialOptions{
		allowControl:    true,
		loopbackOnlyICE: true,
	})
	if sess != nil {
		sess.close()
		t.Fatal("establishSession returned a session without a confirmed HID handshake")
	}
	if err == nil {
		t.Fatal("establishSession unexpectedly accepted an unconfirmed HID channel")
	}
	if !errors.Is(err, ErrHIDClosed) {
		t.Fatalf("error = %v, want ErrHIDClosed", err)
	}
}

func TestSessionRetriesKeyframeRequests(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{VideoInterval: 25 * time.Millisecond})
	sess := connectToFakeDevice(t, fd, "", dialOptions{})

	waitForCondition(t, 5*time.Second, func() bool {
		return sess.diag.snapshot(sess.pc).KeyframeRequestsSent >= 3
	})

	snapshot := sess.diag.snapshot(sess.pc)
	if snapshot.KeyframeRequestsSent < 3 {
		t.Fatalf("keyframe requests sent = %d, want initial, safety, and periodic requests", snapshot.KeyframeRequestsSent)
	}
	if snapshot.KeyframeRequestsFail != 0 {
		t.Fatalf("keyframe request failures = %d, want 0", snapshot.KeyframeRequestsFail)
	}
	if !snapshot.KeyframeRequestOnTarget {
		t.Fatal("keyframe requests did not target the observed video SSRC")
	}
}

func TestPumpSignalingEventsAppliesCandidateQueuedBeforeAnswer(t *testing.T) {
	offerPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("creating offer peer: %v", err)
	}
	defer offerPC.Close()
	answerPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("creating answer peer: %v", err)
	}
	defer answerPC.Close()

	if _, err := offerPC.CreateDataChannel("candidate-ordering", nil); err != nil {
		t.Fatalf("creating data channel: %v", err)
	}
	offer, err := offerPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("creating offer: %v", err)
	}
	if err := offerPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("setting local offer: %v", err)
	}
	if err := answerPC.SetRemoteDescription(offer); err != nil {
		t.Fatalf("setting remote offer: %v", err)
	}

	remoteCandidate := make(chan webrtc.ICECandidateInit, 1)
	answerPC.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		select {
		case remoteCandidate <- candidate.ToJSON():
		default:
		}
	})
	answer, err := answerPC.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("creating answer: %v", err)
	}
	if err := answerPC.SetLocalDescription(answer); err != nil {
		t.Fatalf("setting local answer: %v", err)
	}

	var candidate webrtc.ICECandidateInit
	select {
	case candidate = <-remoteCandidate:
	case <-time.After(5 * time.Second):
		t.Fatal("answer peer did not gather an ICE candidate")
	}

	sig := dialTestSignaler(t, func(ctx context.Context, conn *websocket.Conn) {
		candidateData, _ := json.Marshal(candidate)
		candidateFrame, _ := json.Marshal(signalingMessage{Type: "new-ice-candidate", Data: candidateData})
		if err := conn.Write(ctx, websocket.MessageText, candidateFrame); err != nil {
			return
		}

		answerJSON, _ := json.Marshal(answer)
		answerData, _ := json.Marshal(base64.StdEncoding.EncodeToString(answerJSON))
		answerFrame, _ := json.Marshal(signalingMessage{Type: "answer", Data: answerData})
		if err := conn.Write(ctx, websocket.MessageText, answerFrame); err != nil {
			return
		}

		marker, _ := json.Marshal(map[string]any{"type": "candidate-ordering-observed", "data": nil})
		_ = conn.Write(ctx, websocket.MessageText, marker)
	})

	diag := newVideoDiagnostics()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pumpSignalingEvents(ctx, sig, offerPC, diag)
	}()

	waitForCondition(t, 5*time.Second, func() bool {
		return diag.snapshot(offerPC).UnhandledSignalingMsgs == 1
	})
	snapshot := diag.snapshot(offerPC)
	if !snapshot.AnswerApplied || snapshot.AnswerRejected {
		t.Fatalf("answer outcome = applied %v, rejected %v", snapshot.AnswerApplied, snapshot.AnswerRejected)
	}
	if snapshot.RemoteICECandidatesQueued != 1 {
		t.Fatalf("queued candidates = %d, want 1", snapshot.RemoteICECandidatesQueued)
	}
	if snapshot.RemoteICECandidates != 1 || snapshot.RemoteICECandidatesBad != 0 {
		t.Fatalf("flushed candidate outcome = accepted %d, rejected %d; want 1, 0", snapshot.RemoteICECandidates, snapshot.RemoteICECandidatesBad)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("signaling pump did not stop after cancellation")
	}
}
