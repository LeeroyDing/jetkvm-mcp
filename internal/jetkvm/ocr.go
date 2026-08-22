package jetkvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"time"
	"unicode/utf8"
)

const (
	defaultOCRTimeout       = 10 * time.Second
	ocrProcessWaitDelay     = 2 * time.Second
	maxOCRTextBytes         = 1 << 20
	maxOCRStderrCaptureSize = 16 << 10
)

// ErrOCRUnavailable is the stable sentinel for a host that has no usable OCR
// executable. Callers should use errors.Is rather than matching the
// human-readable installation guidance returned by OCRUnavailableError.
var ErrOCRUnavailable = errors.New("jetkvm: OCR engine unavailable")

// OCRUnavailableError reports that the optional local OCR dependency could
// not be resolved. It intentionally carries no binary path or underlying
// lookup error: either may contain operator-controlled or sensitive text.
type OCRUnavailableError struct{}

func (*OCRUnavailableError) Error() string {
	return "jetkvm: OCR is unavailable; OCR text tools require the tesseract executable on PATH (install with `brew install tesseract` on macOS or your Linux package manager)"
}

func (*OCRUnavailableError) Unwrap() error { return ErrOCRUnavailable }

// OCREngine extracts UTF-8 plain text from an in-memory encoded image.
// CheckAvailable lets callers fail before capturing a screen when the
// optional local engine is absent.
type OCREngine interface {
	CheckAvailable(context.Context) error
	ReadText(context.Context, []byte) (string, error)
}

// TesseractOCREngine invokes the optional system tesseract executable. Image
// bytes travel through stdin and recognized text through stdout; no shell,
// image path, or temporary output file is involved.
type TesseractOCREngine struct {
	// BinaryPath overrides the tesseract executable to resolve. It is intended
	// for embedding and tests, not for caller-controlled MCP arguments.
	BinaryPath string
	// Timeout bounds one OCR subprocess. Values at or below zero use 10s.
	Timeout time.Duration
}

var _ OCREngine = (*TesseractOCREngine)(nil)

func (e *TesseractOCREngine) binary() string {
	if e != nil && e.BinaryPath != "" {
		return e.BinaryPath
	}
	return "tesseract"
}

func (e *TesseractOCREngine) timeout() time.Duration {
	if e != nil && e.Timeout > 0 {
		return e.Timeout
	}
	return defaultOCRTimeout
}

func (e *TesseractOCREngine) resolveBinary() (string, error) {
	path, err := exec.LookPath(e.binary())
	if err != nil {
		return "", &OCRUnavailableError{}
	}
	return path, nil
}

// CheckAvailable performs runtime detection without starting tesseract. The
// real ReadText call resolves the executable again so this preflight is never
// relied on as a stale availability guarantee.
func (e *TesseractOCREngine) CheckAvailable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("jetkvm: tesseract OCR preflight canceled: %w", err)
	}
	_, err := e.resolveBinary()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("jetkvm: tesseract OCR preflight canceled: %w", ctxErr)
	}
	return err
}

// ocrEnvAllowlist is the complete inherited environment exposed to the OCR
// subprocess. In particular, HOME, JETKVM_*, secret-manager credentials, and
// dynamic-loader injection variables are not inherited. TESSDATA_PREFIX is
// retained so an operator can use a non-default trained-data installation.
var ocrEnvAllowlist = []string{
	"PATH",
	"TMPDIR",
	"TMP",
	"TEMP",
	"TESSDATA_PREFIX",
	// Windows needs these to load system DLLs.
	"SystemRoot",
	"WINDIR",
}

// ocrEnv always returns a non-nil slice. A nil exec.Cmd.Env would inherit the
// parent process's complete, potentially credential-bearing environment.
func ocrEnv(lookup func(string) (string, bool)) []string {
	env := make([]string, 0, len(ocrEnvAllowlist))
	for _, key := range ocrEnvAllowlist {
		if value, ok := lookup(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func (e *TesseractOCREngine) newCommand(ctx context.Context, binary string) *exec.Cmd {
	// "stdin" and "stdout" are Tesseract's documented in-memory input and
	// output targets. Keeping this argv fixed removes an argument-injection
	// seam and avoids invoking a shell altogether.
	cmd := exec.CommandContext(ctx, binary, "stdin", "stdout")
	cmd.Env = ocrEnv(os.LookupEnv)
	cmd.WaitDelay = ocrProcessWaitDelay
	return cmd
}

func unavailableOCRStartError(err error) bool {
	return errors.Is(err, exec.ErrNotFound) ||
		errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, fs.ErrPermission)
}

func ocrExitError(err error) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		// Start/wait errors can include the configured executable path. Keep
		// this error static rather than reflecting it across CLI/MCP.
		return errors.New("jetkvm: tesseract OCR process failed")
	}

	// Tesseract diagnostics can include local filesystem paths such as
	// TESSDATA_PREFIX. They are useful at a terminal but are not safe to send
	// across an MCP boundary, and generic credential redaction cannot remove
	// arbitrary operator paths. Keep the stable exit status and drop stderr.
	return fmt.Errorf("jetkvm: tesseract OCR failed (exit code %d)", exitErr.ExitCode())
}

func ocrTextResult(ctx context.Context, stdout *cappedBuffer) (string, error) {
	if stdout.Overflowed() {
		return "", fmt.Errorf("jetkvm: tesseract OCR output exceeded the %d-byte limit", maxOCRTextBytes)
	}
	if !utf8.Valid(stdout.Bytes()) {
		return "", errors.New("jetkvm: tesseract OCR produced invalid UTF-8 output")
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("jetkvm: tesseract OCR canceled: %w", err)
	}
	return stdout.String(), nil
}

// ReadText runs one bounded Tesseract recognition pass. Successful stdout is
// returned byte-for-byte as a Go string after UTF-8 validation: leading and
// trailing whitespace are meaningful, and an empty result is a valid outcome
// for a screen containing no recognizable text.
func (e *TesseractOCREngine) ReadText(ctx context.Context, imageData []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("jetkvm: tesseract OCR canceled: %w", err)
	}
	if len(imageData) == 0 {
		return "", errors.New("jetkvm: OCR input image is empty")
	}

	binary, err := e.resolveBinary()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("jetkvm: tesseract OCR canceled: %w", ctxErr)
	}
	if err != nil {
		return "", err
	}

	runCtx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()

	cmd := e.newCommand(runCtx, binary)
	cmd.Stdin = bytes.NewReader(imageData)
	stdout := newCappedBuffer(maxOCRTextBytes)
	stderr := newCappedBuffer(maxOCRStderrCaptureSize)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := runCtx.Err(); ctxErr != nil {
			return "", fmt.Errorf("jetkvm: tesseract OCR canceled: %w", ctxErr)
		}
		if unavailableOCRStartError(err) {
			return "", &OCRUnavailableError{}
		}
		return "", ocrExitError(err)
	}
	return ocrTextResult(runCtx, stdout)
}
