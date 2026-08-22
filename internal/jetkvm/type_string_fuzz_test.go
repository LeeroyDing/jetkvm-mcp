package jetkvm

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var safeTypeValidationErrorPattern = regexp.MustCompile(
	`^(?:text must not be empty|unsupported character at position [0-9]+ \(category: (?:[A-Z][a-z]|Invalid)\) for US keyboard layout|text exceeds maximum of [0-9]+ runes \(got [0-9]+\))$`,
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

// FuzzValidateTypeDelay pins the millisecond boundary before adapters convert
// the inter-key delay to time.Duration. Zero is deliberately valid, while
// negative and over-limit values must never be silently narrowed.
func FuzzValidateTypeDelay(f *testing.F) {
	for _, delayMS := range []int{
		0,
		MaxTypeDelayMS,
		-1,
		MaxTypeDelayMS + 1,
		math.MinInt,
		math.MaxInt,
	} {
		f.Add(delayMS)
	}

	f.Fuzz(func(t *testing.T, delayMS int) {
		wantValid := delayMS >= 0 && delayMS <= MaxTypeDelayMS
		err := ValidateTypeDelay(delayMS)
		if (err == nil) != wantValid {
			t.Fatalf("ValidateTypeDelay(%d) error = %v, oracle valid=%v", delayMS, err, wantValid)
		}
	})
}
