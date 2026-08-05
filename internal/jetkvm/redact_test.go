package jetkvm

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// canaries are the shapes of credential material this client can plausibly
// end up holding. None of them may survive redaction.
var canaries = []struct {
	name  string
	raw   string
	value string
}{
	{"password field", `{"password":"hunter2-correct-horse"}`, "hunter2-correct-horse"},
	{"password kv", `password=hunter2-correct-horse`, "hunter2-correct-horse"},
	{"auth token", `{"authToken":"eyJhbGciOiJIUzI1NiJ9.payload.signature"}`, "eyJhbGciOiJIUzI1NiJ9.payload.signature"},
	{"session cookie", `Set-Cookie: authToken=abcdef0123456789abcdef0123456789`, "abcdef0123456789abcdef0123456789"},
	{"authorization header", `Authorization: Bearer abcdef0123456789abcdef0123456789`, "abcdef0123456789abcdef0123456789"},
	{"api key", `api_key: sk-0123456789abcdef0123456789abcdef`, "sk-0123456789abcdef0123456789abcdef"},
	{"bare opaque token", `unexpected value abcdef0123456789abcdef0123456789 rejected`, "abcdef0123456789abcdef0123456789"},
	{"secret kv", `secret: 'my-shared-secret-value'`, "my-shared-secret-value"},
}

func TestRedactSensitiveRemovesCredentialMaterial(t *testing.T) {
	for _, c := range canaries {
		t.Run(c.name, func(t *testing.T) {
			got := redactSensitive(c.raw)
			if strings.Contains(got, c.value) {
				t.Errorf("redaction left credential material in place for %s", c.name)
			}
			if !strings.Contains(got, redactionPlaceholder) {
				t.Errorf("redaction did not mark %s as redacted: %q", c.name, got)
			}
		})
	}
}

func TestRedactURLStripsUserinfoAndQuery(t *testing.T) {
	cases := []struct {
		raw      string
		mustDrop []string
	}{
		{"http://user:s3cret-password@device.example/device", []string{"s3cret-password", "user:"}},
		{"http://device.example/auth?token=abcdef0123456789abcdef", []string{"abcdef0123456789abcdef"}},
		{"ws://device.example/webrtc/signaling/client?authToken=abcdef0123456789", []string{"abcdef0123456789"}},
		{"http://device.example/device#authToken=abcdef0123456789", []string{"abcdef0123456789"}},
	}
	for _, tc := range cases {
		got := redactURL(tc.raw)
		for _, banned := range tc.mustDrop {
			if strings.Contains(got, banned) {
				t.Errorf("redactURL(%q) leaked %q: got %q", tc.raw, banned, got)
			}
		}
	}
}

func TestRedactErrorScrubsWrappedTransportErrors(t *testing.T) {
	// This is the shape net/http produces, which the old code wrapped
	// verbatim.
	inner := &url.Error{
		Op:  "Post",
		URL: "http://device.example/auth/login-local?authToken=abcdef0123456789abcdef",
		Err: errors.New("dial tcp: connection refused"),
	}
	wrapped := fmt.Errorf("jetkvm: login failed: %w", inner)

	got := RedactError(wrapped)
	if strings.Contains(got, "abcdef0123456789abcdef") {
		t.Errorf("RedactError leaked a query-string token: %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("RedactError discarded the actionable part of the error: %q", got)
	}
}

func TestRedactErrorHandlesNil(t *testing.T) {
	if got := RedactError(nil); got != "" {
		t.Errorf("RedactError(nil) = %q, want an empty string", got)
	}
}

// TestSanitizeErrorBodyDropsAuthResponses is the core of the reflected-body
// blocker: an authentication endpoint's response body is never quoted back,
// because a reflected credential need not look like one.
func TestSanitizeErrorBodyDropsAuthResponses(t *testing.T) {
	body := []byte(`{"error":"bad password","submitted":"hunter2-correct-horse"}`)

	for _, path := range []string{"/auth/login-local", "/auth/logout", "/AUTH/login-local"} {
		got := sanitizeErrorBody(path, body)
		if strings.Contains(got, "hunter2-correct-horse") {
			t.Errorf("sanitizeErrorBody(%q) reflected a submitted credential", path)
		}
		if !strings.Contains(got, "omitted") {
			t.Errorf("sanitizeErrorBody(%q) = %q, want an explicit omission notice", path, got)
		}
	}
}

func TestSanitizeErrorBodyRedactsNonAuthResponses(t *testing.T) {
	body := []byte(`{"error":"nope","authToken":"abcdef0123456789abcdef0123456789"}`)
	got := sanitizeErrorBody("/device", body)

	if strings.Contains(got, "abcdef0123456789abcdef0123456789") {
		t.Errorf("sanitizeErrorBody leaked a reflected token: %q", got)
	}
	if !strings.Contains(got, "nope") {
		t.Errorf("sanitizeErrorBody discarded the useful diagnostic: %q", got)
	}
}

func TestSanitizeErrorBodyIsBounded(t *testing.T) {
	// Short words, so redaction (which collapses long opaque runs) leaves
	// enough behind for the length cap to be what actually applies.
	got := sanitizeErrorBody("/device", []byte(strings.Repeat("nope ", 1000)))
	if len(got) > 600 {
		t.Errorf("sanitizeErrorBody returned %d bytes; it must stay bounded", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Error("a truncated body should say so")
	}
}

// TestSanitizeErrorBodyCollapsesLongOpaqueRuns documents the interaction
// between the two defences: redaction runs first, so a body that is one
// giant token comes back as a placeholder rather than 500 bytes of it.
func TestSanitizeErrorBodyCollapsesLongOpaqueRuns(t *testing.T) {
	got := sanitizeErrorBody("/device", []byte(strings.Repeat("a", 4096)))
	if len(got) > 64 {
		t.Errorf("a single opaque run should collapse to a placeholder, got %d bytes", len(got))
	}
	if !strings.Contains(got, redactionPlaceholder) {
		t.Errorf("expected a redaction placeholder, got %q", got)
	}
}

func TestAPIErrorNeverCarriesAuthBodies(t *testing.T) {
	err := &APIError{
		Path:       "/auth/login-local",
		StatusCode: 401,
		Body:       sanitizeErrorBody("/auth/login-local", []byte(`{"password":"hunter2-correct-horse"}`)),
	}
	if strings.Contains(err.Error(), "hunter2-correct-horse") {
		t.Error("APIError.Error() leaked a credential from an auth response")
	}
}
