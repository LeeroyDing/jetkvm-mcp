package jetkvm

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var safeTypeValidationErrorPattern = regexp.MustCompile(
	`^(?:unsupported character at position [0-9]+ \(category: (?:[A-Z][a-z]|Invalid)\) for US keyboard layout|text exceeds maximum of [0-9]+ runes \(got [0-9]+\))$`,
)

func FuzzTypeStringMapping(f *testing.F) {
	for _, seed := range []string{
		"",
		"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"!@#$%^&*()_+{}|:\"~<>?",
		" []\\;',./`-=\n\t",
		"Hello, world!\n",
		"café ☃ 🙂",
		"\x00\r\x7f",
		string([]byte{0xff, 0xfe, 0xfd}),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, text string) {
		keypresses, err := MapTypeString(text)
		if err != nil {
			if !safeTypeValidationErrorPattern.MatchString(err.Error()) {
				t.Fatal("type validation error contained text outside its fixed safe format")
			}
			for _, r := range text {
				if _, mapErr := MapUSKeyboardRune(r); mapErr == nil {
					continue
				}
				for _, reflected := range []string{string(r), fmt.Sprintf("%q", r), fmt.Sprintf("%U", r)} {
					if strings.Contains(err.Error(), reflected) {
						t.Fatal("type validation error reflected a caller-supplied character")
					}
				}
			}
			return
		}
		if got, want := len(keypresses), utf8.RuneCountInString(text); got != want {
			t.Fatalf("mapped keypress count = %d, want %d", got, want)
		}
		for i, keypress := range keypresses {
			if err := ValidateKeypress(keypress.HIDUsageCode, keypress.Modifier); err != nil {
				t.Fatalf("mapped keypress %d is invalid: %+v: %v", i, keypress, err)
			}
		}
	})
}
