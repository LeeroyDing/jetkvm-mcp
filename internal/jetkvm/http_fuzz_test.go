package jetkvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// fuzzRoundTripper returns one synthetic device response without any
// network, so the fuzzer exercises exactly the response-handling path a
// live device (or an interposed peer on the plaintext LAN) controls.
type fuzzRoundTripper struct {
	status int
	body   []byte
}

func (rt fuzzRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: rt.status,
		Status:     fmt.Sprintf("%d fuzz", rt.status),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(rt.body)),
		Request:    req,
	}, nil
}

const fuzzHTTPResponseCap = 1 << 20 // mirrors http.go's maxHTTPResponseBytes

// FuzzHTTPResponseHandling drives httpClient.do with attacker-controlled
// status codes and bodies. Properties pinned:
//
//   - no panic on any status/body combination;
//   - status >= 400 always surfaces as *APIError with that status, whose
//     stored body is bounded and, for auth endpoints, omitted entirely;
//   - status < 400 never surfaces as *APIError;
//   - a success body over the 1 MiB cap is rejected as bad-frame without
//     being decoded; an error-status body over the cap still keeps its
//     HTTP status taxonomy;
//   - JSON decoding of a bounded success body fails closed as bad-frame
//     exactly when the body does not decode into the caller's target.
//
// Seeds mirror the mock-harness cases (401 Unauthorized JSON, 503 HTML,
// oversized bodies, truncated JSON). Under plain `go test ./...` only the
// seeds run, so this is CI-safe.
func FuzzHTTPResponseHandling(f *testing.F) {
	f.Add(200, false, true, []byte(`{"isSetup":true}`))
	f.Add(200, false, true, []byte(`{"authMode":"password","deviceId":"fuzz","loopbackOnly":false}`))
	f.Add(401, true, false, []byte(`{"error":"Unauthorized"}`))
	f.Add(401, false, true, []byte(`{"error":"Unauthorized"}`))
	f.Add(403, false, false, []byte(`{"error":"Forbidden","authToken":"AAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`))
	f.Add(503, false, false, []byte(`<html><body>device busy</body></html>`))
	f.Add(200, false, true, []byte(`{"isSetup":`)) // truncated JSON
	f.Add(200, false, true, []byte(`5`))           // JSON, wrong shape for a map target
	f.Add(200, false, false, []byte{})
	f.Add(204, false, true, []byte{})
	f.Add(200, false, true, bytes.Repeat([]byte("x"), fuzzHTTPResponseCap+2))  // over-cap success
	f.Add(500, false, false, bytes.Repeat([]byte("e"), fuzzHTTPResponseCap+2)) // over-cap error

	f.Fuzz(func(t *testing.T, status int, authPath bool, decodeJSON bool, body []byte) {
		// Keep the status inside the range a real transport can produce;
		// the branch space (<400 vs >=400) is fully reachable from it.
		status = 100 + ((status%500)+500)%500

		path := "/device/status"
		if authPath {
			path = "/auth/login-local"
		}

		c, err := newHTTPClient("http://fuzz.invalid", time.Second)
		if err != nil {
			t.Fatalf("newHTTPClient: %v", err)
		}
		c.hc.Transport = fuzzRoundTripper{status: status, body: body}

		var out any
		if decodeJSON {
			out = &map[string]any{}
		}

		_, err = c.do(context.Background(), http.MethodGet, path, nil, out)

		var apiErr *APIError
		if status >= 400 {
			if !errors.As(err, &apiErr) {
				t.Fatalf("status %d did not produce *APIError, got %v", status, err)
			}
			if apiErr.StatusCode != status {
				t.Fatalf("APIError status %d, want %d", apiErr.StatusCode, status)
			}
			if authPath && apiErr.Body != "<response body omitted: authentication endpoint>" {
				t.Fatalf("auth endpoint body not omitted: %q", apiErr.Body)
			}
			const maxStored = 500 + len("...(truncated)")
			if len(apiErr.Body) > maxStored {
				t.Fatalf("stored error body %d bytes exceeds %d", len(apiErr.Body), maxStored)
			}
			return
		}

		if errors.As(err, &apiErr) {
			t.Fatalf("status %d produced *APIError %v", status, apiErr)
		}

		if len(body) > fuzzHTTPResponseCap {
			var de *DeviceError
			if !errors.As(err, &de) || de.Kind != ErrorKindBadFrame {
				t.Fatalf("over-cap success body (%d bytes) yielded %v, want %s",
					len(body), err, ErrorKindBadFrame)
			}
			return
		}

		if !decodeJSON || len(body) == 0 {
			if err != nil {
				t.Fatalf("no-decode success path errored: %v", err)
			}
			return
		}

		// The client must accept the body exactly when it decodes into the
		// caller's target, and fail closed as bad-frame otherwise.
		wantOK := json.Unmarshal(body, &map[string]any{}) == nil
		if wantOK && err != nil {
			t.Fatalf("decodable body rejected: %v", err)
		}
		if !wantOK {
			var de *DeviceError
			if !errors.As(err, &de) || de.Kind != ErrorKindBadFrame {
				t.Fatalf("undecodable body yielded %v, want %s", err, ErrorKindBadFrame)
			}
		}
	})
}
