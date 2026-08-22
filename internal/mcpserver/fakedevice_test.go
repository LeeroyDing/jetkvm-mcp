package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
// used to exercise MCP tool registration, reliability policy, and argument
// handling end to end. The wire-format correctness (HID framing, signaling
// message shapes, video depacketization) is already pinned by internal/jetkvm's
// own tests; this independent implementation keeps this package's tests from
// reaching into another package's unexported test helpers.

type signalingMsg struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type offerMsg struct {
	Sd string `json:"sd"`
}

type fakeRPCResponseMode string

const (
	fakeRPCNormal    fakeRPCResponseMode = ""
	fakeRPCTruncated fakeRPCResponseMode = "truncated"
	fakeRPCOversized fakeRPCResponseMode = "oversized"
)

type fakeDeviceOptions struct {
	// Password enables the same local-password flow as the device. Empty is
	// noPassword mode.
	Password string
	// DeviceStatusFailures returns HTTP 503 for the first N public status
	// probes, allowing the real Connect path to recover on a later attempt.
	DeviceStatusFailures int
	DeviceStatusDelay    time.Duration
	RPCResponseDelay     time.Duration
	RPCResponseMode      fakeRPCResponseMode
	// RPCDisconnects closes the data channel for the first N ping requests,
	// exercising replacement of an already-established but dead session.
	RPCDisconnects int
	// CaptureWire retains raw RPC frames and WebRTC text/binary metadata for
	// exact transport assertions. It is opt-in so tests that only exercise
	// reliability can never backpressure an unconsumed capture queue.
	CaptureWire bool
}

// These two states deliberately mirror the pinned firmware's distinct USB
// gadget files. TypePointerReport updates /dev/hidg1, while TypeMouseReport
// updates /dev/hidg2; a neutral report on one interface must never clear the
// other interface's button mask.
type fakeAbsoluteMouseState struct {
	X, Y    int32
	Buttons byte
	Reports int
}

type fakeRelativeMouseState struct {
	Buttons byte
	Reports int
}

type fakeDevice struct {
	srv       *httptest.Server
	t         *testing.T
	opts      fakeDeviceOptions
	hidFrames chan []byte
	rpcFrames chan []byte

	mu                   sync.Mutex
	authToken            string
	deviceStatusRequests int
	loginRequests        int
	signalingConnections int
	rpcRequests          int
	hidRequests          int
	rpcIsString          []bool
	hidIsString          []bool
	hidg1                fakeAbsoluteMouseState
	hidg2                fakeRelativeMouseState
}

func startFakeDevice(t *testing.T) *fakeDevice {
	return startFakeDeviceWithOptions(t, fakeDeviceOptions{})
}

func startFakeDeviceWithOptions(t *testing.T, opts fakeDeviceOptions) *fakeDevice {
	t.Helper()
	fd := &fakeDevice{
		t:         t,
		opts:      opts,
		hidFrames: make(chan []byte, 32),
	}
	if opts.CaptureWire {
		fd.rpcFrames = make(chan []byte, 32)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/device/status", fd.handleDeviceStatus)
	mux.HandleFunc("/auth/login-local", fd.handleLogin)
	mux.HandleFunc("/device", fd.handleDevice)
	mux.HandleFunc("/webrtc/signaling/client", fd.handleSignaling)
	fd.srv = httptest.NewServer(mux)
	t.Cleanup(fd.srv.Close)
	return fd
}

func (fd *fakeDevice) countDeviceStatus() int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	fd.deviceStatusRequests++
	return fd.deviceStatusRequests
}

func (fd *fakeDevice) countLogin() int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	fd.loginRequests++
	return fd.loginRequests
}

func (fd *fakeDevice) countSignalingConnection() int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	fd.signalingConnections++
	return fd.signalingConnections
}

func (fd *fakeDevice) countRPC() int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	fd.rpcRequests++
	return fd.rpcRequests
}

func (fd *fakeDevice) recordHID(m hidproto.Message, isString bool) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	fd.hidRequests++
	if fd.opts.CaptureWire {
		fd.hidIsString = append(fd.hidIsString, isString)
	}
	switch m.Type {
	case hidproto.TypePointerReport:
		if len(m.Payload) == 9 {
			fd.hidg1.X = int32(binary.BigEndian.Uint32(m.Payload[0:4]))
			fd.hidg1.Y = int32(binary.BigEndian.Uint32(m.Payload[4:8]))
			fd.hidg1.Buttons = m.Payload[8]
			fd.hidg1.Reports++
		}
	case hidproto.TypeMouseReport:
		if len(m.Payload) == 3 {
			fd.hidg2.Buttons = m.Payload[2]
			fd.hidg2.Reports++
		}
	}
}

func (fd *fakeDevice) mouseInterfaceState() (fakeAbsoluteMouseState, fakeRelativeMouseState) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.hidg1, fd.hidg2
}

func (fd *fakeDevice) wireCounts() (rpc, hid int) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.rpcRequests, fd.hidRequests
}

func (fd *fakeDevice) captureRPC(frame []byte, isString bool) {
	if !fd.opts.CaptureWire {
		return
	}
	fd.mu.Lock()
	fd.rpcIsString = append(fd.rpcIsString, isString)
	fd.mu.Unlock()
	fd.rpcFrames <- append([]byte(nil), frame...)
}

func (fd *fakeDevice) counts() (status, login, signaling, rpc int) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.deviceStatusRequests, fd.loginRequests, fd.signalingConnections, fd.rpcRequests
}

func (fd *fakeDevice) requireAuth(r *http.Request) bool {
	if fd.opts.Password == "" {
		return true
	}
	fd.mu.Lock()
	token := fd.authToken
	fd.mu.Unlock()
	cookie, err := r.Cookie("authToken")
	return err == nil && token != "" && cookie.Value == token
}

func waitForRequest(r *http.Request, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

func (fd *fakeDevice) handleDeviceStatus(w http.ResponseWriter, r *http.Request) {
	attempt := fd.countDeviceStatus()
	if !waitForRequest(r, fd.opts.DeviceStatusDelay) {
		return
	}
	if attempt <= fd.opts.DeviceStatusFailures {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "temporarily unavailable"})
		return
	}
	_ = json.NewEncoder(w).Encode(jetkvm.DeviceStatus{IsSetup: true})
}

func (fd *fakeDevice) handleLogin(w http.ResponseWriter, r *http.Request) {
	fd.countLogin()
	var request struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	if fd.opts.Password == "" || request.Password != fd.opts.Password {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid password"})
		return
	}

	fd.mu.Lock()
	fd.authToken = "fake-session-token"
	token := fd.authToken
	fd.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "authToken", Value: token, Path: "/"})
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "Login successful"})
}

func (fd *fakeDevice) handleDevice(w http.ResponseWriter, r *http.Request) {
	if !fd.requireAuth(r) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}
	mode := "noPassword"
	if fd.opts.Password != "" {
		mode = "password"
	}
	_ = json.NewEncoder(w).Encode(jetkvm.LocalDevice{AuthMode: &mode, DeviceID: "fake-device"})
}

func (fd *fakeDevice) baseURL() string {
	return "http" + strings.TrimPrefix(fd.srv.URL, "http")
}

func (fd *fakeDevice) handleSignaling(w http.ResponseWriter, r *http.Request) {
	if !fd.requireAuth(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	fd.countSignalingConnection()
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
	defer func() {
		if pc != nil {
			_ = pc.Close()
		}
	}()
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
	// Loopback-only ICE, mirroring internal/jetkvm's fake device: this rig
	// lives on 127.0.0.1, and non-loopback candidates only slow (or on
	// starved CI runners, break) connectivity checks.
	se := webrtc.SettingEngine{}
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	// pion skips loopback when gathering host candidates unless asked;
	// without it the filter above leaves zero candidates.
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithSettingEngine(se))
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
				fd.captureRPC(msg.Data, msg.IsString)
				var req struct {
					Method string `json:"method"`
					ID     int64  `json:"id"`
				}
				if err := json.Unmarshal(msg.Data, &req); err != nil {
					return
				}
				requestNumber := fd.countRPC()
				if req.Method == "ping" && requestNumber <= fd.opts.RPCDisconnects {
					_ = dc.Close()
					return
				}
				if fd.opts.RPCResponseDelay > 0 {
					time.Sleep(fd.opts.RPCResponseDelay)
				}
				if req.Method == "ping" {
					switch fd.opts.RPCResponseMode {
					case fakeRPCTruncated:
						_ = dc.SendText(`{"jsonrpc":"2.0","result":`)
						return
					case fakeRPCOversized:
						const oversizedPayloadBytes = 70 << 10
						frame := fmt.Sprintf(`{"jsonrpc":"2.0","result":"%s","id":%d}`,
							strings.Repeat("x", oversizedPayloadBytes), req.ID)
						_ = dc.SendText(frame)
						return
					}
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
					return
				}
				if err == nil {
					fd.recordHID(m, msg.IsString)
					fd.hidFrames <- append([]byte(nil), msg.Data...)
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

func (fd *fakeDevice) nextHIDFrame(t *testing.T) []byte {
	t.Helper()
	select {
	case frame := <-fd.hidFrames:
		return frame
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HID frame")
		return nil
	}
}

func (fd *fakeDevice) nextRPCFrame(t *testing.T) []byte {
	t.Helper()
	if fd.rpcFrames == nil {
		t.Fatal("RPC wire capture was not enabled")
	}
	select {
	case frame := <-fd.rpcFrames:
		return frame
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RPC frame")
		return nil
	}
}

func (fd *fakeDevice) nextHIDWireFrame(t *testing.T) ([]byte, bool) {
	t.Helper()
	frame := fd.nextHIDFrame(t)
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.hidIsString) == 0 {
		t.Fatal("HID wire metadata was not captured")
	}
	isString := fd.hidIsString[0]
	fd.hidIsString = fd.hidIsString[1:]
	return frame, isString
}

func (fd *fakeDevice) nextRPCWireFrame(t *testing.T) ([]byte, bool) {
	t.Helper()
	frame := fd.nextRPCFrame(t)
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.rpcIsString) == 0 {
		t.Fatal("RPC wire metadata was not captured")
	}
	isString := fd.rpcIsString[0]
	fd.rpcIsString = fd.rpcIsString[1:]
	return frame, isString
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
