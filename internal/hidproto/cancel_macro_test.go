package hidproto

import (
	"bytes"
	"testing"
)

func TestEncodeCancelKeyboardMacroWireContract(t *testing.T) {
	frame, err := EncodeCancelKeyboardMacro()
	if err != nil {
		t.Fatalf("EncodeCancelKeyboardMacro: %v", err)
	}
	if want := []byte{byte(TypeCancelKeyboardMacroReport)}; !bytes.Equal(frame, want) {
		t.Fatalf("cancel-macro frame = % x, want the one-byte frame % x", frame, want)
	}

	message, err := Unmarshal(frame)
	if err != nil {
		t.Fatalf("Unmarshal cancel-macro frame: %v", err)
	}
	if message.Type != TypeCancelKeyboardMacroReport {
		t.Fatalf("cancel-macro message type = %#x, want %#x", message.Type, TypeCancelKeyboardMacroReport)
	}
	if len(message.Payload) != 0 {
		t.Fatalf("cancel-macro payload = % x, want no payload", message.Payload)
	}
}
