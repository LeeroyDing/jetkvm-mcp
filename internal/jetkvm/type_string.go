package jetkvm

import (
	"fmt"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxTypeStringRunes bounds one text-entry operation so a caller cannot
	// queue an unbounded sequence of live keyboard input.
	MaxTypeStringRunes = 4096

	// DefaultTypeDelayMS and MaxTypeDelayMS define the shared MCP/CLI
	// inter-key delay contract.
	DefaultTypeDelayMS = 0
	MaxTypeDelayMS     = 500
)

const leftShiftModifier = 0x02

type unicodeCategory struct {
	name  string
	table *unicode.RangeTable
}

// unicodeGeneralCategories contains only the disjoint two-letter general
// categories. The unicode.Categories map also contains overlapping aggregate
// categories, so iterating that map would make classification nondeterministic.
var unicodeGeneralCategories = [...]unicodeCategory{
	{name: "Cc", table: unicode.Cc},
	{name: "Cf", table: unicode.Cf},
	{name: "Cn", table: unicode.Cn},
	{name: "Co", table: unicode.Co},
	{name: "Cs", table: unicode.Cs},
	{name: "Ll", table: unicode.Ll},
	{name: "Lm", table: unicode.Lm},
	{name: "Lo", table: unicode.Lo},
	{name: "Lt", table: unicode.Lt},
	{name: "Lu", table: unicode.Lu},
	{name: "Mc", table: unicode.Mc},
	{name: "Me", table: unicode.Me},
	{name: "Mn", table: unicode.Mn},
	{name: "Nd", table: unicode.Nd},
	{name: "Nl", table: unicode.Nl},
	{name: "No", table: unicode.No},
	{name: "Pc", table: unicode.Pc},
	{name: "Pd", table: unicode.Pd},
	{name: "Pe", table: unicode.Pe},
	{name: "Pf", table: unicode.Pf},
	{name: "Pi", table: unicode.Pi},
	{name: "Po", table: unicode.Po},
	{name: "Ps", table: unicode.Ps},
	{name: "Sc", table: unicode.Sc},
	{name: "Sk", table: unicode.Sk},
	{name: "Sm", table: unicode.Sm},
	{name: "So", table: unicode.So},
	{name: "Zl", table: unicode.Zl},
	{name: "Zp", table: unicode.Zp},
	{name: "Zs", table: unicode.Zs},
}

func unicodeGeneralCategory(r rune) string {
	if r < 0 || r > unicode.MaxRune {
		return "Invalid"
	}
	for _, category := range unicodeGeneralCategories {
		if unicode.Is(category.table, r) {
			return category.name
		}
	}
	// Every valid scalar belongs to one general category. Keep the fallback
	// fixed and caller-safe if the standard library's tables ever disagree.
	return "Cn"
}

// TypeCharacterContext returns a caller-safe description for a character in
// text being typed. It deliberately contains only a one-based position and a
// Unicode general category: never the rune, its quoted form, its code point,
// or a byte offset.
func TypeCharacterContext(position int, r rune) string {
	return fmt.Sprintf("character at position %d (category: %s)", position, unicodeGeneralCategory(r))
}

// TypeKeypress is one USB HID keyboard report produced by the US-layout
// text mapper. The fields remain integers until adapters have passed them
// through ValidateKeypress, immediately before narrowing to wire bytes.
type TypeKeypress struct {
	Modifier     int
	HIDUsageCode int
}

// MapUSKeyboardRune maps one supported rune to its USB HID modifier and
// usage code on a US keyboard layout. Supported input is printable ASCII,
// newline (Enter), and tab. The function is pure and never skips input.
func MapUSKeyboardRune(r rune) (TypeKeypress, error) {
	switch {
	case r >= 'a' && r <= 'z':
		return TypeKeypress{HIDUsageCode: 0x04 + int(r-'a')}, nil
	case r >= 'A' && r <= 'Z':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x04 + int(r-'A')}, nil
	case r >= '1' && r <= '9':
		return TypeKeypress{HIDUsageCode: 0x1e + int(r-'1')}, nil
	}

	switch r {
	case '0':
		return TypeKeypress{HIDUsageCode: 0x27}, nil
	case '\n':
		return TypeKeypress{HIDUsageCode: 0x28}, nil
	case '\t':
		return TypeKeypress{HIDUsageCode: 0x2b}, nil
	case ' ':
		return TypeKeypress{HIDUsageCode: 0x2c}, nil
	case '-':
		return TypeKeypress{HIDUsageCode: 0x2d}, nil
	case '=':
		return TypeKeypress{HIDUsageCode: 0x2e}, nil
	case '[':
		return TypeKeypress{HIDUsageCode: 0x2f}, nil
	case ']':
		return TypeKeypress{HIDUsageCode: 0x30}, nil
	case '\\':
		return TypeKeypress{HIDUsageCode: 0x31}, nil
	case ';':
		return TypeKeypress{HIDUsageCode: 0x33}, nil
	case '\'':
		return TypeKeypress{HIDUsageCode: 0x34}, nil
	case '`':
		return TypeKeypress{HIDUsageCode: 0x35}, nil
	case ',':
		return TypeKeypress{HIDUsageCode: 0x36}, nil
	case '.':
		return TypeKeypress{HIDUsageCode: 0x37}, nil
	case '/':
		return TypeKeypress{HIDUsageCode: 0x38}, nil
	case '!':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x1e}, nil
	case '@':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x1f}, nil
	case '#':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x20}, nil
	case '$':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x21}, nil
	case '%':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x22}, nil
	case '^':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x23}, nil
	case '&':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x24}, nil
	case '*':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x25}, nil
	case '(':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x26}, nil
	case ')':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x27}, nil
	case '_':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x2d}, nil
	case '+':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x2e}, nil
	case '{':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x2f}, nil
	case '}':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x30}, nil
	case '|':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x31}, nil
	case ':':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x33}, nil
	case '"':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x34}, nil
	case '~':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x35}, nil
	case '<':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x36}, nil
	case '>':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x37}, nil
	case '?':
		return TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: 0x38}, nil
	default:
		return TypeKeypress{}, fmt.Errorf("unsupported %s for US keyboard layout", TypeCharacterContext(1, r))
	}
}

// MapTypeString expands a complete UTF-8 string into US-layout keypresses.
// It returns no partial sequence when any rune is unsupported.
func MapTypeString(text string) ([]TypeKeypress, error) {
	runeCount := utf8.RuneCountInString(text)
	if runeCount > MaxTypeStringRunes {
		return nil, fmt.Errorf("text exceeds maximum of %d runes (got %d)", MaxTypeStringRunes, runeCount)
	}

	keypresses := make([]TypeKeypress, 0, runeCount)
	runePosition := 0
	for _, r := range text {
		keypress, err := MapUSKeyboardRune(r)
		if err != nil {
			return nil, fmt.Errorf("unsupported %s for US keyboard layout", TypeCharacterContext(runePosition+1, r))
		}
		keypresses = append(keypresses, keypress)
		runePosition++
	}
	return keypresses, nil
}

// ValidateTypeDelay validates the shared inter-key delay before an adapter
// converts it to time.Duration.
func ValidateTypeDelay(delayMS int) error {
	if delayMS < 0 || delayMS > MaxTypeDelayMS {
		return fmt.Errorf("delay must be in [0,%d] milliseconds, got %d", MaxTypeDelayMS, delayMS)
	}
	return nil
}
