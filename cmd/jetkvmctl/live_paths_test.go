package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
	sharedfake "github.com/leeroyding/jetkvm-mcp/internal/testdata/fakedevice"
)

func captureCLIStreams(t *testing.T, fn func() error) (string, string, error) {
	t.Helper()
	originalStdout, originalStderr := os.Stdout, os.Stderr
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = stdoutWrite, stderrWrite
	var runErr error
	func() {
		defer func() { os.Stdout, os.Stderr = originalStdout, originalStderr }()
		runErr = fn()
	}()
	if err := stdoutWrite.Close(); err != nil {
		t.Fatalf("closing captured stdout: %v", err)
	}
	if err := stderrWrite.Close(); err != nil {
		t.Fatalf("closing captured stderr: %v", err)
	}
	stdout, err := io.ReadAll(stdoutRead)
	if err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}
	stderr, err := io.ReadAll(stderrRead)
	if err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}
	_ = stdoutRead.Close()
	_ = stderrRead.Close()
	return string(stdout), string(stderr), runErr
}

// livePathConnectTimeout mirrors the core package's loopback-test timeout
// convention. Race instrumentation on shared CI runners can stretch WebRTC
// negotiation well beyond local timings, so the configured override may only
// extend these upper bounds, never shorten them.
func livePathConnectTimeout(t *testing.T) time.Duration {
	t.Helper()
	const fallback = 15 * time.Second
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
	return max(fallback, override)
}

func parseCLIDiagnostics(t *testing.T, output string) jetkvm.VideoDiagnostics {
	t.Helper()
	jsonStart := strings.IndexByte(output, '{')
	if jsonStart < 0 {
		t.Fatalf("diagnostic output has no JSON object: %q", output)
	}
	var diagnostics jetkvm.VideoDiagnostics
	if err := json.Unmarshal([]byte(output[jsonStart:]), &diagnostics); err != nil {
		t.Fatalf("diagnostic output is not valid JSON: %v\n%s", err, output)
	}
	return diagnostics
}

func assertSyntheticRedPNG(t *testing.T, data []byte) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("captured artifact is not a PNG: %v", err)
	}
	if got := img.Bounds().Size(); got.X != 32 || got.Y != 32 {
		t.Fatalf("captured PNG dimensions = %dx%d, want 32x32", got.X, got.Y)
	}
	r, g, b, a := img.At(16, 16).RGBA()
	if r < 0xc000 || g > 0x3000 || b > 0x3000 || a < 0xf000 {
		t.Fatalf("captured center pixel = rgba16(%d,%d,%d,%d), want the fake device's opaque red frame", r, g, b, a)
	}
}

func TestRunStatusUsesLoopbackRPCAndRejectsBadFrames(t *testing.T) {
	clearCLIAuthEnvironment(t)
	timeout := livePathConnectTimeout(t).String()

	t.Run("reachable", func(t *testing.T) {
		fd := sharedfake.Start(t, sharedfake.Options{})
		out, err := captureStdout(t, func() error {
			return runStatus([]string{"--url", fd.BaseURL(), "--timeout", timeout})
		})
		if err != nil {
			t.Fatalf("runStatus against loopback device: %v", err)
		}
		var got struct {
			DeviceID        string `json:"deviceId"`
			FirmwareVersion string `json:"firmwareVersion"`
			RPCReachable    bool   `json:"rpcReachable"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("status output is not JSON: %v\n%s", err, out)
		}
		if got.DeviceID != "fake-device" || got.FirmwareVersion != "0.4.7+dev" || !got.RPCReachable {
			t.Fatalf("status output = %+v, want loopback identity and a successful RPC ping", got)
		}
		status, login, signaling, rpc := fd.Counts()
		if status != 1 || login != 0 || signaling != 1 || rpc != 1 {
			t.Fatalf("status protocol counts = status %d login %d signaling %d rpc %d, want 1/0/1/1",
				status, login, signaling, rpc)
		}
	})

	t.Run("malformed RPC response", func(t *testing.T) {
		fd := sharedfake.Start(t, sharedfake.Options{RPCResponseMode: sharedfake.RPCTruncated})
		out, err := captureStdout(t, func() error {
			return runStatus([]string{"--url", fd.BaseURL(), "--timeout", timeout})
		})
		if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindBadFrame {
			t.Fatalf("runStatus malformed-RPC error kind = %q, want %q: %v", kind, jetkvm.ErrorKindBadFrame, err)
		}
		if out != "" {
			t.Fatalf("failed status emitted success JSON: %q", out)
		}
		status, login, signaling, rpc := fd.Counts()
		if status != 1 || login != 0 || signaling != 1 || rpc != 1 {
			t.Fatalf("malformed status protocol counts = status %d login %d signaling %d rpc %d, want 1/0/1/1",
				status, login, signaling, rpc)
		}
	})
}

func TestRunScreenshotWritesDecodedFrameAndUsefulDiagnostics(t *testing.T) {
	clearCLIAuthEnvironment(t)
	fd := sharedfake.Start(t, sharedfake.Options{})
	outputPath := filepath.Join(t.TempDir(), "nested", "screen.png")

	stdout, stderr, err := captureCLIStreams(t, func() error {
		return runScreenshot([]string{
			"--url", fd.BaseURL(),
			"--timeout", livePathConnectTimeout(t).String(),
			"--output", outputPath,
			"--diagnostics",
		})
	})
	if err != nil {
		t.Fatalf("runScreenshot against loopback device: %v", err)
	}
	var got struct {
		Path       string `json:"path"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		CapturedAt string `json:"capturedAt"`
		Fresh      bool   `json:"fresh"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("screenshot output is not JSON: %v\n%s", err, stdout)
	}
	if got.Path != outputPath || got.Width != 32 || got.Height != 32 || !got.Fresh {
		t.Fatalf("screenshot output = %+v, want the fresh 32x32 loopback frame at %q", got, outputPath)
	}
	if capturedAt, err := time.Parse(time.RFC3339Nano, got.CapturedAt); err != nil || capturedAt.IsZero() {
		t.Fatalf("screenshot capturedAt = %q, want an RFC3339 timestamp: %v", got.CapturedAt, err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading saved screenshot: %v", err)
	}
	assertSyntheticRedPNG(t, data)

	diagnostics := parseCLIDiagnostics(t, stderr)
	if diagnostics.FailureBoundary != jetkvm.BoundaryNone || diagnostics.DecodeAttempts != 1 ||
		diagnostics.RTPPackets == 0 || diagnostics.FramesAssembled == 0 {
		t.Fatalf("successful screenshot diagnostics do not describe a decoded frame: %+v", diagnostics)
	}
	if strings.Contains(stderr, fd.BaseURL()) || strings.Contains(stderr, outputPath) {
		t.Fatalf("privacy-safe diagnostics exposed a URL or output path: %q", stderr)
	}
	status, login, signaling, rpc := fd.Counts()
	if status != 1 || login != 0 || signaling != 1 || rpc != 0 {
		t.Fatalf("screenshot protocol counts = status %d login %d signaling %d rpc %d, want 1/0/1/0",
			status, login, signaling, rpc)
	}
}

func TestRunScreenshotReportsFilesystemFailureAfterCapturing(t *testing.T) {
	clearCLIAuthEnvironment(t)
	fd := sharedfake.Start(t, sharedfake.Options{})
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block directory creation"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(blocker, "screen.png")

	stdout, stderr, err := captureCLIStreams(t, func() error {
		return runScreenshot([]string{
			"--url", fd.BaseURL(),
			"--timeout", livePathConnectTimeout(t).String(),
			"--output", outputPath,
			"--diagnostics",
		})
	})
	if err == nil || !strings.Contains(err.Error(), "creating output directory") {
		t.Fatalf("runScreenshot filesystem error = %v, want output-directory failure", err)
	}
	if stdout != "" {
		t.Fatalf("failed screenshot emitted success JSON: %q", stdout)
	}
	diagnostics := parseCLIDiagnostics(t, stderr)
	if diagnostics.FailureBoundary != jetkvm.BoundaryNone || diagnostics.DecodeAttempts != 1 {
		t.Fatalf("filesystem failure diagnostics lost evidence of the captured frame: %+v", diagnostics)
	}
	if strings.Contains(stderr, fd.BaseURL()) || strings.Contains(stderr, outputPath) {
		t.Fatalf("failure diagnostics exposed a URL or output path: %q", stderr)
	}
}

func TestRunScreenshotDiagnosticsWriteFailureDoesNotDiscardCapture(t *testing.T) {
	clearCLIAuthEnvironment(t)
	fd := sharedfake.Start(t, sharedfake.Options{})
	outputPath := filepath.Join(t.TempDir(), "screen.png")
	readOnlyPath := filepath.Join(t.TempDir(), "read-only-stderr")
	if err := os.WriteFile(readOnlyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	readOnlyStderr, err := os.Open(readOnlyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnlyStderr.Close()
	originalStderr := os.Stderr
	os.Stderr = readOnlyStderr
	defer func() { os.Stderr = originalStderr }()

	stdout, err := captureStdout(t, func() error {
		return runScreenshot([]string{
			"--url", fd.BaseURL(),
			"--timeout", livePathConnectTimeout(t).String(),
			"--output", outputPath,
			"--diagnostics",
		})
	})
	if err != nil {
		t.Fatalf("diagnostic write failure discarded a successful screenshot: %v", err)
	}
	var got struct {
		Path  string `json:"path"`
		Fresh bool   `json:"fresh"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("screenshot output after diagnostic write failure is not JSON: %v\n%s", err, stdout)
	}
	if got.Path != outputPath || !got.Fresh {
		t.Fatalf("screenshot result after diagnostic write failure = %+v, want fresh output at %q", got, outputPath)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading screenshot preserved after diagnostic failure: %v", err)
	}
	assertSyntheticRedPNG(t, data)
}

func TestRunScreenshotRequiresOutputBeforeDecoderOrDevice(t *testing.T) {
	clearCLIAuthEnvironment(t)
	fd := sharedfake.Start(t, sharedfake.Options{})
	t.Setenv("PATH", t.TempDir())

	err := runScreenshot([]string{"--url", fd.BaseURL()})
	if err == nil || !strings.Contains(err.Error(), "--output is required") {
		t.Fatalf("runScreenshot without output = %v, want required-output error", err)
	}
	status, login, signaling, rpc := fd.Counts()
	if status != 0 || login != 0 || signaling != 0 || rpc != 0 {
		t.Fatalf("missing output crossed a side-effect boundary: status %d login %d signaling %d rpc %d",
			status, login, signaling, rpc)
	}
}

func TestVideoCommandsStopAtDeviceAuthentication(t *testing.T) {
	clearCLIAuthEnvironment(t)
	t.Setenv("JETKVM_PASSWORD", "wrong-password")
	timeout := livePathConnectTimeout(t).String()

	for _, tc := range []struct {
		name string
		run  func(string, string) error
	}{
		{
			name: "screenshot",
			run: func(baseURL, outputPath string) error {
				return runScreenshot([]string{"--url", baseURL, "--timeout", timeout, "--output", outputPath})
			},
		},
		{
			name: "wait stable",
			run: func(baseURL, _ string) error {
				return runWaitStable([]string{"--url", baseURL, "--timeout", timeout})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd := sharedfake.Start(t, sharedfake.Options{Password: "correct-password"})
			outputPath := filepath.Join(t.TempDir(), "must-not-exist.png")
			out, err := captureStdout(t, func() error { return tc.run(fd.BaseURL(), outputPath) })
			if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindAuthFailed {
				t.Fatalf("authentication error kind = %q, want %q: %v", kind, jetkvm.ErrorKindAuthFailed, err)
			}
			if out != "" {
				t.Fatalf("authentication failure emitted success JSON: %q", out)
			}
			if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
				t.Fatalf("authentication failure created screenshot output: %v", err)
			}
			status, login, signaling, rpc := fd.Counts()
			if status != 1 || login != 1 || signaling != 0 || rpc != 0 {
				t.Fatalf("authentication protocol counts = status %d login %d signaling %d rpc %d, want 1/1/0/0",
					status, login, signaling, rpc)
			}
		})
	}
}

func TestRunWaitStableSettlesOnRepeatedLoopbackFrames(t *testing.T) {
	clearCLIAuthEnvironment(t)
	fd := sharedfake.Start(t, sharedfake.Options{})
	out, err := captureStdout(t, func() error {
		return runWaitStable([]string{
			"--url", fd.BaseURL(),
			"--timeout", livePathConnectTimeout(t).String(),
			"--threshold", "0",
			"--stable-frames", "1",
			"--poll-interval", "0",
		})
	})
	if err != nil {
		t.Fatalf("runWaitStable against static loopback video: %v", err)
	}
	var got struct {
		Settled             bool    `json:"settled"`
		FramesSampled       int     `json:"framesSampled"`
		FinalChangeFraction float64 `json:"finalChangeFraction"`
		Elapsed             string  `json:"elapsed"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("wait-stable output is not JSON: %v\n%s", err, out)
	}
	if !got.Settled || got.FramesSampled != 2 || got.FinalChangeFraction != 0 {
		t.Fatalf("wait-stable result = %+v, want two identical request-fresh frames to settle", got)
	}
	if elapsed, err := time.ParseDuration(got.Elapsed); err != nil || elapsed <= 0 || elapsed >= livePathConnectTimeout(t) {
		t.Fatalf("wait-stable elapsed = %q, want a positive duration below the command timeout: %v", got.Elapsed, err)
	}
	status, login, signaling, rpc := fd.Counts()
	if status != 1 || login != 0 || signaling != 1 || rpc != 0 {
		t.Fatalf("wait-stable protocol counts = status %d login %d signaling %d rpc %d, want 1/0/1/0",
			status, login, signaling, rpc)
	}
}

func stubFailingDecodeFFmpeg(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffmpeg")
	contents := `#!/bin/sh
set -eu
if [ "${1-}" = "-version" ]; then
  exit 0
fi
printf '%s\n' 'synthetic decode failure after frame capture' >&2
exit 1
`
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatalf("writing fake ffmpeg binary: %v", err)
	}
	t.Setenv("PATH", dir)
}

func TestRunWaitStableSurfacesDecodeFailureWithoutJSON(t *testing.T) {
	clearCLIAuthEnvironment(t)
	stubFailingDecodeFFmpeg(t)
	fd := sharedfake.Start(t, sharedfake.Options{})
	out, err := captureStdout(t, func() error {
		return runWaitStable([]string{
			"--url", fd.BaseURL(),
			"--timeout", livePathConnectTimeout(t).String(),
		})
	})
	if err == nil || !strings.Contains(err.Error(), "ffmpeg decode failed") ||
		!strings.Contains(err.Error(), "synthetic decode failure after frame capture") {
		t.Fatalf("runWaitStable decode error = %v, want bounded ffmpeg failure after capture", err)
	}
	if out != "" {
		t.Fatalf("failed wait-stable emitted success JSON: %q", out)
	}
	status, login, signaling, rpc := fd.Counts()
	if status != 1 || login != 0 || signaling != 1 || rpc != 0 {
		t.Fatalf("decode-failed wait-stable protocol counts = status %d login %d signaling %d rpc %d, want 1/0/1/0",
			status, login, signaling, rpc)
	}
}

func TestReadTextDeviceAdaptersCaptureFreshFramesAndMatch(t *testing.T) {
	clearCLIAuthEnvironment(t)
	fd := sharedfake.Start(t, sharedfake.Options{})
	timeout := livePathConnectTimeout(t)
	cf := &commonFlags{url: fd.BaseURL(), timeout: timeout}

	captureCtx, cancelCapture := context.WithTimeout(context.Background(), timeout)
	shot, err := captureReadTextScreenshot(captureCtx, cf)
	cancelCapture()
	if err != nil {
		t.Fatalf("captureReadTextScreenshot against loopback device: %v", err)
	}
	if !shot.Fresh || shot.CapturedAt.IsZero() {
		t.Fatalf("read-text capture metadata = %+v, want a request-fresh timestamped frame", shot.ScreenshotResult)
	}
	assertSyntheticRedPNG(t, shot.PNG)

	interval := 100 * time.Millisecond
	waitTimeout := min(timeout, jetkvm.MaxWaitForTextTimeout)
	engine := &fakeCLIOCREngine{text: "booting\nREADY\n"}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), timeout)
	defer cancelWait()
	result, err := waitForTextOnDevice(waitCtx, cf, jetkvm.WaitForTextOptions{
		Text:     "READY",
		Interval: &interval,
		Timeout:  &waitTimeout,
	}, engine)
	if err != nil {
		t.Fatalf("waitForTextOnDevice against loopback device: %v", err)
	}
	if !result.Matched || result.Match != "READY" || result.TimedOut || result.FrameCount != 1 {
		t.Fatalf("wait-for-text result = %+v, want a first-frame literal match", result)
	}
	if engine.checkCalls != 1 || engine.readCalls != 1 {
		t.Fatalf("wait-for-text OCR calls = check %d read %d, want 1/1", engine.checkCalls, engine.readCalls)
	}
	assertSyntheticRedPNG(t, engine.input)
	status, login, signaling, rpc := fd.Counts()
	if status != 2 || login != 0 || signaling != 2 || rpc != 0 {
		t.Fatalf("read-text adapter protocol counts = status %d login %d signaling %d rpc %d, want 2/0/2/0",
			status, login, signaling, rpc)
	}
}

func TestReadTextDeviceAdaptersRejectAuthenticationBeforeOCR(t *testing.T) {
	clearCLIAuthEnvironment(t)
	t.Setenv("JETKVM_PASSWORD", "wrong-password")
	fd := sharedfake.Start(t, sharedfake.Options{Password: "correct-password"})
	timeout := livePathConnectTimeout(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cf := &commonFlags{url: fd.BaseURL(), timeout: timeout}

	if shot, err := captureReadTextScreenshot(ctx, cf); jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindAuthFailed {
		t.Fatalf("captureReadTextScreenshot auth result = shot %+v, error %v; want auth-failed", shot, err)
	}
	interval, waitTimeout := 100*time.Millisecond, time.Second
	engine := &fakeCLIOCREngine{text: "must not be observed"}
	result, err := waitForTextOnDevice(ctx, cf, jetkvm.WaitForTextOptions{
		Text:     "observed",
		Interval: &interval,
		Timeout:  &waitTimeout,
	}, engine)
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindAuthFailed {
		t.Fatalf("waitForTextOnDevice auth error kind = %q, want %q: %v", kind, jetkvm.ErrorKindAuthFailed, err)
	}
	if result != (jetkvm.WaitForTextResult{}) {
		t.Fatalf("failed wait-for-text result = %+v, want zero result", result)
	}
	if engine.checkCalls != 0 || engine.readCalls != 0 || len(engine.input) != 0 {
		t.Fatalf("authentication failure reached OCR: check=%d read=%d input=%d bytes",
			engine.checkCalls, engine.readCalls, len(engine.input))
	}
	status, login, signaling, rpc := fd.Counts()
	if status != 2 || login != 2 || signaling != 0 || rpc != 0 {
		t.Fatalf("read-text authentication counts = status %d login %d signaling %d rpc %d, want 2/2/0/0",
			status, login, signaling, rpc)
	}
}
