package jetkvm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	"github.com/pion/webrtc/v4"
)

// H.264 clock rate is fixed at 90kHz by RFC 6184 / the RTP profile; this
// isn't a JetKVM-specific value.
const h264ClockRate = 90000

// H.264 NAL unit type values (ITU-T H.264 Table 7-1), used to find
// SPS/PPS/IDR boundaries in the depacketized Annex-B stream.
const (
	naluTypeIDR = 5
	naluTypeSPS = 7
	naluTypePPS = 8
)

var annexBStartCode = []byte{0x00, 0x00, 0x00, 0x01}

// frame is one self-contained, decodable Annex-B video frame together with
// the wall-clock time it became available. Bundling SPS+PPS+IDR together is
// what makes it self-contained: a decoder can be started fresh on this byte
// slice alone and produce a picture, which is what makes "one screenshot"
// tractable without modelling the whole GOP structure.
type frame struct {
	annexB     []byte
	capturedAt time.Time
	generation uint64
}

// frameCapture reassembles an incoming H.264 RTP track into access units
// and keeps the most recent one that is independently decodable (i.e.
// starts a GOP). It does no pixel decoding itself; see decoder.go.
type frameCapture struct {
	mu         sync.Mutex
	sps        []byte
	pps        []byte
	latest     *frame
	generation uint64
	err        error
	updated    chan struct{}

	// diag is never nil; newFrameCapture substitutes a throwaway collector
	// so every call site can record unconditionally without a nil check.
	diag *videoDiagnostics
}

func newFrameCapture(diag *videoDiagnostics) *frameCapture {
	if diag == nil {
		diag = newVideoDiagnostics()
	}
	return &frameCapture{diag: diag, updated: make(chan struct{})}
}

// run reads RTP packets from track and feeds them through a sample builder
// until ctx is canceled or the track read fails (e.g. peer connection
// closed). It's meant to be run in its own goroutine for the lifetime of
// the session.
func (f *frameCapture) run(ctx context.Context, track *webrtc.TrackRemote) (err error) {
	defer func() { f.endRun(err) }()

	sb := newH264SampleBuilder()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		pkt, _, err := track.ReadRTP()
		if err != nil {
			return err
		}
		f.push(sb, pkt)
	}
}

// push feeds one RTP packet through the reassembler and ingests every access
// unit that completes as a result.
//
// It is a separate method from run so the whole receive path below the
// transport - classification, reassembly, NAL scanning, frame assembly - can
// be driven from a fixture without a peer connection. Every H.264 receiver
// test in this package goes through here.
func (f *frameCapture) push(sb *samplebuilder.SampleBuilder, pkt *rtp.Packet) {
	f.diag.rtpPacket(pkt)
	sb.Push(pkt)
	for {
		sample := sb.Pop()
		if sample == nil {
			break
		}
		f.diag.sampleAssembled(sample.PrevDroppedPackets)
		f.ingest(sample.Data)
	}
}

// endRun records why the RTP read loop stopped and surfaces it to frame
// waiters as a bounded error.
//
// The raw read error stops here. Transport errors name both endpoints of
// the media path ("read udp <local>-><remote>: ..."), and this error is
// what a caller sees as the reason a screenshot failed - including through
// an MCP tool result. Only the category, drawn from the closed vocabulary
// in diagnostics.go, crosses that boundary.
func (f *frameCapture) endRun(err error) {
	if err == nil {
		return
	}
	category := classifyTrackReadError(err)
	f.diag.trackReadFailed(category)
	f.fail(newSessionTransportError(fmt.Sprintf("video track read ended (%s)", category)))
}

// fail records the first terminal media error and wakes frame waiters. The
// session and RPC data channel can remain connected when media fails, so video
// errors need their own propagation path instead of being discarded.
func (f *frameCapture) fail(err error) {
	if err == nil {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return
	}
	f.err = err
	f.notifyLocked()
}

// ingest scans one access unit for SPS/PPS (cached across calls, since
// encoders commonly send parameter sets only once or once per keyframe)
// and IDR slices. When an IDR is found and parameter sets are known, it
// assembles a fresh self-contained frame and wakes any waiters.
func (f *frameCapture) ingest(annexB []byte) {
	nalus := splitAnnexB(annexB)

	var idrNALUs [][]byte
	for _, n := range nalus {
		if len(n) == 0 {
			continue
		}
		// Record every NAL type present. Seeing STAP-A (24) or FU-A (28)
		// here would mean the depacketizer did not undo aggregation or
		// fragmentation, which is a distinct failure from "no keyframe".
		f.diag.naluSeen(n[0] & 0x1F)
		switch n[0] & 0x1F {
		case naluTypeSPS:
			f.mu.Lock()
			f.sps = cloneBytes(n)
			f.mu.Unlock()
			f.diag.parameterSetSource(paramSetSourceRTP)
		case naluTypePPS:
			f.mu.Lock()
			f.pps = cloneBytes(n)
			f.mu.Unlock()
			f.diag.parameterSetSource(paramSetSourceRTP)
		case naluTypeIDR:
			idrNALUs = append(idrNALUs, n)
		}
	}

	if len(idrNALUs) == 0 {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return
	}
	if f.sps == nil || f.pps == nil {
		return // IDR arrived before we ever saw parameter sets; wait for the next one
	}

	parts := make([][]byte, 0, len(idrNALUs)+2)
	parts = append(parts, f.sps, f.pps)
	parts = append(parts, idrNALUs...)

	f.generation++
	fr := &frame{
		annexB:     buildAnnexB(parts...),
		capturedAt: time.Now(),
		generation: f.generation,
	}
	f.latest = fr
	f.diag.frameAssembled()

	f.notifyLocked()
}

// seedParameterSets adopts SPS/PPS supplied out of band in the SDP answer
// (see parseSpropParameterSets), so an encoder that sent its parameter sets
// once - before this client's track handler existed - does not leave every
// subsequent keyframe undecodable.
//
// In-band parameter sets always win: this only fills a gap, and anything the
// encoder sends later overwrites it in ingest.
func (f *frameCapture) seedParameterSets(sps, pps []byte) {
	if sps == nil && pps == nil {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if sps != nil && f.sps == nil {
		f.sps = cloneBytes(sps)
		f.diag.parameterSetSource(paramSetSourceSDP)
	}
	if pps != nil && f.pps == nil {
		f.pps = cloneBytes(pps)
		f.diag.parameterSetSource(paramSetSourceSDP)
	}
}

// generationBoundary returns the most recent completed frame generation.
// A screenshot request records this boundary after it starts, then waits for
// a strictly greater generation. Taking the snapshot under the same mutex as
// ingest makes the before/after distinction deterministic even when a frame
// arrives concurrently with the request.
func (f *frameCapture) generationBoundary() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generation
}

// waitForFrameAfter blocks until a decodable frame newer than after has been
// captured, the video stream ends, or ctx is done. It never returns the cached
// frame at or below the caller's request boundary.
func (f *frameCapture) waitForFrameAfter(ctx context.Context, after uint64) (*frame, error) {
	for {
		f.mu.Lock()
		if f.latest != nil && f.latest.generation > after {
			fr := f.latest
			f.mu.Unlock()
			return fr, nil
		}
		if f.err != nil {
			err := f.err
			f.mu.Unlock()
			return nil, err
		}
		updated := f.updated
		f.mu.Unlock()

		select {
		case <-updated:
			// Re-check under the mutex. A notification can represent a frame
			// that is still at/below this waiter's boundary when multiple
			// callers use frameCapture directly.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// notifyLocked broadcasts a state change without retaining one channel per
// waiter. The replacement channel is installed before the old one is closed,
// so a caller can never miss a transition between checking state and waiting.
// f.mu must be held.
func (f *frameCapture) notifyLocked() {
	close(f.updated)
	f.updated = make(chan struct{})
}

func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func buildAnnexB(nalus ...[]byte) []byte {
	var out []byte
	for _, n := range nalus {
		out = append(out, annexBStartCode...)
		out = append(out, n...)
	}
	return out
}

// splitAnnexB splits an Annex-B byte stream into individual NAL units
// (without their start codes), handling both 3-byte (00 00 01) and 4-byte
// (00 00 00 01) start codes. The trailing-zero-byte handling mirrors the
// equivalent logic in pion/rtp/codecs's own H264 payloader (emitNalus),
// since both are parsing the same Annex-B convention.
func splitAnnexB(data []byte) [][]byte {
	var nalus [][]byte

	idx := indexStartCode(data, 0)
	for idx != -1 {
		contentStart := idx + 3
		next := indexStartCode(data, contentStart)
		if next == -1 {
			nalus = append(nalus, data[contentStart:])
			break
		}
		end := next
		if end > contentStart && data[end-1] == 0 {
			end-- // the "00" belonged to a 4-byte start code, not this NAL
		}
		nalus = append(nalus, data[contentStart:end])
		idx = next
	}
	return nalus
}

// indexStartCode returns the index of the next 3-byte 00 00 01 pattern at
// or after from, or -1 if none exists.
func indexStartCode(data []byte, from int) int {
	for i := from; i+3 <= len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			return i
		}
	}
	return -1
}
