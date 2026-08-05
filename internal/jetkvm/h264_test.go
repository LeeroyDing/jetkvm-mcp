package jetkvm

import (
	"strconv"
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

// H.264 receive-path tests.
//
// Every fixture here is packetized by Pion's own H264Payloader at Pion's own
// outbound MTU, because that is literally what runs on the device: the
// firmware hands whole Annex-B access units to
// TrackLocalStaticSample.WriteSample and Pion turns them into STAP-A and
// FU-A packets. Testing against a hand-rolled approximation of RFC 6184
// would have missed the defect these tests exist for, which lives in the
// interaction between real fragment counts and the reassembly window.
//
// No fixture contains device pixels, and none needs a peer connection.

// testSSRC is an arbitrary synchronization source for fixtures. It is not a
// device value; RTP SSRCs are randomly chosen per stream.
const testSSRC = 0x5EED5EED

// h264Stream builds RTP packet fixtures the way the device does.
type h264Stream struct {
	pktz rtp.Packetizer
}

func newH264Stream() *h264Stream {
	return &h264Stream{
		pktz: rtp.NewPacketizer(
			pionOutboundMTU,
			102,
			testSSRC,
			&codecs.H264Payloader{},
			rtp.NewFixedSequencer(1000),
			h264ClockRate,
		),
	}
}

// samplesPerFrame is one frame at 60fps in the 90kHz H.264 clock, matching
// the mode the device's EDID advertises as preferred.
const samplesPerFrame = h264ClockRate / 60

// accessUnit packetizes one access unit and advances the RTP timestamp by a
// frame, so consecutive calls produce the timestamp boundaries a real stream
// has.
func (s *h264Stream) accessUnit(nalus ...[]byte) []*rtp.Packet {
	return s.pktz.Packetize(buildAnnexB(nalus...), samplesPerFrame)
}

// filler produces n small inter-frame access units. Real streams keep
// flowing, and the sample builder only releases an access unit once it has
// seen a packet belonging to the next one, so tests that want a frame popped
// must keep the stream moving rather than reaching for Flush.
func (s *h264Stream) filler(n int) []*rtp.Packet {
	var pkts []*rtp.Packet
	for i := 0; i < n; i++ {
		pkts = append(pkts, s.accessUnit(testNonIDR(64))...)
	}
	return pkts
}

// Fixture NAL units. Bodies are filled with bytes in 2..252 so they can
// never contain an Annex-B start code, which would make the payloader split
// a NAL unit that was meant to stay whole.
func naluBody(header byte, size int) []byte {
	nalu := make([]byte, size+1)
	nalu[0] = header
	for i := 1; i < len(nalu); i++ {
		nalu[i] = byte(i%251) + 2
	}
	return nalu
}

func testSPS() []byte            { return []byte{0x67, 0x42, 0x00, 0x1f, 0x8c, 0x8d, 0x40, 0x50, 0x1e, 0xd0} }
func testPPS() []byte            { return []byte{0x68, 0xce, 0x3c, 0x80} }
func testSEI() []byte            { return naluBody(0x06, 12) }
func testIDR(size int) []byte    { return naluBody(0x65, size) }
func testNonIDR(size int) []byte { return naluBody(0x41, size) }
func testIDRFragments(n int) []byte {
	// One byte short of n whole fragments, so the NAL needs exactly n of
	// them once the FU-A header overhead is accounted for.
	return testIDR(n*maxFUAPayloadBytes - 1)
}

// receiveWith drives packets through the real receive path - the same
// frameCapture.push the RTP read loop uses - with a caller-chosen
// reassembler, and returns what the pipeline made of them.
func receiveWith(t *testing.T, sb *samplebuilder.SampleBuilder, pkts []*rtp.Packet) (*frameCapture, VideoDiagnostics) {
	t.Helper()
	diag := newVideoDiagnostics()
	// Packets only reach this path on a session that negotiated and
	// connected, so record those stages as complete. Without them every
	// snapshot here would localize to signaling and the media boundaries
	// under test would be unreachable.
	diag.answerOutcome(true)
	diag.trackStarted(h264CodecPreferences[0])

	fc := newFrameCapture(diag)
	for _, pkt := range pkts {
		fc.push(sb, pkt)
	}
	return fc, diag.snapshot(nil)
}

// receive drives packets through the receive path as shipped.
func receive(t *testing.T, pkts []*rtp.Packet) (*frameCapture, VideoDiagnostics) {
	t.Helper()
	return receiveWith(t, newH264SampleBuilder(), pkts)
}

func capturedFrameNALTypes(t *testing.T, fc *frameCapture) map[byte]int {
	t.Helper()
	fc.mu.Lock()
	latest := fc.latest
	fc.mu.Unlock()
	if latest == nil {
		return nil
	}
	counts := make(map[byte]int)
	for _, nalu := range splitAnnexB(latest.annexB) {
		if len(nalu) > 0 {
			counts[nalu[0]&0x1F]++
		}
	}
	return counts
}

// --- the regression ---------------------------------------------------------

// TestKeyframeLargerThanTheOldWindowSurvivesReassembly is the regression for
// the defect the 2026-08-05 live run actually hit.
//
// A 1080p keyframe needs far more than 50 RTP packets. With the 50-packet
// window this receiver shipped with, the sample builder force-dropped the
// keyframe's leading packets to stay inside the window and then discarded
// the remainder for not starting on a partition head - deleting the IDR and
// the SPS/PPS aggregated with it, while every small inter frame sailed
// through. The trace read as "the encoder never sent a keyframe".
func TestKeyframeLargerThanTheOldWindowSurvivesReassembly(t *testing.T) {
	// The window this receiver used to be built with.
	const oldWindow = 50
	// Comfortably past it, and about the size the live trace's bitrate
	// implies for a real keyframe.
	const keyframeFragments = 120

	build := func() []*rtp.Packet {
		s := newH264Stream()
		pkts := s.accessUnit(testSPS(), testPPS(), testIDRFragments(keyframeFragments))
		if len(pkts) <= oldWindow {
			t.Fatalf("fixture keyframe spans %d packets; it must exceed the old %d-packet window", len(pkts), oldWindow)
		}
		return append(pkts, s.filler(3)...)
	}

	t.Run("old window destroys it", func(t *testing.T) {
		sb := samplebuilder.New(oldWindow, &codecs.H264Packet{}, h264ClockRate)
		fc, snap := receiveWith(t, sb, build())

		if got := capturedFrameNALTypes(t, fc); got != nil {
			t.Fatalf("the old window unexpectedly produced a frame (%v); this test no longer reproduces the defect", got)
		}
		if snap.SawIDR || snap.SawSPS || snap.SawPPS {
			t.Errorf("old window: post-reassembly saw sps=%v pps=%v idr=%v, want none of them",
				snap.SawSPS, snap.SawPPS, snap.SawIDR)
		}
		// The wire-level counters are the whole point: the keyframe was
		// there, and only reassembly lost it.
		if snap.WireNALUnitsByType["IDR"] == 0 {
			t.Error("old window: no IDR seen on the wire, so this fixture cannot show the defect")
		}
		if snap.BuilderDropped == 0 {
			t.Error("old window: expected the reassembler to report dropped packets")
		}
		if got := snap.Boundary(); got != BoundaryReassembly {
			t.Errorf("old window: Boundary() = %q, want %q", got, BoundaryReassembly)
		}
	})

	t.Run("current window keeps it", func(t *testing.T) {
		fc, snap := receive(t, build())

		got := capturedFrameNALTypes(t, fc)
		if got == nil {
			t.Fatal("no decodable frame was captured from a keyframe access unit")
		}
		for naluType, name := range map[byte]string{naluTypeSPS: "SPS", naluTypePPS: "PPS", naluTypeIDR: "IDR"} {
			if got[naluType] == 0 {
				t.Errorf("captured frame is missing its %s", name)
			}
		}
		if !snap.SawIDR || !snap.SawSPS || !snap.SawPPS {
			t.Errorf("post-reassembly saw sps=%v pps=%v idr=%v, want all three",
				snap.SawSPS, snap.SawPPS, snap.SawIDR)
		}
		if snap.BuilderDropped != 0 {
			t.Errorf("reassembler dropped %d packets on a lossless stream", snap.BuilderDropped)
		}
		if got := snap.Boundary(); got != BoundaryNone {
			t.Errorf("Boundary() = %q, want %q", got, BoundaryNone)
		}
	})
}

// TestReassemblyWindowCoversTheDocumentedFrameBound checks the derivation in
// h264.go arithmetically, so the window cannot drift below the largest frame
// the device is able to emit without this failing.
func TestReassemblyWindowCoversTheDocumentedFrameBound(t *testing.T) {
	if maxFUAPayloadBytes != 1186 {
		t.Fatalf("maxFUAPayloadBytes = %d, want 1186 (MTU 1200 - RTP 12 - FU-A 2)", maxFUAPayloadBytes)
	}
	fragmentsNeeded := (deviceMaxEncodedFrameBytes + maxFUAPayloadBytes - 1) / maxFUAPayloadBytes
	if maxAccessUnitPackets < fragmentsNeeded {
		t.Fatalf("reassembly window is %d packets but the device's largest frame needs %d",
			maxAccessUnitPackets, fragmentsNeeded)
	}
	// A window wider than the sequence space the builder can compare would
	// silently misbehave rather than buffer more.
	if maxAccessUnitPackets > 32767 {
		t.Fatalf("reassembly window %d exceeds the sequence distance the builder can order", maxAccessUnitPackets)
	}
}

// TestLargestDocumentedFrameIsReassembled proves the window is not merely
// bigger than the old one but actually sufficient, by pushing an access unit
// the size of the device's encoder output buffer bound through the real path.
func TestLargestDocumentedFrameIsReassembled(t *testing.T) {
	s := newH264Stream()
	pkts := s.accessUnit(testSPS(), testPPS(), testIDR(deviceMaxEncodedFrameBytes))
	if len(pkts) < 2000 {
		t.Fatalf("fixture only produced %d packets; it is not exercising the bound", len(pkts))
	}
	fc, snap := receive(t, append(pkts, s.filler(2)...))

	if got := capturedFrameNALTypes(t, fc); got[naluTypeIDR] == 0 {
		t.Fatalf("a maximum-size keyframe was not reassembled (captured NAL types: %v)", got)
	}
	if snap.MaxPacketsPerFrame < uint64(len(pkts)) {
		t.Errorf("MaxPacketsPerFrame = %d, want at least %d", snap.MaxPacketsPerFrame, len(pkts))
	}
	if snap.MaxPacketsPerKeyframe < uint64(len(pkts)) {
		t.Errorf("MaxPacketsPerKeyframe = %d, want at least %d", snap.MaxPacketsPerKeyframe, len(pkts))
	}
}

// --- packetization shapes ---------------------------------------------------

// TestSingleNALKeyframeIsReassembled covers the small-frame path, where the
// IDR fits in one packet and no fragmentation happens at all.
func TestSingleNALKeyframeIsReassembled(t *testing.T) {
	s := newH264Stream()
	pkts := append(s.accessUnit(testSPS(), testPPS(), testIDR(200)), s.filler(2)...)

	fc, snap := receive(t, pkts)

	if got := capturedFrameNALTypes(t, fc); got[naluTypeIDR] == 0 {
		t.Fatalf("single-packet keyframe was not captured (NAL types: %v)", got)
	}
	if snap.RTPPacketsByClass[rtpClassFUA] != 0 {
		t.Errorf("expected no fragmentation for a small keyframe, got %d FU-A packets",
			snap.RTPPacketsByClass[rtpClassFUA])
	}
	if snap.RTPPacketsByClass[rtpClassSTAPA] == 0 {
		t.Error("expected the parameter sets to travel as a STAP-A")
	}
}

// TestParameterSetsArriveInSTAPA checks the aggregation path specifically:
// the SPS and PPS share one packet with each other, ahead of the slice.
func TestParameterSetsArriveInSTAPA(t *testing.T) {
	s := newH264Stream()
	fc, snap := receive(t, append(s.accessUnit(testSPS(), testPPS(), testIDR(400)), s.filler(2)...))

	if snap.WireNALUnitsByType["SPS"] == 0 || snap.WireNALUnitsByType["PPS"] == 0 {
		t.Errorf("wire accounting missed the aggregated parameter sets: %v", snap.WireNALUnitsByType)
	}
	if snap.ParameterSetSource != paramSetSourceRTP {
		t.Errorf("ParameterSetSource = %q, want %q", snap.ParameterSetSource, paramSetSourceRTP)
	}
	if got := capturedFrameNALTypes(t, fc); got[naluTypeSPS] == 0 || got[naluTypePPS] == 0 {
		t.Errorf("captured frame is missing parameter sets: %v", got)
	}
}

// TestMultiSliceAccessUnitKeepsEverySlice checks that an access unit made of
// several NAL units survives whole: a picture split across two slices is
// only decodable if both reach the decoder.
func TestMultiSliceAccessUnitKeepsEverySlice(t *testing.T) {
	s := newH264Stream()
	pkts := s.accessUnit(testSPS(), testPPS(), testSEI(), testIDRFragments(60), testIDR(500))
	fc, snap := receive(t, append(pkts, s.filler(2)...))

	got := capturedFrameNALTypes(t, fc)
	if got[naluTypeIDR] != 2 {
		t.Errorf("captured frame has %d IDR slices, want 2 (NAL types: %v)", got[naluTypeIDR], got)
	}
	// The builder releases an access unit only once a packet of the next one
	// has arrived, so the last filler is still pending.
	if snap.AccessUnits != 2 {
		t.Errorf("AccessUnits = %d, want 2 (the keyframe plus the first filler)", snap.AccessUnits)
	}
}

// TestAccessUnitsFollowMarkerAndTimestamp checks the two boundary signals the
// reassembler relies on are what the fixtures - and so the device - produce.
func TestAccessUnitsFollowMarkerAndTimestamp(t *testing.T) {
	s := newH264Stream()
	pkts := append(s.accessUnit(testSPS(), testPPS(), testIDRFragments(30)), s.filler(4)...)

	var markers int
	timestamps := map[uint32]int{}
	for _, pkt := range pkts {
		if pkt.Marker {
			markers++
		}
		timestamps[pkt.Timestamp]++
	}
	if markers != 5 {
		t.Errorf("fixture has %d marker bits, want one per access unit (5)", markers)
	}
	if len(timestamps) != 5 {
		t.Errorf("fixture has %d distinct timestamps, want 5", len(timestamps))
	}

	_, snap := receive(t, pkts)
	if snap.MarkedPackets != 5 {
		t.Errorf("MarkedPackets = %d, want 5", snap.MarkedPackets)
	}
	if snap.TimestampChanges != 4 {
		t.Errorf("TimestampChanges = %d, want 4", snap.TimestampChanges)
	}
	if snap.UnmarkedFrames != 0 {
		t.Errorf("UnmarkedFrames = %d, want 0 for a well-formed stream", snap.UnmarkedFrames)
	}
}

// --- loss, reordering, duplication ------------------------------------------

// swapEvery returns pkts with each element at index start+k*(width+1) swapped
// with the one width slots later: a deterministic stand-in for a network that
// delivers packets slightly out of order.
func swapEvery(pkts []*rtp.Packet, start, width int) []*rtp.Packet {
	out := append([]*rtp.Packet{}, pkts...)
	for i := start; i+width < len(out); i += width + 1 {
		out[i], out[i+width] = out[i+width], out[i]
	}
	return out
}

// TestReorderedKeyframeStillReassembles checks the reassembler's whole
// reason for existing: RTP does not promise ordered delivery, and a keyframe
// whose fragments arrive shuffled must still come out whole.
func TestReorderedKeyframeStillReassembles(t *testing.T) {
	for _, width := range []int{1, 2, 3, 5, 10, 20} {
		t.Run("displaced by "+strconv.Itoa(width), func(t *testing.T) {
			s := newH264Stream()
			keyframe := s.accessUnit(testSPS(), testPPS(), testIDRFragments(80))
			tail := s.filler(2)

			// Leave the access unit's first packet in place; see
			// TestAccessUnitWhoseFirstPacketArrivesLateIsLost for why that
			// one is special.
			fc, snap := receive(t, append(swapEvery(keyframe, 1, width), tail...))

			if got := capturedFrameNALTypes(t, fc); got[naluTypeIDR] == 0 {
				t.Fatalf("reordered keyframe was not reassembled (NAL types: %v)", got)
			}
			if snap.RTPPacketsReordered == 0 {
				t.Error("expected the sequence tracker to report reordering")
			}
			if snap.RTPPacketsLost != 0 {
				t.Errorf("RTPPacketsLost = %d; reordering must not be counted as loss", snap.RTPPacketsLost)
			}
		})
	}
}

// TestReorderingAnAccessUnitsFirstPacketCostsIt documents a real limit of
// the reassembler this client builds on, so nobody has to rediscover it from
// a live trace.
//
// Pion's sample builder pins the head of the access unit it is working on to
// the first packet it sees for that unit. A packet belonging before that
// head, arriving after it, is never folded back in - however wide the window
// is. Only an access unit's first packet is affected; anything later
// reordering past it is handled (see TestReorderedKeyframeStillReassembles).
//
// It is left alone deliberately. The repair is a replacement reassembler,
// which is a large change to make on no evidence: the live trace showed no
// reordering at all, the device emits a keyframe about every half second so
// a lost one costs half a second, and the diagnostics now report reordering
// directly, so a run where this actually bites will say so.
func TestReorderingAnAccessUnitsFirstPacketCostsIt(t *testing.T) {
	// Whatever that packet was carrying goes with it. Here it is the STAP-A
	// holding the parameter sets, so the IDR still reassembles but is not
	// decodable on its own.
	t.Run("parameter sets", func(t *testing.T) {
		s := newH264Stream()
		keyframe := s.accessUnit(testSPS(), testPPS(), testIDRFragments(40))

		fc, snap := receive(t, append(swapEvery(keyframe, 0, 1), s.filler(2)...))

		if got := capturedFrameNALTypes(t, fc); got != nil {
			t.Fatalf("the documented limitation no longer reproduces (captured %v); "+
				"if the reassembler was replaced, delete this test", got)
		}
		if !snap.SawIDR {
			t.Error("expected the IDR itself to survive; only its leading packet was displaced")
		}
		if snap.SawSPS || snap.SawPPS {
			t.Error("expected the displaced STAP-A's parameter sets to be missing")
		}
		if got := snap.Boundary(); got != BoundaryNoParamSets {
			t.Errorf("Boundary() = %q, want %q", got, BoundaryNoParamSets)
		}
	})

	// When the displaced packet is the slice's own first fragment, the unit
	// no longer starts on a partition head and the whole thing is discarded.
	t.Run("slice head", func(t *testing.T) {
		s := newH264Stream()
		keyframe := s.accessUnit(testIDRFragments(40))

		_, snap := receive(t, append(swapEvery(keyframe, 0, 1), s.filler(2)...))

		if snap.SawIDR {
			t.Fatal("the documented limitation no longer reproduces; if the reassembler was replaced, delete this test")
		}
		if snap.WireNALUnitsByType["IDR"] == 0 {
			t.Fatal("fixture sent no IDR packets")
		}
		if snap.RTPPacketsReordered == 0 {
			t.Error("expected the reordering to be visible in diagnostics")
		}
		// The point of the wire counters: this is reported as a reassembly
		// failure, not blamed on the encoder.
		if got := snap.Boundary(); got != BoundaryReassembly {
			t.Errorf("Boundary() = %q, want %q", got, BoundaryReassembly)
		}
	})
}

// TestDuplicatePacketsAreCountedNotLost checks that a duplicated packet -
// which a retransmitting network can produce - is reported as a duplicate
// and does not inflate the loss estimate.
func TestDuplicatePacketsAreCountedNotLost(t *testing.T) {
	s := newH264Stream()
	pkts := append(s.accessUnit(testSPS(), testPPS(), testIDRFragments(10)), s.filler(2)...)

	withDupes := make([]*rtp.Packet, 0, len(pkts)+3)
	for i, pkt := range pkts {
		withDupes = append(withDupes, pkt)
		if i%4 == 0 {
			withDupes = append(withDupes, pkt)
		}
	}

	fc, snap := receive(t, withDupes)

	if got := capturedFrameNALTypes(t, fc); got[naluTypeIDR] == 0 {
		t.Fatalf("keyframe with duplicated packets was not reassembled (NAL types: %v)", got)
	}
	if snap.RTPPacketsDuplicated == 0 {
		t.Error("expected duplicates to be counted")
	}
	if snap.RTPPacketsLost != 0 {
		t.Errorf("RTPPacketsLost = %d; duplicates must not be counted as loss", snap.RTPPacketsLost)
	}
}

// recoveryFrames is how many inter frames a stream may need after an
// unrecoverable hole before reassembly is producing keyframes again. It
// follows from maxAccessUnitDelay: the builder gives up on the incomplete
// unit once that much media has queued behind it, which is 15 frames at the
// 60fps mode the device advertises, so 20 leaves a little slack.
const recoveryFrames = 20

// TestLostPacketCostsOnlyItsOwnAccessUnit checks that a hole in one keyframe
// does not poison the ones behind it. The firmware discards inbound RTCP, so
// nothing can be retransmitted; the only repair is the next keyframe, and it
// must not be stuck behind the lost one.
func TestLostPacketCostsOnlyItsOwnAccessUnit(t *testing.T) {
	s := newH264Stream()
	broken := s.accessUnit(testSPS(), testPPS(), testIDRFragments(60))
	// Drop a fragment from the middle.
	lossy := append(append([]*rtp.Packet{}, broken[:len(broken)/2]...), broken[len(broken)/2+1:]...)

	stream := append(lossy, s.filler(recoveryFrames)...)
	stream = append(stream, s.accessUnit(testSPS(), testPPS(), testIDRFragments(60))...)
	stream = append(stream, s.filler(2)...)

	fc, snap := receive(t, stream)

	if got := capturedFrameNALTypes(t, fc); got[naluTypeIDR] == 0 {
		t.Fatalf("the keyframe after the lost packet was not reassembled (NAL types: %v)", got)
	}
	if snap.RTPPacketsLost != 1 {
		t.Errorf("RTPPacketsLost = %d, want 1", snap.RTPPacketsLost)
	}
	if snap.BuilderDropped == 0 {
		t.Error("expected the reassembler to report the packets it gave up on")
	}
	if snap.FramesAssembled == 0 {
		t.Error("expected a frame to be assembled despite the loss")
	}
}

// TestReassemblyIsUnaffectedByTheDelayBound is the counterweight to
// maxAccessUnitDelay being small: on a stream with no loss it must never
// discard anything, whatever the frame size or frame rate. If a future
// tightening of the bound started truncating real frames, this is what
// catches it.
func TestReassemblyIsUnaffectedByTheDelayBound(t *testing.T) {
	t.Run("keyframe sizes", func(t *testing.T) {
		for _, keyframeBytes := range []int{200 * 1024, 1024 * 1024, deviceMaxEncodedFrameBytes} {
			s := newH264Stream()
			var stream []*rtp.Packet
			for i := 0; i < 3; i++ {
				stream = append(stream, s.accessUnit(testSPS(), testPPS(), testIDR(keyframeBytes))...)
				stream = append(stream, s.filler(9)...)
			}

			_, snap := receive(t, stream)
			if snap.BuilderDropped != 0 {
				t.Errorf("%d-byte keyframes: reassembler dropped %d packets on a lossless stream",
					keyframeBytes, snap.BuilderDropped)
			}
			if snap.FramesAssembled != 3 {
				t.Errorf("%d-byte keyframes: assembled %d frames, want 3", keyframeBytes, snap.FramesAssembled)
			}
		}
	})

	t.Run("frame rates", func(t *testing.T) {
		// A near-idle screen updates rarely, which puts a whole second
		// between one access unit's timestamp and the next.
		for _, fps := range []int{1, 5, 15, 30, 60} {
			pktz := rtp.NewPacketizer(pionOutboundMTU, 102, testSSRC, &codecs.H264Payloader{},
				rtp.NewFixedSequencer(1000), h264ClockRate)
			step := uint32(h264ClockRate / fps) //nolint:gosec // fps is from the fixed list above

			var stream []*rtp.Packet
			for i := 0; i < 8; i++ {
				stream = append(stream, pktz.Packetize(buildAnnexB(testSPS(), testPPS(), testIDRFragments(60)), step)...)
				stream = append(stream, pktz.Packetize(buildAnnexB(testNonIDR(64)), step)...)
			}

			_, snap := receive(t, stream)
			if snap.BuilderDropped != 0 {
				t.Errorf("%dfps: reassembler dropped %d packets on a lossless stream", fps, snap.BuilderDropped)
			}
			if snap.FramesAssembled != 8 {
				t.Errorf("%dfps: assembled %d frames, want 8", fps, snap.FramesAssembled)
			}
		}
	})
}

// --- wire-level classification ----------------------------------------------

func TestClassifyRTPPayload(t *testing.T) {
	cases := []struct {
		name      string
		payload   []byte
		class     string
		carries   []byte
		fuStart   bool
		fuEnd     bool
		malformed bool
	}{
		{
			name:    "empty",
			payload: nil,
			class:   rtpClassEmpty,
		},
		{
			name:    "single non-IDR slice",
			payload: []byte{0x41, 0x9a, 0x02},
			class:   rtpClassSingleNALU,
			carries: []byte{1},
		},
		{
			name:    "single IDR slice",
			payload: []byte{0x65, 0x88, 0x84},
			class:   rtpClassSingleNALU,
			carries: []byte{naluTypeIDR},
		},
		{
			name:    "STAP-A with SPS and PPS",
			payload: []byte{0x78, 0x00, 0x04, 0x67, 0x42, 0x00, 0x1f, 0x00, 0x04, 0x68, 0xce, 0x3c, 0x80},
			class:   rtpClassSTAPA,
			carries: []byte{naluTypeSPS, naluTypePPS},
		},
		{
			name:      "STAP-A with a size running past the payload",
			payload:   []byte{0x78, 0x00, 0x40, 0x67, 0x42},
			class:     rtpClassSTAPA,
			malformed: true,
		},
		{
			name:      "STAP-A truncated mid length field",
			payload:   []byte{0x78, 0x00},
			class:     rtpClassSTAPA,
			malformed: true,
		},
		{
			name:    "FU-A first fragment of an IDR",
			payload: []byte{0x7c, 0x85, 0x00, 0x01},
			class:   rtpClassFUA,
			carries: []byte{naluTypeIDR},
			fuStart: true,
		},
		{
			name:    "FU-A middle fragment of an IDR",
			payload: []byte{0x7c, 0x05, 0x00, 0x01},
			class:   rtpClassFUA,
			carries: []byte{naluTypeIDR},
		},
		{
			name:    "FU-A final fragment of an IDR",
			payload: []byte{0x7c, 0x45, 0x00, 0x01},
			class:   rtpClassFUA,
			carries: []byte{naluTypeIDR},
			fuEnd:   true,
		},
		{
			name:      "FU-A missing its header",
			payload:   []byte{0x7c},
			class:     rtpClassFUA,
			malformed: true,
		},
		{
			name:    "FU-B, which this stream should never contain",
			payload: []byte{0x7d, 0x85, 0x00, 0x01},
			class:   rtpClassOther,
		},
		{
			name:    "reserved type 0",
			payload: []byte{0x00, 0x01},
			class:   rtpClassOther,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRTPPayload(tc.payload)
			if got.class != tc.class {
				t.Errorf("class = %q, want %q", got.class, tc.class)
			}
			for _, naluType := range tc.carries {
				if !got.carries(naluType) {
					t.Errorf("payload should account for NAL type %d", naluType)
				}
			}
			if got.fuStart != tc.fuStart {
				t.Errorf("fuStart = %v, want %v", got.fuStart, tc.fuStart)
			}
			if got.fuEnd != tc.fuEnd {
				t.Errorf("fuEnd = %v, want %v", got.fuEnd, tc.fuEnd)
			}
			if got.malformed != tc.malformed {
				t.Errorf("malformed = %v, want %v", got.malformed, tc.malformed)
			}
		})
	}
}

// TestClassifyRTPPayloadNeverPanics feeds byte patterns that a hostile or
// broken sender could produce. Classification runs on every received packet,
// so a panic here would take the session down.
func TestClassifyRTPPayloadNeverPanics(t *testing.T) {
	for header := 0; header < 256; header++ {
		for length := 0; length < 8; length++ {
			payload := make([]byte, length)
			if length > 0 {
				payload[0] = byte(header)
				for i := 1; i < length; i++ {
					payload[i] = 0xFF
				}
			}
			info := classifyRTPPayload(payload)
			if info.class == "" {
				t.Fatalf("header %#x length %d produced an empty class", header, length)
			}
		}
	}
}

func TestRTPSequenceTracker(t *testing.T) {
	t.Run("in order", func(t *testing.T) {
		var tr rtpSequenceTracker
		for seq := uint16(100); seq < 200; seq++ {
			tr.observe(seq)
		}
		if got := tr.lost(); got != 0 {
			t.Errorf("lost() = %d, want 0", got)
		}
		if tr.duplicates != 0 || tr.reordered != 0 {
			t.Errorf("duplicates=%d reordered=%d, want 0/0", tr.duplicates, tr.reordered)
		}
	})

	t.Run("gap never filled", func(t *testing.T) {
		var tr rtpSequenceTracker
		for _, seq := range []uint16{1, 2, 5, 6} {
			tr.observe(seq)
		}
		if got := tr.lost(); got != 2 {
			t.Errorf("lost() = %d, want 2", got)
		}
		if tr.maxJump != 3 {
			t.Errorf("maxJump = %d, want 3", tr.maxJump)
		}
	})

	t.Run("gap filled late is reordering, not loss", func(t *testing.T) {
		var tr rtpSequenceTracker
		for _, seq := range []uint16{1, 2, 5, 3, 4, 6} {
			tr.observe(seq)
		}
		if got := tr.lost(); got != 0 {
			t.Errorf("lost() = %d, want 0", got)
		}
		if tr.reordered != 2 {
			t.Errorf("reordered = %d, want 2", tr.reordered)
		}
	})

	t.Run("duplicates", func(t *testing.T) {
		var tr rtpSequenceTracker
		for _, seq := range []uint16{1, 2, 2, 3, 1} {
			tr.observe(seq)
		}
		if tr.duplicates != 2 {
			t.Errorf("duplicates = %d, want 2", tr.duplicates)
		}
		if got := tr.lost(); got != 0 {
			t.Errorf("lost() = %d, want 0", got)
		}
	})

	t.Run("wraps past 65535", func(t *testing.T) {
		var tr rtpSequenceTracker
		for i := 0; i < 10; i++ {
			tr.observe(uint16(65530 + i)) //nolint:gosec // deliberate wrap
		}
		if got := tr.lost(); got != 0 {
			t.Errorf("lost() = %d across a sequence wrap, want 0", got)
		}
		if tr.reordered != 0 {
			t.Errorf("reordered = %d across a sequence wrap, want 0", tr.reordered)
		}
	})

	t.Run("empty tracker reports nothing", func(t *testing.T) {
		var tr rtpSequenceTracker
		if got := tr.lost(); got != 0 {
			t.Errorf("lost() = %d on an untouched tracker, want 0", got)
		}
	})
}

func TestRTPFrameTrackerMeasuresRuns(t *testing.T) {
	var tr rtpFrameTracker

	// A three-packet keyframe, then a one-packet inter frame.
	tr.observe(90, false, true)
	tr.observe(90, false, true)
	tr.observe(90, true, true)
	tr.observe(1590, true, false)

	if got := tr.largestRun(); got != 3 {
		t.Errorf("largestRun() = %d, want 3", got)
	}
	if tr.maxKeyframeRunPackets != 3 {
		t.Errorf("maxKeyframeRunPackets = %d, want 3", tr.maxKeyframeRunPackets)
	}
	if tr.markers != 2 {
		t.Errorf("markers = %d, want 2", tr.markers)
	}
	if tr.timestampChanges != 1 {
		t.Errorf("timestampChanges = %d, want 1", tr.timestampChanges)
	}
	if tr.unmarkedRuns != 0 {
		t.Errorf("unmarkedRuns = %d, want 0", tr.unmarkedRuns)
	}
}

func TestRTPFrameTrackerFlagsRunsWithoutAMarker(t *testing.T) {
	var tr rtpFrameTracker
	tr.observe(90, false, false)
	tr.observe(90, false, false)
	tr.observe(1590, true, false)

	if tr.unmarkedRuns != 1 {
		t.Errorf("unmarkedRuns = %d, want 1", tr.unmarkedRuns)
	}
}

// --- out-of-band parameter sets ---------------------------------------------

func TestParseSpropParameterSets(t *testing.T) {
	// base64 of the fixture SPS and PPS.
	const spsB64 = "Z0IAH4yNQFAe0A=="
	const ppsB64 = "aM48gA=="

	cases := []struct {
		name    string
		fmtp    string
		wantSPS bool
		wantPPS bool
	}{
		{
			name:    "both parameter sets",
			fmtp:    "packetization-mode=1;sprop-parameter-sets=" + spsB64 + "," + ppsB64,
			wantSPS: true,
			wantPPS: true,
		},
		{
			name:    "unpadded base64",
			fmtp:    "sprop-parameter-sets=Z0IAH4yNQFAe0A," + ppsB64,
			wantSPS: true,
			wantPPS: true,
		},
		{
			name: "absent",
			fmtp: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
		},
		{
			name: "not base64",
			fmtp: "sprop-parameter-sets=not!valid!base64!",
		},
		{
			name: "decodes to something that is not a parameter set",
			fmtp: "sprop-parameter-sets=ZWVlZQ==", // NAL type 5, not SPS/PPS
		},
		{
			name: "forbidden zero bit set",
			fmtp: "sprop-parameter-sets=5wECAw==",
		},
		{
			name: "empty value",
			fmtp: "sprop-parameter-sets=",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sps, pps := parseSpropParameterSets(tc.fmtp)
			if (sps != nil) != tc.wantSPS {
				t.Errorf("sps = %v, want present=%v", sps, tc.wantSPS)
			}
			if (pps != nil) != tc.wantPPS {
				t.Errorf("pps = %v, want present=%v", pps, tc.wantPPS)
			}
			if sps != nil && sps[0]&0x1F != naluTypeSPS {
				t.Errorf("returned SPS has NAL type %d", sps[0]&0x1F)
			}
			if pps != nil && pps[0]&0x1F != naluTypePPS {
				t.Errorf("returned PPS has NAL type %d", pps[0]&0x1F)
			}
		})
	}
}

// TestParseSpropParameterSetsIsBounded checks a hostile fmtp line cannot
// make this client allocate or spin.
func TestParseSpropParameterSetsIsBounded(t *testing.T) {
	huge := make([]byte, 64*1024)
	for i := range huge {
		huge[i] = 'A'
	}
	var value []byte
	for i := 0; i < 200; i++ {
		if i > 0 {
			value = append(value, ',')
		}
		value = append(value, huge...)
	}
	sps, pps := parseSpropParameterSets("sprop-parameter-sets=" + string(value))
	if sps != nil || pps != nil {
		t.Fatal("an oversized sprop value must be rejected entirely")
	}
}

// TestSeededParameterSetsUnblockAKeyframeWithoutInBandSets covers the case
// the SDP bootstrap exists for: the encoder's parameter sets were sent
// before this client was listening, so only IDR slices ever arrive in band.
func TestSeededParameterSetsUnblockAKeyframeWithoutInBandSets(t *testing.T) {
	s := newH264Stream()
	// No SPS/PPS in the stream at all.
	pkts := append(s.accessUnit(testIDRFragments(60)), s.filler(2)...)

	t.Run("without seeding there is nothing to decode", func(t *testing.T) {
		fc, snap := receive(t, pkts)
		if got := capturedFrameNALTypes(t, fc); got != nil {
			t.Errorf("captured a frame with no parameter sets: %v", got)
		}
		if got := snap.Boundary(); got != BoundaryNoParamSets {
			t.Errorf("Boundary() = %q, want %q", got, BoundaryNoParamSets)
		}
	})

	t.Run("seeded from SDP the same stream produces a frame", func(t *testing.T) {
		diag := newVideoDiagnostics()
		fc := newFrameCapture(diag)
		fc.seedParameterSets(testSPS(), testPPS())

		sb := newH264SampleBuilder()
		for _, pkt := range pkts {
			fc.push(sb, pkt)
		}

		got := capturedFrameNALTypes(t, fc)
		if got[naluTypeIDR] == 0 || got[naluTypeSPS] == 0 || got[naluTypePPS] == 0 {
			t.Fatalf("seeded capture is not self-contained: %v", got)
		}
		if src := diag.snapshot(nil).ParameterSetSource; src != paramSetSourceSDP {
			t.Errorf("ParameterSetSource = %q, want %q", src, paramSetSourceSDP)
		}
	})
}

// TestInBandParameterSetsOverrideSeededOnes checks precedence: what the
// encoder is sending now must win over what the SDP claimed once.
func TestInBandParameterSetsOverrideSeededOnes(t *testing.T) {
	diag := newVideoDiagnostics()
	fc := newFrameCapture(diag)

	stale := []byte{0x67, 0x11, 0x22, 0x33}
	fc.seedParameterSets(stale, []byte{0x68, 0x44, 0x55, 0x66})

	fresh := testSPS()
	s := newH264Stream()
	sb := newH264SampleBuilder()
	for _, pkt := range append(s.accessUnit(fresh, testPPS(), testIDR(300)), s.filler(2)...) {
		fc.push(sb, pkt)
	}

	fc.mu.Lock()
	got := append([]byte{}, fc.sps...)
	fc.mu.Unlock()

	if string(got) != string(fresh) {
		t.Errorf("cached SPS = %x, want the in-band one %x", got, fresh)
	}
	if src := diag.snapshot(nil).ParameterSetSource; src != paramSetSourceSDP+"+"+paramSetSourceRTP {
		t.Errorf("ParameterSetSource = %q, want both sources recorded", src)
	}
}
