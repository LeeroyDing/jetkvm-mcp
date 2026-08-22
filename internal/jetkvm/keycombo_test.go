package jetkvm

import (
	"slices"
	"strings"
	"testing"
)

func TestResolveKeyComboRegistry(t *testing.T) {
	tests := map[string]struct {
		modifier byte
		keys     []byte
	}{
		"alt+tab":      {modifier: ModifierLeftAlt, keys: []byte{KeyUsageTab}},
		"cmd":          {modifier: ModifierLeftMeta},
		"cmd+space":    {modifier: ModifierLeftMeta, keys: []byte{KeyUsageSpace}},
		"ctrl+alt+del": {modifier: ModifierLeftControl | ModifierLeftAlt, keys: []byte{KeyUsageDelete}},
		"ctrl+c":       {modifier: ModifierLeftControl, keys: []byte{KeyUsageC}},
		"ctrl+shift+t": {modifier: ModifierLeftControl | ModifierLeftShift, keys: []byte{KeyUsageT}},
		"ctrl+v":       {modifier: ModifierLeftControl, keys: []byte{KeyUsageV}},
		"ctrl+z":       {modifier: ModifierLeftControl, keys: []byte{KeyUsageZ}},
		"e":            {keys: []byte{KeyUsageE}},
		"enter":        {keys: []byte{KeyUsageEnter}},
		"esc":          {keys: []byte{KeyUsageEscape}},
		"m":            {keys: []byte{KeyUsageM}},
		"r":            {keys: []byte{KeyUsageR}},
		"t":            {keys: []byte{KeyUsageT}},
		"win":          {modifier: ModifierLeftMeta},
	}

	if len(tests) != len(keyComboRegistry) {
		t.Fatalf("expected table covers %d combos, registry has %d", len(tests), len(keyComboRegistry))
	}
	for name := range keyComboRegistry {
		if _, ok := tests[name]; !ok {
			t.Errorf("registered combo %q has no expected-value test", name)
		}
	}

	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			modifier, keys, err := ResolveKeyCombo(name)
			if err != nil {
				t.Fatalf("ResolveKeyCombo(%q): %v", name, err)
			}
			if modifier != want.modifier || !slices.Equal(keys, want.keys) {
				t.Fatalf("ResolveKeyCombo(%q) = modifier %#02x keys % x, want %#02x % x", name, modifier, keys, want.modifier, want.keys)
			}
		})
	}
}

func TestResolveKeyComboNormalizesCaseAndSeparators(t *testing.T) {
	for _, name := range []string{
		"CTRL+ALT+DEL",
		"Ctrl-Alt-Del",
		"  ctrl + alt + del  ",
		"ctrl\talt\ndel",
		"ctrl\u2003alt\u00a0del",
		"Ctrl - Alt+ Del",
	} {
		modifier, keys, err := ResolveKeyCombo(name)
		if err != nil {
			t.Errorf("ResolveKeyCombo(%q): %v", name, err)
			continue
		}
		if modifier != ModifierLeftControl|ModifierLeftAlt || !slices.Equal(keys, []byte{KeyUsageDelete}) {
			t.Errorf("ResolveKeyCombo(%q) = modifier %#02x keys % x", name, modifier, keys)
		}
	}

	modifier, keys, err := ResolveKeyCombo(" CMD - SPACE ")
	if err != nil {
		t.Fatalf("ResolveKeyCombo cmd variant: %v", err)
	}
	if modifier != ModifierLeftMeta || !slices.Equal(keys, []byte{KeyUsageSpace}) {
		t.Fatalf("ResolveKeyCombo cmd variant = modifier %#02x keys % x", modifier, keys)
	}
}

func TestResolveKeyComboUnknownIsStableAndNonReflecting(t *testing.T) {
	const canary = "unknown-secret-combo-value"
	_, _, err := ResolveKeyCombo(canary)
	if err == nil {
		t.Fatal("ResolveKeyCombo accepted an unknown combo")
	}
	message := err.Error()
	if strings.Contains(message, canary) {
		t.Fatalf("unknown-combo error reflected input: %q", message)
	}
	if !strings.Contains(message, "unknown key combo") || !strings.Contains(message, "ctrl+alt+del") {
		t.Fatalf("unknown-combo error lacks a clear valid-name hint: %q", message)
	}
	want := "unknown key combo; valid combos: " + strings.Join(validKeyComboNames(), ", ")
	if message != want {
		t.Fatalf("unknown-combo error = %q, want %q", message, want)
	}
}

func TestResolveKeyComboRejectsOversizedNameBeforeNormalization(t *testing.T) {
	canary := strings.Repeat("密", MaxKeyComboNameRunes+1)
	_, _, err := ResolveKeyCombo(canary)
	if err == nil {
		t.Fatal("ResolveKeyCombo accepted an oversized combo name")
	}
	message := err.Error()
	if !strings.Contains(message, "at most 64 runes") {
		t.Fatalf("oversized-name error = %q, want the rune bound", message)
	}
	if strings.Contains(message, canary) {
		t.Fatalf("oversized-name error reflected caller input: %q", message)
	}

	// The bound is measured in runes, not bytes. A multi-byte name at the
	// boundary proceeds to ordinary resolution and receives the stable unknown
	// combo error instead of the size error.
	_, _, err = ResolveKeyCombo(strings.Repeat("密", MaxKeyComboNameRunes))
	if err == nil || !strings.Contains(err.Error(), "unknown key combo") {
		t.Fatalf("boundary-length name error = %v, want ordinary unknown-combo rejection", err)
	}
}

func TestResolveKeyComboReturnsOwnedKeySlice(t *testing.T) {
	_, first, err := ResolveKeyCombo("ctrl+c")
	if err != nil {
		t.Fatal(err)
	}
	first[0] = KeyUsageV

	_, second, err := ResolveKeyCombo("ctrl+c")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(second, []byte{KeyUsageC}) {
		t.Fatalf("mutating one result changed the registry: % x", second)
	}
}

func TestResolveKeySequenceValidatesEntireSequence(t *testing.T) {
	names := []string{"cmd+space", "t", "e", "r", "m", "enter"}
	resolved, err := ResolveKeySequence(names)
	if err != nil {
		t.Fatalf("ResolveKeySequence: %v", err)
	}
	want := []ResolvedKeyCombo{
		{Modifier: ModifierLeftMeta, Keys: []byte{KeyUsageSpace}},
		{Keys: []byte{KeyUsageT}},
		{Keys: []byte{KeyUsageE}},
		{Keys: []byte{KeyUsageR}},
		{Keys: []byte{KeyUsageM}},
		{Keys: []byte{KeyUsageEnter}},
	}
	if len(resolved) != len(want) {
		t.Fatalf("resolved sequence length = %d, want %d", len(resolved), len(want))
	}
	for i := range want {
		if resolved[i].Modifier != want[i].Modifier || !slices.Equal(resolved[i].Keys, want[i].Keys) {
			t.Errorf("resolved[%d] = modifier %#02x keys % x, want %#02x/% x", i, resolved[i].Modifier, resolved[i].Keys, want[i].Modifier, want[i].Keys)
		}
	}
}

func TestResolveKeySequenceRejectsBadEntryWithoutPartialOutputOrReflection(t *testing.T) {
	const canary = "short-sequence-secret-canary"
	resolved, err := ResolveKeySequence([]string{"ctrl+c", canary, "enter"})
	if err == nil {
		t.Fatal("ResolveKeySequence accepted an unknown combo")
	}
	if resolved != nil {
		t.Fatalf("ResolveKeySequence returned partial output: %+v", resolved)
	}
	message := err.Error()
	if !strings.Contains(message, "combos[1]") || !strings.Contains(message, "unknown key combo") {
		t.Fatalf("sequence error does not identify the bad index: %q", message)
	}
	if strings.Contains(message, canary) {
		t.Fatalf("sequence error reflected caller input: %q", message)
	}
}

func TestResolveKeySequenceRejectsOversizedEntryWithoutPartialOutput(t *testing.T) {
	resolved, err := ResolveKeySequence([]string{
		"ctrl+c",
		strings.Repeat("x", MaxKeyComboNameRunes+1),
		"enter",
	})
	if err == nil {
		t.Fatal("ResolveKeySequence accepted an oversized combo name")
	}
	if resolved != nil {
		t.Fatalf("ResolveKeySequence returned partial output: %+v", resolved)
	}
	if message := err.Error(); !strings.Contains(message, "combos[1]") || !strings.Contains(message, "at most 64 runes") {
		t.Fatalf("oversized sequence-entry error lacks index and bound: %q", message)
	}
}
