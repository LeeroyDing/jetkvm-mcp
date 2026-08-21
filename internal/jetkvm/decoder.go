package jetkvm

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Screenshot decode and encode limits. They are deliberately independent of
// the compressed H.264 access-unit bound: a tiny compressed frame can announce
// dimensions that require a very large decoded allocation.
const (
	maxScreenshotDimension = 8192
	maxScreenshotPixels    = 16 * 1024 * 1024

	// MaxScreenshotEncodedBytes bounds both FFmpeg's PNG stdout and every
	// in-process screenshot re-encode. Four bytes per pixel plus two MiB of
	// container/compression overhead comfortably covers incompressible RGBA
	// PNG data at the pixel cap while leaving normal 1080p and 4K captures
	// unchanged.
	MaxScreenshotEncodedBytes = maxScreenshotPixels*4 + 2*1024*1024

	// FFmpeg's -max_alloc limits any one libav allocation. This is above the
	// largest decoded image permitted here, but prevents a malformed stream
	// from requesting an effectively unbounded individual allocation in
	// codec/parser internals.
	maxFFmpegAllocationBytes = 256 * 1024 * 1024
	maxFFmpegStderrBytes     = 8 * 1024
)

// ValidateScreenshotDimensions rejects dimensions that could make image
// decode, crop, scale, or encode allocate outside the screenshot budget.
// Division is used instead of width*height so hostile integers cannot
// overflow the check.
func ValidateScreenshotDimensions(width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("screenshot dimensions %dx%d must be positive", width, height)
	}
	if width > maxScreenshotDimension || height > maxScreenshotDimension {
		return fmt.Errorf("screenshot dimensions %dx%d exceed the %d-pixel per-axis limit", width, height, maxScreenshotDimension)
	}
	if width > maxScreenshotPixels/height {
		return fmt.Errorf("screenshot dimensions %dx%d exceed the %d-pixel limit", width, height, maxScreenshotPixels)
	}
	return nil
}

// cappedBuffer keeps consuming writes after its storage limit is reached so a
// child process cannot deadlock on a full stdout/stderr pipe. Callers inspect
// Overflowed after the producer exits and return a clear size-limit error.
type cappedBuffer struct {
	data       []byte
	limit      int
	overflowed bool
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	remaining := b.limit - len(b.data)
	if remaining <= 0 {
		if written > 0 {
			b.overflowed = true
		}
		return written, nil
	}

	keep := min(written, remaining)
	required := len(b.data) + keep
	if required > cap(b.data) {
		capacity := max(required, cap(b.data)*2)
		capacity = min(capacity, b.limit)
		grown := make([]byte, len(b.data), capacity)
		copy(grown, b.data)
		b.data = grown
	}
	start := len(b.data)
	b.data = b.data[:required]
	copy(b.data[start:], p[:keep])
	if keep < written {
		b.overflowed = true
	}
	return written, nil
}

func (b *cappedBuffer) Bytes() []byte  { return b.data }
func (b *cappedBuffer) String() string { return string(b.data) }
func (b *cappedBuffer) Len() int       { return len(b.data) }
func (b *cappedBuffer) Overflowed() bool {
	return b.overflowed
}

// Decoder turns one self-contained Annex-B H.264 frame into a decoded
// image. It is a narrow interface so the H.264 decode step - the most
// likely source of firmware/codec drift (profile changes, a future H.265
// default, etc.) - can be swapped without touching session or screenshot
// orchestration code.
type Decoder interface {
	DecodeFrame(ctx context.Context, annexB []byte) (image.Image, error)
}

// FFmpegDecoder shells out to the system ffmpeg binary to decode a single
// H.264 frame to an image. FFmpeg is treated as an external, replaceable
// dependency (not vendored, not assumed to be a specific version) exactly
// because the decode step is the piece most likely to need to change
// independently of the WebRTC/signaling code.
type FFmpegDecoder struct {
	// BinaryPath overrides the ffmpeg binary to invoke. Defaults to
	// "ffmpeg" (resolved via PATH) when empty.
	BinaryPath string
	// Timeout bounds how long a single decode may take. Defaults to 10s
	// when zero.
	Timeout time.Duration
}

func (d *FFmpegDecoder) binary() string {
	if d.BinaryPath != "" {
		return d.BinaryPath
	}
	return "ffmpeg"
}

func (d *FFmpegDecoder) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return 10 * time.Second
}

// ffmpegEnvAllowlist is the complete set of environment variables passed
// through to the ffmpeg subprocess. It is an allowlist, not a denylist:
// this process may hold JETKVM_PASSWORD, JETKVM_AUTH_TOKEN, or credentials
// belonging to whatever agent/secret manager launched it, and a decoder
// subprocess has no business seeing any of them. Anything not named here
// - including every JETKVM_* variable - is dropped.
//
// Deliberately excluded even though ffmpeg builds sometimes read them:
// LD_LIBRARY_PATH and DYLD_* (library-injection vectors), HOME (pulls in
// user config), and the FFREPORT/AV_LOG_* family (can direct ffmpeg to
// write files).
var ffmpegEnvAllowlist = []string{
	"PATH",
	"TMPDIR",
	"TMP",
	"TEMP",
	// Windows needs these to load system DLLs at all.
	"SystemRoot",
	"WINDIR",
}

// decoderEnv builds the subprocess environment from the allowlist. It
// always returns a non-nil slice: exec.Cmd inherits the parent's full
// environment when Env is nil, which is precisely what must not happen.
func decoderEnv(lookup func(string) (string, bool)) []string {
	env := make([]string, 0, len(ffmpegEnvAllowlist))
	for _, key := range ffmpegEnvAllowlist {
		if value, ok := lookup(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// newFFmpegCmd builds a subprocess with a minimal environment and bounded
// teardown semantics.
func (d *FFmpegDecoder) newFFmpegCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, d.binary(), args...)
	cmd.Env = decoderEnv(os.LookupEnv)
	// exec.CommandContext kills the process when ctx ends, but Wait still
	// blocks until every inherited pipe is closed - a wedged child (or a
	// grandchild holding stdout) would otherwise hang the caller past its
	// deadline. WaitDelay bounds that: pipes are force-closed and the
	// process is killed, so Wait always returns and the child is always
	// reaped.
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// DecodeFrame pipes annexB to ffmpeg on stdin and reads one PNG-encoded
// frame back on stdout, entirely in memory (no temp files, nothing written
// to disk that could linger with frame content).
//
// The subprocess runs with a minimal allowlisted environment, is bounded
// by both ctx and the decoder's own timeout, and is always reaped.
func (d *FFmpegDecoder) DecodeFrame(ctx context.Context, annexB []byte) (image.Image, error) {
	return d.decodeFrame(ctx, annexB, MaxScreenshotEncodedBytes)
}

func (d *FFmpegDecoder) decodeFrame(ctx context.Context, annexB []byte, outputLimit int) (image.Image, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout())
	defer cancel()

	cmd := d.newFFmpegCmd(ctx,
		"-hide_banner", "-loglevel", "error",
		"-max_alloc", strconv.Itoa(maxFFmpegAllocationBytes),
		"-max_pixels", strconv.Itoa(maxScreenshotPixels),
		"-f", "h264", "-i", "pipe:0",
		"-frames:v", "1",
		"-f", "image2", "-vcodec", "png",
		"pipe:1",
	)
	// bytes.Reader (not an *os.File) means exec copies stdin through a
	// goroutine it owns and closes; there is no pipe fd for this process to
	// leak.
	cmd.Stdin = bytes.NewReader(annexB)

	stdout := newCappedBuffer(outputLimit)
	stderr := newCappedBuffer(maxFFmpegStderrBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("jetkvm: ffmpeg decode canceled: %w", ctxErr)
	}
	if stdout.Overflowed() {
		return nil, fmt.Errorf("jetkvm: ffmpeg PNG output exceeds the %d-byte limit", outputLimit)
	}
	if runErr != nil {
		// ffmpeg's stderr is device/codec diagnostics, but it is redacted
		// anyway: it is attacker-influenced data being placed into an error
		// that may be shown to an agent.
		return nil, fmt.Errorf("jetkvm: ffmpeg decode failed: %s (stderr: %s)",
			RedactError(runErr), redactSensitive(truncate(stderr.String(), 500)))
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("jetkvm: ffmpeg produced no output decoding frame (stderr: %s)",
			redactSensitive(truncate(stderr.String(), 500)))
	}

	return decodeFFmpegPNG(ctx, stdout.Bytes())
}

func decodeFFmpegPNG(ctx context.Context, encoded []byte) (image.Image, error) {
	config, err := png.DecodeConfig(bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("jetkvm: decoding ffmpeg PNG header: %w", err)
	}
	if err := ValidateScreenshotDimensions(config.Width, config.Height); err != nil {
		return nil, fmt.Errorf("jetkvm: rejecting ffmpeg PNG output: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("jetkvm: ffmpeg decode canceled before PNG decode: %w", err)
	}

	img, err := png.Decode(bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("jetkvm: decoding ffmpeg PNG output: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("jetkvm: ffmpeg decode canceled after PNG decode: %w", err)
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if err := ValidateScreenshotDimensions(width, height); err != nil {
		return nil, fmt.Errorf("jetkvm: rejecting decoded ffmpeg PNG: %w", err)
	}
	if width != config.Width || height != config.Height {
		return nil, fmt.Errorf("jetkvm: decoded ffmpeg PNG dimensions do not match its header")
	}
	return img, nil
}

// CheckAvailable runs `ffmpeg -version` to fail fast with an actionable
// error if ffmpeg isn't installed/on PATH, rather than surfacing a raw
// "executable file not found" from deep inside a screenshot call.
func (d *FFmpegDecoder) CheckAvailable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := d.newFFmpegCmd(ctx, "-version")
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("jetkvm: FFmpeg preflight canceled: %w", ctxErr)
		}
		return fmt.Errorf("jetkvm: FFmpeg is unavailable; screenshots and stable-screen waits require the ffmpeg executable on PATH (install with `brew install ffmpeg` on macOS or your Linux package manager). Status remains usable without FFmpeg")
	}
	return nil
}
