package jetkvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPClientDeviceStatusPublic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/device/status" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(DeviceStatus{IsSetup: true})
	}))
	defer srv.Close()

	c, err := newHTTPClient(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	status, err := c.deviceStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.IsSetup {
		t.Error("expected IsSetup = true")
	}
}

func TestHTTPClientLoginSetsSessionCookie(t *testing.T) {
	const wantToken = "test-session-token-value"
	var gotPassword string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/login-local":
			var req struct {
				Password string `json:"password"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotPassword = req.Password
			http.SetCookie(w, &http.Cookie{Name: "authToken", Value: wantToken, Path: "/"})
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "Login successful"})
		case "/device":
			cookie, err := r.Cookie("authToken")
			if err != nil || cookie.Value != wantToken {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
				return
			}
			mode := "password"
			_ = json.NewEncoder(w).Encode(LocalDevice{AuthMode: &mode, DeviceID: "test-device"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := newHTTPClient(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if err := c.login(context.Background(), NewSecret("hunter2")); err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if gotPassword != "hunter2" {
		t.Errorf("server saw password %q, want hunter2", gotPassword)
	}

	dev, err := c.device(context.Background())
	if err != nil {
		t.Fatalf("authenticated /device call failed: %v", err)
	}
	if dev.DeviceID != "test-device" {
		t.Errorf("device ID = %q, want test-device", dev.DeviceID)
	}
}

func TestHTTPClientLoginFailureDoesNotLeakPasswordInError(t *testing.T) {
	const secretPassword = "super-secret-password-xyz"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid password"})
	}))
	defer srv.Close()

	c, err := newHTTPClient(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	err = c.login(context.Background(), NewSecret(secretPassword))
	if err == nil {
		t.Fatal("expected login error")
	}
	if strings.Contains(err.Error(), secretPassword) {
		t.Errorf("login error leaked the password: %v", err)
	}
}

func TestConnectOmitsKnownCredentialReflectedByNonAuthEndpoint(t *testing.T) {
	const shortToken = "tiny7"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/status":
			_ = json.NewEncoder(w).Encode(DeviceStatus{IsSetup: true})
		case "/device":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, "device echoed "+shortToken)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	_, err := Connect(context.Background(), Options{
		BaseURL:     srv.URL,
		Credentials: Credentials{AuthToken: NewSecret(shortToken)},
	})
	if err == nil {
		t.Fatal("expected reflected /device failure")
	}
	if strings.Contains(err.Error(), shortToken) {
		t.Fatalf("Connect leaked a known short credential reflection: %v", err)
	}
	if !strings.Contains(err.Error(), "credential reflection") {
		t.Fatalf("Connect did not preserve the explicit omission reason: %v", err)
	}
}

func TestHTTPClientPreSuppliedAuthToken(t *testing.T) {
	const token = "pre-established-session-token"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("authToken")
		if err != nil || cookie.Value != token {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
			return
		}
		mode := "password"
		_ = json.NewEncoder(w).Encode(LocalDevice{AuthMode: &mode, DeviceID: "dev-1"})
	}))
	defer srv.Close()

	c, err := newHTTPClient(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.setSessionCookie(NewSecret(token))

	dev, err := c.device(context.Background())
	if err != nil {
		t.Fatalf("device call with pre-supplied token failed: %v", err)
	}
	if dev.DeviceID != "dev-1" {
		t.Errorf("device ID = %q, want dev-1", dev.DeviceID)
	}
}

func TestAPIErrorBodyIsTruncated(t *testing.T) {
	huge := strings.Repeat("x", 10000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	c, err := newHTTPClient(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.deviceStatus(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if len(apiErr.Body) > 600 {
		t.Errorf("error body not truncated: %d bytes", len(apiErr.Body))
	}
}

func TestSecretNeverExposedViaFormatting(t *testing.T) {
	s := NewSecret("do-not-leak-me")
	if strings.Contains(s.String(), "do-not-leak-me") {
		t.Error("Secret.String() leaked the value")
	}
	if strings.Contains(fmt.Sprintf("%v %#v", s, s), "do-not-leak-me") {
		t.Error("percent-v formatting leaked the Secret value")
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "do-not-leak-me") {
		t.Error("JSON marshalling leaked the Secret value")
	}
}
