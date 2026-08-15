package mcpserver

import (
	"os"
	"strings"
	"testing"
	"time"
)

// connectTimeout returns fallback, or the JETKVM_TEST_CONNECT_TIMEOUT
// duration when that override is set and longer. It applies only to test
// deadlines that bound loopback WebRTC transport establishment (a full
// jetkvm.Connect, including in-call reconnects): on shared CI runners,
// -race instrumentation can stretch negotiation well past what any
// developer machine needs — TestScreenshotToolReturnsImageAndWritesNothing
// blew its hardcoded 15s connect deadline twice on 2026-08-15 for exactly
// this reason. The override can only extend a deadline — never shorten
// one — so local defaults and every timing assertion stay unchanged.
// The same helper exists in internal/jetkvm's tests; keep them in sync.
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
