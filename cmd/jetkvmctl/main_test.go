package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/buildinfo"
	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func stubSecurity(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "security")
	contents := "#!/bin/sh\nset -eu\n" + script + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatalf("writing fake security binary: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	previous := securityProgram
	securityProgram = "security"
	t.Cleanup(func() { securityProgram = previous })
}

func keychainTestFlags() *commonFlags {
	return &commonFlags{}
}

func configureKeychainTest(t *testing.T) {
	t.Helper()
	t.Setenv("JETKVM_AUTH_TOKEN", "")
	t.Setenv("JETKVM_PASSWORD_KEYCHAIN_SERVICE", "jetkvmctl-tests")
	t.Setenv("JETKVM_PASSWORD_KEYCHAIN_ACCOUNT", "fake-device")
}

func TestCredentialsUsePasswordFoundInKeychain(t *testing.T) {
	configureKeychainTest(t)
	t.Setenv("JETKVM_PASSWORD", "environment-fallback")
	stubSecurity(t, `
[ "$1" = "find-generic-password" ]
[ "$2" = "-s" ]
[ "$3" = "jetkvmctl-tests" ]
[ "$4" = "-a" ]
[ "$5" = "fake-device" ]
[ "$6" = "-w" ]
printf '%s\n' 'keychain-password'
`)

	creds, err := credentialsFromEnv(keychainTestFlags())
	if err != nil {
		t.Fatalf("credentialsFromEnv: %v", err)
	}
	if got := creds.Password.Expose(); got != "keychain-password" {
		t.Fatalf("password = %q, want keychain value", got)
	}
}

func TestCredentialsFallBackWhenKeychainItemIsMissing(t *testing.T) {
	configureKeychainTest(t)
	t.Setenv("JETKVM_PASSWORD", "environment-fallback")
	stubSecurity(t, `exit 44`)

	creds, err := credentialsFromEnv(keychainTestFlags())
	if err != nil {
		t.Fatalf("credentialsFromEnv: %v", err)
	}
	if got := creds.Password.Expose(); got != "environment-fallback" {
		t.Fatalf("password = %q, want environment fallback", got)
	}
}

func TestCredentialsFallBackOnMalformedKeychainOutput(t *testing.T) {
	configureKeychainTest(t)
	t.Setenv("JETKVM_PASSWORD", "environment-fallback")
	stubSecurity(t, `printf 'diagnostic\nnot-a-password\n'`)

	creds, err := credentialsFromEnv(keychainTestFlags())
	if err != nil {
		t.Fatalf("credentialsFromEnv: %v", err)
	}
	if got := creds.Password.Expose(); got != "environment-fallback" {
		t.Fatalf("password = %q, want environment fallback", got)
	}
}

func TestCredentialsRejectMalformedKeychainOutputWithoutFallback(t *testing.T) {
	configureKeychainTest(t)
	t.Setenv("JETKVM_PASSWORD", "")
	stubSecurity(t, `printf '\n'`)

	_, err := credentialsFromEnv(keychainTestFlags())
	if !errors.Is(err, errMalformedKeychainPassword) {
		t.Fatalf("error = %v, want errMalformedKeychainPassword", err)
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

func TestScreenshotMissingFFmpegAvoidsDeviceSession(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("PATH", t.TempDir())

	err := runScreenshot([]string{"--url", server.URL, "--output", filepath.Join(t.TempDir(), "shot.png")})
	if err == nil || !strings.Contains(err.Error(), "FFmpeg") {
		t.Fatalf("screenshot error = %v, want actionable FFmpeg preflight failure", err)
	}
	if requests != 0 {
		t.Fatalf("missing FFmpeg caused %d device requests, want 0", requests)
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

// ---- CLI error-rendering and URL-validation hardening (v0.2.0 parity) ----

// TestTopLevelCLIErrorRedactsURLAndCredentialCanaries pins the single
// rendering boundary: whatever a dependency stuffs into an error, the
// printed form must scrub userinfo, query strings, and key/value
// credential pairs.
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

// TestFlagParseErrorsUseTopLevelRedactionBoundary feeds a credential-bearing
// URL where a duration belongs: the flag package would quote it verbatim,
// so parseCommandFlags must collapse the diagnostic to a fixed message.
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

// TestCLIParseAndUnknownCommandNeverReflectRawValues: positional arguments
// and unknown command tokens may themselves be pasted URLs or secrets and
// must never round-trip into output.
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

// TestNetworkCommandsRejectNonPositiveTimeoutBeforeSideEffects: a
// non-positive --timeout would strip the command context's deadline and
// let a wedged peer hold the CLI open forever, so it is a fixed-message
// error before anything else runs.
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

// TestCommandsValidateURLBeforeCredentialsAndNetwork drives every network
// command with a credential-bearing URL and requires: rejection with the
// fixed device-URL message, no canary in the rendered error, and no
// credential resolution (the hermetic security stub must never run —
// URL validation precedes Keychain lookup by design).
func TestCommandsValidateURLBeforeCredentialsAndNetwork(t *testing.T) {
	const canary = "URL-USERINFO-CANARY"
	marker := filepath.Join(t.TempDir(), "security-invoked")
	configureKeychainTest(t)
	stubSecurity(t, `touch "`+marker+`"
exit 44`)
	hostile := "http://user:" + canary + "@device.invalid/?token=" + canary

	shot := filepath.Join(t.TempDir(), "shot.png")
	cases := map[string][]string{
		"status":      {"status"},
		"screenshot":  {"screenshot", "--output", shot},
		"serve":       {"serve"},
		"keypress":    {"keypress", "--allow-control", "--key", "4"},
		"mouse-move":  {"mouse-move", "--allow-control", "--x", "1", "--y", "1"},
		"release-all": {"release-all", "--allow-control"},
	}
	for name, args := range cases {
		exitCode, err := runCLI(append(args, "--url", hostile))
		if exitCode != 1 || err == nil {
			t.Fatalf("%s accepted a credential-bearing URL: %d, %v", name, exitCode, err)
		}
		if !strings.Contains(err.Error(), "device URL") {
			t.Errorf("%s error is not the fixed URL validation message: %v", name, err)
		}
		if strings.Contains(formatCLIError(err), canary) {
			t.Errorf("%s leaked the userinfo canary: %v", name, err)
		}
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Fatalf("%s resolved credentials before URL validation", name)
		}
	}
}

// TestCLIControlValidationRunsBeforeConnect: out-of-range control input is
// rejected by the shared validators before any connection attempt, so a
// typo'd bitmask can neither reach the device nor silently truncate into a
// different, valid-looking wire report.
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

// ---- version + doctor (v0.2.0 lineage gap: buildinfo/--version/doctor) ----

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	original := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	defer func() { os.Stdout = original }()
	runErr := fn()
	_ = write.Close()
	out, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), runErr
}

func TestVersionCommandPrintsAuthoritativeBuildInfo(t *testing.T) {
	out, err := captureStdout(t, func() error { return runVersion(nil) })
	if err != nil {
		t.Fatalf("runVersion: %v", err)
	}
	var got buildinfo.Info
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("version output is not JSON: %v\n%s", err, out)
	}
	if got.Version != buildinfo.Version {
		t.Errorf("version = %q, want %q", got.Version, buildinfo.Version)
	}
	if got.Commit == "" || got.BuildDate == "" || got.GoVersion == "" || !strings.Contains(got.Platform, "/") {
		t.Errorf("incomplete version info: %+v", got)
	}
}

// TestDoctorIsLocalOnlyAndLeaksNoSecrets is the doctor contract test:
// without --probe-device it must touch no network endpoint, must check the
// Keychain item with an attribute-only query (never -w/-g, which read the
// secret and can prompt), and must never print an environment value.
func TestDoctorIsLocalOnlyAndLeaksNoSecrets(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer server.Close()

	const passwordCanary = "DOCTOR-PASSWORD-CANARY-VALUE"
	t.Setenv("JETKVM_URL", server.URL)
	t.Setenv("JETKVM_AUTH_TOKEN", "")
	t.Setenv("JETKVM_PASSWORD", passwordCanary)
	configureKeychainTest(t)

	secretReadMarker := filepath.Join(t.TempDir(), "secret-read-attempted")
	stubSecurity(t, `for arg in "$@"; do
  if [ "$arg" = "-w" ] || [ "$arg" = "-g" ]; then
    touch "`+secretReadMarker+`"
    exit 99
  fi
done
exit 0`)

	out, err := captureStdout(t, func() error { return runDoctor(nil) })
	if err != nil {
		t.Fatalf("runDoctor: %v", err)
	}

	if requests != 0 {
		t.Fatalf("doctor without --probe-device performed %d network request(s)", requests)
	}
	if _, statErr := os.Stat(secretReadMarker); statErr == nil {
		t.Fatal("doctor asked the security tool for the secret value (-w/-g)")
	}
	if strings.Contains(out, passwordCanary) {
		t.Fatalf("doctor output leaked an environment value:\n%s", out)
	}
	if strings.Contains(out, server.URL) {
		t.Fatalf("doctor output echoed the device URL:\n%s", out)
	}

	var report map[string]any
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("doctor output is not JSON: %v\n%s", err, out)
	}
	keychain, _ := report["keychain"].(map[string]any)
	if keychain["status"] != "present" {
		t.Errorf("keychain status = %v, want present (stub exit 0)", keychain["status"])
	}
	env, _ := report["environment"].(map[string]any)
	if env["url"] != "set (valid)" {
		t.Errorf("environment url = %v, want set (valid)", env["url"])
	}
	if got, _ := env["passwordSource"].(string); !strings.Contains(got, "keychain") {
		t.Errorf("passwordSource = %q, want a keychain source", got)
	}
	if _, ok := report["device"]; ok {
		t.Error("doctor reported a device section without --probe-device")
	}
	for _, section := range []string{"version", "bundle", "codesign", "ffmpeg"} {
		if _, ok := report[section]; !ok {
			t.Errorf("doctor report is missing the %q section", section)
		}
	}
}

func TestDoctorReportsMissingKeychainItem(t *testing.T) {
	t.Setenv("JETKVM_URL", "")
	t.Setenv("JETKVM_AUTH_TOKEN", "")
	t.Setenv("JETKVM_PASSWORD", "")
	configureKeychainTest(t)
	stubSecurity(t, `exit 44`)

	out, err := captureStdout(t, func() error { return runDoctor(nil) })
	if err != nil {
		t.Fatalf("runDoctor: %v", err)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("doctor output is not JSON: %v", err)
	}
	keychain, _ := report["keychain"].(map[string]any)
	if keychain["status"] != "missing" {
		t.Errorf("keychain status = %v, want missing (stub exit 44)", keychain["status"])
	}
	env, _ := report["environment"].(map[string]any)
	if env["url"] != "unset" {
		t.Errorf("environment url = %v, want unset", env["url"])
	}
}

func TestDoctorReportsMissingFFmpeg(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	report := doctorFFmpegReport()
	if report.Status != "unavailable" {
		t.Fatalf("doctor FFmpeg status = %q, want unavailable", report.Status)
	}
	if !strings.Contains(report.Detail, "FFmpeg") || !strings.Contains(report.Detail, "screenshots") {
		t.Fatalf("doctor FFmpeg detail is not actionable: %q", report.Detail)
	}
}

// TestDoctorProbeValidatesURLBeforeConnecting: the explicit probe still
// goes through the same URL validation chokepoint as every other command.
func TestDoctorProbeValidatesURLBeforeConnecting(t *testing.T) {
	const canary = "DOCTOR-URL-CANARY"
	t.Setenv("JETKVM_AUTH_TOKEN", "")
	t.Setenv("JETKVM_PASSWORD", "")
	t.Setenv("JETKVM_PASSWORD_KEYCHAIN_SERVICE", "")
	t.Setenv("JETKVM_PASSWORD_KEYCHAIN_ACCOUNT", "")
	_, err := captureStdout(t, func() error {
		return runDoctor([]string{"--probe-device", "--url", "http://user:" + canary + "@device.invalid"})
	})
	if err == nil {
		t.Fatal("doctor probe accepted a credential-bearing URL")
	}
	if !strings.Contains(err.Error(), "device URL") {
		t.Fatalf("probe error is not the URL validation error: %v", err)
	}
	if strings.Contains(formatCLIError(err), canary) {
		t.Fatalf("probe error leaked the canary: %v", err)
	}
}

func TestParsePlistStringValue(t *testing.T) {
	plist := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
  <key>CFBundleShortVersionString</key>
  <string>9.9.9</string>
</dict></plist>`)
	got, err := parsePlistStringValue(plist, "CFBundleShortVersionString")
	if err != nil || got != "9.9.9" {
		t.Fatalf("parsePlistStringValue = %q, %v; want 9.9.9", got, err)
	}
	if _, err := parsePlistStringValue(plist, "CFBundleVersion"); err == nil {
		t.Error("missing key did not error")
	}
	if _, err := parsePlistStringValue([]byte{0x62, 0x70, 0x6c, 0x69, 0x73, 0x74}, "CFBundleShortVersionString"); err == nil {
		t.Error("binary plist did not error")
	}
}
