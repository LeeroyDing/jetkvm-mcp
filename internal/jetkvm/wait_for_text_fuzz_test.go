package jetkvm

import (
	"math"
	"regexp"
	"strings"
	"testing"
	"time"
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

// FuzzWaitForTextDurationValidation exercises duration defaults, explicit
// values, and ordering independently of text/regexp parsing. Raw int64
// nanoseconds cover the complete time.Duration representation without a
// narrowing conversion in the test itself.
func FuzzWaitForTextDurationValidation(f *testing.F) {
	for _, seed := range []struct {
		intervalNanos int64
		timeoutNanos  int64
		present       uint8
	}{
		{present: 0},
		{intervalNanos: int64(DefaultWaitForTextInterval), timeoutNanos: int64(DefaultWaitForTextTimeout), present: 0b11},
		{intervalNanos: int64(MinWaitForTextInterval), timeoutNanos: int64(MinWaitForTextTimeout), present: 0b11},
		{intervalNanos: int64(MaxWaitForTextInterval), timeoutNanos: int64(MaxWaitForTextTimeout), present: 0b11},
		{intervalNanos: int64(MinWaitForTextInterval - time.Nanosecond), timeoutNanos: int64(DefaultWaitForTextTimeout), present: 0b11},
		{intervalNanos: int64(MaxWaitForTextInterval + time.Nanosecond), timeoutNanos: int64(MaxWaitForTextTimeout), present: 0b11},
		{intervalNanos: int64(DefaultWaitForTextInterval), timeoutNanos: int64(MinWaitForTextTimeout - time.Nanosecond), present: 0b11},
		{intervalNanos: int64(DefaultWaitForTextInterval), timeoutNanos: int64(MaxWaitForTextTimeout + time.Nanosecond), present: 0b11},
		{intervalNanos: int64(2 * time.Second), timeoutNanos: int64(time.Second), present: 0b11},
		{intervalNanos: math.MinInt64, timeoutNanos: math.MaxInt64, present: 0},
		{intervalNanos: math.MinInt64, timeoutNanos: math.MaxInt64, present: 0b11},
	} {
		f.Add(seed.intervalNanos, seed.timeoutNanos, seed.present)
	}

	f.Fuzz(func(t *testing.T, intervalNanos, timeoutNanos int64, present uint8) {
		interval := time.Duration(intervalNanos)
		timeout := time.Duration(timeoutNanos)
		opts := WaitForTextOptions{Text: "READY"}
		if present&0b01 != 0 {
			opts.Interval = &interval
		}
		if present&0b10 != 0 {
			opts.Timeout = &timeout
		}

		wantInterval := DefaultWaitForTextInterval
		if opts.Interval != nil {
			wantInterval = interval
		}
		wantTimeout := DefaultWaitForTextTimeout
		if opts.Timeout != nil {
			wantTimeout = timeout
		}
		wantValid := wantInterval >= MinWaitForTextInterval && wantInterval <= MaxWaitForTextInterval &&
			wantTimeout >= MinWaitForTextTimeout && wantTimeout <= MaxWaitForTextTimeout &&
			wantInterval <= wantTimeout

		resolved, err := resolveWaitForTextOptions(opts)
		validateErr := ValidateWaitForTextOptions(opts)
		if (err == nil) != (validateErr == nil) {
			t.Fatalf("resolver error = %v, validator error = %v", err, validateErr)
		}
		if (err == nil) != wantValid {
			t.Fatalf(
				"duration validation error = %v, oracle valid=%v (interval=%v timeout=%v present=%02b)",
				err, wantValid, wantInterval, wantTimeout, present&0b11,
			)
		}
		if !wantValid {
			if resolved.text != "" || resolved.regexp != nil || resolved.interval != 0 || resolved.timeout != 0 {
				t.Fatalf("invalid durations returned partially resolved options: %+v", resolved)
			}
			return
		}
		if resolved.text != opts.Text || resolved.regexp != nil ||
			resolved.interval != wantInterval || resolved.timeout != wantTimeout {
			t.Fatalf(
				"resolved = %+v, want text=%q interval=%v timeout=%v",
				resolved, opts.Text, wantInterval, wantTimeout,
			)
		}
	})
}
