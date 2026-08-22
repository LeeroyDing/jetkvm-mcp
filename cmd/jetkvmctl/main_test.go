package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/buildinfo"
	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

type fakeCLIOCREngine struct {
	checkErr   error
	readErr    error
	text       string
	checkCalls int
	readCalls  int
	input      []byte
}

func TestReleaseAllUsageStatesTransportProofBoundary(t *testing.T) {
	var output bytes.Buffer
	printUsage(&output)
	usage := output.String()

	for _, want := range []string{
		"release-all sends canonical neutral reports for every input interface the session",
		"acknowledged by the peer SCTP transport",
		"does not prove firmware USB application or attached-host action",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage does not contain %q", want)
		}
	}
	if strings.Contains(usage, "release-all clears every held") {
		t.Errorf("usage retains the release-all host-state overclaim")
	}
}

func TestReleaseAllSuccessResultStatesTransportProofBoundary(t *testing.T) {
	result := releaseAllSuccessResult()
	if got := result["sent"]; got != "release-all" {
		t.Errorf("sent = %#v, want release-all", got)
	}
	if got := result["peerTransportAcknowledged"]; got != true {
		t.Errorf("peerTransportAcknowledged = %#v, want true", got)
	}
	if _, ok := result["cursorMoved"]; ok {
		t.Error("release-all result must not claim observed cursor movement")
	}
	if len(result) != 2 {
		t.Errorf("release-all result = %#v, want only sent and peerTransportAcknowledged", result)
	}
}

func (e *fakeCLIOCREngine) CheckAvailable(context.Context) error {
	e.checkCalls++
	return e.checkErr
}

func (e *fakeCLIOCREngine) ReadText(_ context.Context, pngData []byte) (string, error) {
	e.readCalls++
	e.input = append([]byte(nil), pngData...)
	return e.text, e.readErr
}

func makeCLIReadTextScreenshot(t testing.TB, width, height int) jetkvm.Screenshot {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 17), G: uint8(y * 29), B: uint8(x + y), A: 0xff})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("encoding read-text screenshot fixture: %v", err)
	}
	return jetkvm.Screenshot{
		ScreenshotResult: jetkvm.ScreenshotResult{
			Width: width, Height: height, CapturedAt: time.Now(), Fresh: true,
		},
		PNG: encoded.Bytes(),
	}
}

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

	creds, err := credentialsFromEnv(context.Background(), keychainTestFlags())
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

	creds, err := credentialsFromEnv(context.Background(), keychainTestFlags())
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

	creds, err := credentialsFromEnv(context.Background(), keychainTestFlags())
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

	_, err := credentialsFromEnv(context.Background(), keychainTestFlags())
	if !errors.Is(err, errMalformedKeychainPassword) {
		t.Fatalf("error = %v, want errMalformedKeychainPassword", err)
	}
}

func TestPasswordStdinOverridesAuthTokenAndSkipsConfiguredSources(t *testing.T) {
	const stdinPassword = "explicit-stdin-password"
	t.Setenv("JETKVM_AUTH_TOKEN", "environment-auth-token")
	t.Setenv("JETKVM_PASSWORD", "environment-password")
	t.Setenv("JETKVM_PASSWORD_KEYCHAIN_SERVICE", "jetkvmctl-tests")
	t.Setenv("JETKVM_PASSWORD_KEYCHAIN_ACCOUNT", "fake-device")
	marker := filepath.Join(t.TempDir(), "security-invoked")
	stubSecurity(t, `touch "`+marker+`"
printf '%s\n' 'keychain-password'`)

	inputPath := filepath.Join(t.TempDir(), "password-stdin")
	if err := os.WriteFile(inputPath, []byte(stdinPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close() })
	originalStdin := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = originalStdin })

	creds, err := credentialsFromEnv(context.Background(), &commonFlags{passwordStdin: true})
	if err != nil {
		t.Fatalf("credentialsFromEnv: %v", err)
	}
	if !creds.AuthToken.Empty() {
		t.Fatal("explicit --password-stdin retained JETKVM_AUTH_TOKEN, which would silently win at connect time")
	}
	if got := creds.Password.Expose(); got != stdinPassword {
		t.Fatalf("password = %q, want explicit stdin value", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("explicit --password-stdin consulted configured Keychain source: %v", err)
	}
}

func TestKeychainLookupStopsWhenCommandContextIsCanceled(t *testing.T) {
	configureKeychainTest(t)
	// A fallback must not swallow cancellation and let a command proceed after
	// its own deadline or root context has expired.
	t.Setenv("JETKVM_PASSWORD", "legacy-fallback-must-not-win")
	marker := filepath.Join(t.TempDir(), "security-started")
	stubSecurity(t, `: > "`+marker+`"
exec sleep 30`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := passwordFromConfiguredSources(ctx)
		done <- err
	}()

	deadline := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	started := false
	for !started {
		select {
		case <-ticker.C:
			_, err := os.Stat(marker)
			started = err == nil
		case <-deadline.C:
			t.Fatal("blocking Keychain stub did not start")
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Keychain cancellation error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Keychain lookup outlived the canceled command context")
	}
}

func TestSendControlAndReleaseMakesNeutralizationPartOfSuccess(t *testing.T) {
	sendFailure := errors.New("send failed")
	releaseFailure := errors.New("neutralization unverified")
	for _, tc := range []struct {
		name       string
		sendErr    error
		releaseErr error
	}{
		{name: "success"},
		{name: "send failure", sendErr: sendFailure},
		{name: "release failure", releaseErr: releaseFailure},
		{name: "both failures", sendErr: sendFailure, releaseErr: releaseFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sendCalls := 0
			releaseCalls := 0
			err := sendControlAndRelease(
				func() error { sendCalls++; return tc.sendErr },
				func() error { releaseCalls++; return tc.releaseErr },
			)
			if sendCalls != 1 || releaseCalls != 1 {
				t.Fatalf("calls = send %d release %d, want exactly one each", sendCalls, releaseCalls)
			}
			if (tc.sendErr == nil && tc.releaseErr == nil) != (err == nil) {
				t.Fatalf("result = %v for send/release %v/%v", err, tc.sendErr, tc.releaseErr)
			}
			for _, want := range []error{tc.sendErr, tc.releaseErr} {
				if want != nil && !errors.Is(err, want) {
					t.Errorf("result %v does not retain %v", err, want)
				}
			}
		})
	}
}

func TestSendControlHoldAndReleasePressesHoldsAndReleases(t *testing.T) {
	const holdMS = 5
	events := make([]string, 0, 2)
	started := time.Now()
	err := sendControlHoldAndRelease(
		context.Background(),
		holdMS,
		func() error {
			events = append(events, "press")
			return nil
		},
		func() error {
			events = append(events, "release")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("press-hold-release failed: %v", err)
	}
	if got := strings.Join(events, ","); got != "press,release" {
		t.Fatalf("hold events = %q, want press,release", got)
	}
	if elapsed := time.Since(started); elapsed < holdMS*time.Millisecond {
		t.Fatalf("hold returned after %v, before requested %v", elapsed, holdMS*time.Millisecond)
	}
}

func TestSendControlHoldAndReleaseGuaranteesReleaseOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make([]string, 0, 2)
	releaseCalls := 0

	err := sendControlHoldAndRelease(
		ctx,
		jetkvm.MaxHoldMS,
		func() error {
			events = append(events, "down")
			cancel()
			return nil
		},
		func() error {
			releaseCalls++
			events = append(events, "release")
			return nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled hold error = %v, want context.Canceled", err)
	}
	if releaseCalls != 1 {
		t.Fatalf("release calls = %d, want exactly 1", releaseCalls)
	}
	if strings.Join(events, ",") != "down,release" {
		t.Fatalf("hold events = %v, want down then release", events)
	}
}

func TestSendControlHoldAndReleasePreservesSendAndReleaseErrors(t *testing.T) {
	sendFailure := errors.New("key-down failed")
	releaseFailure := errors.New("neutralization unverified")
	for _, tc := range []struct {
		name       string
		sendErr    error
		releaseErr error
	}{
		{name: "send failure", sendErr: sendFailure},
		{name: "release failure", releaseErr: releaseFailure},
		{name: "both failures", sendErr: sendFailure, releaseErr: releaseFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sendCalls := 0
			releaseCalls := 0
			err := sendControlHoldAndRelease(
				context.Background(),
				1,
				func() error { sendCalls++; return tc.sendErr },
				func() error { releaseCalls++; return tc.releaseErr },
			)
			if sendCalls != 1 || releaseCalls != 1 {
				t.Fatalf("calls = send %d release %d, want exactly one each", sendCalls, releaseCalls)
			}
			for _, want := range []error{tc.sendErr, tc.releaseErr} {
				if want != nil && !errors.Is(err, want) {
					t.Errorf("result %v does not retain %v", err, want)
				}
			}
		})
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

func TestReadTextPrintsExactOCRTextAndPlumbsRegionScale(t *testing.T) {
	engine := &fakeCLIOCREngine{text: "Login:\nPassword: "}
	shot := makeCLIReadTextScreenshot(t, 8, 6)
	var output strings.Builder
	var events []string
	captureCalls := 0

	err := runReadTextWithDependencies([]string{
		"--url", "http://device.invalid",
		"--scale", "0.5",
		"--region", "2, 1, 4, 4",
	}, readTextDependencies{
		checkDecoder: func(context.Context) error {
			events = append(events, "decoder-check")
			return nil
		},
		ocr: engine,
		capture: func(_ context.Context, cf *commonFlags) (jetkvm.Screenshot, error) {
			events = append(events, "capture")
			captureCalls++
			if cf.allowControl {
				t.Error("read-text enabled control on its device connection")
			}
			return shot, nil
		},
		stdout: &output,
	})
	if err != nil {
		t.Fatalf("runReadTextWithDependencies: %v", err)
	}
	if got := output.String(); got != engine.text {
		t.Fatalf("read-text stdout = %q, want exact OCR output %q", got, engine.text)
	}
	if captureCalls != 1 {
		t.Fatalf("fresh-frame capture calls = %d, want exactly 1", captureCalls)
	}
	if engine.checkCalls != 1 || engine.readCalls != 1 {
		t.Fatalf("OCR calls = check %d read %d, want 1/1", engine.checkCalls, engine.readCalls)
	}
	config, err := png.DecodeConfig(bytes.NewReader(engine.input))
	if err != nil {
		t.Fatalf("OCR input is not a PNG: %v", err)
	}
	if config.Width != 2 || config.Height != 2 {
		t.Fatalf("OCR input dimensions = %dx%d, want cropped 4x4 scaled to 2x2", config.Width, config.Height)
	}
	if !shot.Fresh {
		t.Fatal("test fixture was not marked request-fresh")
	}
	if got := strings.Join(events, ","); got != "decoder-check,capture" {
		t.Fatalf("read-text event order = %q, want decoder-check,capture", got)
	}
}

func TestReadTextUnavailableEngineReturnsTypedErrorBeforeCapture(t *testing.T) {
	unavailable := &jetkvm.OCRUnavailableError{}
	engine := &fakeCLIOCREngine{checkErr: unavailable}
	captureCalls := 0
	var output strings.Builder

	err := runReadTextWithDependencies(
		[]string{"--url", "http://device.invalid"},
		readTextDependencies{
			checkDecoder: func(context.Context) error { return nil },
			ocr:          engine,
			capture: func(context.Context, *commonFlags) (jetkvm.Screenshot, error) {
				captureCalls++
				return jetkvm.Screenshot{}, nil
			},
			stdout: &output,
		},
	)
	if !errors.Is(err, jetkvm.ErrOCRUnavailable) {
		t.Fatalf("read-text error = %v, want typed unavailable error", err)
	}
	if captureCalls != 0 || engine.readCalls != 0 {
		t.Fatalf("unavailable OCR crossed capture/read boundary: capture=%d read=%d", captureCalls, engine.readCalls)
	}
	if output.Len() != 0 {
		t.Fatalf("unavailable OCR wrote stdout %q", output.String())
	}
}

func TestReadTextDecoderFailureStopsBeforeOCRAndCapture(t *testing.T) {
	decoderErr := errors.New("decoder preflight failed")
	engine := &fakeCLIOCREngine{text: "must not run"}
	captureCalls := 0
	var output strings.Builder

	err := runReadTextWithDependencies(
		[]string{"--url", "http://device.invalid"},
		readTextDependencies{
			checkDecoder: func(context.Context) error { return decoderErr },
			ocr:          engine,
			capture: func(context.Context, *commonFlags) (jetkvm.Screenshot, error) {
				captureCalls++
				return jetkvm.Screenshot{}, nil
			},
			stdout: &output,
		},
	)
	if !errors.Is(err, decoderErr) {
		t.Fatalf("read-text error = %v, want decoder preflight error", err)
	}
	if engine.checkCalls != 0 || engine.readCalls != 0 || captureCalls != 0 || output.Len() != 0 {
		t.Fatalf("decoder failure crossed a later boundary: OCR-check=%d capture=%d OCR-read=%d stdout=%q",
			engine.checkCalls, captureCalls, engine.readCalls, output.String())
	}
}

func TestReadTextOCRFailureWritesNoSuccessOutput(t *testing.T) {
	ocrErr := errors.New("OCR recognition failed")
	engine := &fakeCLIOCREngine{readErr: ocrErr}
	captureCalls := 0
	var output strings.Builder

	err := runReadTextWithDependencies(
		[]string{"--url", "http://device.invalid"},
		readTextDependencies{
			checkDecoder: func(context.Context) error { return nil },
			ocr:          engine,
			capture: func(context.Context, *commonFlags) (jetkvm.Screenshot, error) {
				captureCalls++
				return makeCLIReadTextScreenshot(t, 8, 6), nil
			},
			stdout: &output,
		},
	)
	if !errors.Is(err, ocrErr) {
		t.Fatalf("read-text error = %v, want OCR recognition error", err)
	}
	if engine.checkCalls != 1 || engine.readCalls != 1 || captureCalls != 1 {
		t.Fatalf("OCR failure calls = check %d capture %d read %d, want 1/1/1",
			engine.checkCalls, captureCalls, engine.readCalls)
	}
	if output.Len() != 0 {
		t.Fatalf("OCR failure wrote success output %q", output.String())
	}
}

func TestReadTextScaleAboveOneDoesNotUpscale(t *testing.T) {
	engine := &fakeCLIOCREngine{text: "screen"}
	var output strings.Builder
	err := runReadTextWithDependencies(
		[]string{"--url", "http://device.invalid", "--scale", "4"},
		readTextDependencies{
			checkDecoder: func(context.Context) error { return nil },
			ocr:          engine,
			capture: func(context.Context, *commonFlags) (jetkvm.Screenshot, error) {
				return makeCLIReadTextScreenshot(t, 8, 6), nil
			},
			stdout: &output,
		},
	)
	if err != nil {
		t.Fatalf("read-text with scale above one: %v", err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(engine.input))
	if err != nil {
		t.Fatalf("decoding OCR input: %v", err)
	}
	if config.Width != 8 || config.Height != 6 {
		t.Fatalf("OCR input dimensions = %dx%d, want original 8x6", config.Width, config.Height)
	}
	if output.String() != engine.text {
		t.Fatalf("read-text stdout = %q, want %q", output.String(), engine.text)
	}
}

func TestReadTextRejectsInvalidOptionsBeforePreflightOrCapture(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "zero scale", args: []string{"--scale", "0"}},
		{name: "negative scale", args: []string{"--scale", "-0.5"}},
		{name: "NaN scale", args: []string{"--scale", "NaN"}},
		{name: "infinite scale", args: []string{"--scale", "+Inf"}},
		{name: "short region", args: []string{"--region", "0,0,1"}},
		{name: "negative origin", args: []string{"--region", "-1,0,1,1"}},
		{name: "zero width", args: []string{"--region", "0,0,0,1"}},
		{name: "overflow", args: []string{"--region", "2147483648,0,1,1"}},
		{name: "control flag absent", args: []string{"--allow-control"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoderChecks := 0
			captureCalls := 0
			engine := &fakeCLIOCREngine{text: "must not run"}
			args := append([]string{"--url", "http://device.invalid"}, tc.args...)
			err := runReadTextWithDependencies(args, readTextDependencies{
				checkDecoder: func(context.Context) error { decoderChecks++; return nil },
				ocr:          engine,
				capture: func(context.Context, *commonFlags) (jetkvm.Screenshot, error) {
					captureCalls++
					return makeCLIReadTextScreenshot(t, 2, 2), nil
				},
				stdout: io.Discard,
			})
			if err == nil {
				t.Fatal("read-text accepted invalid options")
			}
			if decoderChecks != 0 || engine.checkCalls != 0 || captureCalls != 0 || engine.readCalls != 0 {
				t.Fatalf("invalid options caused side effects: decoder=%d OCR-check=%d capture=%d OCR-read=%d",
					decoderChecks, engine.checkCalls, captureCalls, engine.readCalls)
			}
		})
	}
}

func FuzzParseReadTextRegion(f *testing.F) {
	for _, seed := range []string{
		"0,0,1,1",
		" 12, 34, 56, 78 ",
		"2147483647,2147483647,2147483647,2147483647",
		"-1,0,1,1",
		"0,0,0,1",
		"2147483648,0,1,1",
		"0,0,1",
		"0,0,1,1,1",
		"not,a,region,value",
		"0,0,1,1\x00",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		region, err := parseReadTextRegion(value)
		if err != nil {
			return
		}
		if region.X < 0 || region.Y < 0 || region.Width <= 0 || region.Height <= 0 {
			t.Fatalf("successful parse returned invalid region %+v", region)
		}
		if region.X > maxReadTextRegionValue || region.Y > maxReadTextRegionValue ||
			region.Width > maxReadTextRegionValue || region.Height > maxReadTextRegionValue {
			t.Fatalf("successful parse exceeded supported range: %+v", region)
		}
		canonical := fmt.Sprintf("%d,%d,%d,%d", region.X, region.Y, region.Width, region.Height)
		roundTrip, err := parseReadTextRegion(canonical)
		if err != nil || roundTrip != region {
			t.Fatalf("region round trip = %+v, %v; want %+v", roundTrip, err, region)
		}
	})
}

func TestWaitStableMissingFFmpegAvoidsDeviceSession(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("PATH", t.TempDir())

	err := runWaitStable([]string{"--url", server.URL})
	if err == nil || !strings.Contains(err.Error(), "FFmpeg") {
		t.Fatalf("wait-stable error = %v, want actionable FFmpeg preflight failure", err)
	}
	if requests != 0 {
		t.Fatalf("missing FFmpeg caused %d device requests, want 0", requests)
	}
}

// TestWaitStableValidationRunsBeforeSideEffects pins both the public bounds
// and the CLI ordering. Invalid options must be rejected before decoder
// preflight, credential lookup, or a device request; flag.Float64 accepts NaN
// and infinities, so those values need explicit coverage too.
func TestWaitStableValidationRunsBeforeSideEffects(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	for _, tc := range []struct {
		name  string
		field string
		args  []string
	}{
		{name: "negative threshold", field: "threshold", args: []string{"--threshold", "-0.01"}},
		{name: "threshold above one", field: "threshold", args: []string{"--threshold", "1.01"}},
		{name: "NaN threshold", field: "threshold", args: []string{"--threshold", "NaN"}},
		{name: "infinite threshold", field: "threshold", args: []string{"--threshold", "+Inf"}},
		{name: "zero stable frames", field: "stable frame count", args: []string{"--stable-frames", "0"}},
		{name: "negative stable frames", field: "stable frame count", args: []string{"--stable-frames", "-1"}},
		{name: "negative poll interval", field: "poll interval", args: []string{"--poll-interval", "-1ms"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests = 0
			args := append([]string{"--url", server.URL}, tc.args...)
			err := runWaitStable(args)
			if err == nil {
				t.Fatal("wait-stable accepted invalid options")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error = %v, want field name %q", err, tc.field)
			}
			if requests != 0 {
				t.Fatalf("invalid options caused %d device requests, want 0", requests)
			}
		})
	}
}

type fakeCLIWaitForTextOCREngine struct {
	checkCalls int
	readCalls  int
	checkErr   error
}

func (e *fakeCLIWaitForTextOCREngine) CheckAvailable(context.Context) error {
	e.checkCalls++
	return e.checkErr
}

func (e *fakeCLIWaitForTextOCREngine) ReadText(context.Context, []byte) (string, error) {
	e.readCalls++
	return "", nil
}

func TestWaitForTextRequiresTextBeforeSideEffects(t *testing.T) {
	decoderChecks := 0
	runnerCalls := 0
	engine := &fakeCLIWaitForTextOCREngine{}
	err := runWaitForTextWithDependencies(
		[]string{"--url", "http://device.invalid"},
		waitForTextDependencies{
			checkDecoder: func(context.Context) error { decoderChecks++; return nil },
			ocr:          engine,
			run: func(context.Context, *commonFlags, jetkvm.WaitForTextOptions, jetkvm.OCREngine) (jetkvm.WaitForTextResult, error) {
				runnerCalls++
				return jetkvm.WaitForTextResult{}, nil
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "--text") {
		t.Fatalf("wait-for-text without --text = %v, want required-flag error", err)
	}
	if decoderChecks != 0 || engine.checkCalls != 0 || engine.readCalls != 0 || runnerCalls != 0 {
		t.Fatalf("missing text caused side effects: decoder=%d OCR-check=%d OCR-read=%d runner=%d",
			decoderChecks, engine.checkCalls, engine.readCalls, runnerCalls)
	}

	exitCode, dispatchErr := runCLI([]string{"wait-for-text", "--url", "http://device.invalid"})
	if exitCode != 1 || dispatchErr == nil || !strings.Contains(dispatchErr.Error(), "--text") {
		t.Fatalf("runCLI wait-for-text dispatch = %d, %v; want required-flag failure", exitCode, dispatchErr)
	}
}

func TestWaitForTextValidationRunsBeforeSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "invalid regex", args: []string{"--text", "(", "--regex"}},
		{name: "zero interval", args: []string{"--text", "ready", "--interval", "0"}},
		{name: "negative interval", args: []string{"--text", "ready", "--interval", "-1ms"}},
		{name: "interval below minimum", args: []string{"--text", "ready", "--interval", "99ms"}},
		{name: "interval above maximum", args: []string{"--text", "ready", "--interval", "10001ms"}},
		{name: "timeout below minimum", args: []string{"--text", "ready", "--timeout", "99ms"}},
		{name: "timeout above maximum", args: []string{"--text", "ready", "--timeout", "5m1ms"}},
		{name: "interval above timeout", args: []string{"--text", "ready", "--interval", "2s", "--timeout", "1s"}},
		{name: "oversized text", args: []string{"--text", strings.Repeat("x", jetkvm.MaxWaitForTextTextRunes+1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoderChecks := 0
			runnerCalls := 0
			engine := &fakeCLIWaitForTextOCREngine{}
			args := append([]string{"--url", "http://device.invalid"}, tc.args...)
			err := runWaitForTextWithDependencies(args, waitForTextDependencies{
				checkDecoder: func(context.Context) error { decoderChecks++; return nil },
				ocr:          engine,
				run: func(context.Context, *commonFlags, jetkvm.WaitForTextOptions, jetkvm.OCREngine) (jetkvm.WaitForTextResult, error) {
					runnerCalls++
					return jetkvm.WaitForTextResult{}, nil
				},
			})
			if err == nil {
				t.Fatal("wait-for-text accepted invalid options")
			}
			if decoderChecks != 0 || engine.checkCalls != 0 || engine.readCalls != 0 || runnerCalls != 0 {
				t.Fatalf("invalid options caused side effects: decoder=%d OCR-check=%d OCR-read=%d runner=%d",
					decoderChecks, engine.checkCalls, engine.readCalls, runnerCalls)
			}
		})
	}
}

func TestWaitForTextInvalidOptionsPrecedeURLValidation(t *testing.T) {
	t.Setenv("JETKVM_URL", "")
	engine := &fakeCLIWaitForTextOCREngine{}
	err := runWaitForTextWithDependencies(
		[]string{"--text", "ready", "--interval", "0"},
		waitForTextDependencies{
			checkDecoder: func(context.Context) error {
				t.Fatal("invalid options reached decoder preflight")
				return nil
			},
			ocr: engine,
			run: func(context.Context, *commonFlags, jetkvm.WaitForTextOptions, jetkvm.OCREngine) (jetkvm.WaitForTextResult, error) {
				t.Fatal("invalid options reached device runner")
				return jetkvm.WaitForTextResult{}, nil
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "interval") || strings.Contains(err.Error(), "--url") {
		t.Fatalf("invalid options with missing URL = %v, want option error before URL validation", err)
	}
	if engine.checkCalls != 0 || engine.readCalls != 0 {
		t.Fatalf("invalid options reached OCR: check=%d read=%d", engine.checkCalls, engine.readCalls)
	}
}

func TestWaitForTextPreflightsDecoderAndOCRBeforeRunner(t *testing.T) {
	decoderErr := errors.New("decoder unavailable")
	ocrErr := errors.New("OCR unavailable")
	for _, tc := range []struct {
		name              string
		decoderErr        error
		ocrErr            error
		wantDecoderChecks int
		wantOCRChecks     int
	}{
		{name: "decoder failure", decoderErr: decoderErr, wantDecoderChecks: 1},
		{name: "OCR failure", ocrErr: ocrErr, wantDecoderChecks: 1, wantOCRChecks: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoderChecks := 0
			runnerCalls := 0
			engine := &fakeCLIWaitForTextOCREngine{checkErr: tc.ocrErr}
			err := runWaitForTextWithDependencies(
				[]string{"--url", "http://device.invalid", "--text", "ready"},
				waitForTextDependencies{
					checkDecoder: func(context.Context) error { decoderChecks++; return tc.decoderErr },
					ocr:          engine,
					run: func(context.Context, *commonFlags, jetkvm.WaitForTextOptions, jetkvm.OCREngine) (jetkvm.WaitForTextResult, error) {
						runnerCalls++
						return jetkvm.WaitForTextResult{}, nil
					},
				},
			)
			wantErr := tc.decoderErr
			if wantErr == nil {
				wantErr = tc.ocrErr
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("preflight error = %v, want decoder/OCR error", err)
			}
			if decoderChecks != tc.wantDecoderChecks || engine.checkCalls != tc.wantOCRChecks || runnerCalls != 0 {
				t.Fatalf("preflight calls = decoder %d OCR %d runner %d, want %d/%d/0",
					decoderChecks, engine.checkCalls, runnerCalls, tc.wantDecoderChecks, tc.wantOCRChecks)
			}
		})
	}
}

func TestWaitForTextRendersMatchAndTimeoutResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result jetkvm.WaitForTextResult
	}{
		{
			name: "match",
			result: jetkvm.WaitForTextResult{
				Matched: true, Match: "READY", Elapsed: 1250 * time.Millisecond, FrameCount: 3,
			},
		},
		{
			name: "timeout",
			result: jetkvm.WaitForTextResult{
				TimedOut: true, Elapsed: 3 * time.Second, FrameCount: 7,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoderChecks := 0
			runnerCalls := 0
			engine := &fakeCLIWaitForTextOCREngine{}
			out, err := captureStdout(t, func() error {
				return runWaitForTextWithDependencies(
					[]string{
						"--url", "http://device.invalid",
						"--text", "READY|DONE",
						"--regex",
						"--interval", "750ms",
						"--timeout", "3s",
					},
					waitForTextDependencies{
						checkDecoder: func(context.Context) error { decoderChecks++; return nil },
						ocr:          engine,
						run: func(ctx context.Context, cf *commonFlags, opts jetkvm.WaitForTextOptions, gotEngine jetkvm.OCREngine) (jetkvm.WaitForTextResult, error) {
							runnerCalls++
							if _, hasDeadline := ctx.Deadline(); !hasDeadline {
								t.Error("wait-for-text setup context has no overall command deadline")
							}
							if cf.url != "http://device.invalid" || cf.timeout != 3*time.Second {
								t.Errorf("common flags = url %q timeout %s", cf.url, cf.timeout)
							}
							if opts.Text != "READY|DONE" || !opts.Regex || opts.Interval == nil || *opts.Interval != 750*time.Millisecond ||
								opts.Timeout == nil || *opts.Timeout != 3*time.Second {
								t.Errorf("wait options = %+v", opts)
							}
							if gotEngine != engine {
								t.Error("runner did not receive the preflighted OCR engine")
							}
							return tc.result, nil
						},
					},
				)
			})
			if err != nil {
				t.Fatalf("runWaitForTextWithDependencies: %v", err)
			}
			if decoderChecks != 1 || engine.checkCalls != 1 || engine.readCalls != 0 || runnerCalls != 1 {
				t.Fatalf("calls = decoder %d OCR-check %d OCR-read %d runner %d, want 1/1/0/1",
					decoderChecks, engine.checkCalls, engine.readCalls, runnerCalls)
			}

			var got struct {
				Matched    bool   `json:"matched"`
				Match      string `json:"match"`
				TimedOut   bool   `json:"timedOut"`
				Elapsed    string `json:"elapsed"`
				FrameCount int    `json:"frameCount"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("wait-for-text output is not JSON: %v\n%s", err, out)
			}
			if got.Matched != tc.result.Matched || got.Match != tc.result.Match || got.TimedOut != tc.result.TimedOut ||
				got.Elapsed != tc.result.Elapsed.String() || got.FrameCount != tc.result.FrameCount {
				t.Errorf("wait-for-text JSON = %+v, want result %+v", got, tc.result)
			}
		})
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

	cases := map[string][]string{
		"keypress":     {"keypress", "--key", "4"},
		"type":         {"type", "--text", "hello"},
		"key-combo":    {"key-combo", "--combo", "ctrl+c"},
		"hold-key":     {"hold-key", "--combo", "ctrl+c", "--hold-ms", "100"},
		"key-sequence": {"key-sequence", "--combo", "ctrl+c"},
		"mouse-button": {"mouse-button", "--button", "left", "--action", "press"},
		"mouse-move":   {"mouse-move", "--x", "1", "--y", "1"},
		"scroll":       {"scroll", "--dy", "1"},
		"click":        {"click", "--x", "1", "--y", "1"},
		"double-click": {"double-click", "--x", "1", "--y", "1"},
		"drag":         {"drag", "--x1", "1", "--y1", "1", "--x2", "2", "--y2", "2"},
		"release-all":  {"release-all"},
	}
	for name, args := range cases {
		exitCode, err := runCLI(args)
		if exitCode != 1 || err == nil {
			t.Errorf("%s dispatch without --allow-control = %d, %v", name, exitCode, err)
			continue
		}
		if !strings.Contains(err.Error(), "--allow-control") {
			t.Errorf("%s error should name the missing gate, got: %v", name, err)
		}
	}
}

func TestMouseButtonParsesExactNamesAndPrintsSummary(t *testing.T) {
	for _, tc := range []struct {
		button    string
		action    string
		wantMask  byte
		wantPress bool
	}{
		{button: "left", action: "press", wantMask: jetkvm.MouseButtonLeft, wantPress: true},
		{button: "left", action: "release", wantMask: jetkvm.MouseButtonLeft},
		{button: "right", action: "press", wantMask: jetkvm.MouseButtonRight, wantPress: true},
		{button: "right", action: "release", wantMask: jetkvm.MouseButtonRight},
		{button: "middle", action: "press", wantMask: jetkvm.MouseButtonMiddle, wantPress: true},
		{button: "middle", action: "release", wantMask: jetkvm.MouseButtonMiddle},
	} {
		t.Run(tc.button+"/"+tc.action, func(t *testing.T) {
			sendCalls := 0
			out, err := captureStdout(t, func() error {
				return runMouseButtonWithSender([]string{
					"--url", "http://device.invalid",
					"--timeout", "2s",
					"--allow-control",
					"--button", tc.button,
					"--action", tc.action,
				}, func(ctx context.Context, cf *commonFlags, buttonMask byte, press bool) error {
					sendCalls++
					if cf.url != "http://device.invalid" || cf.timeout != 2*time.Second || !cf.allowControl {
						t.Errorf("parsed common flags = %+v", cf)
					}
					if _, ok := ctx.Deadline(); !ok {
						t.Error("mouse-button sender context has no deadline")
					}
					if buttonMask != tc.wantMask || press != tc.wantPress {
						t.Errorf("resolved mouse button = mask %#02x press %t, want %#02x/%t", buttonMask, press, tc.wantMask, tc.wantPress)
					}
					return nil
				})
			})
			if err != nil {
				t.Fatalf("runMouseButtonWithSender: %v", err)
			}
			if sendCalls != 1 {
				t.Fatalf("sender calls = %d, want 1", sendCalls)
			}

			var summary struct {
				Sent   string `json:"sent"`
				Button string `json:"button"`
				Action string `json:"action"`
			}
			if err := json.Unmarshal([]byte(out), &summary); err != nil {
				t.Fatalf("mouse-button output is not JSON: %v\n%s", err, out)
			}
			if summary.Sent != "mouse-button" || summary.Button != tc.button || summary.Action != tc.action {
				t.Errorf("mouse-button output = %+v", summary)
			}
		})
	}
}

func TestMouseButtonRejectsMissingAndUnknownNamesBeforeSend(t *testing.T) {
	for name, args := range map[string][]string{
		"missing button":   {"--action", "press"},
		"missing action":   {"--button", "left"},
		"empty button":     {"--button", "", "--action", "press"},
		"empty action":     {"--button", "left", "--action", ""},
		"unknown button":   {"--button", "back", "--action", "press"},
		"unknown action":   {"--button", "left", "--action", "toggle"},
		"uppercase button": {"--button", "Left", "--action", "press"},
		"uppercase action": {"--button", "left", "--action", "Press"},
	} {
		t.Run(name, func(t *testing.T) {
			sendCalls := 0
			err := runMouseButtonWithSender(
				append([]string{"--url", "http://device.invalid", "--allow-control"}, args...),
				func(context.Context, *commonFlags, byte, bool) error {
					sendCalls++
					return nil
				},
			)
			if err == nil {
				t.Fatal("mouse-button accepted invalid parameters")
			}
			if sendCalls != 0 {
				t.Fatalf("invalid parameters made %d sender calls, want 0", sendCalls)
			}
		})
	}
}

func TestMouseButtonSenderFailureDoesNotPrintSuccess(t *testing.T) {
	wantErr := errors.New("synthetic mouse-button failure")
	out, err := captureStdout(t, func() error {
		return runMouseButtonWithSender(
			[]string{"--url", "http://device.invalid", "--allow-control", "--button", "right", "--action", "press"},
			func(context.Context, *commonFlags, byte, bool) error { return wantErr },
		)
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("runMouseButtonWithSender error = %v, want %v", err, wantErr)
	}
	if out != "" {
		t.Fatalf("failed mouse-button printed success output: %q", out)
	}
}

func TestSendMouseButtonReportUsesZeroDelta(t *testing.T) {
	for _, buttons := range []byte{0, jetkvm.MouseButtonLeft, jetkvm.MouseButtonRight, jetkvm.MouseButtonMiddle} {
		t.Run(strconv.Itoa(int(buttons)), func(t *testing.T) {
			calls := 0
			err := sendMouseButtonReport(func(dx, dy int8, gotButtons byte) error {
				calls++
				if dx != 0 || dy != 0 || gotButtons != buttons {
					t.Errorf("mouse report = dx %d dy %d buttons %#02x, want 0/0/%#02x", dx, dy, gotButtons, buttons)
				}
				return nil
			}, buttons)
			if err != nil {
				t.Fatalf("sendMouseButtonReport: %v", err)
			}
			if calls != 1 {
				t.Fatalf("send calls = %d, want 1", calls)
			}
		})
	}
}

func TestSendMouseButtonValidatesMaskBeforeConnect(t *testing.T) {
	cf := &commonFlags{url: "http://device.invalid", allowControl: true}
	for _, mask := range []byte{0, 3, 8, 255} {
		err := sendMouseButton(context.Background(), cf, mask, true)
		if err == nil {
			t.Fatalf("sendMouseButton accepted invalid mask %#02x", mask)
		}
		if strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "dial") {
			t.Fatalf("sendMouseButton connected before validating mask %#02x: %v", mask, err)
		}
	}
}

func TestKeyComboHappyPath(t *testing.T) {
	const combo = "Ctrl-Alt-Del"
	wantModifier := byte(jetkvm.ModifierLeftControl | jetkvm.ModifierLeftAlt)
	wantKeys := []byte{jetkvm.KeyUsageDelete}

	var (
		sendCalls   int
		gotModifier byte
		gotKeys     []byte
		gotURL      string
		gotControl  bool
		gotDeadline bool
	)
	out, err := captureStdout(t, func() error {
		return runKeyComboWithSender(
			[]string{"--url", "http://device.invalid", "--allow-control", "--combo", combo},
			func(ctx context.Context, cf *commonFlags, modifier byte, keys []byte) error {
				sendCalls++
				gotModifier = modifier
				gotKeys = append([]byte(nil), keys...)
				gotURL = cf.url
				gotControl = cf.allowControl
				_, gotDeadline = ctx.Deadline()
				return nil
			},
		)
	})
	if err != nil {
		t.Fatalf("runKeyComboWithSender: %v", err)
	}
	if sendCalls != 1 {
		t.Fatalf("sender calls = %d, want 1", sendCalls)
	}
	if gotModifier != wantModifier || string(gotKeys) != string(wantKeys) {
		t.Errorf("report = modifier %#02x keys % x, want %#02x/% x", gotModifier, gotKeys, wantModifier, wantKeys)
	}
	if gotURL != "http://device.invalid" || !gotControl || !gotDeadline {
		t.Errorf("sender flags/context = url %q control %t deadline %t", gotURL, gotControl, gotDeadline)
	}

	var result struct {
		Sent     string `json:"sent"`
		Combo    string `json:"combo"`
		Modifier int    `json:"modifier"`
		Keys     []int  `json:"keys"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("key-combo output is not JSON: %v\n%s", err, out)
	}
	if result.Sent != "key-combo" || result.Combo != combo || result.Modifier != int(wantModifier) {
		t.Errorf("key-combo output = %+v", result)
	}
	if len(result.Keys) != len(wantKeys) {
		t.Fatalf("output keys = %v, want %v", result.Keys, wantKeys)
	}
	for i, key := range wantKeys {
		if result.Keys[i] != int(key) {
			t.Errorf("output key[%d] = %d, want %d", i, result.Keys[i], key)
		}
	}
}

func TestKeyComboRejectsUnknownBeforeConnect(t *testing.T) {
	err := runKeyCombo([]string{
		"--url", "http://device.invalid",
		"--allow-control",
		"--combo", "not-a-built-in-combo",
	})
	if err == nil {
		t.Fatal("key-combo accepted an unknown combo")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("unknown combo error is not actionable: %v", err)
	}
	if strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "dial") {
		t.Fatalf("key-combo connected before resolving the combo: %v", err)
	}
}

func TestHoldKeyHappyPath(t *testing.T) {
	const (
		combo      = "Ctrl-Alt-Del"
		wantHoldMS = 250
	)
	wantModifier := byte(jetkvm.ModifierLeftControl | jetkvm.ModifierLeftAlt)
	wantKeys := []byte{jetkvm.KeyUsageDelete}

	var (
		sendCalls   int
		gotModifier byte
		gotKeys     []byte
		gotHoldMS   int
		gotURL      string
		gotControl  bool
		gotDeadline bool
	)
	out, err := captureStdout(t, func() error {
		return runHoldKeyWithSender(
			[]string{
				"--url", "http://device.invalid",
				"--allow-control",
				"--combo", combo,
				"--hold-ms", strconv.Itoa(wantHoldMS),
			},
			func(ctx context.Context, cf *commonFlags, modifier byte, keys []byte, holdMS int) error {
				sendCalls++
				gotModifier = modifier
				gotKeys = append([]byte(nil), keys...)
				gotHoldMS = holdMS
				gotURL = cf.url
				gotControl = cf.allowControl
				_, gotDeadline = ctx.Deadline()
				return nil
			},
		)
	})
	if err != nil {
		t.Fatalf("runHoldKeyWithSender: %v", err)
	}
	if sendCalls != 1 {
		t.Fatalf("sender calls = %d, want 1", sendCalls)
	}
	if gotModifier != wantModifier || string(gotKeys) != string(wantKeys) || gotHoldMS != wantHoldMS {
		t.Errorf("hold report = modifier %#02x keys % x holdMS %d, want %#02x/% x/%d", gotModifier, gotKeys, gotHoldMS, wantModifier, wantKeys, wantHoldMS)
	}
	if gotURL != "http://device.invalid" || !gotControl || !gotDeadline {
		t.Errorf("sender flags/context = url %q control %t deadline %t", gotURL, gotControl, gotDeadline)
	}

	var result struct {
		Sent     string `json:"sent"`
		Combo    string `json:"combo"`
		HoldMS   int    `json:"holdMs"`
		Modifier int    `json:"modifier"`
		Keys     []int  `json:"keys"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("hold-key output is not JSON: %v\n%s", err, out)
	}
	if result.Sent != "hold-key" || result.Combo != combo || result.HoldMS != wantHoldMS || result.Modifier != int(wantModifier) {
		t.Errorf("hold-key output = %+v", result)
	}
	if len(result.Keys) != len(wantKeys) || result.Keys[0] != int(wantKeys[0]) {
		t.Errorf("hold-key output keys = %v, want %v", result.Keys, wantKeys)
	}
}

func TestHoldKeyAcceptsMaximumDurationBeforeSend(t *testing.T) {
	gotHoldMS := 0
	_, err := captureStdout(t, func() error {
		return runHoldKeyWithSender(
			[]string{
				"--url", "http://device.invalid",
				"--allow-control",
				"--combo", "ctrl+c",
				"--hold-ms", strconv.Itoa(jetkvm.MaxHoldMS),
			},
			func(_ context.Context, _ *commonFlags, _ byte, _ []byte, holdMS int) error {
				gotHoldMS = holdMS
				return nil
			},
		)
	})
	if err != nil {
		t.Fatalf("runHoldKeyWithSender: %v", err)
	}
	if gotHoldMS != jetkvm.MaxHoldMS {
		t.Fatalf("sender holdMS = %d, want %d", gotHoldMS, jetkvm.MaxHoldMS)
	}
}

func TestHoldKeyRejectsInvalidInputBeforeSend(t *testing.T) {
	for name, args := range map[string][]string{
		"missing combo":   {"--url", "http://device.invalid", "--allow-control", "--hold-ms", "100"},
		"missing hold-ms": {"--url", "http://device.invalid", "--allow-control", "--combo", "ctrl+c"},
		"zero hold":       {"--url", "http://device.invalid", "--allow-control", "--combo", "ctrl+c", "--hold-ms", "0"},
		"negative hold":   {"--url", "http://device.invalid", "--allow-control", "--combo", "ctrl+c", "--hold-ms", "-1"},
		"oversized hold":  {"--url", "http://device.invalid", "--allow-control", "--combo", "ctrl+c", "--hold-ms", strconv.Itoa(jetkvm.MaxHoldMS + 1)},
		"unknown combo":   {"--url", "http://device.invalid", "--allow-control", "--combo", "not-a-built-in-combo", "--hold-ms", "100"},
	} {
		t.Run(name, func(t *testing.T) {
			sendCalls := 0
			err := runHoldKeyWithSender(args, func(context.Context, *commonFlags, byte, []byte, int) error {
				sendCalls++
				return nil
			})
			if err == nil {
				t.Fatal("invalid hold-key input was accepted")
			}
			if sendCalls != 0 {
				t.Fatalf("invalid hold-key input made %d sender calls, want zero", sendCalls)
			}
		})
	}
}

func TestSendHoldKeyValidatesBeforeConnect(t *testing.T) {
	cf := &commonFlags{url: "http://device.invalid", allowControl: true}
	for _, err := range []error{
		sendHoldKey(context.Background(), cf, 0, nil, 100),
		sendHoldKey(context.Background(), cf, jetkvm.ModifierLeftControl, []byte{jetkvm.KeyUsageC}, 0),
		sendHoldKey(context.Background(), cf, jetkvm.ModifierLeftControl, []byte{jetkvm.KeyUsageC}, jetkvm.MaxHoldMS+1),
	} {
		if err == nil {
			t.Fatal("sendHoldKey accepted invalid input")
		}
		if strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "dial") {
			t.Fatalf("sendHoldKey connected before validating input: %v", err)
		}
	}
}

func TestKeySequenceHappyPath(t *testing.T) {
	wantCombos := []jetkvm.ResolvedKeyCombo{
		{Modifier: jetkvm.ModifierLeftMeta, Keys: []byte{jetkvm.KeyUsageSpace}},
		{Keys: []byte{jetkvm.KeyUsageT}},
		{Keys: []byte{jetkvm.KeyUsageE}},
		{Keys: []byte{jetkvm.KeyUsageR}},
		{Keys: []byte{jetkvm.KeyUsageM}},
		{Keys: []byte{jetkvm.KeyUsageEnter}},
	}
	const wantDelayMS = jetkvm.DefaultTypeDelayMS

	var (
		sendCalls   int
		gotCombos   []jetkvm.ResolvedKeyCombo
		gotDelayMS  int
		gotURL      string
		gotControl  bool
		gotDeadline bool
	)
	out, err := captureStdout(t, func() error {
		return runKeySequenceWithSender(
			[]string{
				"--url", "http://device.invalid",
				"--allow-control",
				"--combo", "cmd+space",
				"--combo", "t",
				"--combo", "e",
				"--combo", "r",
				"--combo", "m",
				"--combo", "enter",
			},
			func(ctx context.Context, cf *commonFlags, combos []jetkvm.ResolvedKeyCombo, delayMS int) error {
				sendCalls++
				gotCombos = make([]jetkvm.ResolvedKeyCombo, len(combos))
				for i, combo := range combos {
					gotCombos[i] = jetkvm.ResolvedKeyCombo{
						Modifier: combo.Modifier,
						Keys:     append([]byte(nil), combo.Keys...),
					}
				}
				gotDelayMS = delayMS
				gotURL = cf.url
				gotControl = cf.allowControl
				_, gotDeadline = ctx.Deadline()
				return nil
			},
		)
	})
	if err != nil {
		t.Fatalf("runKeySequenceWithSender: %v", err)
	}
	if sendCalls != 1 {
		t.Fatalf("sender calls = %d, want 1", sendCalls)
	}
	if len(gotCombos) != len(wantCombos) {
		t.Fatalf("resolved combos = %+v, want %+v", gotCombos, wantCombos)
	}
	for i, want := range wantCombos {
		got := gotCombos[i]
		if got.Modifier != want.Modifier || string(got.Keys) != string(want.Keys) {
			t.Errorf("resolved combo %d = modifier %#02x keys % x, want %#02x/% x", i, got.Modifier, got.Keys, want.Modifier, want.Keys)
		}
	}
	if gotDelayMS != wantDelayMS || gotURL != "http://device.invalid" || !gotControl || !gotDeadline {
		t.Errorf("sender args = delay %d url %q control %t deadline %t", gotDelayMS, gotURL, gotControl, gotDeadline)
	}

	var result struct {
		Sent    string `json:"sent"`
		Combos  int    `json:"combos"`
		DelayMS int    `json:"delayMs"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("key-sequence output is not JSON: %v\n%s", err, out)
	}
	if result.Sent != "key-sequence" || result.Combos != len(wantCombos) || result.DelayMS != wantDelayMS {
		t.Errorf("key-sequence output = %+v", result)
	}
	for _, rawCombo := range []string{"cmd+space", "enter"} {
		if strings.Contains(out, rawCombo) {
			t.Errorf("key-sequence output reflected raw combo %q: %s", rawCombo, out)
		}
	}
}

func TestSendResolvedKeySequenceCompletesEachSendAndReleaseInOrder(t *testing.T) {
	combos := []jetkvm.ResolvedKeyCombo{
		{Keys: []byte{jetkvm.KeyUsageT}},
		{Keys: []byte{jetkvm.KeyUsageE}},
		{Keys: []byte{jetkvm.KeyUsageR}},
	}
	var events []string
	err := sendResolvedKeySequence(context.Background(), combos, 0, func(_ byte, keys []byte) error {
		key := strconv.Itoa(int(keys[0]))
		events = append(events, "send:"+key)
		// Production's synchronous callback runs sendControlAndRelease, whose
		// own test proves release is attempted before it returns.
		events = append(events, "release:"+key)
		return nil
	})
	if err != nil {
		t.Fatalf("sendResolvedKeySequence: %v", err)
	}
	want := []string{
		"send:" + strconv.Itoa(jetkvm.KeyUsageT), "release:" + strconv.Itoa(jetkvm.KeyUsageT),
		"send:" + strconv.Itoa(jetkvm.KeyUsageE), "release:" + strconv.Itoa(jetkvm.KeyUsageE),
		"send:" + strconv.Itoa(jetkvm.KeyUsageR), "release:" + strconv.Itoa(jetkvm.KeyUsageR),
	}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, events[i], want[i])
		}
	}
}

func TestSendResolvedKeySequenceStopsAtFirstFailure(t *testing.T) {
	failure := errors.New("synthetic sequence failure")
	calls := 0
	err := sendResolvedKeySequence(context.Background(), []jetkvm.ResolvedKeyCombo{
		{Keys: []byte{jetkvm.KeyUsageT}},
		{Keys: []byte{jetkvm.KeyUsageE}},
		{Keys: []byte{jetkvm.KeyUsageR}},
	}, 0, func(byte, []byte) error {
		calls++
		if calls == 2 {
			return failure
		}
		return nil
	})
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "index 1") {
		t.Fatalf("sendResolvedKeySequence error = %v, want indexed failure", err)
	}
	if calls != 2 {
		t.Fatalf("send calls = %d, want 2 with nothing after the failure", calls)
	}
}

func TestKeySequenceRejectsLaterInvalidComboBeforeSendWithoutReflectingInput(t *testing.T) {
	const canary = "KEY-SEQUENCE-CALLER-CANARY"
	sendCalls := 0
	err := runKeySequenceWithSender(
		[]string{
			"--url", "http://device.invalid",
			"--allow-control",
			"--combo", "ctrl+c",
			"--combo", canary,
		},
		func(context.Context, *commonFlags, []jetkvm.ResolvedKeyCombo, int) error {
			sendCalls++
			return nil
		},
	)
	if err == nil {
		t.Fatal("key-sequence accepted an unknown later combo")
	}
	if sendCalls != 0 {
		t.Fatalf("sender calls = %d, want 0", sendCalls)
	}
	if !strings.Contains(err.Error(), "combos[1]") {
		t.Errorf("error does not identify the failing sequence index: %v", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Errorf("error reflected raw caller input: %v", err)
	}
}

func TestKeySequenceRejectsDelayAndLengthBeforeSend(t *testing.T) {
	tooLong := []string{"--url", "http://device.invalid", "--allow-control"}
	for range jetkvm.MaxKeySequenceLength + 1 {
		tooLong = append(tooLong, "--combo", "enter")
	}

	tests := []struct {
		name string
		args []string
	}{
		{
			name: "delay above maximum",
			args: []string{
				"--url", "http://device.invalid",
				"--allow-control",
				"--combo", "enter",
				"--delay-ms", strconv.Itoa(jetkvm.MaxTypeDelayMS + 1),
			},
		},
		{name: "sequence above maximum", args: tooLong},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sendCalls := 0
			err := runKeySequenceWithSender(tc.args, func(context.Context, *commonFlags, []jetkvm.ResolvedKeyCombo, int) error {
				sendCalls++
				return nil
			})
			if err == nil {
				t.Fatal("key-sequence accepted out-of-contract input")
			}
			if sendCalls != 0 {
				t.Fatalf("sender calls = %d, want 0", sendCalls)
			}
			if strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "dial") {
				t.Fatalf("key-sequence connected before validating input: %v", err)
			}
		})
	}
}

func TestScrollHappyPath(t *testing.T) {
	const (
		wantDX int8 = 12
		wantDY int8 = 34
	)
	var (
		sendCalls   int
		gotDX       int8
		gotDY       int8
		gotURL      string
		gotControl  bool
		gotDeadline bool
	)
	out, err := captureStdout(t, func() error {
		return runScrollWithSender(
			[]string{"--url", "http://device.invalid", "--allow-control", "--dx", "12", "--dy", "34"},
			func(ctx context.Context, cf *commonFlags, dx, dy int8) error {
				sendCalls++
				gotDX = dx
				gotDY = dy
				gotURL = cf.url
				gotControl = cf.allowControl
				_, gotDeadline = ctx.Deadline()
				return nil
			},
		)
	})
	if err != nil {
		t.Fatalf("runScrollWithSender: %v", err)
	}
	if sendCalls != 1 || gotDX != wantDX || gotDY != wantDY {
		t.Errorf("sender calls/report = %d/(%d,%d), want 1/(%d,%d)", sendCalls, gotDX, gotDY, wantDX, wantDY)
	}
	if gotURL != "http://device.invalid" || !gotControl || !gotDeadline {
		t.Errorf("sender flags/context = url %q control %t deadline %t", gotURL, gotControl, gotDeadline)
	}

	var result struct {
		Sent string `json:"sent"`
		DX   int    `json:"dx"`
		DY   int    `json:"dy"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("scroll output is not JSON: %v\n%s", err, out)
	}
	if result.Sent != "scroll" || result.DX != int(wantDX) || result.DY != int(wantDY) {
		t.Errorf("scroll output = %+v, want sent=scroll dx=%d dy=%d", result, wantDX, wantDY)
	}
}

func TestScrollOptionalDXAndExplicitZeroDY(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		wantDX int8
		wantDY int8
	}{
		{name: "dx defaults to zero", args: []string{"--allow-control", "--dy", "-1"}, wantDX: 0, wantDY: -1},
		{name: "explicit zero dy is present", args: []string{"--allow-control", "--dx", "1", "--dy", "0"}, wantDX: 1, wantDY: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotDX, gotDY int8
			out, err := captureStdout(t, func() error {
				return runScrollWithSender(tc.args, func(_ context.Context, _ *commonFlags, dx, dy int8) error {
					gotDX, gotDY = dx, dy
					return nil
				})
			})
			if err != nil {
				t.Fatalf("runScrollWithSender: %v", err)
			}
			if gotDX != tc.wantDX || gotDY != tc.wantDY {
				t.Errorf("sender report = (%d,%d), want (%d,%d)", gotDX, gotDY, tc.wantDX, tc.wantDY)
			}
			var result struct {
				DX int `json:"dx"`
				DY int `json:"dy"`
			}
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatalf("scroll output is not JSON: %v\n%s", err, out)
			}
			if result.DX != int(tc.wantDX) || result.DY != int(tc.wantDY) {
				t.Errorf("scroll output = (%d,%d), want (%d,%d)", result.DX, result.DY, tc.wantDX, tc.wantDY)
			}
		})
	}
}

func TestScrollSenderFailureDoesNotPrintSuccess(t *testing.T) {
	wantErr := errors.New("wheelReport failed")
	out, err := captureStdout(t, func() error {
		return runScrollWithSender(
			[]string{"--url", "http://device.invalid", "--allow-control", "--dy", "1"},
			func(context.Context, *commonFlags, int8, int8) error { return wantErr },
		)
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("runScrollWithSender error = %v, want %v", err, wantErr)
	}
	if out != "" {
		t.Fatalf("failed scroll printed success output: %q", out)
	}
}

func TestScrollRequiresExplicitDYFlag(t *testing.T) {
	sendCalls := 0
	err := runScrollWithSender(
		[]string{"--url", "http://device.invalid", "--allow-control", "--dx", "1"},
		func(context.Context, *commonFlags, int8, int8) error {
			sendCalls++
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "--dy") {
		t.Fatalf("scroll without --dy = %v, want required-flag error", err)
	}
	if sendCalls != 0 {
		t.Fatalf("scroll without --dy called sender %d times, want 0", sendCalls)
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
		{"read-text", "--region", canary},
		{"wait-stable", "--threshold", canary},
		{"wait-for-text", "--interval", canary},
		{"status", canary},
		{"drag", "--steps", canary},
		{"mouse-button", "--allow-control", "--button", canary, "--action", "press"},
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
		{"read-text", "--timeout", "0"},
		{"wait-stable", "--timeout", "0"},
		{"wait-for-text", "--timeout", "0"},
		{"serve", "--timeout", "0"},
		{"keypress", "--timeout", "-1ns"},
		{"type", "--timeout", "0"},
		{"key-combo", "--timeout", "0"},
		{"hold-key", "--timeout", "0"},
		{"key-sequence", "--timeout", "0"},
		{"mouse-button", "--timeout", "0"},
		{"mouse-move", "--timeout", "0"},
		{"scroll", "--timeout", "0"},
		{"click", "--timeout", "0"},
		{"double-click", "--timeout", "0"},
		{"drag", "--timeout", "0"},
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
		"status":        {"status"},
		"screenshot":    {"screenshot", "--output", shot},
		"read-text":     {"read-text"},
		"wait-stable":   {"wait-stable"},
		"wait-for-text": {"wait-for-text", "--text", "ready"},
		"serve":         {"serve"},
		"keypress":      {"keypress", "--allow-control", "--key", "4"},
		"type":          {"type", "--allow-control", "--text", "hello"},
		"key-combo":     {"key-combo", "--allow-control", "--combo", "ctrl+c"},
		"hold-key":      {"hold-key", "--allow-control", "--combo", "ctrl+c", "--hold-ms", "100"},
		"key-sequence":  {"key-sequence", "--allow-control", "--combo", "enter"},
		"mouse-button":  {"mouse-button", "--allow-control", "--button", "left", "--action", "press"},
		"mouse-move":    {"mouse-move", "--allow-control", "--x", "1", "--y", "1"},
		"scroll":        {"scroll", "--allow-control", "--dy", "1"},
		"click":         {"click", "--allow-control", "--x", "1", "--y", "1"},
		"double-click":  {"double-click", "--allow-control", "--x", "1", "--y", "1"},
		"drag":          {"drag", "--allow-control", "--x1", "1", "--y1", "1", "--x2", "2", "--y2", "2"},
		"release-all":   {"release-all", "--allow-control"},
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
	aboveScrollMax := strconv.Itoa(int(jetkvm.MaxScrollDelta) + 1)
	belowScrollMin := strconv.Itoa(-int(jetkvm.MaxScrollDelta) - 1)
	abovePointerButtonMax := strconv.Itoa(jetkvm.MaxPointerButtonMask + 1)
	for _, err := range []error{
		runKeypress([]string{"--url", "http://device.invalid", "--allow-control", "--key", "4", "--modifier", "256"}),
		runType([]string{"--url", "http://device.invalid", "--allow-control", "--text", "aé"}),
		runType([]string{"--url", "http://device.invalid", "--allow-control", "--text", "a", "--delay-ms", "501"}),
		runType([]string{"--url", "http://device.invalid", "--allow-control", "--text", strings.Repeat("a", jetkvm.MaxTypeStringRunes+1)}),
		runKeyCombo([]string{"--url", "http://device.invalid", "--allow-control", "--combo", "unknown-combo"}),
		runMouseButton([]string{"--url", "http://device.invalid", "--allow-control", "--button", "back", "--action", "press"}),
		runMouseButton([]string{"--url", "http://device.invalid", "--allow-control", "--button", "left", "--action", "toggle"}),
		runHoldKey([]string{"--url", "http://device.invalid", "--allow-control", "--combo", "ctrl+c", "--hold-ms", "0"}),
		runHoldKey([]string{"--url", "http://device.invalid", "--allow-control", "--combo", "unknown-combo", "--hold-ms", "100"}),
		runMouseMove([]string{"--url", "http://device.invalid", "--allow-control", "--x", "32768", "--y", "0"}),
		runMouseMove([]string{"--url", "http://device.invalid", "--allow-control", "--x", "0", "--y", "0", "--buttons", abovePointerButtonMax}),
		runScroll([]string{"--url", "http://device.invalid", "--allow-control", "--dx", aboveScrollMax, "--dy", "1"}),
		runScroll([]string{"--url", "http://device.invalid", "--allow-control", "--dx", "1", "--dy", belowScrollMin}),
		runScroll([]string{"--url", "http://device.invalid", "--allow-control", "--dx", "0", "--dy", "0"}),
		runClick([]string{"--url", "http://device.invalid", "--allow-control", "--x", "32768", "--y", "0"}),
		runClick([]string{"--url", "http://device.invalid", "--allow-control", "--x", "0", "--y", "0", "--button", abovePointerButtonMax}),
		runDoubleClick([]string{"--url", "http://device.invalid", "--allow-control", "--x", "32768", "--y", "0"}),
		runDoubleClick([]string{"--url", "http://device.invalid", "--allow-control", "--x", "0", "--y", "0", "--button", abovePointerButtonMax}),
		runDrag([]string{"--url", "http://device.invalid", "--allow-control", "--x1", "-1", "--y1", "0", "--x2", "1", "--y2", "1"}),
		runDrag([]string{"--url", "http://device.invalid", "--allow-control", "--x1", "0", "--y1", "32768", "--x2", "1", "--y2", "1"}),
		runDrag([]string{"--url", "http://device.invalid", "--allow-control", "--x1", "0", "--y1", "0", "--x2", "32768", "--y2", "1"}),
		runDrag([]string{"--url", "http://device.invalid", "--allow-control", "--x1", "0", "--y1", "0", "--x2", "1", "--y2", "-1"}),
		runDrag([]string{"--url", "http://device.invalid", "--allow-control", "--x1", "0", "--y1", "0", "--x2", "1", "--y2", "1", "--button", "-1"}),
		runDrag([]string{"--url", "http://device.invalid", "--allow-control", "--x1", "0", "--y1", "0", "--x2", "1", "--y2", "1", "--button", abovePointerButtonMax}),
		runDrag([]string{"--url", "http://device.invalid", "--allow-control", "--x1", "0", "--y1", "0", "--x2", "1", "--y2", "1", "--steps", "-1"}),
		runDrag([]string{"--url", "http://device.invalid", "--allow-control", "--x1", "0", "--y1", "0", "--x2", "1", "--y2", "1", "--steps", "257"}),
		runDrag([]string{"--url", "http://device.invalid", "--allow-control", "--x1", "0", "--y1", "0", "--x2", "1"}),
	} {
		if err == nil {
			t.Fatal("CLI accepted out-of-range control input")
		}
		if strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "dial") {
			t.Fatalf("CLI connected before validating control input: %v", err)
		}
	}
}

func TestCLISharedValidationUsesAdapterNeutralNames(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		want      string
		forbidden []string
	}{
		{
			name: "type delay",
			err: runType([]string{
				"--url", "http://device.invalid", "--allow-control",
				"--text", "a", "--delay-ms", strconv.Itoa(jetkvm.MaxTypeDelayMS + 1),
			}),
			want:      "delay must be in [0,500] milliseconds",
			forbidden: []string{"delay_ms"},
		},
		{
			name: "hold duration",
			err: runHoldKey([]string{
				"--url", "http://device.invalid", "--allow-control",
				"--combo", "ctrl+c", "--hold-ms", strconv.Itoa(jetkvm.MaxHoldMS + 1),
			}),
			want:      "hold duration must be in [1,5000] milliseconds",
			forbidden: []string{"hold_ms"},
		},
		{
			name: "stable frame count",
			err: runWaitStable([]string{
				"--url", "http://device.invalid", "--stable-frames", "0",
			}),
			want:      "stable frame count must be at least 1",
			forbidden: []string{"StableFrames", "stable_frames", "stable-frames"},
		},
		{
			name: "stable poll interval",
			err: runWaitStable([]string{
				"--url", "http://device.invalid", "--poll-interval", "-1ms",
			}),
			want:      "poll interval must be non-negative",
			forbidden: []string{"PollInterval", "poll_interval_ms", "poll-interval"},
		},
		{
			name: "OCR pattern",
			err: runWaitForText([]string{
				"--url", "http://device.invalid", "--text", "(", "--regex",
			}),
			want:      "text must use valid RE2 syntax",
			forbidden: []string{"Text:"},
		},
		{
			name: "OCR interval",
			err: runWaitForText([]string{
				"--url", "http://device.invalid", "--text", "READY", "--interval", "1ms",
			}),
			want:      "interval must be between 100ms and 10s",
			forbidden: []string{"Interval:", "interval_ms", "--interval"},
		},
		{
			name: "OCR timeout",
			err: runWaitForText([]string{
				"--url", "http://device.invalid", "--text", "READY", "--timeout", "1ms",
			}),
			want:      "timeout must be between 100ms and 5m0s",
			forbidden: []string{"Timeout:", "timeout_ms", "--timeout"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
				t.Fatalf("validation error = %v, want marker %q", tc.err, tc.want)
			}
			for _, name := range tc.forbidden {
				if strings.Contains(tc.err.Error(), name) {
					t.Fatalf("CLI validation error leaked adapter-specific name %q: %v", name, tc.err)
				}
			}
		})
	}
}

func TestSendPointerClickPressesThenReleasesAtSameCoordinates(t *testing.T) {
	type report struct {
		x, y    int32
		buttons byte
	}
	var got []report
	err := sendPointerClick(func(x, y int32, buttons byte) error {
		got = append(got, report{x: x, y: y, buttons: buttons})
		return nil
	}, 123, 456, 3)
	if err != nil {
		t.Fatalf("sendPointerClick: %v", err)
	}
	want := []report{
		{x: 123, y: 456, buttons: 3},
		{x: 123, y: 456, buttons: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("pointer reports = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pointer report %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRunDoubleClickParsesFlagsAndPrintsSummary(t *testing.T) {
	var (
		senderCalls int
		gotX, gotY  int32
		gotButton   byte
		gotURL      string
		gotControl  bool
		gotDeadline bool
	)
	out, err := captureStdout(t, func() error {
		return runDoubleClickWithSender([]string{
			"--url", "http://device.invalid",
			"--timeout", "2s",
			"--allow-control",
			"--x", "123",
			"--y", "456",
			"--button", "3",
		}, func(ctx context.Context, cf *commonFlags, x, y int32, button byte) error {
			senderCalls++
			gotX, gotY, gotButton = x, y, button
			gotURL, gotControl = cf.url, cf.allowControl
			_, gotDeadline = ctx.Deadline()
			return nil
		})
	})
	if err != nil {
		t.Fatalf("runDoubleClickWithSender: %v", err)
	}
	if senderCalls != 1 {
		t.Fatalf("sender calls = %d, want 1", senderCalls)
	}
	if gotX != 123 || gotY != 456 || gotButton != 3 {
		t.Errorf("sender report = (%d,%d,%d), want (123,456,3)", gotX, gotY, gotButton)
	}
	if gotURL != "http://device.invalid" || !gotControl || !gotDeadline {
		t.Errorf("sender flags/context = url %q control %t deadline %t", gotURL, gotControl, gotDeadline)
	}

	var summary struct {
		Sent   string `json:"sent"`
		X      int    `json:"x"`
		Y      int    `json:"y"`
		Button int    `json:"button"`
	}
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("double-click output is not JSON: %v\n%s", err, out)
	}
	if summary.Sent != "double-click" || summary.X != 123 || summary.Y != 456 || summary.Button != 3 {
		t.Errorf("double-click summary = %+v", summary)
	}
}

func TestRunDoubleClickRequiresAllowControlBeforeSend(t *testing.T) {
	senderCalls := 0
	err := runDoubleClickWithSender(
		[]string{"--url", "http://device.invalid", "--x", "1", "--y", "2"},
		func(context.Context, *commonFlags, int32, int32, byte) error {
			senderCalls++
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "--allow-control") {
		t.Fatalf("double-click without --allow-control = %v, want gate error", err)
	}
	if senderCalls != 0 {
		t.Fatalf("double-click without --allow-control called sender %d times, want 0", senderCalls)
	}
}

func TestRunDoubleClickSenderFailureDoesNotPrintSuccess(t *testing.T) {
	sendFailure := errors.New("double-click send failed")
	releaseFailure := errors.New("double-click neutralization unverified")
	wantErr := errors.Join(sendFailure, releaseFailure)
	out, err := captureStdout(t, func() error {
		return runDoubleClickWithSender(
			[]string{"--url", "http://device.invalid", "--allow-control", "--x", "1", "--y", "2"},
			func(context.Context, *commonFlags, int32, int32, byte) error { return wantErr },
		)
	})
	for _, want := range []error{sendFailure, releaseFailure} {
		if !errors.Is(err, want) {
			t.Errorf("result %v does not retain %v", err, want)
		}
	}
	if out != "" {
		t.Fatalf("failed double-click printed success output: %q", out)
	}
}

func TestRunDoubleClickUsesDefaultButton(t *testing.T) {
	var gotButton byte
	_, err := captureStdout(t, func() error {
		return runDoubleClickWithSender(
			[]string{"--url", "http://device.invalid", "--allow-control", "--x", "1", "--y", "2"},
			func(_ context.Context, _ *commonFlags, _, _ int32, button byte) error {
				gotButton = button
				return nil
			},
		)
	})
	if err != nil {
		t.Fatalf("runDoubleClickWithSender: %v", err)
	}
	if gotButton != 1 {
		t.Errorf("default button = %d, want 1", gotButton)
	}
}

func TestRunDoubleClickRejectsInvalidArgumentsBeforeSend(t *testing.T) {
	aboveMax := strconv.Itoa(jetkvm.MaxAbsoluteCoordinate + 1)
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing x", args: []string{"--y", "0"}},
		{name: "missing y", args: []string{"--x", "0"}},
		{name: "x above wire range", args: []string{"--x", aboveMax, "--y", "0"}},
		{name: "y below wire range", args: []string{"--x", "0", "--y", "-1"}},
		{name: "button below wire range", args: []string{"--x", "0", "--y", "0", "--button", "-1"}},
		{name: "button above wire range", args: []string{"--x", "0", "--y", "0", "--button", strconv.Itoa(jetkvm.MaxPointerButtonMask + 1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			senderCalls := 0
			args := append([]string{"--url", "http://device.invalid", "--allow-control"}, tc.args...)
			err := runDoubleClickWithSender(args, func(context.Context, *commonFlags, int32, int32, byte) error {
				senderCalls++
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "invalid double-click") {
				t.Fatalf("invalid double-click arguments returned %v", err)
			}
			if senderCalls != 0 {
				t.Fatalf("invalid double-click called sender %d times, want 0", senderCalls)
			}
		})
	}
}

func TestSendPointerDoubleClickPressesAndReleasesTwiceAtSameCoordinates(t *testing.T) {
	type report struct {
		x, y    int32
		buttons byte
	}
	var got []report
	err := sendPointerDoubleClick(func(x, y int32, buttons byte) error {
		got = append(got, report{x: x, y: y, buttons: buttons})
		return nil
	}, 123, 456, 3)
	if err != nil {
		t.Fatalf("sendPointerDoubleClick: %v", err)
	}
	want := []report{
		{x: 123, y: 456, buttons: 3},
		{x: 123, y: 456, buttons: 0},
		{x: 123, y: 456, buttons: 3},
		{x: 123, y: 456, buttons: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("pointer reports = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pointer report %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSendPointerDoubleClickAndReleaseRetainsBothFailures(t *testing.T) {
	sendFailure := errors.New("double-click send failed")
	releaseFailure := errors.New("double-click neutralization unverified")
	sendCalls, releaseCalls := 0, 0
	err := sendPointerDoubleClickAndRelease(
		func(int32, int32, byte) error {
			sendCalls++
			if sendCalls == 3 {
				return sendFailure
			}
			return nil
		},
		func() error {
			releaseCalls++
			return releaseFailure
		},
		123,
		456,
		3,
	)
	if sendCalls != 3 || releaseCalls != 1 {
		t.Fatalf("calls = send %d release %d, want send 3 release 1", sendCalls, releaseCalls)
	}
	for _, want := range []error{sendFailure, releaseFailure} {
		if !errors.Is(err, want) {
			t.Errorf("result %v does not retain %v", err, want)
		}
	}
}

func TestRunDragParsesFlagsAndPrintsSummary(t *testing.T) {
	var gotReports []jetkvm.PointerDragReport
	out, err := captureStdout(t, func() error {
		return runDragWithSender([]string{
			"--url", "http://device.invalid",
			"--timeout", "2s",
			"--allow-control",
			"--x1", "0", "--y1", "0",
			"--x2", "9", "--y2", "6",
			"--button", "3", "--steps", "2",
		}, func(ctx context.Context, cf *commonFlags, reports []jetkvm.PointerDragReport) error {
			if cf.url != "http://device.invalid" || cf.timeout != 2*time.Second || !cf.allowControl {
				t.Errorf("parsed common flags = %+v", cf)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Error("drag sender context has no deadline")
			}
			gotReports = append([]jetkvm.PointerDragReport(nil), reports...)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("runDragWithSender: %v", err)
	}
	wantReports := []jetkvm.PointerDragReport{
		{X: 0, Y: 0, Buttons: 3},
		{X: 3, Y: 2, Buttons: 3},
		{X: 6, Y: 4, Buttons: 3},
		{X: 9, Y: 6, Buttons: 3},
		{X: 9, Y: 6, Buttons: 0},
	}
	if len(gotReports) != len(wantReports) {
		t.Fatalf("drag reports = %+v, want %+v", gotReports, wantReports)
	}
	for i := range wantReports {
		if gotReports[i] != wantReports[i] {
			t.Errorf("drag report %d = %+v, want %+v", i+1, gotReports[i], wantReports[i])
		}
	}

	var summary struct {
		Sent   string `json:"sent"`
		X1     int    `json:"x1"`
		Y1     int    `json:"y1"`
		X2     int    `json:"x2"`
		Y2     int    `json:"y2"`
		Button int    `json:"button"`
		Steps  int    `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("drag output is not JSON: %v\n%s", err, out)
	}
	if summary != (struct {
		Sent   string `json:"sent"`
		X1     int    `json:"x1"`
		Y1     int    `json:"y1"`
		X2     int    `json:"x2"`
		Y2     int    `json:"y2"`
		Button int    `json:"button"`
		Steps  int    `json:"steps"`
	}{Sent: "drag", X1: 0, Y1: 0, X2: 9, Y2: 6, Button: 3, Steps: 2}) {
		t.Errorf("drag summary = %+v", summary)
	}
}

func TestRunDragUsesDefaultButtonAndSteps(t *testing.T) {
	out, err := captureStdout(t, func() error {
		return runDragWithSender([]string{
			"--url", "http://device.invalid", "--allow-control",
			"--x1", "1", "--y1", "2", "--x2", "3", "--y2", "4",
		}, func(_ context.Context, _ *commonFlags, reports []jetkvm.PointerDragReport) error {
			if len(reports) != 3 || reports[0].Buttons != 1 || reports[1].Buttons != 1 || reports[2].Buttons != 0 {
				t.Errorf("default drag reports = %+v", reports)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("runDragWithSender defaults: %v", err)
	}
	if !strings.Contains(out, `"button": 1`) || !strings.Contains(out, `"steps": 0`) {
		t.Errorf("default drag summary = %s", out)
	}
}

func TestSendPointerDragSendsValidatedSequenceAndStopsOnFailure(t *testing.T) {
	reports, err := jetkvm.BuildPointerDragReports(0, 0, 9, 6, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	type wireReport struct {
		x, y    int32
		buttons byte
	}
	var got []wireReport
	if err := sendPointerDrag(func(x, y int32, buttons byte) error {
		got = append(got, wireReport{x: x, y: y, buttons: buttons})
		return nil
	}, reports); err != nil {
		t.Fatalf("sendPointerDrag: %v", err)
	}
	if len(got) != len(reports) {
		t.Fatalf("wire reports = %+v, want %d reports", got, len(reports))
	}
	for i, report := range reports {
		want := wireReport{x: int32(report.X), y: int32(report.Y), buttons: byte(report.Buttons)}
		if got[i] != want {
			t.Errorf("wire report %d = %+v, want %+v", i+1, got[i], want)
		}
	}

	wantErr := errors.New("synthetic drag send failure")
	calls := 0
	err = sendPointerDrag(func(int32, int32, byte) error {
		calls++
		if calls == 3 {
			return wantErr
		}
		return nil
	}, reports)
	if !errors.Is(err, wantErr) || calls != 3 {
		t.Fatalf("failed drag = calls %d error %v, want 3 and %v", calls, err, wantErr)
	}

	invalid := append([]jetkvm.PointerDragReport(nil), reports...)
	invalid[len(invalid)-1].X = -1
	calls = 0
	if err := sendPointerDrag(func(int32, int32, byte) error {
		calls++
		return nil
	}, invalid); err == nil {
		t.Fatal("sendPointerDrag accepted an invalid generated coordinate")
	}
	if calls != 0 {
		t.Fatalf("sendPointerDrag sent %d reports before completing validation", calls)
	}
}

func TestCLITypeRequiresExplicitTextFlag(t *testing.T) {
	err := runType([]string{"--url", "http://device.invalid", "--allow-control"})
	if err == nil || !strings.Contains(err.Error(), "--text") {
		t.Fatalf("runType without --text = %v, want required-flag error", err)
	}
	if strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "dial") {
		t.Fatalf("runType connected before requiring --text: %v", err)
	}
}

func TestCLITypeErrorRenderingDoesNotReflectUnsupportedCharacter(t *testing.T) {
	exitCode, err := runCLI([]string{
		"type", "--url", "http://device.invalid", "--allow-control", "--text", "A☃z",
	})
	if exitCode != 1 || err == nil {
		t.Fatal("CLI did not reject unsupported type input")
	}
	assertCLITypeErrorContext(t, err, 2, "So", "☃", "'☃'", "U+2603")
}

func assertCLITypeErrorContext(t *testing.T, err error, position int, category string, reflected ...string) {
	t.Helper()
	for _, message := range []string{err.Error(), formatCLIError(err)} {
		if !strings.Contains(message, "position "+strconv.Itoa(position)) {
			t.Error("CLI type error omitted the one-based character position")
		}
		if !strings.Contains(message, "category: "+category) {
			t.Error("CLI type error omitted the Unicode category")
		}
		for _, candidate := range reflected {
			if strings.Contains(message, candidate) {
				t.Error("CLI type error reflected the caller-supplied character")
			}
		}
	}
}

type fakeTypeKeyboardControl struct {
	send    func(context.Context, byte, []byte) error
	release func() error
}

func (f *fakeTypeKeyboardControl) SendKeyboardReport(ctx context.Context, modifier byte, keys []byte) error {
	if f.send == nil {
		return nil
	}
	return f.send(ctx, modifier, keys)
}

func (f *fakeTypeKeyboardControl) Release() error {
	if f.release == nil {
		return nil
	}
	return f.release()
}

func TestCLITypeAcquireAndSendErrorsDoNotReflectCharacter(t *testing.T) {
	keypresses, err := jetkvm.MapTypeString("a~")
	if err != nil {
		t.Fatal("test fixture did not map")
	}
	wait := func(context.Context, time.Duration) error { return nil }

	t.Run("acquire", func(t *testing.T) {
		wantErr := errors.New("synthetic type acquire failure")
		acquireCalls := 0
		err := sendTypeKeypresses(
			context.Background(), keypresses, []rune("a~"), 0,
			func(context.Context, time.Duration) (typeKeyboardControl, error) {
				acquireCalls++
				if acquireCalls == 2 {
					return nil, wantErr
				}
				return &fakeTypeKeyboardControl{}, nil
			},
			wait,
		)
		if !errors.Is(err, wantErr) || acquireCalls != 2 {
			t.Fatal("CLI type acquire failure did not stop at the failing character")
		}
		assertCLITypeErrorContext(t, err, 2, "Sm", "~", "'~'", "U+007E")
	})

	t.Run("send", func(t *testing.T) {
		wantErr := errors.New("synthetic type send failure")
		sendCalls := 0
		err := sendTypeKeypresses(
			context.Background(), keypresses, []rune("a~"), 0,
			func(context.Context, time.Duration) (typeKeyboardControl, error) {
				return &fakeTypeKeyboardControl{send: func(context.Context, byte, []byte) error {
					sendCalls++
					if sendCalls == 2 {
						return wantErr
					}
					return nil
				}}, nil
			},
			wait,
		)
		if !errors.Is(err, wantErr) || sendCalls != 2 {
			t.Fatal("CLI type send failure did not stop at the failing character")
		}
		assertCLITypeErrorContext(t, err, 2, "Sm", "~", "'~'", "U+007E")
	})
}

func TestCLITypeDelayErrorDoesNotReflectNextCharacter(t *testing.T) {
	keypresses, err := jetkvm.MapTypeString("a~")
	if err != nil {
		t.Fatal("test fixture did not map")
	}
	wantErr := errors.New("synthetic type delay failure")
	acquireCalls := 0
	waitCalls := 0
	err = sendTypeKeypresses(
		context.Background(), keypresses, []rune("a~"), time.Millisecond,
		func(context.Context, time.Duration) (typeKeyboardControl, error) {
			acquireCalls++
			return &fakeTypeKeyboardControl{}, nil
		},
		func(context.Context, time.Duration) error {
			waitCalls++
			return wantErr
		},
	)
	if !errors.Is(err, wantErr) || acquireCalls != 1 || waitCalls != 1 {
		t.Fatal("CLI type delay failure did not stop before the next character")
	}
	assertCLITypeErrorContext(t, err, 2, "Sm", "~", "'~'", "U+007E")
}

func TestCLIKeySequenceRequiresComboBeforeSend(t *testing.T) {
	sendCalls := 0
	err := runKeySequenceWithSender(
		[]string{"--url", "http://device.invalid", "--allow-control"},
		func(context.Context, *commonFlags, []jetkvm.ResolvedKeyCombo, int) error {
			sendCalls++
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "--combo") {
		t.Fatalf("runKeySequenceWithSender without --combo = %v, want required-flag error", err)
	}
	if sendCalls != 0 {
		t.Fatalf("sender calls = %d, want 0", sendCalls)
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
