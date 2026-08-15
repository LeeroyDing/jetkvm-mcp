package jetkvm

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// loopbackSettingEngine restricts a test peer's ICE gathering to loopback
// addresses. Every fake device and raw peer pair in this test suite lives
// on 127.0.0.1, so loopback is the only viable candidate path; gathering
// the runner's other interfaces just slows connectivity checks down (and
// under -race on starved shared CI runners, slow enough to trip pion's
// ~30s ICE failure timer). The production client applies the equivalent
// filter automatically for loopback device URLs (dialOptions.loopbackOnlyICE).
func loopbackSettingEngine() webrtc.SettingEngine {
	se := webrtc.SettingEngine{}
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	// pion skips loopback when gathering host candidates unless asked;
	// without it the filter above leaves zero candidates.
	se.SetIncludeLoopbackCandidate(true)
	return se
}

// contextWithTimeout returns a context canceled after d, with cancellation
// registered via t.Cleanup so tests never leak the timer.
func contextWithTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// connectTimeout returns fallback, or the JETKVM_TEST_CONNECT_TIMEOUT
// duration when that override is set and longer. It applies only to test
// deadlines that bound loopback WebRTC transport establishment (a full
// client connect or raw peer negotiation): on shared CI runners, -race
// instrumentation can stretch negotiation well past what any developer
// machine needs. The override can only extend a deadline — never shorten
// one — so local defaults and every timing assertion stay unchanged.
// The same helper exists in internal/mcpserver's tests; keep them in sync.
func connectTimeout(t *testing.T, fallback time.Duration) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("JETKVM_TEST_CONNECT_TIMEOUT"))
	if raw == "" {
		return fallback
	}
	override, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("invalid JETKVM_TEST_CONNECT_TIMEOUT %q: %v", raw, err)
	}
	if override <= 0 {
		t.Fatalf("JETKVM_TEST_CONNECT_TIMEOUT %q must be positive", raw)
	}
	if override < fallback {
		return fallback
	}
	return override
}
