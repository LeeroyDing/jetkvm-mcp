package jetkvm

import (
	"strings"
	"testing"
)

func TestValidateMouseButton(t *testing.T) {
	for _, tt := range []struct {
		name    string
		button  byte
		wantErr bool
	}{
		{name: "left", button: MouseButtonLeft},
		{name: "right", button: MouseButtonRight},
		{name: "middle", button: MouseButtonMiddle},
		{name: "zero", button: 0, wantErr: true},
		{name: "combined left and right", button: MouseButtonLeft | MouseButtonRight, wantErr: true},
		{name: "combined all", button: MouseButtonLeft | MouseButtonRight | MouseButtonMiddle, wantErr: true},
		{name: "unsupported bit", button: 8, wantErr: true},
		{name: "all bits", button: 255, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMouseButton(tt.button)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateMouseButton(%d) error = %v, wantErr=%v", tt.button, err, tt.wantErr)
			}
		})
	}
}

func TestResolveMouseButton(t *testing.T) {
	tests := []struct {
		name        string
		button      string
		action      string
		wantMask    byte
		wantPressed bool
		wantErr     bool
	}{
		{name: "press left", button: "left", action: "press", wantMask: MouseButtonLeft, wantPressed: true},
		{name: "release left", button: "left", action: "release", wantMask: MouseButtonLeft},
		{name: "press right", button: "right", action: "press", wantMask: MouseButtonRight, wantPressed: true},
		{name: "release right", button: "right", action: "release", wantMask: MouseButtonRight},
		{name: "press middle", button: "middle", action: "press", wantMask: MouseButtonMiddle, wantPressed: true},
		{name: "release middle", button: "middle", action: "release", wantMask: MouseButtonMiddle},
		{name: "missing button", action: "press", wantErr: true},
		{name: "unknown button", button: "side", action: "press", wantErr: true},
		{name: "uppercase button", button: "Left", action: "press", wantErr: true},
		{name: "padded button", button: " left ", action: "press", wantErr: true},
		{name: "missing action", button: "left", wantErr: true},
		{name: "unknown action", button: "left", action: "click", wantErr: true},
		{name: "uppercase action", button: "left", action: "Press", wantErr: true},
		{name: "padded action", button: "left", action: " release ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mask, pressed, err := ResolveMouseButton(tt.button, tt.action)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ResolveMouseButton accepted invalid parameters")
				}
				if mask != 0 || pressed {
					t.Errorf("ResolveMouseButton invalid result = (%d, %v), want zero values", mask, pressed)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveMouseButton returned error: %v", err)
			}
			if mask != tt.wantMask || pressed != tt.wantPressed {
				t.Errorf("ResolveMouseButton = (%d, %v), want (%d, %v)", mask, pressed, tt.wantMask, tt.wantPressed)
			}
		})
	}
}

func TestResolveMouseButtonErrorsDoNotReflectInput(t *testing.T) {
	const canary = "mouse-button-secret-canary-7e8a3f"

	for _, tc := range []struct {
		button string
		action string
	}{
		{button: canary, action: "press"},
		{button: "left", action: canary},
	} {
		_, _, err := ResolveMouseButton(tc.button, tc.action)
		if err == nil {
			t.Fatal("ResolveMouseButton accepted a canary parameter")
		}
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("ResolveMouseButton error reflected caller input: %q", err)
		}
	}
}
