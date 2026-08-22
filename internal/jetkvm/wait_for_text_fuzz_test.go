package jetkvm

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzWaitForTextTextAndRegex(f *testing.F) {
	for _, seed := range []struct {
		pattern    string
		recognized string
		regex      bool
	}{
		{pattern: "READY", recognized: "boot READY now", regex: false},
		{pattern: `user-[0-9]+`, recognized: "user-4821", regex: true},
		{pattern: `^`, recognized: "anything", regex: true},
		{pattern: `(?P<REGEX-SECRET-CANARY>`, recognized: "", regex: true},
		{pattern: "", recognized: "", regex: false},
		{pattern: string([]byte{0xff}), recognized: string([]byte{0xfe}), regex: true},
		{pattern: strings.Repeat("界", MaxWaitForTextTextRunes+1), recognized: "", regex: false},
	} {
		f.Add(seed.pattern, seed.recognized, seed.regex)
	}

	f.Fuzz(func(t *testing.T, pattern, recognized string, regexMode bool) {
		opts := WaitForTextOptions{Text: pattern, Regex: regexMode}
		resolved, err := resolveWaitForTextOptions(opts)
		validateErr := ValidateWaitForTextOptions(opts)
		if (err == nil) != (validateErr == nil) {
			t.Fatalf("resolver error = %v, validator error = %v", err, validateErr)
		}

		textShapeValid := utf8.ValidString(pattern) && pattern != "" &&
			utf8.RuneCountInString(pattern) <= MaxWaitForTextTextRunes
		var expectedRegexp *regexp.Regexp
		regexpValid := true
		if textShapeValid && regexMode {
			var regexpErr error
			expectedRegexp, regexpErr = regexp.Compile(pattern)
			regexpValid = regexpErr == nil
			if !regexpValid && err != nil && err.Error() != "text must use valid RE2 syntax" {
				t.Fatalf("invalid regex error reflected caller text or changed shape: %q", err)
			}
		}
		wantValid := textShapeValid && regexpValid
		if !wantValid {
			if err == nil {
				t.Fatalf("invalid text/regex resolved successfully: pattern=%q regex=%v", pattern, regexMode)
			}
			return
		}
		if err != nil {
			t.Fatalf("valid text/regex rejected: pattern=%q regex=%v: %v", pattern, regexMode, err)
		}

		match, matched := resolved.findMatch(recognized)
		var wantMatch string
		var wantMatched bool
		if regexMode {
			location := expectedRegexp.FindStringIndex(recognized)
			if location != nil {
				wantMatch = recognized[location[0]:location[1]]
				wantMatched = true
			}
		} else if index := strings.Index(recognized, pattern); index >= 0 {
			wantMatch = recognized[index : index+len(pattern)]
			wantMatched = true
		}
		if match != wantMatch || matched != wantMatched {
			t.Fatalf("findMatch(%q) = (%q,%v), want (%q,%v)",
				recognized, match, matched, wantMatch, wantMatched)
		}
	})
}
