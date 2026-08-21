package jetkvm

import "testing"

// FuzzResolveMouseButton pins the exact shared parser contract. Only the six
// lowercase button/action pairs are accepted, and every accepted pair resolves
// to the corresponding HID bit and press state. Rejections use one of the two
// static, non-reflecting errors.
func FuzzResolveMouseButton(f *testing.F) {
	for _, seed := range [][2]string{
		{"left", "press"},
		{"left", "release"},
		{"right", "press"},
		{"right", "release"},
		{"middle", "press"},
		{"middle", "release"},
		{"", ""},
		{"Left", "press"},
		{"left", "Press"},
		{" left ", "release"},
		{"middle", " release "},
		{"side", "click"},
		{"\x00left", "press"},
		{"右", "押す"},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, button, action string) {
		buttonMasks := map[string]byte{
			"left":   MouseButtonLeft,
			"right":  MouseButtonRight,
			"middle": MouseButtonMiddle,
		}
		actionStates := map[string]bool{
			"press":   true,
			"release": false,
		}

		wantMask, validButton := buttonMasks[button]
		wantPressed, validAction := actionStates[action]
		wantValid := validButton && validAction

		mask, pressed, err := ResolveMouseButton(button, action)
		if wantValid {
			if err != nil {
				t.Fatalf("ResolveMouseButton(%q, %q) rejected a valid pair: %v", button, action, err)
			}
			if mask != wantMask || pressed != wantPressed {
				t.Fatalf("ResolveMouseButton(%q, %q) = (%d, %v), want (%d, %v)",
					button, action, mask, pressed, wantMask, wantPressed)
			}
			return
		}

		if err == nil {
			t.Fatalf("ResolveMouseButton(%q, %q) accepted an invalid pair", button, action)
		}
		if mask != 0 || pressed {
			t.Fatalf("invalid pair returned (%d, %v), want zero values", mask, pressed)
		}
		if err != errUnknownMouseButton && err != errUnknownMouseAction {
			t.Fatalf("invalid pair returned a dynamic error %q", err)
		}
	})
}
