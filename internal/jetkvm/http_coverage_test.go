package jetkvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type httpCoverageRoundTripFunc func(*http.Request) (*http.Response, error)

func (f httpCoverageRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type httpCoverageTimeoutError struct{}

func (httpCoverageTimeoutError) Error() string   { return "synthetic transport timeout" }
func (httpCoverageTimeoutError) Timeout() bool   { return true }
func (httpCoverageTimeoutError) Temporary() bool { return true }

type httpCoverageErrorReadCloser struct {
	err        error
	beforeRead func()
}

func (r *httpCoverageErrorReadCloser) Read([]byte) (int, error) {
	if r.beforeRead != nil {
		r.beforeRead()
		r.beforeRead = nil
	}
	return 0, r.err
}

func (*httpCoverageErrorReadCloser) Close() error { return nil }

func TestHTTPClientDoFailureBoundaries(t *testing.T) {
	tests := []struct {
		name         string
		invoke       func(*testing.T, *httpClient) (*http.Response, error)
		wantKind     ErrorKind
		wantText     string
		wantResponse bool
	}{
		{
			name: "request body cannot be encoded",
			invoke: func(t *testing.T, c *httpClient) (*http.Response, error) {
				c.hc.Transport = httpCoverageRoundTripFunc(func(*http.Request) (*http.Response, error) {
					t.Fatal("transport ran after request encoding failed")
					return nil, nil
				})
				return c.do(context.Background(), http.MethodPost, "/device", math.NaN(), nil)
			},
			wantText: "encoding request body",
		},
		{
			name: "invalid path cannot build request",
			invoke: func(t *testing.T, c *httpClient) (*http.Response, error) {
				c.hc.Transport = httpCoverageRoundTripFunc(func(*http.Request) (*http.Response, error) {
					t.Fatal("transport ran after request construction failed")
					return nil, nil
				})
				return c.do(context.Background(), http.MethodGet, "%", nil, nil)
			},
			wantText: "building request",
		},
		{
			name: "transport error is unreachable",
			invoke: func(_ *testing.T, c *httpClient) (*http.Response, error) {
				c.hc.Transport = httpCoverageRoundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("synthetic connection failure")
				})
				return c.do(context.Background(), http.MethodGet, "/device/status", nil, nil)
			},
			wantKind: ErrorKindUnreachable,
			wantText: "GET /device/status",
		},
		{
			name: "net timeout is timeout",
			invoke: func(_ *testing.T, c *httpClient) (*http.Response, error) {
				c.hc.Transport = httpCoverageRoundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, httpCoverageTimeoutError{}
				})
				return c.do(context.Background(), http.MethodGet, "/device/status", nil, nil)
			},
			wantKind: ErrorKindTimeout,
			wantText: "GET /device/status",
		},
		{
			name: "pre-canceled request is timeout",
			invoke: func(_ *testing.T, c *httpClient) (*http.Response, error) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return c.do(ctx, http.MethodGet, "/device/status", nil, nil)
			},
			wantKind: ErrorKindTimeout,
			wantText: "GET /device/status",
		},
		{
			name: "response read error is unreachable",
			invoke: func(_ *testing.T, c *httpClient) (*http.Response, error) {
				c.hc.Transport = httpCoverageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode:    http.StatusOK,
						Header:        make(http.Header),
						Body:          &httpCoverageErrorReadCloser{err: errors.New("synthetic response read failure")},
						ContentLength: -1,
						Request:       req,
					}, nil
				})
				return c.do(context.Background(), http.MethodGet, "/device/status", nil, nil)
			},
			wantKind:     ErrorKindUnreachable,
			wantText:     "reading response from /device/status",
			wantResponse: true,
		},
		{
			name: "context canceled during response read is timeout",
			invoke: func(_ *testing.T, c *httpClient) (*http.Response, error) {
				ctx, cancel := context.WithCancel(context.Background())
				c.hc.Transport = httpCoverageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body: &httpCoverageErrorReadCloser{
							err:        errors.New("synthetic response read failure"),
							beforeRead: cancel,
						},
						ContentLength: -1,
						Request:       req,
					}, nil
				})
				defer cancel()
				return c.do(ctx, http.MethodGet, "/device/status", nil, nil)
			},
			wantKind:     ErrorKindTimeout,
			wantText:     "reading response from /device/status",
			wantResponse: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := newHTTPClient("http://coverage.invalid", time.Second)
			if err != nil {
				t.Fatalf("newHTTPClient: %v", err)
			}

			resp, err := tt.invoke(t, c)
			if err == nil {
				t.Fatal("expected failure")
			}
			if got := ErrorKindOf(err); got != tt.wantKind {
				t.Fatalf("error kind = %q, want %q: %v", got, tt.wantKind, err)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("error %q does not contain %q", err, tt.wantText)
			}
			if got := resp != nil; got != tt.wantResponse {
				t.Fatalf("response present = %v, want %v", got, tt.wantResponse)
			}
		})
	}
}

func TestHTTPClientDoResponseBoundaries(t *testing.T) {
	const responseLimit = 1 << 20
	tests := []struct {
		name             string
		path             string
		status           int
		body             []byte
		decode           bool
		wantKind         ErrorKind
		wantAPIStatus    int
		wantBody         string
		wantBodyMaxBytes int
	}{
		{
			name:   "success body exactly at limit",
			path:   "/device/status",
			status: http.StatusOK,
			body:   bytes.Repeat([]byte("x"), responseLimit),
		},
		{
			name:     "success body one byte over limit",
			path:     "/device/status",
			status:   http.StatusOK,
			body:     bytes.Repeat([]byte("x"), responseLimit+1),
			wantKind: ErrorKindBadFrame,
		},
		{
			name:          "oversized authentication error keeps status",
			path:          "/auth/login-local",
			status:        http.StatusUnauthorized,
			body:          bytes.Repeat([]byte("s"), responseLimit+1),
			wantAPIStatus: http.StatusUnauthorized,
			wantBody:      "<response body omitted: authentication endpoint>",
		},
		{
			name:             "oversized server error keeps status and bounded body",
			path:             "/device/status",
			status:           http.StatusInternalServerError,
			body:             bytes.Repeat([]byte("e"), responseLimit+1),
			wantAPIStatus:    http.StatusInternalServerError,
			wantBodyMaxBytes: 600,
		},
		{
			name:     "malformed nonempty success JSON",
			path:     "/device/status",
			status:   http.StatusOK,
			body:     []byte(`{"isSetup":`),
			decode:   true,
			wantKind: ErrorKindBadFrame,
		},
		{
			name:   "empty success body skips JSON decoding",
			path:   "/device/status",
			status: http.StatusNoContent,
			decode: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := newHTTPClient("http://coverage.invalid", time.Second)
			if err != nil {
				t.Fatalf("newHTTPClient: %v", err)
			}
			c.hc.Transport = fuzzRoundTripper{status: tt.status, body: tt.body}

			var decoded map[string]any
			var out any
			if tt.decode {
				out = &decoded
			}
			resp, err := c.do(context.Background(), http.MethodGet, tt.path, nil, out)
			if resp == nil {
				t.Fatal("response is nil")
			}

			switch {
			case tt.wantAPIStatus != 0:
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("error = %T %v, want *APIError", err, err)
				}
				if apiErr.StatusCode != tt.wantAPIStatus {
					t.Fatalf("API status = %d, want %d", apiErr.StatusCode, tt.wantAPIStatus)
				}
				if tt.wantBody != "" && apiErr.Body != tt.wantBody {
					t.Fatalf("API body = %q, want %q", apiErr.Body, tt.wantBody)
				}
				if tt.wantBodyMaxBytes != 0 && len(apiErr.Body) > tt.wantBodyMaxBytes {
					t.Fatalf("API body length = %d, want <= %d", len(apiErr.Body), tt.wantBodyMaxBytes)
				}
			case tt.wantKind != "":
				if got := ErrorKindOf(err); got != tt.wantKind {
					t.Fatalf("error kind = %q, want %q: %v", got, tt.wantKind, err)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestConnectHTTPPhaseFailureTaxonomy(t *testing.T) {
	const secretPassword = "coverage-password-must-not-leak"
	tests := []struct {
		name        string
		failPath    string
		status      int
		credentials Credentials
		wantKind    ErrorKind
		wantPaths   []string
	}{
		{
			name:      "public status unauthorized",
			failPath:  "/device/status",
			status:    http.StatusUnauthorized,
			wantKind:  ErrorKindAuthFailed,
			wantPaths: []string{"/device/status"},
		},
		{
			name:      "public status server failure",
			failPath:  "/device/status",
			status:    http.StatusServiceUnavailable,
			wantKind:  ErrorKindUnreachable,
			wantPaths: []string{"/device/status"},
		},
		{
			name:        "password login unauthorized",
			failPath:    "/auth/login-local",
			status:      http.StatusUnauthorized,
			credentials: Credentials{Password: NewSecret(secretPassword)},
			wantKind:    ErrorKindAuthFailed,
			wantPaths:   []string{"/device/status", "/auth/login-local"},
		},
		{
			name:      "device session forbidden",
			failPath:  "/device",
			status:    http.StatusForbidden,
			wantKind:  ErrorKindAuthFailed,
			wantPaths: []string{"/device/status", "/device"},
		},
		{
			name:      "device session server failure",
			failPath:  "/device",
			status:    http.StatusInternalServerError,
			wantKind:  ErrorKindUnreachable,
			wantPaths: []string{"/device/status", "/device"},
		},
		{
			name:      "device session other HTTP failure uses auth fallback",
			failPath:  "/device",
			status:    http.StatusTeapot,
			wantKind:  ErrorKindAuthFailed,
			wantPaths: []string{"/device/status", "/device"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pathsMu sync.Mutex
			var paths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pathsMu.Lock()
				paths = append(paths, r.URL.Path)
				pathsMu.Unlock()

				if r.URL.Path == tt.failPath {
					w.WriteHeader(tt.status)
					body := "synthetic HTTP phase failure"
					if !tt.credentials.Password.Empty() {
						body = "device reflected " + secretPassword
					}
					_, _ = w.Write([]byte(body))
					return
				}

				switch r.URL.Path {
				case "/device/status":
					_ = json.NewEncoder(w).Encode(DeviceStatus{IsSetup: true})
				case "/auth/login-local":
					t.Error("login unexpectedly succeeded in an early-failure test")
					w.WriteHeader(http.StatusInternalServerError)
				case "/device":
					mode := "noPassword"
					_ = json.NewEncoder(w).Encode(LocalDevice{AuthMode: &mode, DeviceID: "coverage-device"})
				default:
					t.Errorf("unexpected request path %q", r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer srv.Close()

			client, err := Connect(context.Background(), Options{
				BaseURL:     srv.URL,
				Credentials: tt.credentials,
				HTTPTimeout: time.Second,
			})
			if client != nil {
				_ = client.Close(context.Background())
				t.Fatal("Connect returned a client after an early HTTP failure")
			}
			if got := ErrorKindOf(err); got != tt.wantKind {
				t.Fatalf("error kind = %q, want %q: %v", got, tt.wantKind, err)
			}
			if !tt.credentials.Password.Empty() && strings.Contains(err.Error(), secretPassword) {
				t.Fatalf("Connect error leaked a reflected credential: %v", err)
			}

			pathsMu.Lock()
			gotPaths := strings.Join(paths, ",")
			pathsMu.Unlock()
			if wantPaths := strings.Join(tt.wantPaths, ","); gotPaths != wantPaths {
				t.Fatalf("request paths = %q, want %q", gotPaths, wantPaths)
			}
		})
	}
}
