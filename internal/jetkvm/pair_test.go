package jetkvm

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/transport/v4/vnet"
	"github.com/pion/webrtc/v4"
)

// peerPair is two directly-connected Pion PeerConnections (no external ICE
// server), used to unit test data-channel protocol logic (RPC correlation,
// HID framing over the wire) without any real device or external network.
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
	return newPeerPairFromConnections(t, a, b)
}

// newStallablePeerPair connects two real Pion stacks through a virtual
// router whose packet forwarding can be disabled without closing either
// DataChannel. It models the audited open-but-stalled transport state.
func newStallablePeerPair(t *testing.T) (*peerPair, *atomic.Bool) {
	t.Helper()

	loggerFactory := logging.NewDefaultLoggerFactory()
	wan, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:          "10.13.0.0/24",
		LoggerFactory: loggerFactory,
	})
	if err != nil {
		t.Fatalf("creating virtual router: %v", err)
	}
	offerNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"10.13.0.1"}})
	if err != nil {
		t.Fatalf("creating offer virtual network: %v", err)
	}
	answerNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"10.13.0.2"}})
	if err != nil {
		t.Fatalf("creating answer virtual network: %v", err)
	}
	if err := wan.AddNet(offerNet); err != nil {
		t.Fatalf("adding offer network to virtual router: %v", err)
	}
	if err := wan.AddNet(answerNet); err != nil {
		t.Fatalf("adding answer network to virtual router: %v", err)
	}

	forward := &atomic.Bool{}
	forward.Store(true)
	wan.AddChunkFilter(func(vnet.Chunk) bool { return forward.Load() })
	if err := wan.Start(); err != nil {
		t.Fatalf("starting virtual router: %v", err)
	}
	t.Cleanup(func() {
		forward.Store(true)
		if err := wan.Stop(); err != nil {
			t.Errorf("stopping virtual router: %v", err)
		}
	})

	newPC := func(n *vnet.Net) *webrtc.PeerConnection {
		se := webrtc.SettingEngine{}
		se.SetNet(n)
		// Keep the peer visibly open for the short intentional packet stall.
		se.SetICETimeouts(30*time.Second, 30*time.Second, 5*time.Second)
		pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatalf("creating virtual-network peer: %v", err)
		}
		return pc
	}

	return newPeerPairFromConnections(t, newPC(offerNet), newPC(answerNet)), forward
}

func newPeerPairFromConnections(t *testing.T, a, b *webrtc.PeerConnection) *peerPair {
	t.Helper()

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
