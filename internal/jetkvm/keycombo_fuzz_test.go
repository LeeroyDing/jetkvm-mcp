package jetkvm

import (
	"slices"
	"testing"
)

// FuzzKeyCombo drives the named-combo parser with arbitrary strings. Every
// accepted name must resolve to a report that passes the same validator used
// before HID byte narrowing, and callers must never be able to mutate the
// registry through a returned key slice. Under plain go test, only seeds run.
func FuzzKeyCombo(f *testing.F) {
	for name := range keyComboRegistry {
		f.Add(name)
	}
	for _, seed := range []string{
		"Ctrl-Alt-Del",
		" ctrl + alt + del ",
		"ctrl\tshift\nt",
		"ctrl\u2003alt\u00a0del",
		"CMD SPACE",
		"",
		"+",
		"ctrl+x",
		"ctrl++alt---del",
		"\x00ctrl+alt+del",
		"⌘+space",
		"\xff\xfe",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		modifier, keys, err := ResolveKeyCombo(name)
		if err != nil {
			return
		}

		integerKeys := make([]int, len(keys))
		for i, key := range keys {
			integerKeys[i] = int(key)
		}
		if err := ValidateKeyCombo(int(modifier), integerKeys); err != nil {
			t.Fatalf("ResolveKeyCombo(%q) returned an invalid report: %v", name, err)
		}

		original := slices.Clone(keys)
		if len(keys) > 0 {
			keys[0] ^= 0xff
		}
		modifierAgain, keysAgain, err := ResolveKeyCombo(name)
		if err != nil {
			t.Fatalf("ResolveKeyCombo(%q) was not deterministic: %v", name, err)
		}
		if modifierAgain != modifier || !slices.Equal(keysAgain, original) {
			t.Fatalf("ResolveKeyCombo(%q) changed after its returned slice was mutated", name)
		}
	})
}
