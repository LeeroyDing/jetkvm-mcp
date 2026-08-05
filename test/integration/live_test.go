//go:build integration

// Package integration contains the read-only live integration test against
// a real JetKVM device. It never runs as part of `go test ./...` - it
// requires the `integration` build tag and an explicit opt-in env var,
// because it talks to real hardware on the network.
//
// Run it explicitly:
//
//	# Preload JETKVM_PASSWORD through a secret manager, then:
//	JETKVM_LIVE_TEST=1 JETKVM_URL=http://your-device \
//	  go test -tags integration ./test/integration/... -v
//
// Scope, matching the project's safety boundary: reachability, HTTP/WS
// auth and signaling handshake, WebRTC session negotiation, and exactly
// one screenshot. Nothing here sends keyboard/mouse input or calls any
// state-changing RPC method. The captured screenshot exists only in a private
// test temporary directory and is removed automatically; only bounded metadata
// is logged, never its content, a content-derived hash, device address, device
// ID, or filesystem path.
package integration

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func requireLiveOptIn(t *testing.T) {
	t.Helper()
	if os.Getenv("JETKVM_LIVE_TEST") != "1" {
		t.Skip("set JETKVM_LIVE_TEST=1 to run the live integration test against a real device")
	}
	if liveURL() == "" {
		t.Skip("set JETKVM_URL to the device to test against")
	}
}

// liveURL returns the device address to test against. There is deliberately
// no default: the caller must name their own device rather than have this
// test reach for whatever answers at a baked-in address.
func liveURL() string {
	return os.Getenv("JETKVM_URL")
}

func liveHost() string {
	u := liveURL()
	for _, prefix := range []string{"http://", "https://"} {
		if len(u) > len(prefix) && u[:len(prefix)] == prefix {
			u = u[len(prefix):]
			break
		}
	}
	if _, _, err := net.SplitHostPort(u); err != nil {
		u = net.JoinHostPort(u, "80")
	}
	return u
}

// TestLiveReachability confirms the device is on the network and speaking
// HTTP, and reports its (unauthenticated, non-secret) setup status. This
// portion requires no credentials and always runs when opted in.
func TestLiveReachability(t *testing.T) {
	requireLiveOptIn(t)

	host := liveHost()
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		t.Fatal("TCP dial to the configured device failed")
	}
	_ = conn.Close()

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(liveURL() + "/device/status")
	if err != nil {
		t.Fatal("GET /device/status failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /device/status returned HTTP %d, want 200", resp.StatusCode)
	}
	t.Log("device reachable; /device/status returned HTTP 200")
}

// TestLiveSessionAndScreenshot performs the full browser-free handshake
// (auth, signaling, WebRTC, video) against the real device and captures
// exactly one screenshot, then reports only bounded metadata - never its
// pixel content or a content-derived fingerprint. It requires credentials (JETKVM_PASSWORD
// or JETKVM_AUTH_TOKEN); if neither is set, it explicitly skips rather
// than failing or fabricating a result, since a device in password mode
// cannot be authenticated without one.
func TestLiveSessionAndScreenshot(t *testing.T) {
	requireLiveOptIn(t)

	var creds jetkvm.Credentials
	havePassword := os.Getenv("JETKVM_PASSWORD") != ""
	haveToken := os.Getenv("JETKVM_AUTH_TOKEN") != ""
	if !havePassword && !haveToken {
		t.Skip("device requires authentication and no credentials were supplied " +
			"(set JETKVM_PASSWORD or JETKVM_AUTH_TOKEN); see Walkthrough.md's " +
			"'known gaps' section for how to complete this proof")
	}
	if havePassword {
		creds.Password = jetkvm.NewSecret(os.Getenv("JETKVM_PASSWORD"))
	}
	if haveToken {
		creds.AuthToken = jetkvm.NewSecret(os.Getenv("JETKVM_AUTH_TOKEN"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := jetkvm.Connect(ctx, jetkvm.Options{
		BaseURL:      liveURL(),
		Credentials:  creds,
		AllowControl: false, // read-only proof: never negotiate the HID channel
	})
	if err != nil {
		t.Fatal("Connect failed; transport details withheld from test output")
	}
	defer client.Close(context.Background())

	t.Logf("connected: firmwareVersion=%s", client.FirmwareVersion())

	status, err := client.Status(ctx)
	if err != nil {
		t.Fatal("Status failed")
	}
	if !status.RPCReachable {
		t.Fatal("expected RPCReachable = true against the live device")
	}

	// The screen may contain sensitive material. Keep the image only in the
	// test's private temporary directory, which testing removes automatically.
	outPath := filepath.Join(t.TempDir(), "live-screenshot.png")

	shot, err := client.SaveScreenshot(ctx, outPath)
	if err != nil {
		// This is the one run where the video-pipeline diagnostics matter
		// most, and the error carries only a one-line summary of them. Dump
		// the whole snapshot: it is counts, bounded enums and durations
		// only - no addresses, credentials, SDP or payload bytes - so it is
		// safe to paste into an issue, and it names the stage that stopped
		// instead of leaving the next run to guess.
		logVideoDiagnostics(t, client)
		t.Fatalf("SaveScreenshot failed: %v", err)
	}
	// Report the pipeline's shape on success too. A capture that only
	// succeeded because reassembly happened to get lucky looks identical to
	// a healthy one from the outside; the counts tell them apart.
	diag := logVideoDiagnostics(t, client)

	info, err := os.Stat(shot.Path)
	if err != nil {
		t.Fatal("statting captured screenshot failed")
	}
	if info.Size() == 0 {
		t.Error("expected the captured screenshot file to be non-empty")
	}

	// Report bounded metadata only. In particular, do not log a digest or
	// byte count derived from potentially sensitive screen content.
	t.Logf(
		"screenshot captured: width=%d height=%d capturedAt=%s fresh=%v (temporary image removed after test)",
		shot.Width, shot.Height, shot.CapturedAt.Format(time.RFC3339Nano), shot.Fresh,
	)

	if shot.Width == 0 || shot.Height == 0 {
		t.Error("expected nonzero screenshot dimensions")
	}
	if !shot.Fresh {
		t.Error("expected the live screenshot to be marked fresh")
	}
	if diag.FailureBoundary != jetkvm.BoundaryNone {
		t.Errorf("video failure boundary = %q, want %q", diag.FailureBoundary, jetkvm.BoundaryNone)
	}
	if diag.BuilderDropped != 0 {
		t.Errorf("reassembly dropped %d packets, want 0", diag.BuilderDropped)
	}
	// The original live failure was caused by a 50-packet SampleBuilder
	// window. Requiring a larger on-wire keyframe makes a passing live run
	// evidence for that exact repair, rather than merely a favourable small
	// frame that never exercised the old defect.
	if diag.MaxPacketsPerKeyframe <= 50 {
		t.Errorf("largest on-wire keyframe used %d packets; want > 50 to exercise the repaired reassembly window", diag.MaxPacketsPerKeyframe)
	}
}

// logVideoDiagnostics prints the video-pipeline snapshot as indented JSON.
//
// Every field is a count, a duration in milliseconds, a bounded enum or a
// negotiated codec parameter; the privacy contract in
// internal/jetkvm/diagnostics.go forbids addresses, credentials, SDP, ICE
// candidates, paths and payload bytes, and a test in that package enforces
// it by driving canaries through every input.
func logVideoDiagnostics(t *testing.T, client *jetkvm.Client) jetkvm.VideoDiagnostics {
	t.Helper()
	diag := client.VideoDiagnostics()
	encoded, err := json.MarshalIndent(diag, "  ", "  ")
	if err != nil {
		t.Logf("video diagnostics: boundary=%s (rendering full snapshot failed: %v)", diag.FailureBoundary, err)
		return diag
	}
	t.Logf("video pipeline diagnostics:\n  %s", encoded)
	return diag
}
