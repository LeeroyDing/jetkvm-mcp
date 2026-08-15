package jetkvm

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Secret wraps a credential (password, session cookie, token) so that
// accidental logging, error wrapping, or struct dumping (%v, %+v, JSON
// marshalling) can never leak the underlying value. Call Expose() at the
// one call site that actually needs the raw bytes (building an HTTP
// request); never store the result of Expose() in another field or error.
type Secret struct {
	value string
}

// NewSecret wraps a raw credential value.
func NewSecret(value string) Secret { return Secret{value: value} }

// Expose returns the raw credential. Use only to construct the single
// outbound request that needs it.
func (s Secret) Expose() string { return s.value }

// Empty reports whether no credential was supplied.
func (s Secret) Empty() bool { return s.value == "" }

// ContainedIn reports whether public metadata contains this exact credential.
// The raw value stays encapsulated; callers use the boolean only to replace
// compromised/reflected metadata with the redaction placeholder.
func (s Secret) ContainedIn(public string) bool {
	return s.value != "" && strings.Contains(public, s.value)
}

// String implements fmt.Stringer, deliberately never returning the value.
func (s Secret) String() string {
	if s.value == "" {
		return "<empty>"
	}
	return "<redacted>"
}

// GoString implements fmt.GoStringer so %#v also redacts.
func (s Secret) GoString() string { return s.String() }

// MarshalJSON ensures Secret never round-trips its value through JSON
// (config dumps, diagnostics, error payloads).
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"<redacted>"`), nil
}

var _ fmt.Stringer = Secret{}

// redactionPlaceholder is what replaces anything that might be credential
// material. It is deliberately uniform so a reader can tell redaction
// happened rather than guessing at a truncation.
const redactionPlaceholder = "<redacted>"

// sensitiveKeyPattern matches `key: value`, `key=value` and `"key":"value"`
// forms where the key names something credential-bearing. The value is
// replaced wholesale.
var sensitiveKeyPattern = regexp.MustCompile(
	`(?i)("?\b(?:pass(?:word|wd)?|secret|token|auth[_-]?token|authtoken|authorization|api[_-]?key|apikey|cookie|session(?:[_-]?id)?|bearer|credential)\b"?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;&}]+)`,
)

// longOpaquePattern matches free-standing high-entropy-looking runs -
// base64/hex session tokens echoed without a recognizable key name.
var longOpaquePattern = regexp.MustCompile(`\b[A-Za-z0-9+/_-]{24,}={0,2}\b`)

// redactSensitive scrubs text that is about to be surfaced in an error,
// log line, or MCP tool result. It is intentionally aggressive: over-
// redacting a device error message costs a little debuggability, while
// under-redacting one can publish a session token.
func redactSensitive(s string) string {
	s = sensitiveKeyPattern.ReplaceAllString(s, "${1}"+redactionPlaceholder)
	s = longOpaquePattern.ReplaceAllString(s, redactionPlaceholder)
	return s
}

// redactURL strips anything credential-bearing from a URL before it can
// appear in an error: userinfo (https://user:pass@host) and the entire
// query string and fragment, either of which can carry a token.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return redactionPlaceholder
	}
	u.User = nil
	u.Fragment = ""
	if u.RawQuery != "" {
		u.RawQuery = redactionPlaceholder
	}
	return u.String()
}

// urlInErrorPattern finds URLs embedded in an error string produced
// elsewhere in the stack (net/http's *url.Error being the common case).
var urlInErrorPattern = regexp.MustCompile(`\b(?:https?|wss?)://[^\s"'\\]+`)

// RedactError renders an error as a string safe to hand to a caller, a
// log, or an MCP response. It scrubs URLs (userinfo, query strings),
// key/value credential pairs, and long opaque tokens.
//
// Everything user-facing in this module goes through here rather than
// calling err.Error() directly, so a wrapped error from a dependency
// cannot smuggle credential material into output.
func RedactError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	msg = urlInErrorPattern.ReplaceAllStringFunc(msg, redactURL)
	return redactSensitive(msg)
}

// sanitizeErrorBody prepares a device HTTP response body for inclusion in
// an APIError.
//
// Bodies from authentication endpoints are dropped entirely rather than
// redacted: there is no diagnostic value in an auth endpoint's body that
// justifies the risk of reflecting submitted or issued credential
// material, and a reflected token need not look like one.
func sanitizeErrorBody(path string, b []byte, knownCredentials ...Secret) string {
	if isAuthPath(path) {
		return "<response body omitted: authentication endpoint>"
	}
	text := string(b)
	for _, credential := range knownCredentials {
		if credential.ContainedIn(text) {
			return "<response body omitted: credential reflection>"
		}
	}

	const maxLen = 500
	s := redactSensitive(text)
	if len(s) > maxLen {
		s = s[:maxLen] + "...(truncated)"
	}
	return s
}

// isAuthPath reports whether a request path is part of the authentication
// flow, whose responses are never quoted back.
func isAuthPath(path string) bool {
	p := strings.ToLower(path)
	return strings.Contains(p, "/auth/") || strings.HasSuffix(p, "/auth")
}
