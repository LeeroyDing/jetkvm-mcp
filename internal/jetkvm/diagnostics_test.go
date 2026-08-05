package jetkvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// sensitiveCanaries are representative values that must never reach a
// diagnostic snapshot. Each stands for a category the privacy contract in
// diagnostics.go forbids: addresses, credentials, SDP, ICE candidates,
// filesystem paths, and raw decoder output.
//
// The values are synthetic: RFC 5737 documentation addresses, an RFC 2606
// name, and one deliberate RFC1918 canary. This file never carries a real
// device's address or hostname.
var sensitiveCanaries = []struct {
	category string
	value    string
}{
	{"private LAN address", "192.0.2.138"},
	{"device hostname", "jetkvm.example.invalid"},
	{"credential", "hunter2-correct-horse"},
	{"session cookie", "authToken=abcdef0123456789abcdef0123456789"},
	{"authorization header", "Authorization: Bearer abcdef0123456789"},
	{"SDP body", "v=0\r\no=- 4611731400430051336 2 IN IP4 127.0.0.1\r\ns=-"},
	{"ICE candidate", "candidate:1 1 UDP 2130706431 10.1.2.3 5000 typ host"},
	{"home directory path", "/Users/someone/Developer/secrets"},
	{"device id", "jetkvm-serial-0123456789abcdef"},
	{"ffmpeg stderr", "[h264 @ 0x7f] error while decoding MB 12 3, bytestream -label"},
}

// TestDiagnosticsNeverExposeSensitiveValues drives every input that accepts
// externally-influenced data through the collector and asserts that no
// canary survives into the serialized snapshot. This is the enforcement
// point for the privacy contract documented in diagnostics.go.
func TestDiagnosticsNeverExposeSensitiveValues(t *testing.T) {
	for _, canary := range sensitiveCanaries {
		t.Run(canary.category, func(t *testing.T) {
			d := newVideoDiagnostics()

			// Every path that takes device- or environment-supplied text.
			d.trackStarted(webrtc.RTPCodecParameters{
				RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType:    "video/" + canary.value,
					SDPFmtpLine: "profile-level-id=" + canary.value + ";packetization-mode=" + canary.value + ";extra=" + canary.value,
				},
				PayloadType: 102,
			})
			d.trackReadFailed(classifyTrackReadError(errors.New(canary.value)))
			d.decodeFailed(classifyDecodeError(errors.New(canary.value)))
			d.signalingPumpStopped(classifyTrackReadError(fmt.Errorf("read %s: %w", canary.value, io.EOF)))
			d.decodeAttempted(len(canary.value))
			// Raw RTP is read byte-by-byte by the wire-level accounting, so
			// drive each canary through as a payload as well as through the
			// text inputs. Two shapes: one where the canary is the whole
			// payload, and one where it rides inside a STAP-A whose length
			// fields are read as structure.
			d.rtpPacket(&rtp.Packet{
				Header:  rtp.Header{SequenceNumber: 7, Timestamp: 90000, SSRC: 0x1234, Marker: true},
				Payload: []byte(canary.value),
			})
			d.rtpPacket(&rtp.Packet{
				Header:  rtp.Header{SequenceNumber: 8, Timestamp: 91500, SSRC: 0x1234},
				Payload: append([]byte{0x78, 0x00, byte(len(canary.value))}, canary.value...),
			})
			d.keyframeRequestTarget(0x1234)
			d.parameterSetSource(paramSetSourceRTP)
			d.sampleAssembled(3)
			d.naluSeen(7)
			d.naluSeen(28)

			snap := d.snapshot(nil)

			encoded, err := json.Marshal(snap)
			if err != nil {
				t.Fatalf("marshalling diagnostics: %v", err)
			}
			if strings.Contains(string(encoded), canary.value) {
				t.Errorf("%s leaked into the diagnostic snapshot", canary.category)
			}
			if strings.Contains(snap.Summary(), canary.value) {
				t.Errorf("%s leaked into the diagnostic summary", canary.category)
			}

			// A partial leak is still a leak: check the distinctive
			// substring of each canary, not only the whole string.
			for _, fragment := range strings.FieldsFunc(canary.value, func(r rune) bool {
				return r == ' ' || r == ';' || r == '=' || r == '\r' || r == '\n'
			}) {
				if len(fragment) < 8 {
					continue
				}
				if strings.Contains(string(encoded), fragment) {
					t.Errorf("%s: fragment %q leaked into the snapshot", canary.category, fragment)
				}
			}
		})
	}
}

// TestDiagnosticsErrorTextIsNeverEmbedded guards the specific regression of
// wrapping an underlying error into a diagnostic string.
func TestDiagnosticsErrorTextIsNeverEmbedded(t *testing.T) {
	secret := "connect to 192.0.2.138:80: no route to host"

	for name, got := range map[string]string{
		"track read": classifyTrackReadError(errors.New(secret)),
		"decode":     classifyDecodeError(errors.New(secret)),
	} {
		if strings.Contains(got, "192.0.2") || strings.Contains(got, "route") {
			t.Errorf("%s classification embedded the original error text: %q", name, got)
		}
		if got == "" {
			t.Errorf("%s classification returned an empty category", name)
		}
	}
}

func TestClassifyTrackReadErrorVocabularyIsClosed(t *testing.T) {
	allowed := map[string]bool{
		"": true, "track-ended": true, "canceled": true, "timeout": true,
		"closed": true, "reset": true, "read-error": true,
	}
	cases := []error{
		nil, io.EOF, io.ErrUnexpectedEOF, context.Canceled, context.DeadlineExceeded,
		io.ErrClosedPipe, errors.New("connection reset by peer"),
		errors.New("the pipe is closed"), errors.New("i/o timeout"),
		errors.New("something else entirely"),
		fmt.Errorf("wrapped: %w", io.EOF),
	}
	for _, err := range cases {
		got := classifyTrackReadError(err)
		if !allowed[got] {
			t.Errorf("classifyTrackReadError(%v) = %q, which is outside the closed vocabulary", err, got)
		}
	}
}

func TestClassifyDecodeErrorVocabularyIsClosed(t *testing.T) {
	// A real exit status, so the exec.ExitError branch is exercised.
	exitErr := exec.Command("sh", "-c", "exit 3").Run()
	if exitErr == nil {
		t.Skip("could not produce a nonzero exit status")
	}

	cases := []error{
		nil, context.Canceled, context.DeadlineExceeded, exec.ErrNotFound, exitErr,
		errors.New("jetkvm: ffmpeg produced no output decoding frame"),
		errors.New("executable file not found in $PATH"),
		errors.New("decoding ffmpeg PNG output: invalid image"),
		errors.New("unclassifiable"),
	}
	for _, err := range cases {
		got := classifyDecodeError(err)
		switch {
		case err == nil && got != "":
			t.Errorf("classifyDecodeError(nil) = %q, want empty", got)
		case err != nil && got == "":
			t.Errorf("classifyDecodeError(%v) returned an empty category", err)
		case strings.ContainsAny(got, " /\\"):
			t.Errorf("classifyDecodeError(%v) = %q; categories must be bare tokens", err, got)
		}
	}
}

func TestParseH264FmtpAcceptsOnlyWellFormedValues(t *testing.T) {
	cases := []struct {
		fmtp        string
		wantProfile string
		wantMode    string
	}{
		{"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", "42001f", "1"},
		{"profile-level-id=640C1F;packetization-mode=0", "640c1f", "0"},
		{"profile-level-id=notahexvalue;packetization-mode=9", "invalid", "invalid"},
		{"profile-level-id=;packetization-mode=", "invalid", "invalid"},
		{"profile-level-id=42001fffff", "invalid", ""},
		{"", "", ""},
		{"garbage-without-equals", "", ""},
	}
	for _, tc := range cases {
		profile, mode := parseH264Fmtp(tc.fmtp)
		if profile != tc.wantProfile || mode != tc.wantMode {
			t.Errorf("parseH264Fmtp(%q) = (%q,%q), want (%q,%q)",
				tc.fmtp, profile, mode, tc.wantProfile, tc.wantMode)
		}
	}
}

func TestSanitizeMimeTypeOnlyEchoesKnownCodecs(t *testing.T) {
	cases := map[string]string{
		webrtc.MimeTypeH264:                  "video/H264",
		webrtc.MimeTypeH265:                  "video/H265",
		webrtc.MimeTypeVP8:                   "video/VP8",
		"video/anything-else":                "other",
		"video/192.0.2.138":                  "other",
		"":                                   "",
		strings.ToLower(webrtc.MimeTypeH264): "video/H264",
	}
	for in, want := range cases {
		if got := sanitizeMimeType(in); got != want {
			t.Errorf("sanitizeMimeType(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBoundaryLocalizesEachFailureStage is the heart of the diagnostic
// work: one pipeline state maps to exactly one named boundary, so a live
// run reports where it stopped rather than that it stopped.
func TestBoundaryLocalizesEachFailureStage(t *testing.T) {
	base := VideoDiagnostics{PeerConnectionState: "connected", AnswerApplied: true}

	with := func(mutate func(*VideoDiagnostics)) VideoDiagnostics {
		v := base
		mutate(&v)
		return v
	}

	cases := []struct {
		name string
		diag VideoDiagnostics
		want string
	}{
		{
			name: "no answer applied",
			diag: with(func(v *VideoDiagnostics) { v.AnswerApplied = false }),
			want: BoundaryAnswer,
		},
		{
			name: "answer rejected",
			diag: with(func(v *VideoDiagnostics) { v.AnswerRejected = true }),
			want: BoundaryAnswer,
		},
		{
			name: "answer applied but no track",
			diag: base,
			want: BoundaryNegotiation,
		},
		{
			// A rejected answer must not outrank stages that demonstrably
			// completed: a redundant second answer is rejected for being
			// out of state even on a session whose media is flowing.
			name: "redundant answer rejected while media flows",
			diag: with(func(v *VideoDiagnostics) {
				v.AnswerRejected = true
				v.TrackObserved = true
				v.RTPPackets = 500
				v.AccessUnits = 60
			}),
			want: BoundaryNoKeyframe,
		},
		{
			name: "track rejected by this client",
			diag: with(func(v *VideoDiagnostics) {
				v.TrackObserved = true
				v.TrackRejection = "non-h264-codec"
			}),
			want: BoundaryTrackRejected,
		},
		{
			name: "track but zero RTP",
			diag: with(func(v *VideoDiagnostics) { v.TrackObserved = true }),
			want: BoundaryNoRTP,
		},
		{
			name: "track and no RTP while disconnected",
			diag: with(func(v *VideoDiagnostics) {
				v.TrackObserved = true
				v.PeerConnectionState = "failed"
			}),
			want: BoundaryTransport,
		},
		{
			name: "RTP but no access units",
			diag: with(func(v *VideoDiagnostics) {
				v.TrackObserved = true
				v.RTPPackets = 500
			}),
			want: BoundaryDepacketize,
		},
		{
			name: "fragmented NALs left unreassembled",
			diag: with(func(v *VideoDiagnostics) {
				v.TrackObserved = true
				v.RTPPackets = 500
				v.NALUnitsByType = map[string]uint64{"FU-A": 400}
			}),
			want: BoundaryFragmentation,
		},
		{
			name: "access units but never a keyframe",
			diag: with(func(v *VideoDiagnostics) {
				v.TrackObserved = true
				v.RTPPackets = 500
				v.AccessUnits = 60
				v.KeyframeRequestsSent = 20
			}),
			want: BoundaryNoKeyframe,
		},
		{
			// The shape of the 2026-08-05 live run, replayed against the
			// diagnostics that can now name it: keyframe packets on the
			// wire, no keyframe access unit out of reassembly. Blaming the
			// encoder for this is what cost the previous investigation.
			name: "IDR packets arrived but reassembly lost them",
			diag: with(func(v *VideoDiagnostics) {
				v.TrackObserved = true
				v.RTPPackets = 10186
				v.AccessUnits = 2106
				v.KeyframeRequestsSent = 20
				v.WireNALUnitsByType = map[string]uint64{"non-IDR": 9000, "IDR": 1100, "SPS": 43, "PPS": 43}
				v.BuilderDropped = 900
			}),
			want: BoundaryReassembly,
		},
		{
			name: "IDR without parameter sets",
			diag: with(func(v *VideoDiagnostics) {
				v.TrackObserved = true
				v.RTPPackets = 500
				v.AccessUnits = 60
				v.SawIDR = true
			}),
			want: BoundaryNoParamSets,
		},
		{
			name: "frame assembled then decode failed",
			diag: with(func(v *VideoDiagnostics) {
				v.TrackObserved = true
				v.RTPPackets = 500
				v.AccessUnits = 60
				v.SawIDR, v.SawSPS, v.SawPPS = true, true, true
				v.FramesAssembled = 1
				v.DecodeFailure = "decoder-not-found"
			}),
			want: BoundaryDecode,
		},
		{
			name: "success",
			diag: with(func(v *VideoDiagnostics) {
				v.TrackObserved = true
				v.RTPPackets = 500
				v.AccessUnits = 60
				v.SawIDR, v.SawSPS, v.SawPPS = true, true, true
				v.FramesAssembled = 1
			}),
			want: BoundaryNone,
		},
		{
			name: "RTP stalled mid-session",
			diag: with(func(v *VideoDiagnostics) {
				v.TrackObserved = true
				v.RTPPackets = 500
				v.AccessUnits = 60
				v.SawIDR, v.SawSPS, v.SawPPS = true, true, true
				v.LastRTPAfterMillis = 1000
				v.ElapsedMillis = 45000
			}),
			want: BoundaryRTPStalled,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.diag.Boundary(); got != tc.want {
				t.Errorf("Boundary() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBoundaryIsDeterministic guards against map-iteration or timing
// dependence in the classifier.
func TestBoundaryIsDeterministic(t *testing.T) {
	v := VideoDiagnostics{
		PeerConnectionState: "connected",
		AnswerApplied:       true,
		TrackObserved:       true,
		RTPPackets:          10,
		NALUnitsByType:      map[string]uint64{"FU-A": 1, "STAP-A": 2, "non-IDR": 3, "SEI": 4},
	}
	first := v.Boundary()
	for i := 0; i < 200; i++ {
		if got := v.Boundary(); got != first {
			t.Fatalf("Boundary() is not deterministic: %q then %q", first, got)
		}
	}
}

// TestDiagnosticsSnapshotIsBounded proves the report cannot grow without
// limit no matter how much traffic passed through.
func TestDiagnosticsSnapshotIsBounded(t *testing.T) {
	d := newVideoDiagnostics()
	for naluType := 0; naluType < 300; naluType++ {
		for i := 0; i < 10; i++ {
			d.naluSeen(byte(naluType))
		}
	}
	for i := 0; i < 10000; i++ {
		// Cycle the payload's first byte through every packetization class,
		// so the wire-level maps are as full as they can ever get.
		d.rtpPacket(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i) * 1500}, //nolint:gosec // fixture
			Payload: []byte{byte(i % 32), 0x85, 0x01, 0x02},
		})
		d.sampleAssembled(uint16(i % 7)) //nolint:gosec // fixture
	}

	snap := d.snapshot(nil)
	if len(snap.NALUnitsByType) > naluTypeCount {
		t.Errorf("NAL map has %d entries, want at most %d", len(snap.NALUnitsByType), naluTypeCount)
	}
	if len(snap.WireNALUnitsByType) > naluTypeCount {
		t.Errorf("wire NAL map has %d entries, want at most %d", len(snap.WireNALUnitsByType), naluTypeCount)
	}
	if len(snap.RTPPacketsByClass) > 5 {
		t.Errorf("packetization class map has %d entries; the vocabulary is closed at 5", len(snap.RTPPacketsByClass))
	}
	encoded, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 4096 {
		t.Errorf("diagnostic snapshot is %d bytes; it must stay small enough to attach to an error", len(encoded))
	}
	if strings.Count(snap.Summary(), "\n") != 0 {
		t.Error("Summary must be a single line")
	}
}

// TestDiagnosticsConcurrentUpdatesAreRaceFree exercises the split between
// atomic hot-path counters and the mutex-guarded fields. Run under -race.
func TestDiagnosticsConcurrentUpdatesAreRaceFree(t *testing.T) {
	d := newVideoDiagnostics()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				d.rtpPacket(&rtp.Packet{
					Header:  rtp.Header{SequenceNumber: uint16(i), SSRC: 0x1234}, //nolint:gosec // fixture
					Payload: []byte{0x7c, 0x85, 0x01, 0x02},
				})
				d.sampleAssembled(1)
				d.naluSeen(byte(i % naluTypeCount))
				d.frameAssembled()
				d.parameterSetSource(paramSetSourceRTP)
				d.keyframeRequestTarget(0x1234)
				d.keyframeRequested(nil)
				d.keyframeRequested(errors.New("x"))
				d.localCandidate()
				d.remoteCandidate(i%2 == 0)
				d.unhandledMessage()
				d.malformedMessage()
				d.answerOutcome(true)
				d.decodeAttempted(10)
			}
		}(i)
	}

	// Snapshot concurrently with all of that.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := d.snapshot(nil)
				_ = snap.Boundary()
				_ = snap.Summary()
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	snap := d.snapshot(nil)
	if snap.RTPPackets == 0 {
		t.Fatal("expected RTP packets to have been counted")
	}
	if snap.RTPPackets != snap.AccessUnits {
		t.Errorf("counters diverged under concurrency: rtp=%d accessUnits=%d", snap.RTPPackets, snap.AccessUnits)
	}
}

func TestDiagnosticsRecordFirstAndLastRTPTimes(t *testing.T) {
	d := newVideoDiagnostics()
	if snap := d.snapshot(nil); snap.FirstRTPAfterMillis != 0 || snap.LastRTPAfterMillis != 0 {
		t.Fatal("RTP timings should stay zero until a packet arrives")
	}

	payload := make([]byte, 100)
	payload[0] = 0x41
	d.rtpPacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: 1}, Payload: payload})
	time.Sleep(20 * time.Millisecond)
	d.rtpPacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: 2}, Payload: payload})

	snap := d.snapshot(nil)
	if snap.RTPPackets != 2 {
		t.Fatalf("RTPPackets = %d, want 2", snap.RTPPackets)
	}
	if snap.RTPPayloadBytes != 200 {
		t.Fatalf("RTPPayloadBytes = %d, want 200", snap.RTPPayloadBytes)
	}
	if snap.LastRTPAfterMillis < snap.FirstRTPAfterMillis {
		t.Errorf("last RTP (%dms) precedes first (%dms)", snap.LastRTPAfterMillis, snap.FirstRTPAfterMillis)
	}
}

// TestDiagnosticsReportOfferedCodecParameters checks the snapshot carries
// what this client offered, so a live run can be compared against what the
// device answered without needing the SDP itself.
func TestDiagnosticsReportOfferedCodecParameters(t *testing.T) {
	snap := newVideoDiagnostics().snapshot(nil)
	if snap.OfferedProfileLevel != offeredProfileLevelID {
		t.Errorf("OfferedProfileLevel = %q, want %q", snap.OfferedProfileLevel, offeredProfileLevelID)
	}
	if snap.OfferedPacketMode != offeredPacketizationMode {
		t.Errorf("OfferedPacketMode = %q, want %q", snap.OfferedPacketMode, offeredPacketizationMode)
	}
	if snap.OfferedPayloadType != 102 {
		t.Errorf("OfferedPayloadType = %d, want 102", snap.OfferedPayloadType)
	}
	// The offered fmtp line and these constants must not drift apart.
	if !strings.Contains(h264CodecPreferences[0].SDPFmtpLine, offeredProfileLevelID) {
		t.Error("offeredProfileLevelID does not match the fmtp line actually offered")
	}
}

func TestNALUTypeNamesAreStable(t *testing.T) {
	cases := map[byte]string{
		1: "non-IDR", 5: "IDR", 6: "SEI", 7: "SPS", 8: "PPS",
		9: "AUD", 12: "filler", 24: "STAP-A", 28: "FU-A", 31: "type-31",
	}
	for in, want := range cases {
		if got := naluTypeName(in); got != want {
			t.Errorf("naluTypeName(%d) = %q, want %q", in, got, want)
		}
	}
}
