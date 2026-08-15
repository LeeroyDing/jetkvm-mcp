package hidproto

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// FuzzMessageRoundTrip pins the wire format's decode/encode inverse: any
// frame that Unmarshal accepts re-encodes byte-identically via Marshal
// (except the reserved type 0, which Marshal must reject), and
// DecodeKeydownState accepts exactly well-typed, non-empty keydown-state
// payloads. Under plain `go test ./...` only the seeds run, so this is
// CI-safe.
func FuzzMessageRoundTrip(f *testing.F) {
	handshake, _ := EncodeHandshake()
	releaseKeyboard, _ := ReleaseAllKeyboardReport()
	releaseMouse, _ := ReleaseAllMouseReport()
	pointer, _ := EncodePointerReport(32767, 0, 0x1)
	keypress, _ := EncodeKeypressReport(0x04, true)
	keepalive, _ := EncodeKeypressKeepAlive()
	for _, seed := range [][]byte{
		handshake,
		releaseKeyboard,
		releaseMouse,
		pointer,
		keypress,
		keepalive,
		{byte(TypeKeydownState), 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // release-all keydown state
		{byte(TypeKeydownState), 0x02, 0x04},                               // shift+A held
		{byte(TypeKeydownState)},                                           // too short
		{byte(TypeKeyboardLedState), 0x01},
		{0x00},       // reserved type
		{0x00, 0x01}, // reserved type with payload
		{},           // empty frame
		{0xff, 0xfe, 0xfd},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := Unmarshal(data)
		if err != nil {
			if len(data) >= 1 {
				t.Fatalf("Unmarshal rejected a non-empty frame: %v", err)
			}
			return
		}
		if len(data) < 1 {
			t.Fatal("Unmarshal accepted an empty frame")
		}
		if m.Type != MessageType(data[0]) || !bytes.Equal(m.Payload, data[1:]) {
			t.Fatalf("Unmarshal(%x) = %+v, does not mirror input", data, m)
		}

		re, err := Marshal(m)
		if m.Type == 0 {
			if err == nil {
				t.Fatal("Marshal accepted reserved type 0")
			}
		} else {
			if err != nil {
				t.Fatalf("Marshal rejected a decoded message: %v", err)
			}
			if !bytes.Equal(re, data) {
				t.Fatalf("round trip %x -> %x is not identity", data, re)
			}
		}

		state, err := DecodeKeydownState(m)
		if m.Type == TypeKeydownState && len(m.Payload) >= 1 {
			if err != nil {
				t.Fatalf("DecodeKeydownState rejected a well-typed payload: %v", err)
			}
			if state.Modifier != m.Payload[0] || !bytes.Equal(state.Keys, m.Payload[1:]) {
				t.Fatalf("DecodeKeydownState(%x) = %+v, does not mirror payload", data, state)
			}
			_ = state.IsReleaseAll()
		} else if err == nil {
			t.Fatalf("DecodeKeydownState accepted type %d payload %x", m.Type, m.Payload)
		}
	})
}

// FuzzEncodeDecode pins each encoder's validation boundary and its decode
// inverse: encoders accept exactly the documented domains, and every
// successfully encoded report decodes back to the values passed in.
func FuzzEncodeDecode(f *testing.F) {
	f.Add(byte(0), []byte{}, int32(0), int32(0), int8(0), int8(0), byte(0), byte(0), false)
	f.Add(byte(0x02), []byte{0x04, 0x05}, int32(100), int32(200), int8(-5), int8(5), byte(0x1), byte(0x28), true)
	f.Add(byte(0xff), []byte{1, 2, 3, 4, 5, 6}, int32(32767), int32(32767), int8(-128), int8(127), byte(0xff), byte(0xe0), false)
	f.Add(byte(1), []byte{1, 2, 3, 4, 5, 6, 7}, int32(-1), int32(32768), int8(1), int8(-1), byte(2), byte(0), true)

	f.Fuzz(func(t *testing.T, modifier byte, keys []byte, x, y int32, dx, dy int8, buttons byte, key byte, press bool) {
		kb, err := EncodeKeyboardReport(modifier, keys)
		if len(keys) > HIDKeyBufferSize {
			if err == nil {
				t.Fatalf("keyboard report accepted %d keys", len(keys))
			}
		} else {
			if err != nil {
				t.Fatalf("keyboard report rejected %d keys: %v", len(keys), err)
			}
			m, err := Unmarshal(kb)
			if err != nil || m.Type != TypeKeyboardReport {
				t.Fatalf("keyboard report frame %x undecodable: %+v %v", kb, m, err)
			}
			if m.Payload[0] != modifier || !bytes.Equal(m.Payload[1:], keys) {
				t.Fatalf("keyboard report %x does not mirror modifier %x keys %x", kb, modifier, keys)
			}
		}

		ptr, err := EncodePointerReport(x, y, buttons)
		if x < 0 || x > MaxAbsoluteCoordinate || y < 0 || y > MaxAbsoluteCoordinate {
			if err == nil {
				t.Fatalf("pointer report accepted out-of-range %d,%d", x, y)
			}
		} else {
			if err != nil {
				t.Fatalf("pointer report rejected in-range %d,%d: %v", x, y, err)
			}
			m, err := Unmarshal(ptr)
			if err != nil || m.Type != TypePointerReport || len(m.Payload) != 9 {
				t.Fatalf("pointer frame %x undecodable: %+v %v", ptr, m, err)
			}
			gx := int32(binary.BigEndian.Uint32(m.Payload[0:4]))
			gy := int32(binary.BigEndian.Uint32(m.Payload[4:8]))
			if gx != x || gy != y || m.Payload[8] != buttons {
				t.Fatalf("pointer round trip got %d,%d,%x want %d,%d,%x", gx, gy, m.Payload[8], x, y, buttons)
			}
		}

		mouse, err := EncodeMouseReport(dx, dy, buttons)
		if err != nil {
			t.Fatalf("mouse report rejected dx=%d dy=%d: %v", dx, dy, err)
		}
		m, err := Unmarshal(mouse)
		if err != nil || m.Type != TypeMouseReport || len(m.Payload) != 3 {
			t.Fatalf("mouse frame %x undecodable: %+v %v", mouse, m, err)
		}
		if int8(m.Payload[0]) != dx || int8(m.Payload[1]) != dy || m.Payload[2] != buttons {
			t.Fatalf("mouse round trip got %d,%d,%x want %d,%d,%x",
				int8(m.Payload[0]), int8(m.Payload[1]), m.Payload[2], dx, dy, buttons)
		}

		kp, err := EncodeKeypressReport(key, press)
		if err != nil {
			t.Fatalf("keypress report rejected key=%x press=%v: %v", key, press, err)
		}
		m, err = Unmarshal(kp)
		if err != nil || m.Type != TypeKeypressReport || len(m.Payload) != 2 {
			t.Fatalf("keypress frame %x undecodable: %+v %v", kp, m, err)
		}
		wantPress := byte(0)
		if press {
			wantPress = 1
		}
		if m.Payload[0] != key || m.Payload[1] != wantPress {
			t.Fatalf("keypress round trip got %x,%x want %x,%x", m.Payload[0], m.Payload[1], key, wantPress)
		}
	})
}
