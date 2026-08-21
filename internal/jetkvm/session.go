package jetkvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

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
}

type dialOptions struct {
	// allowControl opens the "hidrpc" data channel in addition to "rpc".
	// When false, this client never even negotiates a HID data channel,
	// so it is structurally incapable of sending binary keyboard/pointer
	// reports. The firmware's legacy wheelReport method lives on the always-
	// present RPC channel, so Client.Scroll independently enforces the same
	// AllowControl opt-in before it can use that exceptional path.
	allowControl bool

	// loopbackOnlyICE restricts ICE candidate gathering to loopback
	// addresses. Connect sets it when the device URL host is itself a
	// loopback address: such a device is reachable over loopback and
	// nothing else, so candidates on every other interface add no viable
	// pair - they only slow connectivity checking down. On starved shared
	// CI runners that waste was enough to blow pion's ~30s ICE failure
	// timer under -race (2026-08-15 quality-job failures), so the loopback
	// test rigs depend on this staying structural, not cosmetic.
	loopbackOnlyICE bool
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
func establishSession(ctx context.Context, sig *signaler, opts dialOptions) (*session, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("jetkvm: registering codecs: %w", err)
	}
	apiOptions := []func(*webrtc.API){webrtc.WithMediaEngine(mediaEngine)}
	if opts.loopbackOnlyICE {
		se := webrtc.SettingEngine{}
		se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
		// pion skips loopback when gathering host candidates unless asked;
		// without it the filter above leaves zero candidates.
		se.SetIncludeLoopbackCandidate(true)
		apiOptions = append(apiOptions, webrtc.WithSettingEngine(se))
	}
	api := webrtc.NewAPI(apiOptions...)

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("jetkvm: creating peer connection: %w", err)
	}

	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	diag := newVideoDiagnostics()
	s := &session{
		pc:        pc,
		video:     newFrameCapture(diag),
		diag:      diag,
		ctx:       sessionCtx,
		cancel:    sessionCancel,
		connected: make(chan struct{}),
		closed:    make(chan struct{}),
	}

	// Recvonly: this client only ever receives video. It never offers to
	// send video/audio, keeping the "read-only WebRTC session" property
	// structural rather than a matter of not calling a send method.
	if err := addH264RecvonlyTransceiver(pc); err != nil {
		s.close()
		return nil, err
	}

	rpcDC, err := pc.CreateDataChannel("rpc", nil)
	if err != nil {
		s.close()
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
			s.close()
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
			s.video.fail(newDeviceError(ErrorKindUnreachable, "receiving video",
				fmt.Errorf("peer connection entered %s state before a video frame was available", state)))
			if s.hid != nil {
				s.hid.closeWith(fmt.Errorf("peer connection entered %s state", state))
			}
			s.closedOnce.Do(func() { close(s.closed) })
		}
	})

	// Local candidates must never reach the wire before the offer they
	// belong to: the device processes signaling messages in order, and a
	// candidate arriving before the offer is dropped by a receiver that
	// has no remote description yet. Gathering starts inside
	// SetLocalDescription and (on fast paths - loopback especially) can
	// complete before sendOffer runs, so the handler queues candidates
	// until the offer is on the wire, then trickles directly.
	var (
		localCandidateMu sync.Mutex
		offerOnWire      bool
		queuedLocal      []webrtc.ICECandidateInit
	)
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		s.diag.localCandidate()
		init := c.ToJSON()
		localCandidateMu.Lock()
		if !offerOnWire {
			queuedLocal = append(queuedLocal, init)
			localCandidateMu.Unlock()
			return
		}
		localCandidateMu.Unlock()
		_ = sig.sendICECandidate(s.ctx, init)
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		s.close()
		return nil, fmt.Errorf("jetkvm: creating offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		s.close()
		return nil, fmt.Errorf("jetkvm: setting local description: %w", err)
	}

	if err := sig.sendOffer(ctx, offer); err != nil {
		s.close()
		return nil, fmt.Errorf("jetkvm: sending offer: %w", err)
	}
	localCandidateMu.Lock()
	flushLocal := queuedLocal
	queuedLocal = nil
	offerOnWire = true
	localCandidateMu.Unlock()
	for _, c := range flushLocal {
		_ = sig.sendICECandidate(s.ctx, c)
	}

	go pumpSignalingEvents(s.ctx, sig, pc, s.diag)

	select {
	case <-s.connected:
	case <-s.closed:
		s.close()
		return nil, fmt.Errorf("jetkvm: peer connection closed before becoming connected")
	case <-ctx.Done():
		s.close()
		return nil, fmt.Errorf("jetkvm: waiting for connection: %w", ctx.Err())
	}

	// ICE/DTLS reaching "Connected" doesn't guarantee the SCTP-backed data
	// channels have finished their own open handshake yet; callers need
	// working channels, not just a connected transport.
	select {
	case <-rpcOpen:
	case <-s.closed:
		s.close()
		return nil, fmt.Errorf("jetkvm: peer connection closed while waiting for rpc data channel to open")
	case <-ctx.Done():
		s.close()
		return nil, fmt.Errorf("jetkvm: waiting for rpc data channel to open: %w", ctx.Err())
	}
	select {
	case <-hidOpen:
	case <-s.closed:
		s.close()
		return nil, fmt.Errorf("jetkvm: peer connection closed while waiting for hidrpc data channel to open")
	case <-ctx.Done():
		s.close()
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
			s.close()
			return nil, err
		}
	}

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
	var pendingRemote []webrtc.ICECandidateInit
	answerApplied := false
	for {
		if ctx.Err() != nil {
			diag.signalingPumpStopped("session-closed")
			return
		}
		ev, err := sig.next(ctx)
		if err != nil {
			diag.signalingPumpStopped(classifyTrackReadError(err))
			return
		}

		switch {
		case ev.Answer != nil:
			applyErr := pc.SetRemoteDescription(*ev.Answer)
			diag.answerOutcome(applyErr == nil)
			if applyErr == nil {
				answerApplied = true
				for _, c := range pendingRemote {
					addErr := pc.AddICECandidate(c)
					diag.remoteCandidate(addErr == nil)
				}
				pendingRemote = nil
			}
		case ev.Candidate != nil:
			// Trickle ICE explicitly allows candidates to race the answer
			// (the answerer emits both concurrently), and pion rejects
			// AddICECandidate before a remote description exists. Queue
			// early arrivals and apply them the moment the answer lands;
			// dropping them can leave ICE with no remote candidates at
			// all, which is an unrecoverable dead connection.
			if !answerApplied {
				if len(pendingRemote) < maxPendingRemoteCandidates {
					pendingRemote = append(pendingRemote, *ev.Candidate)
					diag.remoteCandidateQueued()
				} else {
					diag.remoteCandidate(false)
				}
				continue
			}
			addErr := pc.AddICECandidate(*ev.Candidate)
			diag.remoteCandidate(addErr == nil)
		case ev.Malformed != "":
			diag.malformedMessage()
		case ev.Unhandled != "":
			diag.unhandledMessage()
		}
	}
}

// maxPendingRemoteCandidates bounds the pre-answer trickle queue. A
// well-behaved device sends a handful of candidates; the cap only guards
// against a broken or hostile peer streaming them forever.
const maxPendingRemoteCandidates = 64

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

func (s *session) close() {
	// Drive the HID state machine terminal before tearing down the
	// transport, so anything still queued is drained with an explicit
	// error rather than silently disappearing with the channel.
	if s.hid != nil {
		s.hid.closeWith(errSessionClosed)
	}
	s.cancel()
	_ = s.pc.Close()
}

var errSessionClosed = errors.New("session closed")
