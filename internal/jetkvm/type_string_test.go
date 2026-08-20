package jetkvm

import (
	"strings"
	"testing"
)

func TestMapUSKeyboardRuneLetters(t *testing.T) {
	for i := 0; i < 26; i++ {
		usage := 0x04 + i
		for _, tc := range []struct {
			rune     rune
			modifier int
		}{
			{rune: rune('a' + i), modifier: 0},
			{rune: rune('A' + i), modifier: leftShiftModifier},
		} {
			got, err := MapUSKeyboardRune(tc.rune)
			if err != nil {
				t.Fatalf("MapUSKeyboardRune(%q): %v", tc.rune, err)
			}
			want := TypeKeypress{Modifier: tc.modifier, HIDUsageCode: usage}
			if got != want {
				t.Errorf("MapUSKeyboardRune(%q) = %+v, want %+v", tc.rune, got, want)
			}
		}
	}
}

func TestMapUSKeyboardRuneDigits(t *testing.T) {
	for i, r := range "1234567890" {
		wantUsage := 0x1e + i
		got, err := MapUSKeyboardRune(r)
		if err != nil {
			t.Fatalf("MapUSKeyboardRune(%q): %v", r, err)
		}
		want := TypeKeypress{HIDUsageCode: wantUsage}
		if got != want {
			t.Errorf("MapUSKeyboardRune(%q) = %+v, want %+v", r, got, want)
		}
	}
}

func TestMapUSKeyboardRuneUnshiftedSymbols(t *testing.T) {
	cases := map[rune]int{
		' ':  0x2c,
		'-':  0x2d,
		'=':  0x2e,
		'[':  0x2f,
		']':  0x30,
		'\\': 0x31,
		';':  0x33,
		'\'': 0x34,
		'`':  0x35,
		',':  0x36,
		'.':  0x37,
		'/':  0x38,
	}
	for r, usage := range cases {
		got, err := MapUSKeyboardRune(r)
		if err != nil {
			t.Fatalf("MapUSKeyboardRune(%q): %v", r, err)
		}
		want := TypeKeypress{HIDUsageCode: usage}
		if got != want {
			t.Errorf("MapUSKeyboardRune(%q) = %+v, want %+v", r, got, want)
		}
	}
}

func TestMapUSKeyboardRuneEveryShiftedSymbol(t *testing.T) {
	cases := map[rune]int{
		'!': 0x1e,
		'@': 0x1f,
		'#': 0x20,
		'$': 0x21,
		'%': 0x22,
		'^': 0x23,
		'&': 0x24,
		'*': 0x25,
		'(': 0x26,
		')': 0x27,
		'_': 0x2d,
		'+': 0x2e,
		'{': 0x2f,
		'}': 0x30,
		'|': 0x31,
		':': 0x33,
		'"': 0x34,
		'~': 0x35,
		'<': 0x36,
		'>': 0x37,
		'?': 0x38,
	}
	for r, usage := range cases {
		got, err := MapUSKeyboardRune(r)
		if err != nil {
			t.Fatalf("MapUSKeyboardRune(%q): %v", r, err)
		}
		want := TypeKeypress{Modifier: leftShiftModifier, HIDUsageCode: usage}
		if got != want {
			t.Errorf("MapUSKeyboardRune(%q) = %+v, want %+v", r, got, want)
		}
	}
}

func TestMapUSKeyboardRuneEnterAndTab(t *testing.T) {
	for r, want := range map[rune]TypeKeypress{
		'\n': {HIDUsageCode: 0x28},
		'\t': {HIDUsageCode: 0x2b},
	} {
		got, err := MapUSKeyboardRune(r)
		if err != nil {
			t.Fatalf("MapUSKeyboardRune(%q): %v", r, err)
		}
		if got != want {
			t.Errorf("MapUSKeyboardRune(%q) = %+v, want %+v", r, got, want)
		}
	}
}

func TestMapUSKeyboardRuneSupportsAllPrintableASCII(t *testing.T) {
	for r := rune(0x20); r <= 0x7e; r++ {
		keypress, err := MapUSKeyboardRune(r)
		if err != nil {
			t.Errorf("printable ASCII %q was rejected: %v", r, err)
			continue
		}
		if err := ValidateKeypress(keypress.HIDUsageCode, keypress.Modifier); err != nil {
			t.Errorf("printable ASCII %q mapped to invalid keypress %+v: %v", r, keypress, err)
		}
	}
}

func TestMapUSKeyboardRuneRejectsUnsupportedRunes(t *testing.T) {
	for _, r := range []rune{'\x00', '\r', '\x7f', 'é', '☃', '🙂'} {
		_, err := MapUSKeyboardRune(r)
		if err == nil {
			t.Errorf("MapUSKeyboardRune(%q) succeeded, want rejection", r)
			continue
		}
		if !strings.Contains(err.Error(), string(r)) && r != '\x00' && r != '\r' && r != '\x7f' {
			t.Errorf("error %q does not name offending rune %q", err, r)
		}
	}
}

func TestMapTypeStringRejectsWithoutPartialOutputAndNamesPosition(t *testing.T) {
	got, err := MapTypeString("Aéz")
	if err == nil {
		t.Fatal("MapTypeString accepted an unsupported rune")
	}
	if got != nil {
		t.Fatalf("MapTypeString returned partial output %+v", got)
	}
	for _, want := range []string{"character 2", "byte offset 1", "'é'", "U+00E9"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestMapTypeStringLengthBound(t *testing.T) {
	if _, err := MapTypeString(strings.Repeat("a", MaxTypeStringRunes)); err != nil {
		t.Fatalf("maximum-length text was rejected: %v", err)
	}
	if got, err := MapTypeString(strings.Repeat("a", MaxTypeStringRunes+1)); err == nil || got != nil {
		t.Fatalf("overlong text = (%v, %v), want nil result and error", got, err)
	}
}

func TestValidateTypeDelay(t *testing.T) {
	for _, tc := range []struct {
		delay int
		valid bool
	}{
		{delay: 0, valid: true},
		{delay: MaxTypeDelayMS, valid: true},
		{delay: -1, valid: false},
		{delay: MaxTypeDelayMS + 1, valid: false},
	} {
		if err := ValidateTypeDelay(tc.delay); (err == nil) != tc.valid {
			t.Errorf("ValidateTypeDelay(%d) error = %v, valid=%v", tc.delay, err, tc.valid)
		}
	}
}
