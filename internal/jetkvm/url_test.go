package jetkvm

import (
	"strings"
	"testing"
	"time"
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
		{"HTTP://jetkvm.local", "http://jetkvm.local"},
		{"  http://jetkvm.local  ", "http://jetkvm.local"},
		{"http://192.0.2.10", "http://192.0.2.10"},
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
		"http://" + canary + "@jetkvm.local",
		"http://:" + canary + "@jetkvm.local",
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
		"http://jetkvm.local:8o80",
		"http://jetkvm.local:-1",
	} {
		if _, err := CanonicalBaseURL(raw); err == nil {
			t.Errorf("accepted invalid port in %q", raw)
		}
	}
}

// TestCanonicalBaseURLRejectsHostileShapes drives the validator with URL
// shapes an attacker (or a paste accident) could supply: alien and
// case-tricked schemes, opaque URLs, missing hosts, zone abuse, and
// whitespace/control characters. Every rejection must also keep the raw
// input out of the error text.
func TestCanonicalBaseURLRejectsHostileShapes(t *testing.T) {
	for _, raw := range []string{
		// Alien or non-network schemes, including scheme case tricks.
		"file:///etc/passwd",
		"ftp://jetkvm.local",
		"javascript:alert(1)",
		"jetkvm://device",
		"httpx://jetkvm.local",
		"HTTPS+ssh://jetkvm.local",
		// Opaque (non-hierarchical) URLs.
		"mailto:user@jetkvm.local",
		"http:opaque-form",
		// Missing scheme or host entirely.
		"",
		"   ",
		"jetkvm.local",
		"//jetkvm.local",
		"http://",
		"http:///path-only",
		"http://.",
		"http://..",
		"http://jetkvm.local..",
		"http://jetkvm.local...",
		// Userinfo smuggling variants beyond the classic user:pass pair.
		"https://admin@jetkvm.local",
		"http://%61dmin:secret@jetkvm.local",
		// Zone identifiers where they make no sense.
		"http://[fe80::1%25]",
		"http://192.0.2.1%25eth0",
		// Whitespace and control characters embedded in the authority.
		"http://jet kvm.local",
		"http://jetkvm.local\x00",
		"http://jetkvm.\tlocal",
	} {
		got, err := CanonicalBaseURL(raw)
		if err == nil {
			t.Errorf("CanonicalBaseURL(%q) = %q, want rejection", raw, got)
			continue
		}
		if !strings.HasPrefix(err.Error(), "jetkvm: device URL") {
			t.Errorf("CanonicalBaseURL(%q) error is not a fixed-message device URL error: %v", raw, err)
		}
		if raw != "" && strings.TrimSpace(raw) != "" && strings.Contains(err.Error(), strings.TrimSpace(raw)) {
			t.Errorf("CanonicalBaseURL(%q) reflected raw input in error: %v", raw, err)
		}
	}
}

// TestConnectRejectsHostileURLBeforeAnyIO pins the boundary inside the
// library: a hostile BaseURL must be rejected by Connect (and by
// newHTTPClient as defense-in-depth) without any network attempt, and the
// failure must never echo the embedded credential.
func TestConnectRejectsHostileURLBeforeAnyIO(t *testing.T) {
	const canary = "CONNECT-URL-CREDENTIAL-CANARY"
	raw := "http://user:" + canary + "@device.invalid/?token=" + canary

	if _, err := newHTTPClient(raw, 0); err == nil {
		t.Fatal("newHTTPClient accepted a credential-bearing URL")
	} else if strings.Contains(err.Error(), canary) {
		t.Fatalf("newHTTPClient error leaked credential: %v", err)
	}

	_, err := Connect(contextWithTimeout(t, connectTimeout(t, 15*time.Second)), Options{BaseURL: raw})
	if err == nil {
		t.Fatal("Connect accepted a credential-bearing URL")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("Connect error leaked credential: %v", err)
	}
	if strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "lookup") {
		t.Fatalf("Connect attempted network I/O before URL validation: %v", err)
	}
}
