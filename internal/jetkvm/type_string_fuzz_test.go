package jetkvm

import (
	"testing"
	"unicode/utf8"
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
