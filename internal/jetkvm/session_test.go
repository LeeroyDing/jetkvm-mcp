package jetkvm

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// connectToFakeDevice runs the same steps Client.Connect will run
// (see client.go): unauthenticated status check, login if needed, dial
// signaling, establish the WebRTC session. It's factored out here so
// session- and client-level tests can share it against fakeDeviceServer.
func connectToFakeDevice(t *testing.T, fd *fakeDeviceServer, password string, opts dialOptions) *session {
	t.Helper()
	hc, err := newHTTPClient(fd.baseURL(), 5*time.Second)
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}

	ctx := contextWithTimeout(t, connectTimeout(t, 15*time.Second))

	status, err := hc.deviceStatus(ctx)
	if err != nil {
		t.Fatalf("deviceStatus: %v", err)
	}
	if !status.IsSetup {
		t.Fatal("expected fake device to report IsSetup")
	}

	if password != "" {
		if err := hc.login(ctx, NewSecret(password)); err != nil {
			t.Fatalf("login: %v", err)
		}
	}

	u, _ := url.Parse(fd.baseURL())
	cookies := hc.hc.Jar.Cookies(u)

	sig, meta, err := dialSignaling(ctx, fd.baseURL(), cookies)
	if err != nil {
		t.Fatalf("dialSignaling: %v", err)
	}
	if meta.DeviceVersion == "" {
		t.Fatal("expected a non-empty device version from signaling handshake")
	}

	// Every fake device lives on 127.0.0.1; keep test ICE on loopback the
	// same way Connect does automatically for loopback URLs.
	opts.loopbackOnlyICE = true
	s, err := establishSession(ctx, sig, opts)
	if err != nil {
		t.Fatalf("establishSession: %v", err)
	}
	t.Cleanup(s.close)
	return s
}

func TestEstablishSessionNoPasswordMode(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{})
	s := connectToFakeDevice(t, fd, "", dialOptions{})

	ctx := contextWithTimeout(t, 5*time.Second)
	var result string
	if err := s.rpc.call(ctx, "ping", nil, &result); err != nil {
		t.Fatalf("ping call failed: %v", err)
	}
	if result != "pong" {
		t.Errorf("ping result = %q, want pong", result)
	}
}

func TestVideoOfferAdvertisesOnlyH264(t *testing.T) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	if err := addH264RecvonlyTransceiver(pc); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	sdp := strings.ToUpper(offer.SDP)
	if !strings.Contains(sdp, "A=RECVONLY") {
		t.Fatal("video offer is not recvonly")
	}
	if !strings.Contains(sdp, "H264/90000") {
		t.Fatal("video offer does not advertise H.264")
	}
	for _, unsupported := range []string{"H265/90000", "VP8/90000", "VP9/90000", "AV1/90000"} {
		if strings.Contains(sdp, unsupported) {
			t.Fatalf("video offer unexpectedly advertises %s", unsupported)
		}
	}
}

func TestEstablishSessionPasswordMode(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{Password: "correct-horse"})
	s := connectToFakeDevice(t, fd, "correct-horse", dialOptions{})

	ctx := contextWithTimeout(t, 5*time.Second)
	var result string
	if err := s.rpc.call(ctx, "ping", nil, &result); err != nil {
		t.Fatalf("ping call failed: %v", err)
	}
	if result != "pong" {
		t.Errorf("ping result = %q, want pong", result)
	}
}

func TestEstablishSessionReceivesVideoFrame(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{})
	s := connectToFakeDevice(t, fd, "", dialOptions{})

	ctx := contextWithTimeout(t, 10*time.Second)
	fr, err := s.video.waitForFrameAfter(ctx, 0)
	if err != nil {
		t.Fatalf("waitForFrame failed: %v", err)
	}
	if len(fr.annexB) == 0 {
		t.Fatal("expected non-empty video frame")
	}
	if time.Since(fr.capturedAt) > 10*time.Second {
		t.Errorf("frame looks stale: capturedAt=%v", fr.capturedAt)
	}
}

func TestEstablishSessionWithControlOpensHIDChannel(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{})
	s := connectToFakeDevice(t, fd, "", dialOptions{allowControl: true})

	if s.hid == nil {
		t.Fatal("expected hid client to be present when allowControl is true")
	}
	ctx := contextWithTimeout(t, 5*time.Second)
	if err := s.hid.handshake(ctx); err != nil {
		t.Fatalf("HID handshake over a full session failed: %v", err)
	}
}

func TestEstablishSessionWithoutControlHasNoHIDChannel(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{})
	s := connectToFakeDevice(t, fd, "", dialOptions{allowControl: false})

	if s.hid != nil {
		t.Fatal("expected no hid client when allowControl is false")
	}
}

func TestDialSignalingCompatibilityErrorPropagatesBeforeSession(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{SkipDeviceMetadata: true})
	hc, err := newHTTPClient(fd.baseURL(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := contextWithTimeout(t, 5*time.Second)
	_, _, err = dialSignaling(ctx, fd.baseURL(), hc.hc.Jar.Cookies(mustParseURL(t, fd.baseURL())))
	if err == nil {
		t.Fatal("expected a compatibility error when device-metadata is missing")
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestSignalingPumpSurvivesUnhandledMessages is the regression test for the
// bug this branch fixes.
//
// pumpSignalingEvents used to return on any error from next(), and next()
// returned an error for a message type it had no case for - despite its own
// comment saying such messages must not abort the session. One
// informational message from the device therefore killed the pump
// permanently, and with it every ICE candidate that arrived afterwards.
// Because the SDP answer normally arrives first, the peer connection could
// still come up and data channels could still work, so the only visible
// symptom was media that never arrived.
//
// The pump must process what comes after an unhandled message.
func TestSignalingPumpSurvivesUnhandledMessages(t *testing.T) {
	candidateSeen := make(chan struct{})

	s := dialTestSignaler(t, func(ctx context.Context, conn *websocket.Conn) {
		send := func(v any) {
			payload, _ := json.Marshal(v)
			_ = conn.Write(ctx, websocket.MessageText, payload)
		}
		// Two message types this client has no case for...
		send(map[string]any{"type": "heartbeat", "data": map[string]any{"seq": 1}})
		send(map[string]any{"type": "some-future-notice", "data": "informational"})
		// ...one malformed message, one frame that is not JSON at all...
		send(map[string]any{"type": "new-ice-candidate", "data": "not-an-object"})
		_ = conn.Write(ctx, websocket.MessageText, []byte("not json"))
		// ...and then something the pump must still act on.
		send(map[string]any{
			"type": "new-ice-candidate",
			"data": map[string]any{"candidate": "candidate:1 1 UDP 2130706431 192.0.2.1 5000 typ host"},
		})
		close(candidateSeen)
	})

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	defer pc.Close()

	diag := newVideoDiagnostics()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		pumpSignalingEvents(ctx, s, pc, diag)
	}()

	<-candidateSeen

	// The pump must have seen the trailing candidate, which can only happen
	// if it kept reading past the unhandled and malformed messages. With no
	// answer on this synthetic session the candidate is queued (never
	// dropped) until a remote description would exist.
	waitForCondition(t, 5*time.Second, func() bool {
		snap := diag.snapshot(pc)
		return snap.RemoteICECandidates+snap.RemoteICECandidatesBad+snap.RemoteICECandidatesQueued >= 1
	})

	snap := diag.snapshot(pc)
	if snap.UnhandledSignalingMsgs != 2 {
		t.Errorf("UnhandledSignalingMsgs = %d, want 2", snap.UnhandledSignalingMsgs)
	}
	if snap.MalformedSignalingMsgs != 2 {
		t.Errorf("MalformedSignalingMsgs = %d, want 2", snap.MalformedSignalingMsgs)
	}
	if !snap.SignalingPumpStillActive {
		t.Errorf("the signaling pump stopped (%q); unhandled messages must not end it", snap.SignalingPumpStopped)
	}

	cancel()
	select {
	case <-pumpDone:
	case <-time.After(5 * time.Second):
		t.Fatal("pump did not exit after its context was canceled")
	}
}

// TestSignalingPumpStopsOnTransportFailure is the other half: a genuinely
// dead signaling connection must end the pump rather than spin.
func TestSignalingPumpStopsOnTransportFailure(t *testing.T) {
	s := dialTestSignaler(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	defer pc.Close()

	diag := newVideoDiagnostics()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pumpSignalingEvents(context.Background(), s, pc, diag)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pump did not stop after the signaling transport closed")
	}

	snap := diag.snapshot(pc)
	if snap.SignalingPumpStillActive {
		t.Error("pump should be recorded as stopped after a transport failure")
	}
	if snap.SignalingPumpStopped == "" {
		t.Error("expected a bounded stop reason category")
	}
}
