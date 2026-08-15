package main

import (
	"strings"
	"testing"
)

// FuzzParseKeychainPassword pins the one-line rule for `security …
// find-generic-password -w` output: a lookup result is usable only if it is
// exactly one non-empty line (one optional trailing LF, CR, or CRLF), and
// the accepted secret never contains CR, LF, or NUL. Anything else must be
// rejected as a failed lookup rather than partially salvaged, because a
// diagnostic or interactive prompt echoed by the tool must never become a
// password.
//
// Under plain `go test ./...` only the seed corpus runs, so this is CI-safe.
func FuzzParseKeychainPassword(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("hunter2\n"),     // canonical security -w output
		[]byte("hunter2"),       // no trailing newline
		[]byte("hunter2\r\n"),   // CRLF terminator
		[]byte("hunter2\r"),     // bare CR terminator
		[]byte(""),              // empty output
		[]byte("\n"),            // newline only
		[]byte("\r\n"),          // CRLF only
		[]byte("two\nlines\n"),  // multi-line diagnostic
		[]byte("nul\x00byte\n"), // embedded NUL
		[]byte("trailing space \n"),
		[]byte(" leading space\n"),
		[]byte("password: hunter2\nkeychain: \"login.keychain\"\n"), // -w missing style dump
		[]byte("\xf0\x9f\x94\x91secret\n"),                          // multi-byte UTF-8
		[]byte("a\rb\n"),                                            // interior CR
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, output []byte) {
		password, err := parseKeychainPassword(output)
		if err != nil {
			if password != "" {
				t.Fatalf("rejected lookup still returned a password: %q", password)
			}
			return
		}

		// Accepted secrets are non-empty and contain no line-structure or
		// NUL bytes that could smuggle a second line into downstream use.
		if password == "" {
			t.Fatal("accepted an empty password")
		}
		if strings.ContainsAny(password, "\r\n\x00") {
			t.Fatalf("accepted password contains CR/LF/NUL: %q", password)
		}

		// The accepted value must be the input minus exactly one optional
		// trailing newline sequence - never a substring picked out of a
		// larger response, and never bytes that were not in the input.
		switch string(output) {
		case password, password + "\n", password + "\r", password + "\r\n":
		default:
			t.Fatalf("accepted password %q does not reconstruct input %q", password, output)
		}
	})
}
