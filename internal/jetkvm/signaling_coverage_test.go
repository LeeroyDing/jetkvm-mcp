package jetkvm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

func TestToWebsocketURLBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		path     string
		want     string
		wantText string
	}{
		{
			name:    "http becomes ws",
			baseURL: "http://jetkvm.local/base",
			path:    "/webrtc/signaling/client",
			want:    "ws://jetkvm.local/webrtc/signaling/client",
		},
		{
			name:    "https becomes wss",
			baseURL: "https://jetkvm.local:8443",
			path:    "/webrtc/signaling/client",
			want:    "wss://jetkvm.local:8443/webrtc/signaling/client",
		},
		{
			name:     "malformed base URL",
			baseURL:  "%",
			path:     "/webrtc/signaling/client",
			wantText: "invalid base URL",
		},
		{
			name:     "unsupported base URL scheme",
			baseURL:  "ftp://jetkvm.local",
			path:     "/webrtc/signaling/client",
			wantText: "unsupported base URL scheme",
		},
		{
			name:     "malformed signaling path",
			baseURL:  "http://jetkvm.local",
			path:     "%",
			wantText: "invalid path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := toWebsocketURL(tt.baseURL, tt.path)
			if tt.wantText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantText) {
					t.Fatalf("toWebsocketURL(%q, %q) = %q, %v; want error containing %q", tt.baseURL, tt.path, got, err, tt.wantText)
				}
				return
			}
			if err != nil {
				t.Fatalf("toWebsocketURL(%q, %q): %v", tt.baseURL, tt.path, err)
			}
			if got != tt.want {
				t.Fatalf("toWebsocketURL(%q, %q) = %q, want %q", tt.baseURL, tt.path, got, tt.want)
			}
		})
	}
}

func TestDeviceMetadataCompatibilityBoundaries(t *testing.T) {
	valid, err := json.Marshal(DeviceMetadata{DeviceVersion: "0.5.8"})
	if err != nil {
		t.Fatalf("marshal valid metadata: %v", err)
	}

	tests := []struct {
		name        string
		messageType string
		data        []byte
		wantVersion string
		wantDetail  string
	}{
		{
			name:        "valid metadata",
			messageType: "device-metadata",
			data:        valid,
			wantVersion: "0.5.8",
		},
		{
			name:        "wrong first message type",
			messageType: "answer",
			data:        valid,
			wantDetail:  "expected the first signaling message",
		},
		{
			name:        "malformed metadata JSON",
			messageType: "device-metadata",
			data:        []byte(`{"deviceVersion":`),
			wantDetail:  "did not decode as expected",
		},
		{
			name:        "empty device version",
			messageType: "device-metadata",
			data:        []byte(`{"deviceVersion":""}`),
			wantDetail:  "empty deviceVersion",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, err := checkDeviceMetadata(tt.messageType, tt.data)
			if tt.wantDetail == "" {
				if err != nil {
					t.Fatalf("checkDeviceMetadata: %v", err)
				}
				if meta.DeviceVersion != tt.wantVersion {
					t.Fatalf("DeviceVersion = %q, want %q", meta.DeviceVersion, tt.wantVersion)
				}
				return
			}

			var compatibilityErr *CompatibilityError
			if !errors.As(err, &compatibilityErr) {
				t.Fatalf("checkDeviceMetadata error = %T %v, want *CompatibilityError", err, err)
			}
			if compatibilityErr.Stage != "signaling-metadata" || !strings.Contains(compatibilityErr.Detail, tt.wantDetail) {
				t.Fatalf("compatibility error = %+v, want signaling-metadata detail containing %q", compatibilityErr, tt.wantDetail)
			}
			text := compatibilityErr.Error()
			for _, want := range []string{tt.wantDetail, EvidenceBranch, EvidenceCommit} {
				if !strings.Contains(text, want) {
					t.Errorf("CompatibilityError.Error() = %q, want %q", text, want)
				}
			}
		})
	}
}

func TestDialSignalingClassifiesInitialFailures(t *testing.T) {
	websocketFrame := func(write func(context.Context, *websocket.Conn)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			write(r.Context(), conn)
		}
	}

	tests := []struct {
		name        string
		handler     http.HandlerFunc
		cancelFirst bool
		wantKind    ErrorKind
	}{
		{
			name: "unauthorized upgrade",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			},
			wantKind: ErrorKindAuthFailed,
		},
		{
			name: "forbidden upgrade",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "forbidden", http.StatusForbidden)
			},
			wantKind: ErrorKindAuthFailed,
		},
		{
			name: "server rejects upgrade",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			},
			wantKind: ErrorKindUnreachable,
		},
		{
			name: "dial context already canceled",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			},
			cancelFirst: true,
			wantKind:    ErrorKindTimeout,
		},
		{
			name: "initial frame is malformed JSON",
			handler: websocketFrame(func(ctx context.Context, conn *websocket.Conn) {
				_ = conn.Write(ctx, websocket.MessageText, []byte("not-json"))
				_ = conn.Close(websocket.StatusNormalClosure, "")
			}),
			wantKind: ErrorKindBadFrame,
		},
		{
			name: "initial frame exceeds peer limit",
			handler: websocketFrame(func(_ context.Context, conn *websocket.Conn) {
				_ = conn.Close(websocket.StatusMessageTooBig, "")
			}),
			wantKind: ErrorKindBadFrame,
		},
		{
			name: "connection closes before metadata",
			handler: websocketFrame(func(_ context.Context, conn *websocket.Conn) {
				_ = conn.Close(websocket.StatusNormalClosure, "")
			}),
			wantKind: ErrorKindUnreachable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			ctx := context.Background()
			if tt.cancelFirst {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			s, _, err := dialSignaling(ctx, srv.URL, nil)
			if s != nil {
				_ = s.close()
			}
			if err == nil {
				t.Fatal("dialSignaling unexpectedly succeeded")
			}
			if got := ErrorKindOf(err); got != tt.wantKind {
				t.Fatalf("ErrorKindOf(%v) = %q, want %q", err, got, tt.wantKind)
			}
		})
	}
}

func TestSignalerWriteFailuresAreClassified(t *testing.T) {
	tests := []struct {
		name        string
		closeFirst  bool
		cancelFirst bool
		send        func(*signaler, context.Context) error
		wantKind    ErrorKind
	}{
		{
			name:       "offer on closed connection",
			closeFirst: true,
			wantKind:   ErrorKindUnreachable,
			send: func(s *signaler, ctx context.Context) error {
				return s.sendOffer(ctx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\n"})
			},
		},
		{
			name:        "offer with canceled context",
			closeFirst:  true,
			cancelFirst: true,
			wantKind:    ErrorKindTimeout,
			send: func(s *signaler, ctx context.Context) error {
				return s.sendOffer(ctx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\n"})
			},
		},
		{
			name:       "candidate on closed connection",
			closeFirst: true,
			wantKind:   ErrorKindUnreachable,
			send: func(s *signaler, ctx context.Context) error {
				return s.sendICECandidate(ctx, webrtc.ICECandidateInit{Candidate: "candidate:test"})
			},
		},
		{
			name:        "candidate with canceled context",
			closeFirst:  true,
			cancelFirst: true,
			wantKind:    ErrorKindTimeout,
			send: func(s *signaler, ctx context.Context) error {
				return s.sendICECandidate(ctx, webrtc.ICECandidateInit{Candidate: "candidate:test"})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := dialTestSignaler(t, func(context.Context, *websocket.Conn) {})
			if tt.closeFirst {
				_ = s.close()
			}
			ctx := context.Background()
			if tt.cancelFirst {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			err := tt.send(s, ctx)
			if err == nil {
				t.Fatal("signaling write unexpectedly succeeded")
			}
			if got := ErrorKindOf(err); got != tt.wantKind {
				t.Fatalf("ErrorKindOf(%v) = %q, want %q", err, got, tt.wantKind)
			}
		})
	}
}

func TestSignalingNextRejectsMalformedAnswerEncodings(t *testing.T) {
	tests := []struct {
		name string
		data any
	}{
		{name: "invalid base64", data: "%%%"},
		{name: "base64 containing invalid SDP JSON", data: base64.StdEncoding.EncodeToString([]byte("not-json"))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := dialTestSignaler(t, func(ctx context.Context, conn *websocket.Conn) {
				frame, err := json.Marshal(map[string]any{"type": "answer", "data": tt.data})
				if err != nil {
					t.Errorf("marshal test frame: %v", err)
					return
				}
				_ = conn.Write(ctx, websocket.MessageText, frame)
			})

			event, err := s.next(context.Background())
			if err != nil {
				t.Fatalf("next returned transport error for malformed answer: %v", err)
			}
			if event.Malformed != "answer" || event.Answer != nil || event.Candidate != nil || event.Unhandled != "" {
				t.Fatalf("malformed answer event = %+v", event)
			}
		})
	}
}
