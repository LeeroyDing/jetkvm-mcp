package jetkvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/png" // decode dimensions from the PNG ffmpeg produces
	"os"
	"os/exec"
	"time"
)

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

const (
	// JetKVM's advertised preferred/current maximum is 1920x1080. A pixel
	// count cap permits an equivalent portrait frame while the per-axis cap
	// rejects pathological narrow images whose row bookkeeping can itself be
	// expensive. The encoded PNG bound is deliberately above an uncompressed
	// RGBA 1080p frame while still preventing device-controlled stdout growth.
	maxDecodedPixels     = 1920 * 1080
	maxDecodedDimension  = 4096
	maxFFmpegPNGBytes    = 16 << 20
	maxFFmpegStderrBytes = 16 << 10
)

var errFFmpegOutputLimit = errors.New("ffmpeg output exceeded safety limit")

// cappedBuffer stops a subprocess pipe once its safety bound is reached.
// It is used only for frame bytes: returning an error makes os/exec close the
// read side promptly rather than continuing to drain attacker-controlled data.
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.exceeded = true
		return 0, errFFmpegOutputLimit
	}
	if len(p) > remaining {
		n, _ := b.buf.Write(p[:remaining])
		b.exceeded = true
		return n, errFFmpegOutputLimit
	}
	return b.buf.Write(p)
}

// boundedCapture retains only a prefix but reports every byte consumed. It
// keeps diagnostic stderr memory-bounded without turning harmless extra log
// output into a pipe failure that could obscure the real ffmpeg exit status.
type boundedCapture struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedCapture) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = b.buf.Write(p[:remaining])
	}
	return len(p), nil
}

type contextReader struct {
	ctx  context.Context
	next *bytes.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.next.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return 0, ctxErr
	}
	return n, err
}

func decodeBoundedPNG(ctx context.Context, data []byte) (image.Image, error) {
	config, format, err := image.DecodeConfig(&contextReader{ctx: ctx, next: bytes.NewReader(data)})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &CompatibilityError{Stage: "video-decode", Detail: "FFmpeg output was not a valid PNG image"}
	}
	if format != "png" || config.Width <= 0 || config.Height <= 0 ||
		config.Width > maxDecodedDimension || config.Height > maxDecodedDimension ||
		config.Width*config.Height > maxDecodedPixels {
		return nil, &CompatibilityError{Stage: "video-decode", Detail: "decoded frame dimensions exceed the supported safety bound"}
	}
	img, format, err := image.Decode(&contextReader{ctx: ctx, next: bytes.NewReader(data)})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &CompatibilityError{Stage: "video-decode", Detail: "FFmpeg output was not a valid PNG image"}
	}
	if format != "png" {
		return nil, &CompatibilityError{Stage: "video-decode", Detail: "FFmpeg output was not a PNG image"}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return img, nil
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
	ctx, cancel := context.WithTimeout(ctx, d.timeout())
	defer cancel()

	cmd := d.newFFmpegCmd(ctx,
		"-hide_banner", "-loglevel", "error",
		"-f", "h264", "-i", "pipe:0",
		"-frames:v", "1",
		"-f", "image2", "-vcodec", "png",
		"pipe:1",
	)
	// bytes.Reader (not an *os.File) means exec copies stdin through a
	// goroutine it owns and closes; there is no pipe fd for this process to
	// leak.
	cmd.Stdin = bytes.NewReader(annexB)

	stdout := &cappedBuffer{limit: maxFFmpegPNGBytes}
	stderr := &boundedCapture{limit: maxFFmpegStderrBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		if stdout.exceeded {
			return nil, fmt.Errorf("jetkvm: FFmpeg PNG output exceeded the supported safety bound")
		}
		// ffmpeg's stderr is device/codec diagnostics, but it is redacted
		// anyway: it is attacker-influenced data being placed into an error
		// that may be shown to an agent.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("jetkvm: ffmpeg decode canceled: %w", ctxErr)
		}
		return nil, fmt.Errorf("jetkvm: ffmpeg decode failed: %s (stderr: %s)",
			RedactError(err), redactSensitive(truncate(stderr.buf.String(), 500)))
	}
	if stdout.buf.Len() == 0 {
		return nil, fmt.Errorf("jetkvm: ffmpeg produced no output decoding frame (stderr: %s)",
			redactSensitive(truncate(stderr.buf.String(), 500)))
	}

	img, err := decodeBoundedPNG(ctx, stdout.buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("jetkvm: decoding FFmpeg PNG output: %w", err)
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
		return fmt.Errorf("jetkvm: FFmpeg is unavailable; screenshots require the ffmpeg executable on PATH (install with `brew install ffmpeg` on macOS or your Linux package manager). Status remains usable without FFmpeg")
	}
	return nil
}
