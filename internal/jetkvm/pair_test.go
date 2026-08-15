package jetkvm

import (
	"context"
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
}

func newPeerPair(t *testing.T) *peerPair {
	t.Helper()

	a, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("creating peer a: %v", err)
	}
	b, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("creating peer b: %v", err)
	}

	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})

	a.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = b.AddICECandidate(c.ToJSON())
	})
	b.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = a.AddICECandidate(c.ToJSON())
	})

	return &peerPair{a: a, b: b}
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
