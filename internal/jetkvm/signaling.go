package jetkvm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// signalingMessage is the envelope used on the local signaling websocket in
// both directions. Evidence: web.go's handleWebRTCSignalWsMessages, which
// reads `{"type":..., "data":...}` and switches on Type for "offer" /
// "new-ice-candidate", and cloud.go's handleSessionRequest, which writes
// `{"type":"answer","data":sd}` and `{"type":"new-ice-candidate","data":candidate}`.
type signalingMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// offerData is the payload of an outbound "offer" message. Evidence:
// web.go's WebRTCSessionRequest struct (Sd/OidcGoogle/IP/ICEServers json
// tags), consumed by cloud.go's handleSessionRequest -> Session.ExchangeOffer.
type offerData struct {
	Sd         string   `json:"sd"`
	OidcGoogle string   `json:"OidcGoogle,omitempty"`
	IP         string   `json:"ip,omitempty"`
	ICEServers []string `json:"iceServers,omitempty"`
}

// signaler owns the local signaling websocket connection: GET
// /webrtc/signaling/client, upgraded to a websocket by the device (web.go's
// handleLocalWebRTCSignal). It is intentionally minimal - it only ferries
// JSON envelopes and knows nothing about WebRTC internals, so it can be
// tested against a fake HTTP/websocket server without any real SDP.
type signaler struct {
	conn *websocket.Conn
}

// dialSignaling opens the signaling websocket against baseURL, using the
// same cookie jar as httpClient so the auth session carries over exactly
// like a browser tab reusing its login. It reads and validates the very
// first message as device-metadata (see compat.go) before returning, so
// callers get an actionable compatibility error immediately rather than a
// timeout deep in offer/answer exchange.
func dialSignaling(ctx context.Context, baseURL string, cookies []*http.Cookie) (*signaler, DeviceMetadata, error) {
	wsURL, err := toWebsocketURL(baseURL, "/webrtc/signaling/client")
	if err != nil {
		return nil, DeviceMetadata{}, err
	}

	header := http.Header{}
	for _, c := range cookies {
		header.Add("Cookie", c.Name+"="+c.Value)
	}

	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: header,
	})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return nil, DeviceMetadata{}, newDeviceError(ErrorKindAuthFailed, "dialing signaling websocket", err)
		}
		kind := ErrorKindUnreachable
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			kind = ErrorKindTimeout
		}
		return nil, DeviceMetadata{}, newDeviceError(kind, "dialing signaling websocket", err)
	}

	s := &signaler{conn: conn}

	var first signalingMessage
	if err := s.readInto(ctx, &first); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "")
		return nil, DeviceMetadata{}, err
	}

	meta, err := checkDeviceMetadata(first.Type, first.Data)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "")
		return nil, DeviceMetadata{}, err
	}

	return s, meta, nil
}

func toWebsocketURL(baseURL, path string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("jetkvm: invalid base URL: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("jetkvm: unsupported base URL scheme %q", u.Scheme)
	}
	ref, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("jetkvm: invalid path %q: %w", path, err)
	}
	return u.ResolveReference(ref).String(), nil
}

// readFrame returns one raw signaling frame. An error from it always means
// the transport failed - it never reports anything about the frame's
// contents, which is what lets next() tell those two apart.
func (s *signaler) readFrame(ctx context.Context) ([]byte, error) {
	_, data, err := s.conn.Read(ctx)
	return data, err
}

func (s *signaler) readInto(ctx context.Context, v *signalingMessage) error {
	data, err := s.readFrame(ctx)
	if err != nil {
		kind := ErrorKindUnreachable
		switch {
		case errors.Is(err, websocket.ErrMessageTooBig) || websocket.CloseStatus(err) == websocket.StatusMessageTooBig:
			kind = ErrorKindBadFrame
		case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
			kind = ErrorKindTimeout
		}
		return newDeviceError(kind, "reading initial signaling frame", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return newDeviceError(ErrorKindBadFrame, "decoding initial signaling frame", err)
	}
	return nil
}

// sendOffer base64-encodes and sends a local WebRTC SDP offer, matching the
// browser client's `{"type":"offer","data":{"sd": base64(json(offer))}}`.
func (s *signaler) sendOffer(ctx context.Context, offer webrtc.SessionDescription) error {
	sdJSON, err := json.Marshal(offer)
	if err != nil {
		return fmt.Errorf("jetkvm: encoding SDP offer: %w", err)
	}
	msg := signalingMessage{Type: "offer"}
	msg.Data, err = json.Marshal(offerData{Sd: base64.StdEncoding.EncodeToString(sdJSON)})
	if err != nil {
		return fmt.Errorf("jetkvm: encoding offer envelope: %w", err)
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("jetkvm: encoding signaling message: %w", err)
	}
	if err := s.conn.Write(ctx, websocket.MessageText, b); err != nil {
		kind := ErrorKindUnreachable
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			kind = ErrorKindTimeout
		}
		return newDeviceError(kind, "sending signaling offer", err)
	}
	return nil
}

// sendICECandidate trickles a local ICE candidate to the device, matching
// `{"type":"new-ice-candidate","data":candidate.ToJSON()}`.
func (s *signaler) sendICECandidate(ctx context.Context, c webrtc.ICECandidateInit) error {
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("jetkvm: encoding ICE candidate: %w", err)
	}
	msg := signalingMessage{Type: "new-ice-candidate", Data: data}
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("jetkvm: encoding signaling message: %w", err)
	}
	if err := s.conn.Write(ctx, websocket.MessageText, b); err != nil {
		kind := ErrorKindUnreachable
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			kind = ErrorKindTimeout
		}
		return newDeviceError(kind, "sending ICE candidate", err)
	}
	return nil
}

// signalingEvent is one classified signaling message.
//
// Exactly one field is ever set. Unhandled and Malformed are *not* errors:
// an error from next() means the signaling transport itself failed and the
// session is over, so callers are entitled to stop reading on one. A
// message this client has no case for, or one that fails to decode, must
// not have that effect - the device is free to add informational message
// types, and dropping the connection over one would silently stop ICE
// trickling for the rest of the session.
type signalingEvent struct {
	Answer    *webrtc.SessionDescription
	Candidate *webrtc.ICECandidateInit

	// Unhandled names the type of a message this client does not act on.
	Unhandled string
	// Malformed names the type of a message that failed to decode. The
	// payload is deliberately not retained: it can carry session material.
	Malformed string
}

// Placeholders used where a malformed message has no usable type name to
// report. They are fixed strings, never anything the device supplied.
const (
	malformedNoType   = "(no type field)"
	malformedEnvelope = "(unparseable envelope)"
)

// next blocks for the next signaling message and classifies it. It returns
// an error only when the underlying transport fails.
func (s *signaler) next(ctx context.Context) (signalingEvent, error) {
	data, err := s.readFrame(ctx)
	if err != nil {
		return signalingEvent{}, err
	}

	// A frame that isn't valid JSON is classified, not returned as an
	// error, for the same reason an unknown message type is: the connection
	// is healthy, so ending the pump over it would stop ICE trickling for
	// the rest of the session. The frame's bytes are deliberately dropped
	// rather than reported - they are device-supplied and unparsed.
	var msg signalingMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return signalingEvent{Malformed: malformedEnvelope}, nil
	}

	switch msg.Type {
	case "answer":
		var sdB64 string
		if err := json.Unmarshal(msg.Data, &sdB64); err != nil {
			return signalingEvent{Malformed: "answer"}, nil
		}
		raw, err := base64.StdEncoding.DecodeString(sdB64)
		if err != nil {
			return signalingEvent{Malformed: "answer"}, nil
		}
		var answer webrtc.SessionDescription
		if err := json.Unmarshal(raw, &answer); err != nil {
			return signalingEvent{Malformed: "answer"}, nil
		}
		return signalingEvent{Answer: &answer}, nil
	case "new-ice-candidate":
		var c webrtc.ICECandidateInit
		if err := json.Unmarshal(msg.Data, &c); err != nil {
			return signalingEvent{Malformed: "new-ice-candidate"}, nil
		}
		return signalingEvent{Candidate: &c}, nil
	case "":
		return signalingEvent{Malformed: malformedNoType}, nil
	default:
		return signalingEvent{Unhandled: msg.Type}, nil
	}
}

func (s *signaler) close() error {
	return s.conn.Close(websocket.StatusNormalClosure, "")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "...(truncated)"
}
