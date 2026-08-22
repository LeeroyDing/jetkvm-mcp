// Package hidproto encodes and decodes the JetKVM HID-RPC wire format used
// on the "hidrpc" WebRTC data channel.
//
// This is not a published or versioned public API. It is reverse-engineered
// from the jetkvm/kvm firmware source (dev branch, commit b3c29a4, read
// 2026-08-02): internal/hidrpc/hidrpc.go, internal/hidrpc/message.go and
// hidrpc.go (session message dispatch) at the repository root. Every
// message layout below cites the evidence it was derived from so it can be
// re-verified against a newer firmware checkout.
//
// Wire shape: every message is `[1 type byte][payload]`, sent as a binary
// (non-string) WebRTC data channel message. There is no length prefix; the
// data channel message boundary is the frame boundary.
package hidproto

import (
	"encoding/binary"
	"fmt"
)

// MessageType identifies the payload layout of a HID-RPC message.
//
// Values and comments mirror internal/hidrpc/hidrpc.go's MessageType
// constants exactly, including the gap between 0x09 and 0x32.
type MessageType byte

const (
	TypeHandshake                 MessageType = 0x01
	TypeKeyboardReport            MessageType = 0x02
	TypePointerReport             MessageType = 0x03 // absolute mouse
	TypeWheelReport               MessageType = 0x04
	TypeKeypressReport            MessageType = 0x05
	TypeKeyboardMacroReport       MessageType = 0x07
	TypeMouseReport               MessageType = 0x06 // relative mouse
	TypeCancelKeyboardMacroReport MessageType = 0x08
	TypeKeypressKeepAliveReport   MessageType = 0x09
	TypeKeyboardLedState          MessageType = 0x32 // device -> client only
	TypeKeydownState              MessageType = 0x33 // device -> client only
	TypeKeyboardMacroState        MessageType = 0x34 // device -> client only
)

// ProtocolVersion is the HID-RPC handshake version this package speaks.
// Mirrors internal/hidrpc/hidrpc.go's `Version byte = 0x01`.
const ProtocolVersion byte = 0x01

// HIDKeyBufferSize is the number of simultaneous keys in a keyboard report,
// matching the USB boot-keyboard report and internal/hidrpc's
// HidKeyBufferSize constant.
const HIDKeyBufferSize = 6

// WheelReportUnsupported documents a firmware gap found during protocol
// research: the dev-branch server's handleHidRPCMessage switch (hidrpc.go)
// has no case for hidrpc.TypeWheelReport, so a wheel report sent on the
// hidrpc data channel falls through to "unknown HID RPC message type" and is
// dropped. Only the legacy "wheelReport" JSON-RPC method (jsonrpc.go) is
// wired up on this firmware. Callers that need scroll wheel input must use
// the JSON-RPC path, not this package's EncodeWheelReport.
const WheelReportUnsupported = true

// Message is a decoded HID-RPC frame.
type Message struct {
	Type    MessageType
	Payload []byte
}

// Marshal encodes m to its wire form: type byte followed by payload.
func Marshal(m Message) ([]byte, error) {
	if m.Type == 0 {
		return nil, fmt.Errorf("hidproto: invalid message type: %d", m.Type)
	}
	out := make([]byte, len(m.Payload)+1)
	out[0] = byte(m.Type)
	copy(out[1:], m.Payload)
	return out, nil
}

// Unmarshal decodes a wire frame into a Message.
func Unmarshal(data []byte) (Message, error) {
	if len(data) < 1 {
		return Message{}, fmt.Errorf("hidproto: invalid data length: %d", len(data))
	}
	return Message{Type: MessageType(data[0]), Payload: data[1:]}, nil
}

// EncodeHandshake builds the handshake message the client sends first on
// the hidrpc channel. The server echoes an identical message back and only
// then sets hidRPCAvailable = true (webrtc.go / hidrpc.go), so callers must
// wait for that echo before sending any other message type.
func EncodeHandshake() ([]byte, error) {
	return Marshal(Message{Type: TypeHandshake, Payload: []byte{ProtocolVersion}})
}

// EncodeKeyboardReport builds a full keyboard state report: modifier byte
// followed by up to HIDKeyBufferSize key codes. This mirrors
// hidrpc.NewKeyboardReportMessage, which does not pad or validate length -
// the caller is expected to pass a well-formed slice.
func EncodeKeyboardReport(modifier byte, keys []byte) ([]byte, error) {
	if len(keys) > HIDKeyBufferSize {
		return nil, fmt.Errorf("hidproto: keyboard report has %d keys, max %d", len(keys), HIDKeyBufferSize)
	}
	payload := make([]byte, 0, HIDKeyBufferSize+1)
	payload = append(payload, modifier)
	payload = append(payload, keys...)
	return Marshal(Message{Type: TypeKeyboardReport, Payload: payload})
}

// ReleaseAllKeyboardReport is the canonical "nothing is held down" keyboard
// report: modifier 0 and an all-zero key buffer. It matches
// keyboardClearStateKeys / rpcKeyboardReport(0, keyboardClearStateKeys) in
// jsonrpc.go and webrtc.go, which is what the firmware itself sends on
// disconnect, session handoff and last-session-disconnected.
func ReleaseAllKeyboardReport() ([]byte, error) {
	return EncodeKeyboardReport(0, make([]byte, HIDKeyBufferSize))
}

// ReleaseAllMouseReport is the canonical "no button held, no movement"
// mouse report. It is deliberately a *relative* mouse report (dx=0, dy=0)
// rather than an absolute PointerReport: an absolute report necessarily
// carries a coordinate, so using one to clear buttons would also warp the
// attached computer's cursor to that coordinate. A relative report with a
// zero delta clears the button mask without moving the pointer at all.
func ReleaseAllMouseReport() ([]byte, error) {
	return EncodeMouseReport(0, 0, 0)
}

// EncodeKeypressReport builds a single key press/release event: key code
// followed by a 1-byte press flag (1 = down, 0 = up).
func EncodeKeypressReport(key byte, press bool) ([]byte, error) {
	var p byte
	if press {
		p = 1
	}
	return Marshal(Message{Type: TypeKeypressReport, Payload: []byte{key, p}})
}

// EncodeKeypressKeepAlive builds the keepalive message a client must send
// periodically (~50ms, per the firmware's expectedRate constant in
// hidrpc.go) while a key from EncodeKeypressReport is held down, or the
// firmware's auto-release timer will release it.
func EncodeKeypressKeepAlive() ([]byte, error) {
	return Marshal(Message{Type: TypeKeypressKeepAliveReport, Payload: nil})
}

const (
	// MaxAbsoluteCoordinate is the maximum value for PointerReport X/Y,
	// matching the USB absolute-mouse HID report descriptor's
	// logical/physical maximum of 32767.
	MaxAbsoluteCoordinate = 32767

	// MaxAbsoluteButtonMask is the five data bits in the firmware's absolute
	// mouse report descriptor. The upper three bits are constant padding, not
	// additional buttons.
	MaxAbsoluteButtonMask = 1<<5 - 1

	// MaxRelativeMouseDelta is the inclusive magnitude supported for X/Y in
	// the firmware's signed relative-mouse HID report descriptor. Although
	// -128 fits in int8, the descriptor's logical minimum is -127.
	MaxRelativeMouseDelta = 127
)

// EncodePointerReport builds an absolute-mouse report: X and Y each as a
// big-endian int32 in [0, MaxAbsoluteCoordinate], followed by a button
// bitmask byte. Layout matches PointerReport() in internal/hidrpc/message.go
// (9-byte payload: 4+4+1).
func EncodePointerReport(x, y int32, buttons byte) ([]byte, error) {
	if x < 0 || x > MaxAbsoluteCoordinate {
		return nil, fmt.Errorf("hidproto: x %d out of range [0,%d]", x, MaxAbsoluteCoordinate)
	}
	if y < 0 || y > MaxAbsoluteCoordinate {
		return nil, fmt.Errorf("hidproto: y %d out of range [0,%d]", y, MaxAbsoluteCoordinate)
	}
	if buttons > MaxAbsoluteButtonMask {
		return nil, fmt.Errorf("hidproto: buttons %d out of range [0,%d]", buttons, MaxAbsoluteButtonMask)
	}
	payload := make([]byte, 9)
	binary.BigEndian.PutUint32(payload[0:4], uint32(x))
	binary.BigEndian.PutUint32(payload[4:8], uint32(y))
	payload[8] = buttons
	return Marshal(Message{Type: TypePointerReport, Payload: payload})
}

// EncodeMouseReport builds a relative-mouse report: dx, dy as signed bytes
// followed by a button bitmask byte, matching MouseReport() in
// internal/hidrpc/message.go (3-byte payload).
func EncodeMouseReport(dx, dy int8, buttons byte) ([]byte, error) {
	if dx < -MaxRelativeMouseDelta || dy < -MaxRelativeMouseDelta {
		return nil, fmt.Errorf(
			"hidproto: dx and dy must be in [%d,%d]",
			-MaxRelativeMouseDelta,
			MaxRelativeMouseDelta,
		)
	}
	return Marshal(Message{Type: TypeMouseReport, Payload: []byte{byte(dx), byte(dy), buttons}})
}

// EncodeCancelKeyboardMacro builds the macro-cancel message. It carries no
// payload (hidrpc.go's handleHidRPCMessage reads no fields off it).
func EncodeCancelKeyboardMacro() ([]byte, error) {
	return Marshal(Message{Type: TypeCancelKeyboardMacroReport, Payload: nil})
}

// KeydownState is the device's report of which keys/modifier are currently
// considered "held" from the firmware's point of view. Decoded from
// TypeKeydownState messages sent proactively by the device
// (reportHidRPCKeysDownState in hidrpc.go), used here to verify our
// release-all logic actually clears state rather than merely being called.
type KeydownState struct {
	Modifier byte
	Keys     []byte
}

// DecodeKeydownState parses a TypeKeydownState payload, matching
// hidrpc.NewKeydownStateMessage's layout (modifier byte + key bytes).
func DecodeKeydownState(m Message) (KeydownState, error) {
	if m.Type != TypeKeydownState {
		return KeydownState{}, fmt.Errorf("hidproto: not a keydown-state message: type %d", m.Type)
	}
	if len(m.Payload) < 1 {
		return KeydownState{}, fmt.Errorf("hidproto: keydown-state payload too short: %d bytes", len(m.Payload))
	}
	return KeydownState{Modifier: m.Payload[0], Keys: m.Payload[1:]}, nil
}

// IsReleaseAll reports whether a KeydownState represents "nothing held":
// zero modifier and an all-zero (or empty) key buffer.
func (s KeydownState) IsReleaseAll() bool {
	if s.Modifier != 0 {
		return false
	}
	for _, k := range s.Keys {
		if k != 0 {
			return false
		}
	}
	return true
}
