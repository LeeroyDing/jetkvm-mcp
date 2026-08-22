package jetkvm

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxKeyComboNameRunes bounds caller-controlled combo names before
// normalization allocates a lower-cased, tokenized copy. It applies to each
// entry of a key sequence as well as to a standalone combo.
const MaxKeyComboNameRunes = 64

// USB boot-keyboard modifier bits used by the named combo registry.
const (
	ModifierLeftControl = 1 << iota
	ModifierLeftShift
	ModifierLeftAlt
	ModifierLeftMeta
	ModifierRightControl
	ModifierRightShift
	ModifierRightAlt
	ModifierRightMeta
)

// USB HID keyboard usage codes used by the named combo registry.
const (
	KeyUsageC      = 0x06
	KeyUsageE      = 0x08
	KeyUsageM      = 0x10
	KeyUsageR      = 0x15
	KeyUsageT      = 0x17
	KeyUsageV      = 0x19
	KeyUsageZ      = 0x1d
	KeyUsageEnter  = 0x28
	KeyUsageEscape = 0x29
	KeyUsageTab    = 0x2b
	KeyUsageSpace  = 0x2c
	KeyUsageDelete = 0x4c
)

type keyComboDefinition struct {
	modifier int
	keys     []int
}

// keyComboRegistry is the canonical set of names exposed by both MCP and
// jetkvmctl. Definitions deliberately remain ints until ValidateKeyCombo has
// checked them; ResolveKeyCombo is the only narrowing boundary.
var keyComboRegistry = map[string]keyComboDefinition{
	"alt+tab":      {modifier: ModifierLeftAlt, keys: []int{KeyUsageTab}},
	"cmd":          {modifier: ModifierLeftMeta},
	"cmd+space":    {modifier: ModifierLeftMeta, keys: []int{KeyUsageSpace}},
	"ctrl+alt+del": {modifier: ModifierLeftControl | ModifierLeftAlt, keys: []int{KeyUsageDelete}},
	"ctrl+c":       {modifier: ModifierLeftControl, keys: []int{KeyUsageC}},
	"ctrl+shift+t": {modifier: ModifierLeftControl | ModifierLeftShift, keys: []int{KeyUsageT}},
	"ctrl+v":       {modifier: ModifierLeftControl, keys: []int{KeyUsageV}},
	"ctrl+z":       {modifier: ModifierLeftControl, keys: []int{KeyUsageZ}},
	"e":            {keys: []int{KeyUsageE}},
	"enter":        {keys: []int{KeyUsageEnter}},
	"esc":          {keys: []int{KeyUsageEscape}},
	"m":            {keys: []int{KeyUsageM}},
	"r":            {keys: []int{KeyUsageR}},
	"t":            {keys: []int{KeyUsageT}},
	"win":          {modifier: ModifierLeftMeta},
}

// ResolvedKeyCombo is one named chord after it has crossed the shared
// resolver and integer validation boundary. Keys is owned by the caller.
type ResolvedKeyCombo struct {
	Modifier byte
	Keys     []byte
}

// ResolveKeyCombo resolves a case-insensitive named keyboard chord to one
// validated HID keyboard report. Plus signs, hyphens, and whitespace are
// interchangeable separators. The returned key slice is owned by the caller.
func ResolveKeyCombo(name string) (modifier byte, keys []byte, err error) {
	runeCount := utf8.RuneCountInString(name)
	if runeCount > MaxKeyComboNameRunes {
		return 0, nil, fmt.Errorf("key combo name must contain at most %d runes, got %d", MaxKeyComboNameRunes, runeCount)
	}
	normalized := normalizeKeyComboName(name)
	combo, ok := keyComboRegistry[normalized]
	if !ok {
		return 0, nil, fmt.Errorf("unknown key combo; valid combos: %s", strings.Join(validKeyComboNames(), ", "))
	}
	if err := ValidateKeyCombo(combo.modifier, combo.keys); err != nil {
		return 0, nil, fmt.Errorf("invalid registered key combo %q: %w", normalized, err)
	}

	keys = make([]byte, len(combo.keys))
	for i, key := range combo.keys {
		keys[i] = byte(key)
	}
	return byte(combo.modifier), keys, nil
}

// ResolveKeySequence resolves and validates a complete ordered sequence before
// returning any reports. Errors identify the failing array index without
// reflecting the caller-controlled chord name.
func ResolveKeySequence(names []string) ([]ResolvedKeyCombo, error) {
	if err := ValidateKeySequenceLength(len(names)); err != nil {
		return nil, err
	}

	resolved := make([]ResolvedKeyCombo, len(names))
	for i, name := range names {
		modifier, keys, err := ResolveKeyCombo(name)
		if err != nil {
			return nil, fmt.Errorf("combos[%d]: %w", i, err)
		}

		integerKeys := make([]int, len(keys))
		for keyIndex, key := range keys {
			integerKeys[keyIndex] = int(key)
		}
		if err := ValidateKeyCombo(int(modifier), integerKeys); err != nil {
			return nil, fmt.Errorf("combos[%d]: invalid resolved key combo: %w", i, err)
		}
		resolved[i] = ResolvedKeyCombo{Modifier: modifier, Keys: keys}
	}
	return resolved, nil
}

func normalizeKeyComboName(name string) string {
	parts := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == '+' || r == '-' || unicode.IsSpace(r)
	})
	return strings.Join(parts, "+")
}

func validKeyComboNames() []string {
	names := make([]string, 0, len(keyComboRegistry))
	for name := range keyComboRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
