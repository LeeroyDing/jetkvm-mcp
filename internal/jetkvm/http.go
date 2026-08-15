package jetkvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"
)

// Credentials configures how the client authenticates to the device's local
// web API. At most one of Password or AuthToken should be set:
//
//   - Password performs the real login flow (POST /auth/login-local) and
//     receives a fresh authToken cookie, exactly like a browser logging in.
//   - AuthToken supplies an already-valid session cookie value directly,
//     skipping login. This exists for automation contexts where a session
//     was already established out of band; it is still handled as a
//     Secret and never logged.
//
// Neither field is required if the device is in "noPassword" mode.
type Credentials struct {
	Password  Secret
	AuthToken Secret
}

// DeviceStatus mirrors web.go's DeviceStatus response from GET /device/status
// (public, unauthenticated).
type DeviceStatus struct {
	IsSetup bool `json:"isSetup"`
}

// LocalDevice mirrors web.go's LocalDevice response from GET /device
// (requires auth unless the device is in noPassword mode).
type LocalDevice struct {
	AuthMode     *string `json:"authMode"`
	DeviceID     string  `json:"deviceId"`
	LoopbackOnly bool    `json:"loopbackOnly"`
}

// httpClient is the browser-free equivalent of the JetKVM web UI's fetch
// calls: it authenticates and holds the resulting session cookie for
// subsequent requests, including the signaling websocket upgrade.
type httpClient struct {
	baseURL          *url.URL
	hc               *http.Client
	knownCredentials []Secret
}

func newHTTPClient(baseURL string, timeout time.Duration) (*httpClient, error) {
	canonical, err := CanonicalBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(canonical)
	if err != nil {
		return nil, fmt.Errorf("jetkvm: invalid base URL: %w", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("jetkvm: creating cookie jar: %w", err)
	}
	return &httpClient{
		baseURL: u,
		hc: &http.Client{
			Jar:     jar,
			Timeout: timeout,
		},
	}, nil
}

func (c *httpClient) url(path string) string {
	ref, err := url.Parse(path)
	if err != nil {
		return c.baseURL.String() + path
	}
	return c.baseURL.ResolveReference(ref).String()
}

func (c *httpClient) do(ctx context.Context, method, path string, body any, out any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("jetkvm: encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.url(path), reader)
	if err != nil {
		return nil, fmt.Errorf("jetkvm: building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		kind := ErrorKindUnreachable
		var netErr net.Error
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) ||
			(errors.As(err, &netErr) && netErr.Timeout()) {
			kind = ErrorKindTimeout
		}
		// newDeviceError stores only a redacted rendering, not the original
		// *url.Error, so callers cannot unwrap back to credential-bearing URL
		// userinfo or query parameters.
		return nil, newDeviceError(kind, method+" "+path, err)
	}
	defer resp.Body.Close()

	const maxHTTPResponseBytes = 1 << 20
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPResponseBytes+1))
	if err != nil {
		kind := ErrorKindUnreachable
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			kind = ErrorKindTimeout
		}
		return resp, newDeviceError(kind, "reading response from "+path, err)
	}
	if len(respBody) > maxHTTPResponseBytes && resp.StatusCode < 400 {
		return resp, &DeviceError{
			Kind:      ErrorKindBadFrame,
			Operation: "reading response from " + path,
			Detail:    fmt.Sprintf("response exceeded %d-byte limit", maxHTTPResponseBytes),
		}
	}
	if len(respBody) > maxHTTPResponseBytes {
		// Preserve the HTTP status taxonomy (especially 401/403 auth
		// failures) while still bounding an attacker-controlled error body.
		respBody = respBody[:maxHTTPResponseBytes]
	}

	if resp.StatusCode >= 400 {
		return resp, &APIError{
			Path:       path,
			StatusCode: resp.StatusCode,
			Body:       sanitizeErrorBody(path, respBody, c.knownCredentials...),
		}
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return resp, newDeviceError(ErrorKindBadFrame, "decoding response from "+path, err)
		}
	}
	return resp, nil
}

// APIError represents a non-2xx HTTP response from the device. The body is
// sanitized before storage (see sanitizeErrorBody) so an error surfaced to
// logs, CLI output, or an MCP response can never carry a reflected
// cookie/token/password - including one the device itself echoed back.
type APIError struct {
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("jetkvm: %s returned HTTP %d: %s", e.Path, e.StatusCode, e.Body)
}

// deviceStatus calls the public, unauthenticated GET /device/status.
func (c *httpClient) deviceStatus(ctx context.Context) (DeviceStatus, error) {
	var status DeviceStatus
	_, err := c.do(ctx, http.MethodGet, "/device/status", nil, &status)
	return status, err
}

// login performs POST /auth/login-local with the given password. On
// success the resulting authToken cookie is stored in the client's cookie
// jar for subsequent requests, matching the browser flow in web.go's
// handleLogin.
func (c *httpClient) login(ctx context.Context, password Secret) error {
	type loginRequest struct {
		Password string `json:"password"`
	}
	_, err := c.do(ctx, http.MethodPost, "/auth/login-local", loginRequest{Password: password.Expose()}, nil)
	if err != nil {
		return fmt.Errorf("jetkvm: login failed: %w", err)
	}
	return nil
}

// setSessionCookie installs a pre-obtained authToken cookie directly,
// bypassing the login call. Used when Credentials.AuthToken is supplied.
func (c *httpClient) setSessionCookie(token Secret) {
	c.hc.Jar.SetCookies(c.baseURL, []*http.Cookie{
		{Name: "authToken", Value: token.Expose(), Path: "/"},
	})
}

// device calls the protected GET /device endpoint, confirming the session
// is actually authenticated (or that the device is in noPassword mode).
func (c *httpClient) device(ctx context.Context) (LocalDevice, error) {
	var dev LocalDevice
	_, err := c.do(ctx, http.MethodGet, "/device", nil, &dev)
	return dev, err
}
