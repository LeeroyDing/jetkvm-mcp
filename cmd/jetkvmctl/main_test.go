package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/buildinfo"
	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func captureStdout(t *testing.T, fn func() error) ([]byte, error) {
	t.Helper()
	original := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	defer func() { os.Stdout = original }()

	fnErr := fn()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	return out, fnErr
}

func TestVersionNeedsNoURLAndUsesAuthoritativeMetadata(t *testing.T) {
	t.Setenv("JETKVM_URL", "")
	out, err := captureStdout(t, func() error { return runVersion(nil) })
	if err != nil {
		t.Fatalf("runVersion failed: %v", err)
	}
	var info buildinfo.Info
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("version output is not JSON: %v (%q)", err, out)
	}
	if info.Version != buildinfo.Version || info.Commit == "" || info.GoVersion == "" {
		t.Fatalf("version metadata = %+v", info)
	}
}

func TestDoctorNeedsNoURL(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable on test host")
	}
	t.Setenv("JETKVM_URL", "")
	out, err := captureStdout(t, func() error { return runDoctor(nil) })
	if err != nil {
		t.Fatalf("runDoctor failed: %v", err)
	}
	if !bytes.Contains(out, []byte(`"ffmpeg": "available"`)) {
		t.Fatalf("doctor output did not report FFmpeg availability: %q", out)
	}
}

func TestScreenshotMissingFFmpegAvoidsDeviceSession(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("PATH", t.TempDir())

	err := runScreenshot([]string{"--url", server.URL, "--output", t.TempDir() + "/shot.png"})
	if err == nil || !strings.Contains(err.Error(), "FFmpeg") {
		t.Fatalf("screenshot error = %v, want actionable FFmpeg preflight failure", err)
	}
	if requests != 0 {
		t.Fatalf("missing FFmpeg caused %d device requests, want 0", requests)
	}
}

func TestCLIControlValidationRunsBeforeConnect(t *testing.T) {
	for _, err := range []error{
		runKeypress([]string{"--url", "http://device.invalid", "--allow-control", "--key", "4", "--modifier", "256"}),
		runMouseMove([]string{"--url", "http://device.invalid", "--allow-control", "--x", "32768", "--y", "0"}),
		runMouseMove([]string{"--url", "http://device.invalid", "--allow-control", "--x", "0", "--y", "0", "--buttons", "256"}),
	} {
		if err == nil {
			t.Fatal("CLI accepted out-of-range control input")
		}
		if strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "dial") {
			t.Fatalf("CLI connected before validating control input: %v", err)
		}
	}
}

func TestTopLevelCLIErrorRedactsURLAndCredentialCanaries(t *testing.T) {
	const userinfo = "USERINFO-CREDENTIAL-CANARY"
	const query = "QUERY-CREDENTIAL-CANARY-0123456789"
	const password = "PASSWORD-CREDENTIAL-CANARY"
	err := errors.New("send failed for http://user:" + userinfo + "@device.invalid/?token=" + query + " password=" + password)
	got := formatCLIError(err)
	for _, canary := range []string{userinfo, query, password, "user:"} {
		if strings.Contains(got, canary) {
			t.Errorf("top-level CLI error leaked %q: %s", canary, got)
		}
	}
}

func TestFlagParseErrorsUseTopLevelRedactionBoundary(t *testing.T) {
	const canary = "FLAG-QUERY-CREDENTIAL-CANARY-0123456789"
	exitCode, err := runCLI([]string{"status", "--timeout", "http://user:pass@device.invalid/?token=" + canary})
	if exitCode != 1 || err == nil {
		t.Fatalf("runCLI exit/error = %d/%v, want 1/non-nil", exitCode, err)
	}
	rendered := formatCLIError(err)
	for _, forbidden := range []string{canary, "user:pass"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("redacted flag parse error leaked %q: %s", forbidden, rendered)
		}
	}
}

func TestCLIParseAndUnknownCommandNeverReflectRawValues(t *testing.T) {
	const canary = "short-credential-canary"
	for _, args := range [][]string{
		{"status", "--timeout", canary},
		{"status", canary},
		{canary},
	} {
		exitCode, err := runCLI(args)
		if exitCode == 0 || err == nil {
			t.Fatalf("runCLI(%v) = %d, %v; want failure", args, exitCode, err)
		}
		if got := formatCLIError(err); strings.Contains(got, canary) {
			t.Errorf("runCLI(%v) reflected raw argument: %q", args, got)
		}
	}
}

func TestNetworkCommandsRejectNonPositiveTimeoutBeforeSideEffects(t *testing.T) {
	for _, args := range [][]string{
		{"status", "--timeout", "0"},
		{"screenshot", "--timeout", "-1s"},
		{"serve", "--timeout", "0"},
		{"keypress", "--timeout", "-1ns"},
		{"mouse-move", "--timeout", "0"},
		{"release-all", "--timeout", "-1m"},
	} {
		exitCode, err := runCLI(args)
		if exitCode != 1 || err == nil {
			t.Errorf("runCLI(%v) = %d, %v; want fixed timeout failure", args, exitCode, err)
			continue
		}
		if got := err.Error(); got != "--timeout must be greater than zero" {
			t.Errorf("runCLI(%v) error = %q, want fixed timeout failure", args, got)
		}
	}
}

func TestScreenshotValidatesURLBeforeFFmpegPreflight(t *testing.T) {
	const canary = "URL-USERINFO-CANARY"
	t.Setenv("PATH", t.TempDir())
	err := runScreenshot([]string{
		"--url", "http://user:" + canary + "@device.invalid",
		"--output", t.TempDir() + "/shot.png",
	})
	if err == nil {
		t.Fatal("screenshot accepted a credential-bearing URL")
	}
	if strings.Contains(err.Error(), "FFmpeg") {
		t.Fatalf("FFmpeg preflight ran before URL validation: %v", err)
	}
	if strings.Contains(formatCLIError(err), canary) {
		t.Fatalf("URL validation leaked userinfo: %v", err)
	}
}

func TestCommandContextHasDeadline(t *testing.T) {
	ctx, cancel := commandContext(50 * time.Millisecond)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("one-shot command context has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > time.Second {
		t.Fatalf("unexpected command deadline: remaining=%v", remaining)
	}

	select {
	case <-ctx.Done():
		if ctx.Err() == nil {
			t.Fatal("context completed without an error")
		}
	case <-time.After(time.Second):
		t.Fatal("command context did not time out")
	}
}

// TestServeRejectsPasswordStdin is the stdin-ownership blocker. `serve`
// speaks JSON-RPC over stdin/stdout; reading a password line from stdin
// would consume the MCP client's first protocol message. The two modes are
// genuinely incompatible, so this must fail fast and say why - before any
// byte of stdin is read and before any network activity.
func TestServeRejectsPasswordStdin(t *testing.T) {
	t.Setenv("JETKVM_URL", "http://device.invalid")

	err := runServe([]string{"--password-stdin"})
	if err == nil {
		t.Fatal("serve accepted --password-stdin; it must refuse, or it would corrupt the MCP transport")
	}
	if !errors.Is(err, errPasswordStdinWithServe) {
		t.Fatalf("serve error = %v, want errPasswordStdinWithServe", err)
	}
	// The message has to be actionable, not just a refusal.
	for _, want := range []string{"stdin", "JETKVM_PASSWORD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message does not mention %q: %v", want, err)
		}
	}
}

// TestServeRequiresURL guards the removal of the hardcoded device address:
// with no default, a missing --url must be an explicit error rather than a
// silent attempt against whatever happens to answer at a baked-in address.
func TestServeRequiresURL(t *testing.T) {
	t.Setenv("JETKVM_URL", "")

	err := runServe(nil)
	if err == nil {
		t.Fatal("serve ran without a device URL")
	}
	if !strings.Contains(err.Error(), "--url") {
		t.Fatalf("error should name the missing flag, got: %v", err)
	}
}

func TestRequireURLAcceptsEnvironmentValue(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	t.Setenv("JETKVM_URL", "http://device.invalid")
	cf := addCommonFlags(fs, false)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := requireURL(cf); err != nil {
		t.Fatalf("requireURL rejected a URL supplied via the environment: %v", err)
	}
	if cf.url != "http://device.invalid" {
		t.Fatalf("url = %q, want the environment value", cf.url)
	}
}

// TestNoHardcodedDeviceAddress keeps a private deployment detail from
// creeping back in as a default. A device address belongs to the operator's
// network, not to this tool.
func TestNoHardcodedDeviceAddress(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	t.Setenv("JETKVM_URL", "")
	cf := addCommonFlags(fs, false)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if cf.url != "" {
		t.Fatalf("default --url = %q, want empty (no baked-in device address)", cf.url)
	}
	if err := requireURL(cf); err == nil {
		t.Fatal("requireURL accepted an empty URL")
	}
}

// TestControlCommandsRequireAllowControl checks the opt-in gate is enforced
// before anything connects.
func TestControlCommandsRequireAllowControl(t *testing.T) {
	t.Setenv("JETKVM_URL", "http://device.invalid")

	cases := map[string]func([]string) error{
		"keypress":    runKeypress,
		"mouse-move":  runMouseMove,
		"release-all": runReleaseAll,
	}
	for name, run := range cases {
		err := run(nil)
		if err == nil {
			t.Errorf("%s ran without --allow-control", name)
			continue
		}
		if !strings.Contains(err.Error(), "--allow-control") {
			t.Errorf("%s error should name the missing gate, got: %v", name, err)
		}
	}
}

// TestPrintDiagnosticsIsSafeAndGoesToStderr checks the opt-in report is
// machine-readable, written where it cannot corrupt the result JSON on
// stdout, and free of anything sensitive.
func TestPrintDiagnosticsIsSafeAndGoesToStderr(t *testing.T) {
	originalStderr, originalStdout := os.Stderr, os.Stdout
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr, os.Stdout = errWrite, outWrite
	t.Cleanup(func() { os.Stderr, os.Stdout = originalStderr, originalStdout })

	diag := jetkvm.VideoDiagnostics{
		PeerConnectionState: "connected",
		AnswerApplied:       true,
		TrackObserved:       true,
		TrackMimeType:       "video/H264",
		TrackPayloadType:    102,
		RTPPackets:          0,
		FailureBoundary:     jetkvm.BoundaryNoRTP,
	}
	if err := printDiagnostics(diag); err != nil {
		t.Fatalf("printDiagnostics: %v", err)
	}

	errWrite.Close()
	outWrite.Close()
	stderrBytes, _ := io.ReadAll(errRead)
	stdoutBytes, _ := io.ReadAll(outRead)
	os.Stderr, os.Stdout = originalStderr, originalStdout

	if len(stdoutBytes) != 0 {
		t.Errorf("diagnostics wrote %d bytes to stdout; the result JSON must stay clean", len(stdoutBytes))
	}

	report := string(stderrBytes)
	if !strings.Contains(report, jetkvm.BoundaryNoRTP) {
		t.Errorf("report does not name the boundary: %s", report)
	}
	// The JSON body must parse on its own.
	start := strings.Index(report, "{")
	if start < 0 {
		t.Fatalf("no JSON object in report: %s", report)
	}
	var round map[string]any
	if err := json.Unmarshal([]byte(report[start:]), &round); err != nil {
		t.Fatalf("diagnostics JSON did not parse: %v", err)
	}
	if round["peerConnectionState"] != "connected" {
		t.Errorf("peerConnectionState = %v, want connected", round["peerConnectionState"])
	}

	for _, forbidden := range []string{"192.168.", "authToken", "Bearer", "/Users/", "candidate:", "v=0"} {
		if strings.Contains(report, forbidden) {
			t.Errorf("diagnostics report contained %q", forbidden)
		}
	}
}
