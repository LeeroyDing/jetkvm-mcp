package jetkvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// session owns one WebRTC peer connection to the device: the "rpc" and
// "hidrpc" data channels and the incoming video track. It's the browser-
// free equivalent of newSession/ExchangeOffer in the firmware's webrtc.go,
// from the client's side of the wire.
type session struct {
	pc  *webrtc.PeerConnection
	rpc *rpcClient
	hid *hidClient // nil unless control was requested

	video *frameCapture

	// diag records privacy-safe evidence about the video pipeline so a
	// screenshot that never arrives can name the stage it died at instead
	// of reporting an undifferentiated timeout. See diagnostics.go.
	diag *videoDiagnostics

	// ctx/cancel govern this session's own long-lived background work
	// (video depacketization, signaling pump, keyframe requests). It is
	// deliberately independent of the ctx passed into establishSession,
	// which only bounds the handshake itself: a caller's request-scoped
	// context expiring right after Connect() returns must not silently
	// kill an otherwise-healthy session's video/RPC pipes.
	ctx    context.Context
	cancel context.CancelFunc

	connected     chan struct{}
	connectedOnce sync.Once
	closed        chan struct{}
	closedOnce    sync.Once
	handshakeErr  chan error
	handshakeMu   sync.Mutex
	handshakeLive bool
}

func (s *session) reportHandshakeFailure(err error) bool {
	if err == nil {
		return false
	}
	s.handshakeMu.Lock()
	defer s.handshakeMu.Unlock()
	if !s.handshakeLive {
		return false
	}
	select {
	case s.handshakeErr <- err:
	default:
	}
	return true
}

func (s *session) finishHandshake() error {
	s.handshakeMu.Lock()
	defer s.handshakeMu.Unlock()
	s.handshakeLive = false
	select {
	case err := <-s.handshakeErr:
		return err
	default:
		return nil
	}
}

func (s *session) abandonHandshake() {
	s.handshakeMu.Lock()
	s.handshakeLive = false
	s.handshakeMu.Unlock()
}

type dialOptions struct {
	// allowControl opens the "hidrpc" data channel in addition to "rpc".
	// When false, this client never even negotiates a HID data channel,
	// so it is structurally incapable of sending keyboard/mouse input -
	// not merely refusing to at the call site.
	allowControl bool
}

// localCandidateQueue prevents trickled ICE from overtaking the offer that
// creates the firmware session. Pion may emit candidates synchronously after
// SetLocalDescription; those candidates remain queued until sendOffer has
// completed, then all later writes serialize behind the initial flush.
type localCandidateQueue struct {
	mu        sync.Mutex
	offerSent bool
	pending   []webrtc.ICECandidateInit
	send      func(context.Context, webrtc.ICECandidateInit) error
}

func (q *localCandidateQueue) add(ctx context.Context, candidate webrtc.ICECandidateInit) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.offerSent {
		q.pending = append(q.pending, candidate)
		return nil
	}
	return q.send(ctx, candidate)
}

func (q *localCandidateQueue) markOfferSent(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, candidate := range q.pending {
		if err := q.send(ctx, candidate); err != nil {
			return err
		}
	}
	q.pending = nil
	q.offerSent = true
	return nil
}

// remoteCandidateQueue is the mirror image: a device candidate cannot be
// applied until its answer has established a remote description. The pump is
// its sole caller, so it needs no mutex and preserves wire order exactly.
type remoteCandidateQueue struct {
	remoteSet bool
	pending   []webrtc.ICECandidateInit
	add       func(webrtc.ICECandidateInit) error
	record    func(error)
}

func (q *remoteCandidateQueue) addOrQueue(candidate webrtc.ICECandidateInit) error {
	if !q.remoteSet {
		q.pending = append(q.pending, candidate)
		return nil
	}
	err := q.add(candidate)
	q.record(err)
	return err
}

func (q *remoteCandidateQueue) markRemoteDescriptionSet() error {
	q.remoteSet = true
	pending := q.pending
	q.pending = nil
	for _, candidate := range pending {
		err := q.add(candidate)
		q.record(err)
		if err != nil {
			return err
		}
	}
	return nil
}

// offeredProfileLevelID and offeredPacketizationMode mirror the fmtp line
// in h264CodecPreferences below. Diagnostics report them alongside whatever
// the device actually negotiated, because "what we offered vs what came
// back" is the fastest way to tell a codec-negotiation failure from a
// device that simply is not streaming.
const (
	offeredProfileLevelID    = "42001f"
	offeredPacketizationMode = "1"
)

// h264CodecPreferences deliberately keeps H.265 out of the offer. JetKVM's
// automatic codec selection prefers H.265 whenever the offer advertises it,
// while this client's screenshot pipeline is intentionally H.264-only. Pion's
// default codec set includes H.265, so relying on RegisterDefaultCodecs alone
// would negotiate a stream this client cannot decode.
var h264CodecPreferences = []webrtc.RTPCodecParameters{
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeH264,
			ClockRate:    h264ClockRate,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=" + offeredPacketizationMode + ";profile-level-id=" + offeredProfileLevelID,
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}},
		},
		PayloadType: 102,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeRTX,
			ClockRate:   h264ClockRate,
			SDPFmtpLine: "apt=102",
		},
		PayloadType: 103,
	},
}

type discardLoggerFactory struct {
	// writer is test-only observability. Production leaves it nil, selecting
	// io.Discard below; the disabled level is retained even when a test writer
	// is supplied so hostile remote SDP/ICE text can never be emitted.
	writer io.Writer
}

func (f discardLoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	w := f.writer
	if w == nil {
		w = io.Discard
	}
	return logging.NewDefaultLeveledLoggerForScope(scope, logging.LogLevelDisabled, w)
}

var _ logging.LoggerFactory = discardLoggerFactory{}

func addH264RecvonlyTransceiver(pc *webrtc.PeerConnection) error {
	videoTransceiver, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	})
	if err != nil {
		return fmt.Errorf("jetkvm: adding video transceiver: %w", err)
	}
	if err := videoTransceiver.SetCodecPreferences(h264CodecPreferences); err != nil {
		return fmt.Errorf("jetkvm: constraining video negotiation to H.264: %w", err)
	}
	return nil
}

// establishSession performs the full browser-free handshake: build a Pion
// PeerConnection with a recvonly video transceiver plus "rpc" (and
// optionally "hidrpc") data channels, create an offer, exchange it over
// sig, apply the answer, trickle ICE, and wait for the connection to come
// up. sig must already have completed the device-metadata compatibility
// check (see dialSignaling).
func establishSession(ctx context.Context, sig *signaler, opts dialOptions) (_ *session, retErr error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("jetkvm: registering codecs: %w", err)
	}
	settingEngine := webrtc.SettingEngine{LoggerFactory: discardLoggerFactory{}}
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithSettingEngine(settingEngine),
	)

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("jetkvm: creating peer connection: %w", err)
	}

	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	diag := newVideoDiagnostics()
	s := &session{
		pc:            pc,
		video:         newFrameCapture(diag),
		diag:          diag,
		ctx:           sessionCtx,
		cancel:        sessionCancel,
		connected:     make(chan struct{}),
		closed:        make(chan struct{}),
		handshakeErr:  make(chan error, 1),
		handshakeLive: true,
	}
	cleanupNeeded := true
	defer func() {
		if !cleanupNeeded {
			return
		}
		s.abandonHandshake()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), failedConnectCleanupTimeout)
		defer cancel()
		retErr = errors.Join(retErr, s.close(cleanupCtx))
	}()

	// Recvonly: this client only ever receives video. It never offers to
	// send video/audio, keeping the "read-only WebRTC session" property
	// structural rather than a matter of not calling a send method.
	if err := addH264RecvonlyTransceiver(pc); err != nil {
		return nil, err
	}

	rpcDC, err := pc.CreateDataChannel("rpc", nil)
	if err != nil {
		return nil, fmt.Errorf("jetkvm: creating rpc data channel: %w", err)
	}
	s.rpc = newRPCClient(rpcDC)
	rpcOpen := make(chan struct{})
	var rpcOpenOnce sync.Once
	rpcDC.OnOpen(func() { rpcOpenOnce.Do(func() { close(rpcOpen) }) })

	var hidDC *webrtc.DataChannel
	hidOpen := make(chan struct{})
	close(hidOpen) // no-op wait when control isn't requested
	if opts.allowControl {
		hidDC, err = pc.CreateDataChannel("hidrpc", nil)
		if err != nil {
			return nil, fmt.Errorf("jetkvm: creating hidrpc data channel: %w", err)
		}
		s.hid = newHIDClient(hidDC)
		hidDC.OnMessage(func(msg webrtc.DataChannelMessage) { s.hid.handleMessage(msg.Data) })
		// Any way the control channel can go away must drive the HID state
		// machine to its terminal state, which revokes the active lease
		// generation - so a queued frame authorized before the drop can
		// never be written afterwards.
		hidDC.OnClose(func() {
			s.hid.closeWith(errors.New("hidrpc data channel closed"))
		})
		hidDC.OnError(func(err error) {
			s.hid.closeWith(fmt.Errorf("hidrpc data channel error: %w", err))
		})
		hidOpen = make(chan struct{})
		var hidOpenOnce sync.Once
		hidDC.OnOpen(func() { hidOpenOnce.Do(func() { close(hidOpen) }) })
	}

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		codec := track.Codec()
		s.diag.trackStarted(codec)
		if !strings.EqualFold(codec.MimeType, webrtc.MimeTypeH264) {
			s.diag.trackRejected("non-h264-codec")
			s.video.fail(fmt.Errorf(
				"unsupported negotiated video codec %q (H.264 required)",
				sanitizeMimeType(codec.MimeType),
			))
			return
		}
		// RFC 6184 allows an encoder to publish its parameter sets in the
		// SDP instead of, or as well as, in the stream. Adopting them here
		// costs nothing and removes a way for a session to receive keyframes
		// it cannot turn into a picture.
		s.video.seedParameterSets(parseSpropParameterSets(codec.SDPFmtpLine))
		go requestKeyframes(s.ctx, pc, track, s.diag)
		_ = s.video.run(s.ctx, track)
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			s.connectedOnce.Do(func() { close(s.connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			transportErr := newSessionTransportError(fmt.Sprintf("peer connection entered %s state", state))
			s.video.fail(transportErr)
			s.rpc.closeWith(transportErr)
			if s.hid != nil {
				s.hid.closeWith(transportErr)
			}
			s.closedOnce.Do(func() { close(s.closed) })
		}
	})

	localCandidates := &localCandidateQueue{
		send: func(candidateCtx context.Context, candidate webrtc.ICECandidateInit) error {
			return sig.sendICECandidate(candidateCtx, candidate)
		},
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		s.diag.localCandidate()
		if err := localCandidates.add(s.ctx, c.ToJSON()); err != nil {
			s.reportHandshakeFailure(err)
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return nil, fmt.Errorf("jetkvm: creating offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return nil, fmt.Errorf("jetkvm: setting local description: %w", err)
	}

	if err := sig.sendOffer(ctx, offer); err != nil {
		return nil, fmt.Errorf("jetkvm: sending offer: %w", err)
	}
	if err := localCandidates.markOfferSent(ctx); err != nil {
		return nil, fmt.Errorf("jetkvm: sending queued ICE candidate after offer: %w", err)
	}

	go pumpSignalingEventsWithFailure(s.ctx, sig, pc, s.diag, s.reportHandshakeFailure)

	select {
	case <-s.connected:
	case <-s.closed:
		return nil, newSessionTransportError("jetkvm: peer connection closed before becoming connected")
	case err := <-s.handshakeErr:
		return nil, err
	case <-ctx.Done():
		return nil, fmt.Errorf("jetkvm: waiting for connection: %w", ctx.Err())
	}

	// ICE/DTLS reaching "Connected" doesn't guarantee the SCTP-backed data
	// channels have finished their own open handshake yet; callers need
	// working channels, not just a connected transport.
	select {
	case <-rpcOpen:
	case <-s.closed:
		return nil, newSessionTransportError("jetkvm: peer connection closed while waiting for RPC data channel")
	case err := <-s.handshakeErr:
		return nil, err
	case <-ctx.Done():
		return nil, fmt.Errorf("jetkvm: waiting for rpc data channel to open: %w", ctx.Err())
	}
	select {
	case <-hidOpen:
	case <-s.closed:
		return nil, newSessionTransportError("jetkvm: peer connection closed while waiting for HID data channel")
	case err := <-s.handshakeErr:
		return nil, err
	case <-ctx.Done():
		return nil, fmt.Errorf("jetkvm: waiting for hidrpc data channel to open: %w", ctx.Err())
	}

	// An open hidrpc channel is not a usable one. The firmware ignores
	// HID-RPC frames until it has echoed the handshake back
	// (hidrpc.go only sets hidRPCAvailable = true on that echo), so
	// completing this handshake is what separates "we can send bytes" from
	// "the device will act on them". Failing here is deliberate: a control
	// session that silently drops every keystroke is worse than one that
	// refuses to start.
	if s.hid != nil {
		if err := s.hid.handshake(ctx); err != nil {
			return nil, err
		}
	}
	if err := s.finishHandshake(); err != nil {
		return nil, err
	}

	cleanupNeeded = false
	return s, nil
}

// pumpSignalingEvents reads answer/ICE-candidate messages from sig and
// applies them to pc until ctx is done or the signaling *transport* fails.
//
// It deliberately does not stop for a message it does not understand. An
// earlier version returned on any error from next(), including the error
// next() produced for an unrecognized message type - whose own comment said
// such messages "don't abort the session". They did: one informational
// message this client had no case for permanently killed the pump, and with
// it every subsequent trickled ICE candidate. Because the answer usually
// arrives first, the connection could still come up and data channels could
// still work, making the damage invisible except as media that never
// arrives. Unknown and malformed messages are now counted and skipped;
// only a transport failure ends the loop.
func pumpSignalingEvents(ctx context.Context, sig *signaler, pc *webrtc.PeerConnection, diag *videoDiagnostics) {
	pumpSignalingEventsWithFailure(ctx, sig, pc, diag, nil)
}

func pumpSignalingEventsWithFailure(
	ctx context.Context,
	sig *signaler,
	pc *webrtc.PeerConnection,
	diag *videoDiagnostics,
	reportHandshakeFailure func(error) bool,
) {
	report := func(err error) bool {
		return reportHandshakeFailure != nil && reportHandshakeFailure(err)
	}
	remoteCandidates := &remoteCandidateQueue{
		add:    pc.AddICECandidate,
		record: func(err error) { diag.remoteCandidate(err == nil) },
	}
	for {
		if ctx.Err() != nil {
			diag.signalingPumpStopped("session-closed")
			return
		}
		ev, err := sig.next(ctx)
		if err != nil {
			diag.signalingPumpStopped(classifyTrackReadError(err))
			if ctx.Err() == nil {
				report(err)
			}
			return
		}

		switch {
		case ev.Answer != nil:
			applyErr := pc.SetRemoteDescription(*ev.Answer)
			diag.answerOutcome(applyErr == nil)
			if applyErr != nil {
				report(&CompatibilityError{
					Stage:  "signaling-answer",
					Detail: "the device's signaling answer could not be applied",
				})
				return
			}
			if err := remoteCandidates.markRemoteDescriptionSet(); err != nil {
				if report(newSessionTransportError("jetkvm: applying queued remote ICE candidate failed")) {
					return
				}
			}
		case ev.Candidate != nil:
			if err := remoteCandidates.addOrQueue(*ev.Candidate); err != nil {
				if report(newSessionTransportError("jetkvm: applying remote ICE candidate failed")) {
					return
				}
			}
		case ev.Malformed != "":
			diag.malformedMessage()
			if ev.Malformed == "answer" && report(&CompatibilityError{
				Stage:  "signaling-answer",
				Detail: "the device's signaling answer was malformed",
			}) {
				return
			}
		case ev.Unhandled != "":
			diag.unhandledMessage()
		}
	}
}

// keyframeRetryInterval is how often a PLI is repeated while a session is
// open. Standard WebRTC receiver behavior: a receiver that cannot produce a
// picture keeps asking. Repeating also turns "no keyframe" into a
// quantified diagnostic - N requests over M seconds with no IDR is evidence
// about the encoder, whereas two requests in the first half-second is not.
const keyframeRetryInterval = 2 * time.Second

// requestKeyframes sends a PLI (Picture Loss Indication) as soon as a track
// starts, again shortly after as a safety net, and then periodically for
// the life of the session: standard WebRTC receiver behavior for a receiver
// that cannot produce a picture.
//
// On this firmware it is not what produces the keyframe, and the diagnostics
// must not imply that it is. jetkvm/kvm's webrtc.go hands the video sender
// to drainRTCP, which reads inbound RTCP into a scratch buffer and discards
// it without parsing - so a PLI that this client successfully writes is
// still never acted on. What actually delivers keyframes is the encoder's
// own cadence: internal/native/cgo/video.c sets the GOP to half the source
// framerate ("gop = fps > 0 ? fps / 2 : 30"), i.e. an IDR about every half
// second, unprompted.
//
// The requests stay because they are correct, harmless and read-only, and
// because a firmware that starts honoring them should find this client
// already asking. They are not load-bearing, and no failure boundary blames
// the encoder for ignoring them.
func requestKeyframes(ctx context.Context, pc *webrtc.PeerConnection, track *webrtc.TrackRemote, diag *videoDiagnostics) {
	mediaSSRC := uint32(track.SSRC())
	diag.keyframeRequestTarget(mediaSSRC)

	send := func() {
		err := pc.WriteRTCP([]rtcp.Packet{
			&rtcp.PictureLossIndication{MediaSSRC: mediaSSRC},
		})
		diag.keyframeRequested(err)
	}

	send()
	select {
	case <-time.After(500 * time.Millisecond):
		send()
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(keyframeRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			send()
		case <-ctx.Done():
			return
		}
	}
}

func (s *session) close(ctx context.Context) error {
	// Drive the HID state machine terminal before tearing down the
	// transport, so anything still queued is drained with an explicit
	// error rather than silently disappearing with the channel.
	if s.hid != nil {
		s.hid.closeWith(errSessionClosed)
	}
	s.cancel()
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan error, 1)
	go func() { done <- s.pc.Close() }()
	select {
	case err := <-done:
		if err != nil {
			// Pion teardown errors can retain transport endpoints. Keep only the
			// actionable category at the public cleanup boundary.
			return newSessionCleanupError("jetkvm: closing peer connection failed")
		}
		return nil
	case <-ctx.Done():
		return newSessionCleanupError("jetkvm: peer connection cleanup could not be confirmed before its deadline")
	}
}

var errSessionClosed = errors.New("session closed")
