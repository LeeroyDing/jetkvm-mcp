package hidproto

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncodeHandshake(t *testing.T) {
	b, err := EncodeHandshake()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{byte(TypeHandshake), ProtocolVersion}
	if !bytes.Equal(b, want) {
		t.Errorf("handshake = % x, want % x", b, want)
	}
}

func TestEncodeKeyboardReport(t *testing.T) {
	b, err := EncodeKeyboardReport(0x02, []byte{0x04, 0x05})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{byte(TypeKeyboardReport), 0x02, 0x04, 0x05}
	if !bytes.Equal(b, want) {
		t.Errorf("keyboard report = % x, want % x", b, want)
	}
}

func TestEncodeKeyboardReportRejectsTooManyKeys(t *testing.T) {
	_, err := EncodeKeyboardReport(0, make([]byte, HIDKeyBufferSize+1))
	if err == nil {
		t.Fatal("expected error for oversized key buffer, got nil")
	}
}

func TestReleaseAllKeyboardReport(t *testing.T) {
	b, err := ReleaseAllKeyboardReport()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{byte(TypeKeyboardReport), 0, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(b, want) {
		t.Errorf("release-all report = % x, want % x", b, want)
	}

	msg, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	report, err := decodeKeyboardReportForTest(msg)
	if err != nil {
		t.Fatal(err)
	}
	if report.Modifier != 0 {
		t.Errorf("modifier = %d, want 0", report.Modifier)
	}
	for i, k := range report.Keys {
		if k != 0 {
			t.Errorf("key[%d] = %d, want 0", i, k)
		}
	}
}

// decodeKeyboardReportForTest mirrors internal/hidrpc/message.go's
// KeyboardReport() decoder, kept local to the test since production code
// only ever needs to encode outbound keyboard reports.
type keyboardReport struct {
	Modifier byte
	Keys     []byte
}

func decodeKeyboardReportForTest(m Message) (keyboardReport, error) {
	if m.Type != TypeKeyboardReport {
		return keyboardReport{}, errors.New("not a keyboard report")
	}
	return keyboardReport{Modifier: m.Payload[0], Keys: m.Payload[1:]}, nil
}

func TestEncodeKeypressReport(t *testing.T) {
	down, err := EncodeKeypressReport(0x1E, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{byte(TypeKeypressReport), 0x1E, 1}; !bytes.Equal(down, want) {
		t.Errorf("keypress down = % x, want % x", down, want)
	}

	up, err := EncodeKeypressReport(0x1E, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{byte(TypeKeypressReport), 0x1E, 0}; !bytes.Equal(up, want) {
		t.Errorf("keypress up = % x, want % x", up, want)
	}
}

func TestEncodePointerReport(t *testing.T) {
	b, err := EncodePointerReport(100, 200, 0x01)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != TypePointerReport {
		t.Fatalf("type = %d, want %d", msg.Type, TypePointerReport)
	}
	if len(msg.Payload) != 9 {
		t.Fatalf("payload length = %d, want 9", len(msg.Payload))
	}
	wantX := []byte{0, 0, 0, 100}
	wantY := []byte{0, 0, 0, 200}
	if !bytes.Equal(msg.Payload[0:4], wantX) {
		t.Errorf("x bytes = % x, want % x", msg.Payload[0:4], wantX)
	}
	if !bytes.Equal(msg.Payload[4:8], wantY) {
		t.Errorf("y bytes = % x, want % x", msg.Payload[4:8], wantY)
	}
	if msg.Payload[8] != 0x01 {
		t.Errorf("buttons = %d, want 1", msg.Payload[8])
	}
}

func TestEncodePointerReportRangeValidation(t *testing.T) {
	cases := []struct {
		x, y int32
	}{
		{-1, 0},
		{0, -1},
		{MaxAbsoluteCoordinate + 1, 0},
		{0, MaxAbsoluteCoordinate + 1},
	}
	for _, c := range cases {
		if _, err := EncodePointerReport(c.x, c.y, 0); err == nil {
			t.Errorf("EncodePointerReport(%d, %d, 0): expected range error, got nil", c.x, c.y)
		}
	}
	// boundary values must succeed
	if _, err := EncodePointerReport(0, 0, 0); err != nil {
		t.Errorf("EncodePointerReport(0,0,0): unexpected error: %v", err)
	}
	if _, err := EncodePointerReport(MaxAbsoluteCoordinate, MaxAbsoluteCoordinate, 0); err != nil {
		t.Errorf("EncodePointerReport(max,max,0): unexpected error: %v", err)
	}
}

func TestEncodeMouseReport(t *testing.T) {
	b, err := EncodeMouseReport(-5, 10, 0x02)
	if err != nil {
		t.Fatal(err)
	}
	dx := int8(-5)
	want := []byte{byte(TypeMouseReport), byte(dx), 10, 0x02}
	if !bytes.Equal(b, want) {
		t.Errorf("mouse report = % x, want % x", b, want)
	}
}

func TestDecodeKeydownStateIsReleaseAll(t *testing.T) {
	allZero := KeydownState{Modifier: 0, Keys: []byte{0, 0, 0, 0, 0, 0}}
	if !allZero.IsReleaseAll() {
		t.Error("all-zero keydown state should report IsReleaseAll() == true")
	}

	stuckModifier := KeydownState{Modifier: 0x02, Keys: []byte{0, 0, 0, 0, 0, 0}}
	if stuckModifier.IsReleaseAll() {
		t.Error("nonzero modifier should report IsReleaseAll() == false")
	}

	stuckKey := KeydownState{Modifier: 0, Keys: []byte{0x04, 0, 0, 0, 0, 0}}
	if stuckKey.IsReleaseAll() {
		t.Error("nonzero key should report IsReleaseAll() == false")
	}
}

func TestDecodeKeydownStateRoundTrip(t *testing.T) {
	msg := Message{Type: TypeKeydownState, Payload: []byte{0x01, 0x04, 0x05, 0, 0, 0, 0}}
	state, err := DecodeKeydownState(msg)
	if err != nil {
		t.Fatal(err)
	}
	if state.Modifier != 0x01 {
		t.Errorf("modifier = %d, want 1", state.Modifier)
	}
	if !bytes.Equal(state.Keys, []byte{0x04, 0x05, 0, 0, 0, 0}) {
		t.Errorf("keys = % x", state.Keys)
	}
}

func TestDecodeKeydownStateWrongType(t *testing.T) {
	_, err := DecodeKeydownState(Message{Type: TypeHandshake, Payload: []byte{1}})
	if err == nil {
		t.Fatal("expected error decoding wrong message type as keydown state")
	}
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	orig := Message{Type: TypeKeyboardReport, Payload: []byte{0x02, 0x04}}
	b, err := Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != orig.Type || !bytes.Equal(got.Payload, orig.Payload) {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, orig)
	}
}

func TestUnmarshalRejectsEmpty(t *testing.T) {
	if _, err := Unmarshal(nil); err == nil {
		t.Fatal("expected error unmarshalling empty data")
	}
}

func TestMarshalRejectsZeroType(t *testing.T) {
	if _, err := Marshal(Message{Type: 0}); err == nil {
		t.Fatal("expected error marshalling zero message type")
	}
}
