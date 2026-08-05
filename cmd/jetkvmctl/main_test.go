package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

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
