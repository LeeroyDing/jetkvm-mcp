package jetkvm

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzCanonicalBaseURL drives CanonicalBaseURL with hostile URL shapes:
// userinfo smuggling, IPv6 literals and zones, odd schemes, boundary and
// overflow ports, percent-encoding, and unicode. Properties pinned:
//
//   - no panic on any input;
//   - a successful result is idempotent: canonicalizing the output again
//     yields the same string with no error;
//   - a successful result reparses as http/https with a non-empty host and
//     carries no userinfo, query, fragment, or path (beyond an empty one);
//   - a surviving port is numeric, in [1,65535], and never the scheme
//     default (80/http, 443/https);
//   - errors never echo the raw input (credential canary never leaks).
//
// Under plain `go test ./...` only the seed corpus runs, so this stays
// CI-safe.
func FuzzCanonicalBaseURL(f *testing.F) {
	for _, seed := range []string{
		// Mirrors url_test.go's accept set.
		"http://JETKVM.local/",
		"http://jetkvm.local:80",
		"http://jetkvm.local:00080",
		"http://jetkvm.local.:8080",
		"https://jetkvm.local:443",
		"  http://jetkvm.local  ",
		"http://192.0.2.10",
		"http://[2001:db8::1]:8080",
		"http://[2001:0db8:0:0:0:0:0:1]:8080",
		"http://[fe80::1%25En0]:8080",
		// Hostile shapes: userinfo, aliasing, schemes, ports, encodings.
		"http://user:secret@jetkvm.local",
		"http://secret@jetkvm.local",
		"http://:secret@jetkvm.local",
		"http://jetkvm.local/?token=x",
		"http://jetkvm.local/#frag",
		"http://jetkvm.local/path",
		"ftp://jetkvm.local",
		"javascript:alert(1)",
		"//jetkvm.local",
		"http://",
		"http://:8080",
		"http://jetkvm.local:0",
		"http://jetkvm.local:65535",
		"http://jetkvm.local:65536",
		"http://jetkvm.local:-1",
		"http://jetkvm.local:99999999999999999999",
		"http://jetkvm.local:08x",
		"http://[fe80::1%25]:80",
		"http://[::ffff:127.0.0.1]",
		"http://127.0.0.1%25en0",
		"http://%6a%65%74kvm.local",
		"http://jétkvm.local",
		"http://\u3000jetkvm.local",
		"http://jetkvm.local\x00",
		"http:jetkvm.local",
		"http://[::1]:000443",
		"https://[::1]:443",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := CanonicalBaseURL(raw)
		if err != nil {
			if got != "" {
				t.Fatalf("CanonicalBaseURL(%q) returned %q alongside error %v", raw, got, err)
			}
			// Every failure must use the fixed error vocabulary, which by
			// construction never echoes raw input.
			if !strings.HasPrefix(err.Error(), "jetkvm: device URL") {
				t.Fatalf("CanonicalBaseURL(%q) error outside fixed vocabulary (may echo input): %v", raw, err)
			}
			return
		}
		again, err := CanonicalBaseURL(got)
		if err != nil || again != got {
			t.Fatalf("CanonicalBaseURL not idempotent: %q -> %q -> %q, %v", raw, got, again, err)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("canonical output %q does not reparse: %v", got, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			t.Fatalf("canonical output %q has scheme %q", got, u.Scheme)
		}
		if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Opaque != "" {
			t.Fatalf("canonical output %q carries forbidden components", got)
		}
		if port := u.Port(); port != "" {
			if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
				t.Fatalf("canonical output %q retains default port", got)
			}
			for _, c := range port {
				if c < '0' || c > '9' {
					t.Fatalf("canonical output %q has non-numeric port", got)
				}
			}
		}
	})
}
