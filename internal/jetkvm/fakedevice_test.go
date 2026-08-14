package jetkvm

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
)

// fakeDeviceOptions configures the behavior of a fakeDeviceServer instance.
type fakeDeviceOptions struct {
	Password      string // empty = noPassword mode
	DeviceID      string // empty defaults to "fake-device-id"
	DeviceVersion string // empty defaults to "0.4.7+dev"
	PingError     string // non-empty is returned as a raw device RPC error string
	// SkipDeviceMetadata simulates a firmware whose signaling handshake no
	// longer matches this client's compatibility assumptions.
	SkipDeviceMetadata bool
	// WithoutVideo negotiates the video track but never streams a frame,
	// simulating a device whose encoder never produces a usable picture.
	WithoutVideo bool
}

// fakeDeviceServer is a from-scratch, in-process re-implementation of just
// enough of jetkvm/kvm's web.go + webrtc.go + jsonrpc.go + hidrpc.go to
// validate this client's full connect -> auth -> signal -> WebRTC ->
// video/RPC/HID pipeline without any real hardware. It is intentionally
// independent code (not a copy of the firmware) so that a bug shared
// between the fake and the real client wouldn't be masked - it exists to
// pin the documented wire behavior, not to be a mock of convenience.
type fakeDeviceServer struct {
	t    *testing.T
	opts fakeDeviceOptions
	srv  *httptest.Server

	authToken string
}

func startFakeDevice(t *testing.T, opts fakeDeviceOptions) *fakeDeviceServer {
	t.Helper()
	if opts.DeviceVersion == "" {
		opts.DeviceVersion = "0.4.7+dev"
	}
	if opts.DeviceID == "" {
		opts.DeviceID = "fake-device-id"
	}
	fd := &fakeDeviceServer{t: t, opts: opts}

	mux := http.NewServeMux()
	mux.HandleFunc("/device/status", fd.handleDeviceStatus)
	mux.HandleFunc("/auth/login-local", fd.handleLogin)
	mux.HandleFunc("/device", fd.handleDevice)
	mux.HandleFunc("/webrtc/signaling/client", fd.handleSignaling)

	fd.srv = httptest.NewServer(mux)
	t.Cleanup(fd.srv.Close)
	return fd
}

func (fd *fakeDeviceServer) baseURL() string {
	return "http" + strings.TrimPrefix(fd.srv.URL, "http")
}

func (fd *fakeDeviceServer) requireAuth(r *http.Request) bool {
	if fd.opts.Password == "" {
		return true
	}
	cookie, err := r.Cookie("authToken")
	return err == nil && fd.authToken != "" && cookie.Value == fd.authToken
}

func (fd *fakeDeviceServer) handleDeviceStatus(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(DeviceStatus{IsSetup: true})
}

func (fd *fakeDeviceServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if fd.opts.Password == "" || req.Password != fd.opts.Password {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid password"})
		return
	}
	fd.authToken = "fake-session-token"
	http.SetCookie(w, &http.Cookie{Name: "authToken", Value: fd.authToken, Path: "/"})
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "Login successful"})
}

func (fd *fakeDeviceServer) handleDevice(w http.ResponseWriter, r *http.Request) {
	if !fd.requireAuth(r) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}
	mode := "noPassword"
	if fd.opts.Password != "" {
		mode = "password"
	}
	_ = json.NewEncoder(w).Encode(LocalDevice{AuthMode: &mode, DeviceID: fd.opts.DeviceID})
}

func (fd *fakeDeviceServer) handleSignaling(w http.ResponseWriter, r *http.Request) {
	if !fd.requireAuth(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ctx := r.Context()

	if !fd.opts.SkipDeviceMetadata {
		meta, _ := json.Marshal(map[string]any{
			"type": "device-metadata",
			"data": map[string]string{"deviceVersion": fd.opts.DeviceVersion},
		})
		if err := conn.Write(ctx, websocket.MessageText, meta); err != nil {
			return
		}
	}

	var pc *webrtc.PeerConnection
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg signalingMessage
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

// handleOffer mimics webrtc.go's newSession + ExchangeOffer: build a peer
// connection, add an H.264 video track, wire up "rpc" and "hidrpc" data
// channel handlers, answer the offer, and start streaming the synthetic
// test fixture as the "video".
func (fd *fakeDeviceServer) handleOffer(ctx context.Context, conn *websocket.Conn, raw json.RawMessage) (*webrtc.PeerConnection, error) {
	var off offerData
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

	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "kvm")
	if err != nil {
		return nil, err
	}
	if _, err := pc.AddTrack(videoTrack); err != nil {
		return nil, err
	}

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case "rpc":
			dc.OnMessage(fd.rpcResponder(dc))
		case "hidrpc":
			dc.OnMessage(fd.hidResponder(dc))
		}
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, _ := json.Marshal(c.ToJSON())
		msg, _ := json.Marshal(signalingMessage{Type: "new-ice-candidate", Data: data})
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
	respMsg, _ := json.Marshal(signalingMessage{Type: "answer", Data: dataBytes})
	if err := conn.Write(ctx, websocket.MessageText, respMsg); err != nil {
		return nil, err
	}

	// WithoutVideo negotiates a video track but never sends a frame on it -
	// the "connected peer that produces no decodable picture" case, which is
	// exactly what a screenshot has to time out on rather than hang.
	if !fd.opts.WithoutVideo {
		go fd.streamSyntheticVideo(ctx, videoTrack)
	}

	return pc, nil
}

func (fd *fakeDeviceServer) rpcResponder(dc *webrtc.DataChannel) func(webrtc.DataChannelMessage) {
	return func(msg webrtc.DataChannelMessage) {
		var req rpcRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			return
		}
		var resp rpcResponse
		resp.JSONRPC = "2.0"
		resp.ID = json.Number(itoa(req.ID))
		switch req.Method {
		case "ping":
			if fd.opts.PingError != "" {
				resp.Error, _ = json.Marshal(fd.opts.PingError)
			} else {
				resp.Result = json.RawMessage(`"pong"`)
			}
		case "getLocalVersion":
			b, _ := json.Marshal(map[string]string{"appVersion": fd.opts.DeviceVersion})
			resp.Result = b
		default:
			resp.Result = json.RawMessage(`null`)
		}
		b, _ := json.Marshal(resp)
		_ = dc.SendText(string(b))
	}
}

func (fd *fakeDeviceServer) hidResponder(dc *webrtc.DataChannel) func(webrtc.DataChannelMessage) {
	return func(msg webrtc.DataChannelMessage) {
		m, err := hidproto.Unmarshal(msg.Data)
		if err != nil {
			return
		}
		if m.Type == hidproto.TypeHandshake {
			_ = dc.Send(msg.Data)
		}
	}
}

// streamSyntheticVideo repeatedly writes the same synthetic (non-live) IDR
// frame as RTP samples, simulating a low-framerate video source. One
// self-contained IDR is all frameCapture needs to consider a frame
// "decodable", so looping it is sufficient to test the full pipeline.
func (fd *fakeDeviceServer) streamSyntheticVideo(ctx context.Context, track *webrtc.TrackLocalStaticSample) {
	data := readSyntheticFixture(fd.t)
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

// readSyntheticFixture loads the same fixture used by video_test.go,
// resolved relative to this source file so it works regardless of the
// test binary's working directory.
func readSyntheticFixture(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test source file location")
	}
	path := filepath.Join(filepath.Dir(thisFile), "testdata", "synthetic_red_32x32.h264")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading synthetic fixture: %v", err)
	}
	return b
}
