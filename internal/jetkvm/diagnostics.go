package jetkvm

import (
	"context"
	"errors"
	"io"
	"math/bits"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Video-pipeline diagnostics.
//
// A screenshot that never arrives produces one error - "no video frame
// available: context deadline exceeded" - no matter which stage actually
// failed. That single message is identical whether the video track was
// never negotiated, the device never sent a packet, the depacketizer never
// assembled an access unit, or the encoder never emitted a keyframe. This
// file exists to tell those apart from one bounded run.
//
// # Privacy contract
//
// Everything here is a count, a duration, a bounded enum, or a codec
// parameter needed to reason about compatibility. Nothing in this file may
// record or return: credentials, cookies, auth bodies, device IDs, base
// URLs, hostnames, IP addresses, SDP bodies, ICE candidates or their
// addresses, filesystem paths, raw FFmpeg stderr, or packet payload bytes.
// Error text from other layers is never embedded verbatim - it is mapped
// to a closed vocabulary by the classify* functions below.
//
// diagnosticsPrivacyTest in diagnostics_test.go enforces this by feeding
// representative sensitive values through every input and asserting none
// survives into the snapshot. RTP payloads go through it too: the wire-level
// accounting below reads packet bytes, so the test drives whole canary
// strings through as payloads and asserts nothing survives.
//
// # Wire-level observation
//
// Everything downstream of the sample builder can only see access units the
// builder chose to emit. That made two very different failures - "the device
// never sent a keyframe" and "this client destroyed the keyframe while
// reassembling it" - indistinguishable, and the 2026-08-05 live run was read
// as the first when it was the second. The wire-level counters here are read
// from raw RTP before reassembly, so the next run can tell them apart.

// naluTypeCount is the number of distinct H.264 NAL unit types (the header
// field is 5 bits). Fixing the array at this size keeps NAL accounting
// bounded by construction - there is no map to grow.
const naluTypeCount = 32

// videoDiagnostics collects evidence about one session's video pipeline.
//
// Every field belongs to exactly one of three regimes, and never to two:
// plain counters are atomics so the RTP read loop does not block on them;
// the wire-level trackers are guarded by wireMu, which the read loop is the
// sole writer of; everything else is guarded by mu.
type videoDiagnostics struct {
	startedAt time.Time

	// Hot path (RTP read loop / depacketizer).
	rtpPackets     atomic.Uint64
	rtpBytes       atomic.Uint64
	samples        atomic.Uint64
	builderDropped atomic.Uint64
	framesBuilt    atomic.Uint64
	pliSent        atomic.Uint64
	pliFailed      atomic.Uint64
	firstRTPNanos  atomic.Int64
	lastRTPNanos   atomic.Int64
	naluCounts     [naluTypeCount]atomic.Uint64

	// Wire-level RTP observation, guarded by wireMu.
	//
	// These are stateful trackers rather than plain counters, so they cannot
	// be atomics. The lock is still off the contended path: the RTP read
	// loop is their only writer, and it only ever contends with a snapshot.
	wireMu         sync.Mutex
	wireEmpty      uint64
	wireSingleNALU uint64
	wireSTAPA      uint64
	wireFUA        uint64
	wireOther      uint64
	wireMalformed  uint64
	wireNALUCounts [naluTypeCount]uint64
	fuStarts       uint64
	fuEnds         uint64
	seq            rtpSequenceTracker
	frames         rtpFrameTracker
	mediaSSRC      uint32
	mediaSSRCSeen  bool
	mediaSSRCMoved uint64
	pliTargetSSRC  uint32
	pliTargetSeen  bool

	paramSetFromRTP bool
	paramSetFromSDP bool

	// Signaling and negotiation.
	mu                  sync.Mutex
	answerApplied       bool
	answerRejected      bool
	remoteCandidates    int
	remoteCandidatesBad int
	localCandidates     int
	unhandledMessages   int
	malformedMessages   int
	pumpStopped         string

	// Track.
	trackObserved  bool
	trackAtNanos   int64
	trackMimeType  string
	trackPayload   uint8
	trackProfile   string
	trackPktMode   string
	trackRejection string
	trackReadFail  string

	// Decode.
	decodeAttempts int
	decodeBytesIn  uint64
	decodeFailure  string
}

func newVideoDiagnostics() *videoDiagnostics {
	return &videoDiagnostics{startedAt: time.Now()}
}

func (d *videoDiagnostics) since() time.Duration { return time.Since(d.startedAt) }

// --- signaling -------------------------------------------------------------

func (d *videoDiagnostics) answerOutcome(applied bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if applied {
		d.answerApplied = true
	} else {
		d.answerRejected = true
	}
}

func (d *videoDiagnostics) remoteCandidate(accepted bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if accepted {
		d.remoteCandidates++
	} else {
		d.remoteCandidatesBad++
	}
}

func (d *videoDiagnostics) localCandidate() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.localCandidates++
}

func (d *videoDiagnostics) unhandledMessage() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.unhandledMessages++
}

func (d *videoDiagnostics) malformedMessage() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.malformedMessages++
}

func (d *videoDiagnostics) signalingPumpStopped(reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pumpStopped == "" {
		d.pumpStopped = reason
	}
}

// --- track -----------------------------------------------------------------

// trackStarted records the negotiated codec parameters. Only the specific
// H.264 fields needed to reason about compatibility are extracted; the raw
// fmtp line is deliberately not stored, since it is device-supplied text.
func (d *videoDiagnostics) trackStarted(codec webrtc.RTPCodecParameters) {
	profile, pktMode := parseH264Fmtp(codec.SDPFmtpLine)

	d.mu.Lock()
	defer d.mu.Unlock()
	d.trackObserved = true
	d.trackAtNanos = int64(d.since())
	d.trackMimeType = sanitizeMimeType(codec.MimeType)
	d.trackPayload = uint8(codec.PayloadType)
	d.trackProfile = profile
	d.trackPktMode = pktMode
}

func (d *videoDiagnostics) trackRejected(reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.trackRejection = reason
}

// trackReadFailed records why the RTP read loop ended, as a category.
func (d *videoDiagnostics) trackReadFailed(category string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.trackReadFail == "" {
		d.trackReadFail = category
	}
}

// --- RTP / depacketization -------------------------------------------------

// rtpPacket records one packet as it came off the wire, before any
// reassembly. Only the header fields and the packetization structure of the
// payload are read; payload bytes are never retained or reported.
func (d *videoDiagnostics) rtpPacket(pkt *rtp.Packet) {
	now := int64(d.since())
	if d.rtpPackets.Add(1) == 1 {
		d.firstRTPNanos.Store(now)
	}
	d.lastRTPNanos.Store(now)
	if len(pkt.Payload) > 0 {
		d.rtpBytes.Add(uint64(len(pkt.Payload)))
	}

	info := classifyRTPPayload(pkt.Payload)

	d.wireMu.Lock()
	defer d.wireMu.Unlock()

	switch info.class {
	case rtpClassEmpty:
		d.wireEmpty++
	case rtpClassSingleNALU:
		d.wireSingleNALU++
	case rtpClassSTAPA:
		d.wireSTAPA++
	case rtpClassFUA:
		d.wireFUA++
		if info.fuStart {
			d.fuStarts++
		}
		if info.fuEnd {
			d.fuEnds++
		}
	default:
		d.wireOther++
	}
	if info.malformed {
		d.wireMalformed++
	}
	// Walk only the bits that are set: at most a handful per packet, and no
	// per-packet cost proportional to the type space.
	for mask := info.naluTypeMask; mask != 0; mask &= mask - 1 {
		d.wireNALUCounts[bits.TrailingZeros32(mask)]++
	}

	d.seq.observe(pkt.SequenceNumber)
	d.frames.observe(pkt.Timestamp, pkt.Marker, info.carries(naluTypeIDR))

	if !d.mediaSSRCSeen {
		d.mediaSSRCSeen = true
		d.mediaSSRC = pkt.SSRC
	} else if pkt.SSRC != d.mediaSSRC {
		// The track changing synchronization source mid-session would leave
		// feedback aimed at the old one pointing at nothing. The SSRC value
		// itself is never reported, only whether it moved.
		d.mediaSSRC = pkt.SSRC
		d.mediaSSRCMoved++
	}
}

// sampleAssembled records one access unit emitted by the sample builder,
// along with how many RTP packets the builder discarded to get there.
//
// droppedBefore is the counter that would have named the receiver bug
// immediately: it is nonzero exactly when the builder threw packets away,
// which is what silently deleted every keyframe before maxAccessUnitPackets
// was sized from the device's real frame bound.
func (d *videoDiagnostics) sampleAssembled(droppedBefore uint16) {
	d.samples.Add(1)
	if droppedBefore > 0 {
		d.builderDropped.Add(uint64(droppedBefore))
	}
}

// parameterSetSource records where an SPS/PPS came from: in-band in the RTP
// stream, or out-of-band from the SDP answer's sprop-parameter-sets.
func (d *videoDiagnostics) parameterSetSource(source string) {
	d.wireMu.Lock()
	defer d.wireMu.Unlock()
	switch source {
	case paramSetSourceRTP:
		d.paramSetFromRTP = true
	case paramSetSourceSDP:
		d.paramSetFromSDP = true
	}
}

// keyframeRequestTarget records which synchronization source this client
// aimed its keyframe requests at, so a live run can prove the feedback named
// the stream it was actually receiving rather than assuming it.
func (d *videoDiagnostics) keyframeRequestTarget(ssrc uint32) {
	d.wireMu.Lock()
	defer d.wireMu.Unlock()
	d.pliTargetSeen = true
	d.pliTargetSSRC = ssrc
}

// naluSeen records one NAL unit type from a depacketized access unit.
// Seeing types 24 (STAP-A) or 28 (FU-A) here would mean aggregation or
// fragmentation was *not* undone by the depacketizer, which is itself the
// answer to one of the failure boundaries.
func (d *videoDiagnostics) naluSeen(naluType byte) {
	if int(naluType) < naluTypeCount {
		d.naluCounts[naluType].Add(1)
	}
}

func (d *videoDiagnostics) frameAssembled() { d.framesBuilt.Add(1) }

func (d *videoDiagnostics) keyframeRequested(err error) {
	if err != nil {
		d.pliFailed.Add(1)
		return
	}
	d.pliSent.Add(1)
}

// --- decode ----------------------------------------------------------------

func (d *videoDiagnostics) decodeAttempted(bytesIn int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.decodeAttempts++
	if bytesIn > 0 {
		d.decodeBytesIn += uint64(bytesIn)
	}
}

func (d *videoDiagnostics) decodeFailed(category string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.decodeFailure = category
}

// --- snapshot --------------------------------------------------------------

// VideoDiagnostics is a point-in-time, privacy-safe summary of the video
// pipeline. Every field is a count, a duration in milliseconds, a bounded
// enum, or a codec parameter. It is safe to serialize and hand to an
// operator or an agent.
type VideoDiagnostics struct {
	// Transport state at snapshot time.
	PeerConnectionState string `json:"peerConnectionState"`
	ICEConnectionState  string `json:"iceConnectionState"`
	SignalingState      string `json:"signalingState"`

	// Signaling exchange.
	AnswerApplied            bool   `json:"answerApplied"`
	AnswerRejected           bool   `json:"answerRejected"`
	LocalICECandidates       int    `json:"localIceCandidates"`
	RemoteICECandidates      int    `json:"remoteIceCandidates"`
	RemoteICECandidatesBad   int    `json:"remoteIceCandidatesRejected"`
	UnhandledSignalingMsgs   int    `json:"unhandledSignalingMessages"`
	MalformedSignalingMsgs   int    `json:"malformedSignalingMessages"`
	SignalingPumpStopped     string `json:"signalingPumpStopped,omitempty"`
	SignalingPumpStillActive bool   `json:"signalingPumpStillActive"`

	// Negotiated video track.
	TrackObserved     bool   `json:"trackObserved"`
	TrackAfterMillis  int64  `json:"trackAfterMillis"`
	TrackMimeType     string `json:"trackMimeType,omitempty"`
	TrackPayloadType  uint8  `json:"trackPayloadType,omitempty"`
	TrackProfileLevel string `json:"trackProfileLevelId,omitempty"`
	TrackPacketMode   string `json:"trackPacketizationMode,omitempty"`
	TrackRejection    string `json:"trackRejection,omitempty"`

	// What this client offered, for comparison against what came back.
	OfferedPayloadType  uint8  `json:"offeredPayloadType"`
	OfferedProfileLevel string `json:"offeredProfileLevelId"`
	OfferedPacketMode   string `json:"offeredPacketizationMode"`

	// RTP.
	RTPPackets           uint64 `json:"rtpPackets"`
	RTPPayloadBytes      uint64 `json:"rtpPayloadBytes"`
	FirstRTPAfterMillis  int64  `json:"firstRtpAfterMillis"`
	LastRTPAfterMillis   int64  `json:"lastRtpAfterMillis"`
	TrackReadFailure     string `json:"trackReadFailure,omitempty"`
	KeyframeRequestsSent uint64 `json:"keyframeRequestsSent"`
	KeyframeRequestsFail uint64 `json:"keyframeRequestsFailed"`

	// What the RTP stream looked like on the wire, before reassembly. This
	// is the independent witness: WireNALUnitsByType["IDR"] counts keyframe
	// packets the device actually sent, whether or not any of them survived
	// into an access unit.
	RTPPacketsByClass     map[string]uint64 `json:"rtpPacketsByClass,omitempty"`
	WireNALUnitsByType    map[string]uint64 `json:"wireNalUnitsByType,omitempty"`
	MalformedRTPPayloads  uint64            `json:"malformedRtpPayloads"`
	FUStarts              uint64            `json:"fuStarts"`
	FUEnds                uint64            `json:"fuEnds"`
	RTPPacketsLost        uint64            `json:"rtpPacketsLost"`
	RTPPacketsReordered   uint64            `json:"rtpPacketsReordered"`
	RTPPacketsDuplicated  uint64            `json:"rtpPacketsDuplicated"`
	MaxSequenceJump       uint16            `json:"maxSequenceJump"`
	MarkedPackets         uint64            `json:"markedPackets"`
	TimestampChanges      uint64            `json:"timestampChanges"`
	UnmarkedFrames        uint64            `json:"unmarkedFrames"`
	MaxPacketsPerFrame    uint64            `json:"maxPacketsPerFrame"`
	MaxPacketsPerKeyframe uint64            `json:"maxPacketsPerKeyframe"`
	ReassemblyWindow      uint64            `json:"reassemblyWindowPackets"`
	MediaSourceChanges    uint64            `json:"mediaSourceChanges"`

	// Whether this client's keyframe requests named the synchronization
	// source it was actually receiving. Note that the device's firmware
	// drains inbound RTCP without parsing it, so a matching target still
	// does not mean the request was honored - see requestKeyframes.
	KeyframeRequestOnTarget bool `json:"keyframeRequestOnTarget"`

	// Depacketization.
	AccessUnits        uint64            `json:"accessUnits"`
	NALUnitsByType     map[string]uint64 `json:"nalUnitsByType,omitempty"`
	SawSPS             bool              `json:"sawSps"`
	SawPPS             bool              `json:"sawPps"`
	SawIDR             bool              `json:"sawIdr"`
	FramesAssembled    uint64            `json:"framesAssembled"`
	BuilderDropped     uint64            `json:"reassemblyDroppedPackets"`
	ParameterSetSource string            `json:"parameterSetSource,omitempty"`

	// Decode.
	DecodeAttempts  int    `json:"decodeAttempts"`
	DecodeBytesIn   uint64 `json:"decodeBytesIn"`
	DecodeFailure   string `json:"decodeFailure,omitempty"`
	ElapsedMillis   int64  `json:"elapsedMillis"`
	FailureBoundary string `json:"failureBoundary"`
}

// snapshot builds a VideoDiagnostics. pc may be nil (then transport state
// is reported as "unknown"), which keeps the snapshot usable from tests and
// from a session that never got that far.
func (d *videoDiagnostics) snapshot(pc *webrtc.PeerConnection) VideoDiagnostics {
	out := VideoDiagnostics{
		PeerConnectionState: "unknown",
		ICEConnectionState:  "unknown",
		SignalingState:      "unknown",
		ElapsedMillis:       d.since().Milliseconds(),

		OfferedPayloadType:  uint8(h264CodecPreferences[0].PayloadType),
		OfferedProfileLevel: offeredProfileLevelID,
		OfferedPacketMode:   offeredPacketizationMode,

		RTPPackets:           d.rtpPackets.Load(),
		RTPPayloadBytes:      d.rtpBytes.Load(),
		AccessUnits:          d.samples.Load(),
		FramesAssembled:      d.framesBuilt.Load(),
		BuilderDropped:       d.builderDropped.Load(),
		KeyframeRequestsSent: d.pliSent.Load(),
		KeyframeRequestsFail: d.pliFailed.Load(),
		ReassemblyWindow:     maxAccessUnitPackets,
	}
	if pc != nil {
		out.PeerConnectionState = pc.ConnectionState().String()
		out.ICEConnectionState = pc.ICEConnectionState().String()
		out.SignalingState = pc.SignalingState().String()
	}
	if out.RTPPackets > 0 {
		out.FirstRTPAfterMillis = time.Duration(d.firstRTPNanos.Load()).Milliseconds()
		out.LastRTPAfterMillis = time.Duration(d.lastRTPNanos.Load()).Milliseconds()
	}

	for naluType := 0; naluType < naluTypeCount; naluType++ {
		count := d.naluCounts[naluType].Load()
		if count == 0 {
			continue
		}
		if out.NALUnitsByType == nil {
			out.NALUnitsByType = make(map[string]uint64, 4)
		}
		out.NALUnitsByType[naluTypeName(byte(naluType))] = count
	}
	out.SawSPS = d.naluCounts[naluTypeSPS].Load() > 0
	out.SawPPS = d.naluCounts[naluTypePPS].Load() > 0
	out.SawIDR = d.naluCounts[naluTypeIDR].Load() > 0

	d.wireMu.Lock()
	out.RTPPacketsByClass = nonZeroCounts(map[string]uint64{
		rtpClassEmpty:      d.wireEmpty,
		rtpClassSingleNALU: d.wireSingleNALU,
		rtpClassSTAPA:      d.wireSTAPA,
		rtpClassFUA:        d.wireFUA,
		rtpClassOther:      d.wireOther,
	})
	for naluType := 0; naluType < naluTypeCount; naluType++ {
		count := d.wireNALUCounts[naluType]
		if count == 0 {
			continue
		}
		if out.WireNALUnitsByType == nil {
			out.WireNALUnitsByType = make(map[string]uint64, 4)
		}
		out.WireNALUnitsByType[naluTypeName(byte(naluType))] = count
	}
	out.MalformedRTPPayloads = d.wireMalformed
	out.FUStarts = d.fuStarts
	out.FUEnds = d.fuEnds
	out.RTPPacketsLost = d.seq.lost()
	out.RTPPacketsReordered = d.seq.reordered
	out.RTPPacketsDuplicated = d.seq.duplicates
	out.MaxSequenceJump = d.seq.maxJump
	out.MarkedPackets = d.frames.markers
	out.TimestampChanges = d.frames.timestampChanges
	out.UnmarkedFrames = d.frames.unmarkedRuns
	out.MaxPacketsPerFrame = d.frames.largestRun()
	out.MaxPacketsPerKeyframe = d.frames.maxKeyframeRunPackets
	out.MediaSourceChanges = d.mediaSSRCMoved
	out.ParameterSetSource = parameterSetSourceName(d.paramSetFromSDP, d.paramSetFromRTP)
	out.KeyframeRequestOnTarget = d.pliTargetSeen && d.mediaSSRCSeen && d.pliTargetSSRC == d.mediaSSRC
	d.wireMu.Unlock()

	d.mu.Lock()
	out.AnswerApplied = d.answerApplied
	out.AnswerRejected = d.answerRejected
	out.LocalICECandidates = d.localCandidates
	out.RemoteICECandidates = d.remoteCandidates
	out.RemoteICECandidatesBad = d.remoteCandidatesBad
	out.UnhandledSignalingMsgs = d.unhandledMessages
	out.MalformedSignalingMsgs = d.malformedMessages
	out.SignalingPumpStopped = d.pumpStopped
	out.SignalingPumpStillActive = d.pumpStopped == ""
	out.TrackObserved = d.trackObserved
	out.TrackMimeType = d.trackMimeType
	out.TrackPayloadType = d.trackPayload
	out.TrackProfileLevel = d.trackProfile
	out.TrackPacketMode = d.trackPktMode
	out.TrackRejection = d.trackRejection
	out.TrackReadFailure = d.trackReadFail
	out.DecodeAttempts = d.decodeAttempts
	out.DecodeBytesIn = d.decodeBytesIn
	out.DecodeFailure = d.decodeFailure
	if d.trackObserved {
		out.TrackAfterMillis = time.Duration(d.trackAtNanos).Milliseconds()
	}
	d.mu.Unlock()

	out.FailureBoundary = out.Boundary()
	return out
}

// Failure boundaries. These are the distinct stages a screenshot can die
// at; the whole point of this file is that the next live run reports
// exactly one of them instead of a single undifferentiated timeout.
const (
	BoundaryNone          = "none: a frame was captured"
	BoundaryTransport     = "transport: peer connection never reached connected"
	BoundaryAnswer        = "signaling: no SDP answer was applied"
	BoundaryNegotiation   = "negotiation: no video track (OnTrack never fired)"
	BoundaryTrackRejected = "negotiation: video track rejected by this client"
	BoundaryNoRTP         = "media: video track negotiated but zero RTP packets arrived"
	BoundaryRTPStalled    = "media: RTP arrived then stopped"
	BoundaryDepacketize   = "depacketization: RTP arrived but no access unit was assembled"
	BoundaryFragmentation = "depacketization: fragmented/aggregated NAL units were not reassembled"
	BoundaryReassembly    = "depacketization: the device sent IDR packets but no IDR access unit was reassembled"
	BoundaryNoKeyframe    = "encoder: access units arrived but the device never sent an IDR keyframe"
	BoundaryNoParamSets   = "encoder: IDR arrived but SPS/PPS parameter sets never did"
	BoundaryDecode        = "decoder: a frame was assembled but decoding failed"
	BoundaryUndetermined  = "undetermined"
)

// Boundary localizes the failure to a single stage. It is a pure function
// of the snapshot, so it is exhaustively unit-testable without any device.
//
// Order matters: the earliest stage that did not complete is the one worth
// reporting, because every later stage is downstream of it.
func (v VideoDiagnostics) Boundary() string {
	switch {
	case v.FramesAssembled > 0 && v.DecodeFailure != "":
		return BoundaryDecode
	case v.FramesAssembled > 0:
		return BoundaryNone
	case v.TrackRejection != "":
		return BoundaryTrackRejected
	case !v.TrackObserved && (v.AnswerRejected || !v.AnswerApplied):
		// Only when nothing downstream got going. A device that sends a
		// second, redundant answer gets it rejected for being out of state,
		// and reporting that as "no answer was applied" while media is
		// flowing would send the next live run chasing the wrong stage.
		return BoundaryAnswer
	case !v.TrackObserved:
		// The answer applied but no media section ever produced a track:
		// the device did not accept anything this client offered.
		return BoundaryNegotiation
	case v.RTPPackets == 0:
		if v.PeerConnectionState != "connected" {
			return BoundaryTransport
		}
		return BoundaryNoRTP
	case v.AccessUnits == 0:
		if v.NALUnitsByType["FU-A"] > 0 || v.NALUnitsByType["STAP-A"] > 0 {
			return BoundaryFragmentation
		}
		return BoundaryDepacketize
	case !v.SawIDR:
		// The wire-level counter breaks the tie the old diagnostics could
		// not: keyframe packets that arrived and then failed to reassemble
		// are a receiver defect, and blaming the encoder for them is what
		// sent the previous investigation in the wrong direction.
		if v.WireNALUnitsByType["IDR"] > 0 {
			return BoundaryReassembly
		}
		return BoundaryNoKeyframe
	case !v.SawSPS || !v.SawPPS:
		return BoundaryNoParamSets
	case v.LastRTPAfterMillis > 0 && v.ElapsedMillis-v.LastRTPAfterMillis > 5000:
		return BoundaryRTPStalled
	default:
		return BoundaryUndetermined
	}
}

// Summary renders a single safe line suitable for embedding in an error
// message. It carries the boundary plus the few counts that make the
// boundary actionable, and nothing else.
func (v VideoDiagnostics) Summary() string {
	var b strings.Builder
	b.WriteString(v.FailureBoundary)
	if b.Len() == 0 {
		b.WriteString(v.Boundary())
	}
	b.WriteString(" [pc=")
	b.WriteString(v.PeerConnectionState)
	b.WriteString(" track=")
	b.WriteString(strconv.FormatBool(v.TrackObserved))
	if v.TrackObserved && v.TrackMimeType != "" {
		b.WriteString(" codec=")
		b.WriteString(v.TrackMimeType)
		b.WriteString("/pt")
		b.WriteString(strconv.Itoa(int(v.TrackPayloadType)))
	}
	b.WriteString(" rtp=")
	b.WriteString(strconv.FormatUint(v.RTPPackets, 10))
	b.WriteString(" au=")
	b.WriteString(strconv.FormatUint(v.AccessUnits, 10))
	b.WriteString(" idr=")
	b.WriteString(strconv.FormatBool(v.SawIDR))
	// Whether IDR packets were on the wire, and whether reassembly threw
	// packets away, are the two numbers that separate a receiver bug from an
	// encoder that never produced a keyframe.
	b.WriteString(" wireIdr=")
	b.WriteString(strconv.FormatUint(v.WireNALUnitsByType["IDR"], 10))
	b.WriteString(" dropped=")
	b.WriteString(strconv.FormatUint(v.BuilderDropped, 10))
	b.WriteString(" lost=")
	b.WriteString(strconv.FormatUint(v.RTPPacketsLost, 10))
	b.WriteString(" pli=")
	b.WriteString(strconv.FormatUint(v.KeyframeRequestsSent, 10))
	b.WriteString(" elapsed=")
	b.WriteString(strconv.FormatInt(v.ElapsedMillis, 10))
	b.WriteString("ms]")
	return b.String()
}

// --- bounded classification helpers ----------------------------------------

// nonZeroCounts drops the zero entries from a fixed-key counter map so an
// idle stage costs nothing in the snapshot. It never adds keys, so the
// result stays inside the closed vocabulary its caller built.
func nonZeroCounts(counts map[string]uint64) map[string]uint64 {
	for key, count := range counts {
		if count == 0 {
			delete(counts, key)
		}
	}
	if len(counts) == 0 {
		return nil
	}
	return counts
}

// parameterSetSourceName renders where the decoder's parameter sets came
// from as one of a closed set of tokens.
func parameterSetSourceName(fromSDP, fromRTP bool) string {
	switch {
	case fromSDP && fromRTP:
		return paramSetSourceSDP + "+" + paramSetSourceRTP
	case fromSDP:
		return paramSetSourceSDP
	case fromRTP:
		return paramSetSourceRTP
	default:
		return ""
	}
}

// naluTypeName maps an H.264 NAL unit type to a short stable name. Unknown
// types render as "type-N" rather than anything device-supplied.
func naluTypeName(t byte) string {
	switch t {
	case 1:
		return "non-IDR"
	case 5:
		return "IDR"
	case 6:
		return "SEI"
	case 7:
		return "SPS"
	case 8:
		return "PPS"
	case 9:
		return "AUD"
	case 12:
		return "filler"
	case 24:
		return "STAP-A"
	case 28:
		return "FU-A"
	default:
		return "type-" + strconv.Itoa(int(t))
	}
}

// sanitizeMimeType only echoes MIME types this client knows about, so a
// device-supplied string can never flow through into output.
func sanitizeMimeType(mime string) string {
	switch strings.ToLower(mime) {
	case strings.ToLower(webrtc.MimeTypeH264):
		return "video/H264"
	case strings.ToLower(webrtc.MimeTypeH265):
		return "video/H265"
	case strings.ToLower(webrtc.MimeTypeVP8):
		return "video/VP8"
	case strings.ToLower(webrtc.MimeTypeVP9):
		return "video/VP9"
	case strings.ToLower(webrtc.MimeTypeAV1):
		return "video/AV1"
	case "":
		return ""
	default:
		return "other"
	}
}

// parseH264Fmtp extracts only the two fmtp parameters that matter for
// compatibility, validating their shape so arbitrary device-supplied text
// can never be echoed back. Anything unrecognized becomes "invalid".
func parseH264Fmtp(fmtp string) (profileLevelID, packetizationMode string) {
	for _, part := range strings.Split(fmtp, ";") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "profile-level-id":
			profileLevelID = validHexToken(strings.TrimSpace(value), 6)
		case "packetization-mode":
			packetizationMode = validSmallInt(strings.TrimSpace(value))
		}
	}
	return profileLevelID, packetizationMode
}

// validHexToken returns v only if it is at most maxLen hex digits.
func validHexToken(v string, maxLen int) string {
	if v == "" || len(v) > maxLen {
		return "invalid"
	}
	for _, r := range v {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return "invalid"
		}
	}
	return strings.ToLower(v)
}

// validSmallInt returns v only if it is a single digit 0-2 (the range RFC
// 6184 defines for packetization-mode).
func validSmallInt(v string) string {
	if len(v) == 1 && v[0] >= '0' && v[0] <= '2' {
		return v
	}
	return "invalid"
}

// classifyTrackReadError maps an RTP read failure to a closed vocabulary.
// The underlying error text is never included: it can name addresses.
func classifyTrackReadError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "track-ended"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, io.ErrClosedPipe):
		return "closed"
	default:
		lowered := strings.ToLower(err.Error())
		switch {
		case strings.Contains(lowered, "closed"):
			return "closed"
		case strings.Contains(lowered, "timeout"), strings.Contains(lowered, "deadline"):
			return "timeout"
		case strings.Contains(lowered, "reset"):
			return "reset"
		default:
			return "read-error"
		}
	}
}

// classifyDecodeError maps a decoder failure to a closed vocabulary.
// FFmpeg's stderr is deliberately never propagated here.
func classifyDecodeError(err error) string {
	if err == nil {
		return ""
	}
	var execErr *exec.ExitError
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, exec.ErrNotFound):
		return "decoder-not-found"
	case errors.As(err, &execErr):
		return "decoder-exit-" + strconv.Itoa(execErr.ExitCode())
	}

	lowered := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lowered, "no output"):
		return "decoder-no-output"
	case strings.Contains(lowered, "executable file not found"), strings.Contains(lowered, "no such file"):
		return "decoder-not-found"
	case strings.Contains(lowered, "png"), strings.Contains(lowered, "image"):
		return "decoder-bad-image"
	default:
		return "decode-failed"
	}
}
