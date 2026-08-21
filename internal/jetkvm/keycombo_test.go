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
		"enter":        {keys: []byte{KeyUsageEnter}},
		"esc":          {keys: []byte{KeyUsageEscape}},
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
