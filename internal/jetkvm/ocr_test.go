package jetkvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMain doubles this package's own test binary as a deterministic stand-in
// for tesseract. A normal go test invocation starts with -test.* flags; the OCR
// subprocess is deliberately invoked with exactly the fixed argv below.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-test.") {
		if !slices.Equal(os.Args[1:], []string{"stdin", "stdout"}) {
			fmt.Fprint(os.Stderr, "unexpected OCR argv")
			os.Exit(91)
		}
		runOCRHelperProcess()
	}
	os.Exit(m.Run())
}

func runOCRHelperProcess() {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(92)
	}
	switch string(input) {
	case "blank":
		os.Exit(0)
	case "fail":
		fmt.Fprintf(os.Stderr, "password=OCR-SECRET-CANARY tessdata=%s", os.Getenv("TESSDATA_PREFIX"))
		os.Exit(23)
	case "large":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), maxOCRTextBytes+1))
		os.Exit(0)
	case "invalid-utf8":
		_, _ = os.Stdout.Write([]byte{0xff, 0xfe})
		os.Exit(0)
	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "env":
		for _, entry := range os.Environ() {
			fmt.Fprintln(os.Stdout, entry)
		}
		os.Exit(0)
	default:
		_, _ = os.Stdout.Write(input)
		os.Exit(0)
	}
}

func testOCREngine(t *testing.T) *TesseractOCREngine {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("locating test executable: %v", err)
	}
	return &TesseractOCREngine{BinaryPath: binary, Timeout: 5 * time.Second}
}

func TestTesseractOCREngineCheckAvailableMissingIsTypedAndSafe(t *testing.T) {
	const canary = "OCR-PATH-SECRET-CANARY"
	engine := &TesseractOCREngine{BinaryPath: "/private/" + canary + "/tesseract"}
	err := engine.CheckAvailable(context.Background())
	if err == nil {
		t.Fatal("CheckAvailable succeeded for a missing executable")
	}
	if !errors.Is(err, ErrOCRUnavailable) {
		t.Fatalf("CheckAvailable error = %v, want ErrOCRUnavailable", err)
	}
	var unavailable *OCRUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("CheckAvailable error type = %T, want *OCRUnavailableError", err)
	}
	if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "/private/") {
		t.Fatalf("unavailable error leaked the configured binary path: %v", err)
	}
	for _, want := range []string{"OCR", "OCR text tools", "tesseract", "brew install tesseract"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("unavailable error is missing actionable text %q: %v", want, err)
		}
	}
}

func TestTesseractOCREngineCheckAvailableHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := testOCREngine(t).CheckAvailable(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckAvailable error = %v, want context.Canceled", err)
	}
}

func TestTesseractOCREngineReadTextPreservesExactOutput(t *testing.T) {
	engine := testOCREngine(t)
	want := "  JetKVM 100%\nsecond line\n\f"
	got, err := engine.ReadText(context.Background(), []byte(want))
	if err != nil {
		t.Fatalf("ReadText failed: %v", err)
	}
	if got != want {
		t.Fatalf("ReadText output = %q, want exact %q", got, want)
	}
}

func TestTesseractOCREngineReadTextAllowsBlankOutput(t *testing.T) {
	got, err := testOCREngine(t).ReadText(context.Background(), []byte("blank"))
	if err != nil {
		t.Fatalf("ReadText failed: %v", err)
	}
	if got != "" {
		t.Fatalf("ReadText blank output = %q, want empty string", got)
	}
}

func TestTesseractOCREngineReadTextRejectsEmptyInput(t *testing.T) {
	_, err := testOCREngine(t).ReadText(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "input image is empty") {
		t.Fatalf("ReadText empty input error = %v", err)
	}
}

func TestTesseractOCREngineReadTextDoesNotExposeFailureStderr(t *testing.T) {
	const pathCanary = "/Users/OCR-PATH-CANARY/private/tessdata"
	t.Setenv("TESSDATA_PREFIX", pathCanary)
	_, err := testOCREngine(t).ReadText(context.Background(), []byte("fail"))
	if err == nil {
		t.Fatal("ReadText succeeded after helper failure")
	}
	if strings.Contains(err.Error(), "OCR-SECRET-CANARY") || strings.Contains(err.Error(), pathCanary) {
		t.Fatalf("ReadText error leaked subprocess stderr: %v", err)
	}
	if !strings.Contains(err.Error(), "exit code 23") || strings.Contains(err.Error(), "stderr") {
		t.Fatalf("ReadText error does not expose only safe failure detail: %v", err)
	}
}

func TestTesseractOCREngineReadTextCapsAndDrainsOutput(t *testing.T) {
	_, err := testOCREngine(t).ReadText(context.Background(), []byte("large"))
	if err == nil || !strings.Contains(err.Error(), "output exceeded") {
		t.Fatalf("ReadText oversized output error = %v", err)
	}
}

func TestTesseractOCREngineReadTextRejectsInvalidUTF8(t *testing.T) {
	_, err := testOCREngine(t).ReadText(context.Background(), []byte("invalid-utf8"))
	if err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Fatalf("ReadText invalid UTF-8 error = %v", err)
	}
}

func TestOCRTextResultRejectsCancellationAfterSuccessfulOutput(t *testing.T) {
	stdout := newCappedBuffer(maxOCRTextBytes)
	if _, err := stdout.Write([]byte("recognized text")); err != nil {
		t.Fatalf("writing successful OCR output: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := ocrTextResult(ctx, stdout)
	if got != "" {
		t.Fatalf("ocrTextResult output = %q after cancellation, want empty", got)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ocrTextResult error = %v, want context.Canceled", err)
	}
}

func TestTesseractOCREngineCancellationReapsSubprocess(t *testing.T) {
	engine := testOCREngine(t)
	engine.Timeout = 75 * time.Millisecond
	start := time.Now()
	_, err := engine.ReadText(context.Background(), []byte("sleep"))
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadText cancellation error = %v, want context deadline", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("ReadText cancellation took %v; subprocess teardown is not bounded", elapsed)
	}
}

func TestOCREnvExcludesCredentials(t *testing.T) {
	toxic := map[string]bool{
		"JETKVM_PASSWORD":          true,
		"JETKVM_AUTH_TOKEN":        true,
		"OP_SERVICE_ACCOUNT_TOKEN": true,
		"HOME":                     true,
		"LD_LIBRARY_PATH":          true,
		"DYLD_INSERT_LIBRARIES":    true,
	}
	lookup := func(key string) (string, bool) {
		if toxic[key] {
			return "OCR-ENV-SECRET-CANARY", true
		}
		switch key {
		case "PATH":
			return "/usr/bin:/bin", true
		case "TESSDATA_PREFIX":
			return "/opt/share/tessdata", true
		default:
			return "", false
		}
	}

	env := ocrEnv(lookup)
	if env == nil {
		t.Fatal("ocrEnv returned nil; exec would inherit the complete parent environment")
	}
	for _, entry := range env {
		if strings.Contains(entry, "OCR-ENV-SECRET-CANARY") {
			t.Fatalf("ocrEnv inherited a credential: %q", entry)
		}
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(ocrEnvAllowlist, name) {
			t.Fatalf("ocrEnv included non-allowlisted variable %q", name)
		}
	}
	for _, want := range []string{"PATH=/usr/bin:/bin", "TESSDATA_PREFIX=/opt/share/tessdata"} {
		if !slices.Contains(env, want) {
			t.Errorf("ocrEnv = %v, missing %q", env, want)
		}
	}
}

func TestTesseractOCRSubprocessDoesNotSeeCredentials(t *testing.T) {
	t.Setenv("JETKVM_PASSWORD", "OCR-SUBPROCESS-SECRET-CANARY")
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "OCR-SUBPROCESS-SECRET-CANARY")
	output, err := testOCREngine(t).ReadText(context.Background(), []byte("env"))
	if err != nil {
		t.Fatalf("ReadText environment helper failed: %v", err)
	}
	if strings.Contains(output, "OCR-SUBPROCESS-SECRET-CANARY") || strings.Contains(output, "JETKVM_") {
		t.Fatalf("OCR subprocess inherited credentials: %s", output)
	}
}
