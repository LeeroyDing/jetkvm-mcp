package jetkvm

import (
	"fmt"
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
		return TypeKeypress{}, fmt.Errorf("unsupported rune %q (%U) for US keyboard layout", r, r)
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
	for byteOffset, r := range text {
		keypress, err := MapUSKeyboardRune(r)
		if err != nil {
			return nil, fmt.Errorf("cannot type character %d (byte offset %d): %w", runePosition+1, byteOffset, err)
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
		return fmt.Errorf("delay_ms must be in [0,%d], got %d", MaxTypeDelayMS, delayMS)
	}
	return nil
}
