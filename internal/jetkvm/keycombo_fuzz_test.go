package jetkvm

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func fuzzNormalizeKeyComboName(name string) string {
	parts := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == '+' || r == '-' || unicode.IsSpace(r)
	})
	return strings.Join(parts, "+")
}

// fuzzExpectedKeyCombo is an independent acceptance oracle for the named
// parser. It intentionally reads the static registry directly rather than
// calling either ResolveKeyCombo or its normalization helper, so a resolver
// that silently broadens its accepted names cannot make the fuzz test agree
// with the same bug.
func fuzzExpectedKeyCombo(name string) (ResolvedKeyCombo, bool) {
	if utf8.RuneCountInString(name) > MaxKeyComboNameRunes {
		return ResolvedKeyCombo{}, false
	}
	definition, ok := keyComboRegistry[fuzzNormalizeKeyComboName(name)]
	if !ok {
		return ResolvedKeyCombo{}, false
	}
	keys := make([]byte, len(definition.keys))
	for i, key := range definition.keys {
		keys[i] = byte(key)
	}
	return ResolvedKeyCombo{Modifier: byte(definition.modifier), Keys: keys}, true
}

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
		strings.Repeat("a", MaxKeyComboNameRunes+1),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		want, wantValid := fuzzExpectedKeyCombo(name)
		modifier, keys, err := ResolveKeyCombo(name)
		if !wantValid {
			if err == nil {
				t.Fatalf("ResolveKeyCombo(%q) accepted an unknown or oversized name", name)
			}
			if modifier != 0 || keys != nil {
				t.Fatalf("ResolveKeyCombo(%q) returned partial output (%d, %v) with error %v", name, modifier, keys, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("ResolveKeyCombo(%q) rejected a registered name: %v", name, err)
		}
		if modifier != want.Modifier || !slices.Equal(keys, want.Keys) {
			t.Fatalf("ResolveKeyCombo(%q) = (%d, %v), want (%d, %v)", name, modifier, keys, want.Modifier, want.Keys)
		}

		integerKeys := make([]int, len(keys))
		for i, key := range keys {
			integerKeys[i] = int(key)
		}
		if err := ValidateKeyCombo(int(modifier), integerKeys); err != nil {
			t.Fatalf("ResolveKeyCombo(%q) returned an invalid report: %v", name, err)
		}

		original := slices.Clone(want.Keys)
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

// FuzzResolveKeySequence exercises the complete sequence parser rather than
// only its individual-name component. It covers both length boundaries and
// indexed resolution failures, and requires every failure to return no
// partially resolved reports. Successful reports must remain valid and owned
// independently of both sibling entries and the shared registry.
func FuzzResolveKeySequence(f *testing.F) {
	for _, seed := range []struct {
		first, second string
		shape         uint8
	}{
		{first: "ctrl+c", second: "alt+tab", shape: 3},
		{first: "ctrl+c", second: "not-a-combo", shape: 3},
		{first: "enter", shape: 2},
		{shape: 0},
		{shape: 1},
		{first: "cmd", second: "enter", shape: 4},
		{shape: 5},
	} {
		f.Add(seed.first, seed.second, seed.shape)
	}

	f.Fuzz(func(t *testing.T, first, second string, shape uint8) {
		var names []string
		switch shape % 6 {
		case 0:
			// Keep nil distinct from an explicitly supplied empty sequence.
			names = nil
		case 1:
			names = []string{}
		case 2:
			names = []string{first}
		case 3:
			names = []string{first, second}
		case 4:
			names = make([]string, MaxKeySequenceLength)
			for i := range names {
				if i%2 == 0 {
					names[i] = first
				} else {
					names[i] = second
				}
			}
		case 5:
			// The length check must run before any entry is resolved.
			names = make([]string, MaxKeySequenceLength+1)
			for i := range names {
				names[i] = "enter"
			}
		}

		wantValid := len(names) >= 1 && len(names) <= MaxKeySequenceLength
		invalidIndex := -1
		want := make([]ResolvedKeyCombo, len(names))
		if wantValid {
			for i, name := range names {
				combo, ok := fuzzExpectedKeyCombo(name)
				if !ok {
					wantValid = false
					invalidIndex = i
					break
				}
				want[i] = combo
			}
		}

		got, err := ResolveKeySequence(names)
		if !wantValid {
			if err == nil {
				t.Fatalf("ResolveKeySequence accepted invalid sequence of length %d", len(names))
			}
			if got != nil {
				t.Fatalf("invalid sequence returned partial reports: %+v", got)
			}
			if invalidIndex >= 0 {
				wantIndex := fmt.Sprintf("combos[%d]", invalidIndex)
				if !strings.Contains(err.Error(), wantIndex) {
					t.Fatalf("resolution error = %q, want indexed context %q", err, wantIndex)
				}
			}
			return
		}

		if err != nil {
			t.Fatalf("ResolveKeySequence rejected valid sequence: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("resolved report count = %d, want %d", len(got), len(want))
		}

		snapshots := make([][]byte, len(got))
		for i := range want {
			if got[i].Modifier != want[i].Modifier || !slices.Equal(got[i].Keys, want[i].Keys) {
				t.Fatalf("resolved report %d = %+v, want %+v", i, got[i], want[i])
			}
			integerKeys := make([]int, len(got[i].Keys))
			for keyIndex, key := range got[i].Keys {
				integerKeys[keyIndex] = int(key)
			}
			if err := ValidateKeyCombo(int(got[i].Modifier), integerKeys); err != nil {
				t.Fatalf("resolved report %d is invalid: %v", i, err)
			}
			snapshots[i] = slices.Clone(got[i].Keys)
		}

		// Mutating one returned slice must not alter a sibling entry or the
		// registry used by a later resolution.
		mutated := -1
		for i := range got {
			if len(got[i].Keys) > 0 {
				got[i].Keys[0] ^= 0xff
				mutated = i
				break
			}
		}
		if mutated >= 0 {
			for i := range got {
				if i != mutated && !slices.Equal(got[i].Keys, snapshots[i]) {
					t.Fatalf("mutating report %d changed sibling report %d", mutated, i)
				}
			}
		}

		again, err := ResolveKeySequence(names)
		if err != nil {
			t.Fatalf("resolving valid sequence again: %v", err)
		}
		for i := range again {
			if again[i].Modifier != want[i].Modifier || !slices.Equal(again[i].Keys, snapshots[i]) {
				t.Fatalf("resolution changed after returned data was mutated: report %d = %+v", i, again[i])
			}
		}
	})
}
