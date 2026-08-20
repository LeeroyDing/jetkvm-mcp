package jetkvm

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

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
	"enter":        {keys: []int{KeyUsageEnter}},
	"esc":          {keys: []int{KeyUsageEscape}},
	"win":          {modifier: ModifierLeftMeta},
}

// ResolveKeyCombo resolves a case-insensitive named keyboard chord to one
// validated HID keyboard report. Plus signs, hyphens, and whitespace are
// interchangeable separators. The returned key slice is owned by the caller.
func ResolveKeyCombo(name string) (modifier byte, keys []byte, err error) {
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
