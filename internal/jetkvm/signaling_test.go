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
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// fakeDeviceSignalingServer emulates just enough of web.go's
// handleLocalWebRTCSignal / handleWebRTCSignalWsMessages to test our
// signaler against the documented message shapes without a real device.
func fakeDeviceSignalingServer(t *testing.T, deviceVersion string, onOffer func(sd string) (answerSD string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx := context.Background()

		if deviceVersion != "" {
			meta, _ := json.Marshal(map[string]any{
				"type": "device-metadata",
				"data": map[string]string{"deviceVersion": deviceVersion},
			})
			if err := conn.Write(ctx, websocket.MessageText, meta); err != nil {
				return
			}
		}

		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var msg signalingMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			if msg.Type == "offer" {
				var off offerData
				_ = json.Unmarshal(msg.Data, &off)
				raw, _ := base64.StdEncoding.DecodeString(off.Sd)
				var sdp webrtc.SessionDescription
				_ = json.Unmarshal(raw, &sdp)

				answerSD := onOffer(sdp.SDP)
				answerJSON, _ := json.Marshal(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSD})
				answerB64 := base64.StdEncoding.EncodeToString(answerJSON)
				dataBytes, _ := json.Marshal(answerB64)
				respMsg, _ := json.Marshal(signalingMessage{Type: "answer", Data: dataBytes})
				_ = conn.Write(ctx, websocket.MessageText, respMsg)
			}
		}
	}))
}

func TestDialSignalingReadsDeviceMetadata(t *testing.T) {
	srv := fakeDeviceSignalingServer(t, "0.4.7+dev", nil)
	defer srv.Close()

	baseURL := "http" + strings.TrimPrefix(srv.URL, "http")
	s, meta, err := dialSignaling(context.Background(), baseURL, nil)
	if err != nil {
		t.Fatalf("dialSignaling failed: %v", err)
	}
	defer s.close()

	if meta.DeviceVersion != "0.4.7+dev" {
		t.Errorf("deviceVersion = %q, want 0.4.7+dev", meta.DeviceVersion)
	}
}

func TestDialSignalingRejectsWrongFirstMessageType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		// Simulate a firmware that changed the handshake shape.
		msg, _ := json.Marshal(map[string]any{"type": "hello", "data": map[string]string{}})
		_ = conn.Write(context.Background(), websocket.MessageText, msg)
		_, _, _ = conn.Read(context.Background()) // block until the client closes
	}))
	defer srv.Close()

	baseURL := "http" + strings.TrimPrefix(srv.URL, "http")
	_, _, err := dialSignaling(context.Background(), baseURL, nil)
	if err == nil {
		t.Fatal("expected compatibility error for unexpected first message type")
	}
	var compatErr *CompatibilityError
	if !errors.As(err, &compatErr) {
		t.Fatalf("expected *CompatibilityError, got %T: %v", err, err)
	}
	if compatErr.Stage != "signaling-metadata" {
		t.Errorf("stage = %q, want signaling-metadata", compatErr.Stage)
	}
}

func TestDialSignalingRejectsMissingDeviceVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		msg, _ := json.Marshal(map[string]any{"type": "device-metadata", "data": map[string]string{}})
		_ = conn.Write(context.Background(), websocket.MessageText, msg)
		_, _, _ = conn.Read(context.Background()) // block until the client closes
	}))
	defer srv.Close()

	baseURL := "http" + strings.TrimPrefix(srv.URL, "http")
	_, _, err := dialSignaling(context.Background(), baseURL, nil)
	if err == nil {
		t.Fatal("expected compatibility error for empty deviceVersion")
	}
}

func TestSignalingOfferAnswerRoundTrip(t *testing.T) {
	const fakeOffer = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"
	const fakeAnswer = "v=0\r\no=- 2 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"

	var gotOffer string
	srv := fakeDeviceSignalingServer(t, "0.4.7+dev", func(sd string) string {
		gotOffer = sd
		return fakeAnswer
	})
	defer srv.Close()

	baseURL := "http" + strings.TrimPrefix(srv.URL, "http")
	s, _, err := dialSignaling(context.Background(), baseURL, nil)
	if err != nil {
		t.Fatalf("dialSignaling failed: %v", err)
	}
	defer s.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: fakeOffer}
	if err := s.sendOffer(ctx, offer); err != nil {
		t.Fatalf("sendOffer failed: %v", err)
	}

	ev, err := s.next(ctx)
	if err != nil {
		t.Fatalf("next failed: %v", err)
	}
	if ev.Answer == nil {
		t.Fatal("expected an answer event")
	}
	if ev.Answer.SDP != fakeAnswer {
		t.Errorf("answer SDP = %q, want %q", ev.Answer.SDP, fakeAnswer)
	}
	if gotOffer != fakeOffer {
		t.Errorf("server saw offer SDP = %q, want %q", gotOffer, fakeOffer)
	}
}

func TestSignalingTrickleICECandidate(t *testing.T) {
	received := make(chan webrtc.ICECandidateInit, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		meta, _ := json.Marshal(map[string]any{"type": "device-metadata", "data": map[string]string{"deviceVersion": "0.4.7+dev"}})
		_ = conn.Write(context.Background(), websocket.MessageText, meta)

		_, data, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		var msg signalingMessage
		_ = json.Unmarshal(data, &msg)
		if msg.Type == "new-ice-candidate" {
			var c webrtc.ICECandidateInit
			_ = json.Unmarshal(msg.Data, &c)
			received <- c
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	baseURL := "http" + strings.TrimPrefix(srv.URL, "http")
	s, _, err := dialSignaling(context.Background(), baseURL, nil)
	if err != nil {
		t.Fatalf("dialSignaling failed: %v", err)
	}
	defer s.close()

	candidate := webrtc.ICECandidateInit{Candidate: "candidate:1 1 UDP 2130706431 192.0.2.1 5000 typ host"}
	if err := s.sendICECandidate(context.Background(), candidate); err != nil {
		t.Fatalf("sendICECandidate failed: %v", err)
	}

	select {
	case got := <-received:
		if got.Candidate != candidate.Candidate {
			t.Errorf("candidate = %q, want %q", got.Candidate, candidate.Candidate)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for server to receive ICE candidate")
	}
}

// TestSignalingNextClassifiesUnknownTypesWithoutErroring pins the contract
// pumpSignalingEvents depends on: an error from next() means the transport
// is gone, and nothing else. A message type this client has no case for is
// data, not a failure.
func TestSignalingNextClassifiesUnknownTypesWithoutErroring(t *testing.T) {
	cases := []struct {
		name string
		// message is marshalled and sent, unless rawFrame is set - which is
		// the only way to express a frame that is not JSON at all.
		message       map[string]any
		rawFrame      string
		wantUnhandled string
		wantMalformed string
	}{
		{
			name:          "unknown type",
			message:       map[string]any{"type": "heartbeat", "data": map[string]any{"seq": 1}},
			wantUnhandled: "heartbeat",
		},
		{
			name:          "future error type",
			message:       map[string]any{"type": "error", "data": "something happened"},
			wantUnhandled: "error",
		},
		{
			name:          "missing type",
			message:       map[string]any{"data": "orphaned"},
			wantMalformed: malformedNoType,
		},
		{
			name:          "undecodable answer",
			message:       map[string]any{"type": "answer", "data": map[string]any{"not": "a base64 string"}},
			wantMalformed: "answer",
		},
		{
			name:          "undecodable ice candidate",
			message:       map[string]any{"type": "new-ice-candidate", "data": "not-an-object"},
			wantMalformed: "new-ice-candidate",
		},
		{
			// The envelope itself failing to parse is the same situation as
			// an unknown type - the transport is fine - so it must not be
			// reported as an error either.
			name:          "frame that is not json",
			rawFrame:      "this is not json",
			wantMalformed: malformedEnvelope,
		},
		{
			name:          "empty frame",
			rawFrame:      "",
			wantMalformed: malformedEnvelope,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := dialTestSignaler(t, func(ctx context.Context, conn *websocket.Conn) {
				payload := []byte(tc.rawFrame)
				if tc.message != nil {
					payload, _ = json.Marshal(tc.message)
				}
				_ = conn.Write(ctx, websocket.MessageText, payload)
			})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			ev, err := s.next(ctx)
			if err != nil {
				t.Fatalf("next() returned an error for a non-transport condition: %v", err)
			}
			if ev.Unhandled != tc.wantUnhandled {
				t.Errorf("Unhandled = %q, want %q", ev.Unhandled, tc.wantUnhandled)
			}
			if ev.Malformed != tc.wantMalformed {
				t.Errorf("Malformed = %q, want %q", ev.Malformed, tc.wantMalformed)
			}
			if ev.Answer != nil || ev.Candidate != nil {
				t.Error("an unhandled/malformed message must not produce an answer or candidate")
			}
		})
	}
}

// TestSignalingNextReportsTransportFailure confirms the other half of the
// contract: a genuinely dead connection still surfaces as an error, so the
// pump can stop.
func TestSignalingNextReportsTransportFailure(t *testing.T) {
	s := dialTestSignaler(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := s.next(ctx); err == nil {
		t.Fatal("expected an error once the signaling transport closed")
	}
}

// dialTestSignaler starts a websocket server that sends the device-metadata
// message and then runs handle, and returns a signaler connected to it.
func dialTestSignaler(t *testing.T, handle func(ctx context.Context, conn *websocket.Conn)) *signaler {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		meta, _ := json.Marshal(map[string]any{
			"type": "device-metadata",
			"data": map[string]string{"deviceVersion": "0.5.8"},
		})
		if err := conn.Write(r.Context(), websocket.MessageText, meta); err != nil {
			return
		}
		handle(r.Context(), conn)
		// Hold the connection open until the client disconnects. Reading
		// returns as soon as that happens, so the test never waits on a
		// timeout it does not need.
		_, _, _ = conn.Read(r.Context())
	}))
	t.Cleanup(srv.Close)

	baseURL := "http" + strings.TrimPrefix(srv.URL, "http")
	s, _, err := dialSignaling(context.Background(), baseURL, nil)
	if err != nil {
		t.Fatalf("dialSignaling failed: %v", err)
	}
	t.Cleanup(func() { _ = s.close() })
	return s
}
