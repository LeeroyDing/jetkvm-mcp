package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/leeroyding/jetkvm-mcp/internal/hidproto"
	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

// This is a second, independent, deliberately minimal re-implementation of
// a fake JetKVM device (distinct from internal/jetkvm's own test fixture),
// used only to exercise the MCP tool-registration and argument-handling
// layer end to end. The wire-format correctness (HID framing, signaling
// message shapes, video depacketization) is already pinned by
// internal/jetkvm's much more thorough tests; duplicating a trimmed
// version here keeps this package's tests from reaching into another
// package's unexported test helpers.

type signalingMsg struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type offerMsg struct {
	Sd string `json:"sd"`
}

type fakeDevice struct {
	srv *httptest.Server
	t   *testing.T
}

func startFakeDevice(t *testing.T) *fakeDevice {
	t.Helper()
	fd := &fakeDevice{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/device/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jetkvm.DeviceStatus{IsSetup: true})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		mode := "noPassword"
		_ = json.NewEncoder(w).Encode(jetkvm.LocalDevice{AuthMode: &mode, DeviceID: "fake-device"})
	})
	mux.HandleFunc("/webrtc/signaling/client", fd.handleSignaling)
	fd.srv = httptest.NewServer(mux)
	t.Cleanup(fd.srv.Close)
	return fd
}

func (fd *fakeDevice) baseURL() string {
	return "http" + strings.TrimPrefix(fd.srv.URL, "http")
}

func (fd *fakeDevice) handleSignaling(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ctx := r.Context()

	meta, _ := json.Marshal(map[string]any{"type": "device-metadata", "data": map[string]string{"deviceVersion": "0.4.7+dev"}})
	if err := conn.Write(ctx, websocket.MessageText, meta); err != nil {
		return
	}

	var pc *webrtc.PeerConnection
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg signalingMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "offer":
			pc, err = fd.handleOffer(ctx, conn, msg.Data)
			if err != nil {
				fd.t.Logf("fake device: handleOffer: %v", err)
				return
			}
		case "new-ice-candidate":
			if pc == nil {
				continue
			}
			var c webrtc.ICECandidateInit
			if err := json.Unmarshal(msg.Data, &c); err == nil {
				_ = pc.AddICECandidate(c)
			}
		}
	}
}

func (fd *fakeDevice) handleOffer(ctx context.Context, conn *websocket.Conn, raw json.RawMessage) (*webrtc.PeerConnection, error) {
	var off offerMsg
	if err := json.Unmarshal(raw, &off); err != nil {
		return nil, err
	}
	sdJSON, err := base64.StdEncoding.DecodeString(off.Sd)
	if err != nil {
		return nil, err
	}
	var offer webrtc.SessionDescription
	if err := json.Unmarshal(sdJSON, &offer); err != nil {
		return nil, err
	}

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}

	videoTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "kvm")
	if err != nil {
		return nil, err
	}
	if _, err := pc.AddTrack(videoTrack); err != nil {
		return nil, err
	}

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case "rpc":
			dc.OnMessage(func(msg webrtc.DataChannelMessage) {
				var req struct {
					Method string `json:"method"`
					ID     int64  `json:"id"`
				}
				if err := json.Unmarshal(msg.Data, &req); err != nil {
					return
				}
				result := `null`
				if req.Method == "ping" {
					result = `"pong"`
				}
				resp, _ := json.Marshal(map[string]any{
					"jsonrpc": "2.0",
					"result":  json.RawMessage(result),
					"id":      req.ID,
				})
				_ = dc.SendText(string(resp))
			})
		case "hidrpc":
			dc.OnMessage(func(msg webrtc.DataChannelMessage) {
				m, err := hidproto.Unmarshal(msg.Data)
				if err == nil && m.Type == hidproto.TypeHandshake {
					_ = dc.Send(msg.Data)
				}
			})
		}
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, _ := json.Marshal(c.ToJSON())
		msg, _ := json.Marshal(signalingMsg{Type: "new-ice-candidate", Data: data})
		_ = conn.Write(ctx, websocket.MessageText, msg)
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		return nil, err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return nil, err
	}

	answerJSON, err := json.Marshal(pc.LocalDescription())
	if err != nil {
		return nil, err
	}
	answerB64 := base64.StdEncoding.EncodeToString(answerJSON)
	dataBytes, _ := json.Marshal(answerB64)
	respMsg, _ := json.Marshal(signalingMsg{Type: "answer", Data: dataBytes})
	if err := conn.Write(ctx, websocket.MessageText, respMsg); err != nil {
		return nil, err
	}

	go fd.streamVideo(ctx, videoTrack)
	return pc, nil
}

func (fd *fakeDevice) streamVideo(ctx context.Context, track *webrtc.TrackLocalStaticSample) {
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "jetkvm", "testdata", "synthetic_red_32x32.h264")
	data, err := os.ReadFile(path)
	if err != nil {
		fd.t.Logf("fake device: reading synthetic fixture: %v", err)
		return
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = track.WriteSample(media.Sample{Data: data, Duration: 200 * time.Millisecond})
		}
	}
}
