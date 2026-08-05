package jetkvm

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

// H.264 RTP receive path: how RTP packets become access units, and what one
// raw packet can be made to say before that happens.
//
// The two RFC 6184 packet type values that are not themselves H.264 NAL unit
// types but do appear in this stream. The others it defines - STAP-B (25),
// MTAP16 (26), MTAP24 (27) and FU-B (29) - are never produced by Pion's
// H264Payloader, which is what packetizes on the device, so one showing up
// is itself the finding and is counted as rtpClassOther.
const (
	rtpPacketTypeSTAPA = 24
	rtpPacketTypeFUA   = 28
)

// FU header bits (RFC 6184 section 5.8).
const (
	fuStartBit = 0x80
	fuEndBit   = 0x40
)

// Sizing the access-unit reassembly window.
//
// The sample builder holds RTP packets until it can prove an access unit is
// complete, and force-drops the oldest ones once the buffered sequence range
// exceeds its window. Anything dropped that way takes the whole access unit
// with it: the remainder no longer starts on a partition head, so the
// builder discards it (samplebuilder.go, buildSample's IsPartitionHead
// check).
//
// This window used to be 50 packets, which is smaller than a single 1080p
// keyframe. The effect was invisible in aggregate and precisely wrong: every
// small inter frame passed through, every keyframe was shredded, and with it
// the SPS/PPS that the encoder aggregates into the same access unit. The
// 2026-08-05 live run showed exactly that shape - 10,186 RTP packets, 2,106
// assembled access units, every one of them non-IDR, no SPS, no PPS - and it
// was read as "the encoder never emitted a keyframe" when the device had in
// fact emitted one roughly every half second.
//
// The replacement is derived from published bounds rather than chosen:
//
//	device max encoded frame   width*height*3/2 bytes, at the 1920x1080 mode
//	                           the device's EDID advertises as preferred
//	                           (jetkvm/kvm internal/native/cgo/video.c:
//	                           "stAttr->stVencAttr.u32BufSize = width * height * 3 / 2")
//	Pion outbound MTU          1200 bytes (pion/webrtc/v4 constants.go,
//	                           applied by track_local_static.go to every
//	                           TrackLocalStaticSample - which is what the
//	                           firmware writes frames to)
//	RTP fixed header           12 bytes, subtracted by pion/rtp's packetizer
//	                           before it reaches the payloader
//	FU-A payload header        2 bytes (RFC 6184 section 5.8)
//
// leaving 1186 bytes of NAL unit per fragment, so a maximal frame needs 2622
// fragments. nonSliceNALUAllowance covers the parameter-set and SEI NAL
// units the encoder bundles into the same access unit, each of which is its
// own packet.
//
// Memory stays bounded: the builder can hold at most maxAccessUnitPackets
// packets, each a single TrackRemote.ReadRTP buffer of receiveMTU (1500)
// bytes, so the ceiling is under 4 MiB and is only approached by a frame
// that genuinely is that large.
const (
	deviceMaxEncodedFrameBytes = 1920 * 1080 * 3 / 2
	pionOutboundMTU            = 1200
	rtpFixedHeaderBytes        = 12
	fuAPayloadHeaderBytes      = 2

	maxFUAPayloadBytes    = pionOutboundMTU - rtpFixedHeaderBytes - fuAPayloadHeaderBytes
	nonSliceNALUAllowance = 8

	maxAccessUnitPackets = deviceMaxEncodedFrameBytes/maxFUAPayloadBytes + 1 + nonSliceNALUAllowance
)

// maxAccessUnitDelay bounds how much media may pile up behind an incomplete
// access unit before the builder gives up on it. It is measured in the RTP
// timestamp clock, not wall time.
//
// Widening the packet window above without this would have traded one stall
// for another: a single lost packet would hold every later frame hostage
// until maxAccessUnitPackets of them had accumulated, which at the observed
// packet rate is the better part of ten seconds.
//
// The bound is small because waiting is worthless here. The firmware drains
// inbound RTCP without parsing it (jetkvm/kvm webrtc.go, drainRTCP), so
// neither a NACK nor a PLI from this client can bring the missing packet
// back; the only repair is the encoder's next unprompted IDR, which arrives
// about every half second (internal/native/cgo/video.c: "gop = fps > 0 ?
// fps / 2 : 30"). Giving up within half a GOP costs at most one keyframe.
//
// What sets the floor is reordering tolerance, not frame size: every packet
// of one access unit carries the same RTP timestamp, so a frame needing 2624
// fragments still measures as a span of zero. 250ms is roughly 15 frames of
// slack at the 60fps mode the device advertises, far past any reordering a
// LAN produces. TestReassemblyIsUnaffectedByTheDelayBound holds that claim
// down against maximum-size keyframes and frame rates from 1 to 60fps.
const maxAccessUnitDelay = 250 * time.Millisecond

// newH264SampleBuilder builds the depacketizer/reassembler pair used for the
// whole life of a session. IsAVC stays false so samples come out as Annex-B,
// which is what the decoder and frameCapture.ingest expect.
func newH264SampleBuilder() *samplebuilder.SampleBuilder {
	return samplebuilder.New(
		maxAccessUnitPackets,
		&codecs.H264Packet{},
		h264ClockRate,
		samplebuilder.WithMaxTimeDelay(maxAccessUnitDelay),
	)
}

// Packetization classes, a closed vocabulary so a device-supplied byte can
// never turn into a novel string in a diagnostic snapshot.
const (
	rtpClassEmpty      = "empty"
	rtpClassSingleNALU = "single-nalu"
	rtpClassSTAPA      = "STAP-A"
	rtpClassFUA        = "FU-A"
	rtpClassOther      = "other"
)

// rtpPayloadInfo is what one raw RTP payload says about the H.264 stream,
// read before the sample builder gets a chance to reassemble or discard it.
//
// This is the independent witness the post-depacketization NAL accounting
// lacks. "No IDR access unit was assembled" and "no IDR was ever sent" look
// identical downstream of the builder; they differ here, because an IDR too
// large for one packet still announces its type in the FU header of every
// one of its fragments.
type rtpPayloadInfo struct {
	class string

	// naluTypeMask has bit N set when H.264 NAL unit type N is carried by
	// this payload (single NAL), aggregated into it (STAP-A), or being
	// fragmented by it (FU-A). A mask rather than a list: no allocation on
	// the read loop's hot path, and no way for a malformed packet to make it
	// grow.
	naluTypeMask uint32

	fuStart bool
	fuEnd   bool

	// malformed marks a payload this client could not fully parse: a
	// truncated FU header, or a STAP-A whose declared aggregation-unit size
	// runs past the end of the packet.
	malformed bool
}

func (i *rtpPayloadInfo) addNALUType(naluType byte) {
	i.naluTypeMask |= 1 << (naluType & 0x1F)
}

func (i rtpPayloadInfo) carries(naluType byte) bool {
	return i.naluTypeMask&(1<<(naluType&0x1F)) != 0
}

// classifyRTPPayload parses one RTP payload far enough to name its
// packetization and the NAL unit types it accounts for. It never retains,
// copies or returns payload bytes.
func classifyRTPPayload(payload []byte) rtpPayloadInfo {
	if len(payload) == 0 {
		return rtpPayloadInfo{class: rtpClassEmpty}
	}

	switch naluType := payload[0] & 0x1F; {
	case naluType >= 1 && naluType <= 23:
		info := rtpPayloadInfo{class: rtpClassSingleNALU}
		info.addNALUType(naluType)
		return info
	case naluType == rtpPacketTypeSTAPA:
		return classifySTAPA(payload)
	case naluType == rtpPacketTypeFUA:
		return classifyFUA(payload)
	default:
		// STAP-B, MTAP, FU-B and the reserved types. Pion's payloader emits
		// none of them, so seeing one at all is the finding.
		return rtpPayloadInfo{class: rtpClassOther}
	}
}

// classifySTAPA walks a single-time aggregation packet's aggregation units
// (RFC 6184 section 5.7.1). This is where the device's SPS and PPS travel:
// Pion's H264Payloader holds them back and emits them as a STAP-A in front
// of the next slice.
func classifySTAPA(payload []byte) rtpPayloadInfo {
	info := rtpPayloadInfo{class: rtpClassSTAPA}

	for offset := 1; offset < len(payload); {
		if offset+2 > len(payload) {
			info.malformed = true
			break
		}
		size := int(payload[offset])<<8 | int(payload[offset+1])
		offset += 2
		if size == 0 || offset+size > len(payload) {
			info.malformed = true
			break
		}
		info.addNALUType(payload[offset] & 0x1F)
		offset += size
	}
	return info
}

// classifyFUA reads a fragmentation unit's header (RFC 6184 section 5.8).
// The FU header's type field is the type of the NAL unit being fragmented,
// so every fragment of a keyframe - not just the first - identifies it as
// an IDR.
func classifyFUA(payload []byte) rtpPayloadInfo {
	if len(payload) < fuAPayloadHeaderBytes {
		return rtpPayloadInfo{class: rtpClassFUA, malformed: true}
	}
	info := rtpPayloadInfo{
		class:   rtpClassFUA,
		fuStart: payload[1]&fuStartBit != 0,
		fuEnd:   payload[1]&fuEndBit != 0,
	}
	info.addNALUType(payload[1] & 0x1F)
	return info
}

// --- wire-level trackers ----------------------------------------------------

// seqWindow is how many sequence numbers back duplicate and reordering
// detection reaches. Real-world reordering is a handful of packets; 64 is
// generous and fits in one word, so the tracker cannot grow.
const seqWindow = 64

// rtpSequenceTracker follows the RTP sequence number space so a live run can
// tell "the device never sent it" from "it arrived out of order" from "it
// arrived twice".
//
// Loss is derived the RFC 3550 way - sequence numbers expected minus unique
// packets received - rather than counted from forward gaps, so a reordered
// packet that shows up late does not leave a phantom loss behind.
type rtpSequenceTracker struct {
	started bool
	first   uint16
	highest uint16
	cycles  uint32

	received   uint64
	duplicates uint64
	reordered  uint64
	maxJump    uint16

	// recent is a bitmap of the seqWindow sequence numbers ending at
	// highest: bit i is set when highest-i has been seen.
	recent uint64
}

func (t *rtpSequenceTracker) observe(seq uint16) {
	t.received++

	if !t.started {
		t.started = true
		t.first, t.highest = seq, seq
		t.recent = 1
		return
	}

	switch delta := int16(seq - t.highest); {
	case delta > 0:
		if uint16(delta) >= seqWindow {
			t.recent = 1
		} else {
			t.recent = t.recent<<uint(delta) | 1
		}
		if uint16(delta) > t.maxJump {
			t.maxJump = uint16(delta)
		}
		if seq < t.highest {
			t.cycles++ // the 16-bit sequence number space wrapped
		}
		t.highest = seq
	case delta == 0:
		t.duplicates++
	default:
		// uint16 rather than uint: the one delta that cannot be negated
		// inside int16 lands on 32768 and falls out of the window below,
		// instead of becoming an enormous shift count.
		back := uint16(-delta)
		if back < seqWindow {
			if t.recent&(1<<back) != 0 {
				t.duplicates++
				return
			}
			t.recent |= 1 << back
		}
		t.reordered++
	}
}

// lost reports how many sequence numbers never arrived at all.
func (t *rtpSequenceTracker) lost() uint64 {
	if !t.started {
		return 0
	}
	extended := uint64(t.cycles)<<16 + uint64(t.highest)
	first := uint64(t.first)
	if extended < first {
		return 0
	}
	expected := extended - first + 1
	unique := t.received - t.duplicates
	if expected <= unique {
		return 0
	}
	return expected - unique
}

// rtpFrameTracker follows marker bits and timestamps, the two signals the
// sample builder uses to decide where an access unit ends.
//
// maxRunPackets is the measurement that sizes the reassembly window against
// real hardware: it is how many RTP packets the device's largest access unit
// actually needed. Recording it next to maxKeyframeRunPackets is what turns
// "the window is big enough" from an assumption into an observation.
type rtpFrameTracker struct {
	started    bool
	timestamp  uint32
	runPackets uint64
	runHasIDR  bool
	runMarked  bool

	markers               uint64
	timestampChanges      uint64
	unmarkedRuns          uint64
	maxRunPackets         uint64
	maxKeyframeRunPackets uint64
}

func (t *rtpFrameTracker) observe(timestamp uint32, marker, carriesIDR bool) {
	switch {
	case !t.started:
		t.started = true
		t.timestamp = timestamp
	case timestamp != t.timestamp:
		t.endRun()
		t.timestamp = timestamp
	}

	t.runPackets++
	t.runHasIDR = t.runHasIDR || carriesIDR
	if marker {
		t.markers++
		t.runMarked = true
	}
}

// endRun closes the current same-timestamp run of packets.
func (t *rtpFrameTracker) endRun() {
	t.timestampChanges++
	if !t.runMarked {
		// A run that ended without a marker bit means the builder had no
		// partition tail to key on and had to infer the boundary from the
		// timestamp change instead.
		t.unmarkedRuns++
	}
	if t.runPackets > t.maxRunPackets {
		t.maxRunPackets = t.runPackets
	}
	if t.runHasIDR && t.runPackets > t.maxKeyframeRunPackets {
		t.maxKeyframeRunPackets = t.runPackets
	}
	t.runPackets, t.runHasIDR, t.runMarked = 0, false, false
}

// largestRun reports the biggest same-timestamp run so far, including one
// still in progress, so a snapshot taken mid-frame does not under-report.
func (t *rtpFrameTracker) largestRun() uint64 {
	if t.runPackets > t.maxRunPackets {
		return t.runPackets
	}
	return t.maxRunPackets
}

// --- out-of-band parameter sets ---------------------------------------------

// Where a decoder's SPS/PPS came from. A closed vocabulary: these are the
// only two ways this client can learn them.
const (
	paramSetSourceRTP = "rtp"
	paramSetSourceSDP = "sdp"
)

// maxParameterSetBytes bounds a single decoded parameter set. Real SPS and
// PPS NAL units are tens of bytes; this only exists so a hostile or broken
// fmtp line cannot make this client allocate.
const maxParameterSetBytes = 512

// maxSpropParameterSets bounds how many comma-separated entries are parsed
// out of one sprop-parameter-sets value.
const maxSpropParameterSets = 8

// parseSpropParameterSets extracts SPS and PPS NAL units carried in the
// answer's fmtp line (RFC 6184 section 8.1: base64 NAL units, comma
// separated, in decoding order).
//
// This is a read-only bootstrap for the case where the encoder sends its
// parameter sets once, before this client's track handler is running: the
// decoder needs an SPS and a PPS to make a picture out of an IDR, and if
// they only ever arrived in-band before we were listening, the session can
// receive keyframes forever and still produce nothing.
//
// The device is not currently expected to send this attribute - the firmware
// builds its track from a bare MimeType and lets Pion generate the fmtp line
// - so this is defensive rather than load-bearing. Everything it returns is
// validated to be an actual SPS/PPS NAL unit of a plausible size; a value
// that is not is ignored, never echoed.
func parseSpropParameterSets(fmtp string) (sps, pps []byte) {
	for _, part := range strings.Split(fmtp, ";") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "sprop-parameter-sets") {
			continue
		}
		for i, encoded := range strings.Split(strings.TrimSpace(value), ",") {
			if i >= maxSpropParameterSets {
				break
			}
			nalu := decodeParameterSet(encoded)
			if nalu == nil {
				continue
			}
			switch nalu[0] & 0x1F {
			case naluTypeSPS:
				if sps == nil {
					sps = nalu
				}
			case naluTypePPS:
				if pps == nil {
					pps = nalu
				}
			}
		}
	}
	return sps, pps
}

// decodeParameterSet base64-decodes one sprop entry, tolerating the
// unpadded form that some SDP writers emit, and rejects anything that is not
// a plausible NAL unit.
func decodeParameterSet(encoded string) []byte {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" || len(encoded) > 4*maxParameterSetBytes {
		return nil
	}
	nalu, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		if nalu, err = base64.RawStdEncoding.DecodeString(encoded); err != nil {
			return nil
		}
	}
	// A NAL unit is at minimum its header byte plus some RBSP, and a set
	// forbidden_zero_bit means the first byte is not a NAL header at all.
	if len(nalu) < 2 || len(nalu) > maxParameterSetBytes || nalu[0]&0x80 != 0 {
		return nil
	}
	return nalu
}
