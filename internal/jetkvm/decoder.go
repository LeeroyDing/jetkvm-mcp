package jetkvm

import (
	"bytes"
	"context"
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

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// ffmpeg's stderr is device/codec diagnostics, but it is redacted
		// anyway: it is attacker-influenced data being placed into an error
		// that may be shown to an agent.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("jetkvm: ffmpeg decode canceled: %w", ctxErr)
		}
		return nil, fmt.Errorf("jetkvm: ffmpeg decode failed: %s (stderr: %s)",
			RedactError(err), redactSensitive(truncate(stderr.String(), 500)))
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("jetkvm: ffmpeg produced no output decoding frame (stderr: %s)",
			redactSensitive(truncate(stderr.String(), 500)))
	}

	img, _, err := image.Decode(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("jetkvm: decoding ffmpeg PNG output: %w", err)
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
