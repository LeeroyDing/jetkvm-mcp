package jetkvm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// peerPair is two directly-connected Pion PeerConnections (no external ICE
// server; loopback host candidates only), used to unit test data-channel
// protocol logic (RPC correlation, HID framing over the wire) without any
// real device or network.
type peerPair struct {
	a, b *webrtc.PeerConnection

	// candidateExchange queues each side's trickled candidates until the
	// receiving peer has a remote description. Adding a candidate to a
	// peer with no remote description fails and the candidate is lost;
	// with loopback-only gathering that race is the common case, not the
	// exception (candidates appear the instant SetLocalDescription runs).
	mu             sync.Mutex
	aReady, bReady bool // remote description applied on a / b
	forA, forB     []webrtc.ICECandidateInit
}

func newPeerPair(t *testing.T) *peerPair {
	t.Helper()

	api := webrtc.NewAPI(webrtc.WithSettingEngine(loopbackSettingEngine()))
	a, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("creating peer a: %v", err)
	}
	b, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("creating peer b: %v", err)
	}

	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})

	p := &peerPair{a: a, b: b}
	a.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		p.deliver(c.ToJSON(), p.b, &p.bReady, &p.forB)
	})
	b.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		p.deliver(c.ToJSON(), p.a, &p.aReady, &p.forA)
	})

	return p
}

// deliver hands a candidate to the target peer, or queues it while the
// target still lacks a remote description.
func (p *peerPair) deliver(c webrtc.ICECandidateInit, target *webrtc.PeerConnection, ready *bool, queue *[]webrtc.ICECandidateInit) {
	p.mu.Lock()
	if !*ready {
		*queue = append(*queue, c)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	_ = target.AddICECandidate(c)
}

// flushTo marks a peer as having its remote description and applies every
// candidate queued for it.
func (p *peerPair) flushTo(target *webrtc.PeerConnection, ready *bool, queue *[]webrtc.ICECandidateInit) {
	p.mu.Lock()
	pending := *queue
	*queue = nil
	*ready = true
	p.mu.Unlock()
	for _, c := range pending {
		_ = target.AddICECandidate(c)
	}
}

// connect performs a manual offer/answer exchange directly between the two
// peer connections (in-process, no signaling server involved) and waits
// for both to report an established connection.
func (p *peerPair) connect(t *testing.T) {
	t.Helper()

	offer, err := p.a.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := p.a.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription(a): %v", err)
	}
	if err := p.b.SetRemoteDescription(offer); err != nil {
		t.Fatalf("SetRemoteDescription(b): %v", err)
	}
	p.flushTo(p.b, &p.bReady, &p.forB)

	answer, err := p.b.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	if err := p.b.SetLocalDescription(answer); err != nil {
		t.Fatalf("SetLocalDescription(b): %v", err)
	}
	if err := p.a.SetRemoteDescription(answer); err != nil {
		t.Fatalf("SetRemoteDescription(a): %v", err)
	}
	p.flushTo(p.a, &p.aReady, &p.forA)

	deadline := time.After(connectTimeout(t, 10*time.Second))
	for p.a.ConnectionState() != webrtc.PeerConnectionStateConnected ||
		p.b.ConnectionState() != webrtc.PeerConnectionStateConnected {
		select {
		case <-deadline:
			t.Fatalf("peer pair did not connect in time: a=%s b=%s", p.a.ConnectionState(), p.b.ConnectionState())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// waitDataChannelOpen blocks until dc's ready state is Open.
func waitDataChannelOpen(t *testing.T, ctx context.Context, dc *webrtc.DataChannel) {
	t.Helper()
	if dc.ReadyState() == webrtc.DataChannelStateOpen {
		return
	}
	done := make(chan struct{})
	dc.OnOpen(func() { close(done) })
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("data channel %q did not open in time", dc.Label())
	}
}
