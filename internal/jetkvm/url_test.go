package jetkvm

import (
	"strings"
	"testing"
)

func TestCanonicalBaseURL(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
	}{
		{"http://JETKVM.local/", "http://jetkvm.local"},
		{"http://jetkvm.local:80", "http://jetkvm.local"},
		{"http://jetkvm.local:00080", "http://jetkvm.local"},
		{"http://jetkvm.local.:8080", "http://jetkvm.local:8080"},
		{"https://jetkvm.local:443", "https://jetkvm.local"},
		{"http://[2001:db8::1]:8080", "http://[2001:db8::1]:8080"},
		{"http://[2001:0db8:0:0:0:0:0:1]:8080", "http://[2001:db8::1]:8080"},
		{"http://[fe80::1%25En0]:8080", "http://[fe80::1%25En0]:8080"},
	} {
		got, err := CanonicalBaseURL(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("CanonicalBaseURL(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}

	const canary = "URL-CREDENTIAL-CANARY"
	for _, raw := range []string{
		"http://user:" + canary + "@jetkvm.local",
		"http://jetkvm.local/?token=" + canary,
		"http://jetkvm.local/#" + canary,
		"http://jetkvm.local/" + canary,
	} {
		_, err := CanonicalBaseURL(raw)
		if err == nil {
			t.Errorf("accepted non-canonical URL %q", raw)
		} else if strings.Contains(err.Error(), canary) {
			t.Errorf("validation error leaked URL canary: %v", err)
		}
	}

	for _, raw := range []string{
		"http://jetkvm.local:0",
		"http://jetkvm.local:65536",
	} {
		if _, err := CanonicalBaseURL(raw); err == nil {
			t.Errorf("accepted invalid port in %q", raw)
		}
	}
}
