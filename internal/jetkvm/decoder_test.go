package jetkvm

import (
	"bytes"
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found on PATH; skipping decoder test")
	}
}

func TestFFmpegDecoderDecodesSyntheticFrame(t *testing.T) {
	requireFFmpeg(t)

	dec := &FFmpegDecoder{Timeout: 10 * time.Second}
	if err := dec.CheckAvailable(context.Background()); err != nil {
		t.Fatalf("ffmpeg reported unavailable: %v", err)
	}

	data := loadSyntheticFrame(t)
	img, err := dec.DecodeFrame(context.Background(), data)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}

	bounds := img.Bounds()
	if bounds.Dx() != 32 || bounds.Dy() != 32 {
		t.Errorf("decoded image size = %dx%d, want 32x32", bounds.Dx(), bounds.Dy())
	}

	// The fixture is a solid red frame; sample the center pixel and check
	// it's red-dominant rather than asserting an exact RGB match (libx264
	// chroma subsampling means it won't be pure #FF0000).
	r, g, b, _ := img.At(bounds.Dx()/2, bounds.Dy()/2).RGBA()
	if !(r > g && r > b) {
		t.Errorf("expected a red-dominant pixel, got r=%d g=%d b=%d", r, g, b)
	}
}

func TestFFmpegDecoderRejectsGarbage(t *testing.T) {
	requireFFmpeg(t)

	dec := &FFmpegDecoder{Timeout: 5 * time.Second}
	_, err := dec.DecodeFrame(context.Background(), []byte{0x00, 0x01, 0x02, 0x03})
	if err == nil {
		t.Fatal("expected an error decoding non-H.264 garbage")
	}
}

func TestFFmpegDecoderCheckAvailableReportsMissingBinary(t *testing.T) {
	dec := &FFmpegDecoder{BinaryPath: "definitely-not-a-real-ffmpeg-binary-xyz"}
	err := dec.CheckAvailable(context.Background())
	if err == nil {
		t.Fatal("expected an error for a nonexistent ffmpeg binary")
	}
}

// toxicEnv is the set of variables this process can plausibly be holding
// when it launches a decoder: the device credentials it was configured
// with, plus secrets belonging to whatever agent or secret manager started
// it. None of them are the decoder's business.
var toxicEnv = []string{
	"JETKVM_PASSWORD",
	"JETKVM_AUTH_TOKEN",
	"JETKVM_URL",
	"OP_SERVICE_ACCOUNT_TOKEN",
	"OP_SESSION_my",
	"OPENCLAW_TOKEN",
	"OPENCLAW_CONFIG",
	"ANTHROPIC_API_KEY",
	"AWS_SECRET_ACCESS_KEY",
	"GITHUB_TOKEN",
	"SSH_AUTH_SOCK",
	"LD_LIBRARY_PATH",
	"DYLD_INSERT_LIBRARIES",
	"HOME",
}

// TestDecoderEnvExcludesCredentials is the process-boundary proof: the
// ffmpeg subprocess receives an allowlist, so no credential this process
// holds can be inherited by it (or by anything it might exec).
func TestDecoderEnvExcludesCredentials(t *testing.T) {
	lookup := func(key string) (string, bool) {
		for _, toxic := range toxicEnv {
			if key == toxic {
				return "leaked-value-must-never-appear", true
			}
		}
		switch key {
		case "PATH":
			return "/usr/bin:/bin", true
		case "TMPDIR":
			return "/tmp", true
		}
		return "", false
	}

	env := decoderEnv(lookup)

	for _, entry := range env {
		if strings.Contains(entry, "leaked-value-must-never-appear") {
			name, _, _ := strings.Cut(entry, "=")
			t.Errorf("decoder environment inherited %s, which may carry credentials", name)
		}
	}

	// The allowlist is a positive list: every entry must be one of the
	// names we chose, not merely "not obviously a secret".
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(ffmpegEnvAllowlist, name) {
			t.Errorf("decoder environment contains %s, which is not on the allowlist", name)
		}
	}

	// And PATH has to survive, or ffmpeg could not be found at all.
	var sawPath bool
	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			sawPath = true
		}
	}
	if !sawPath {
		t.Error("decoder environment dropped PATH; ffmpeg would not be resolvable")
	}
}

// TestDecoderEnvIsNeverNil matters more than it looks: exec.Cmd inherits
// the parent's entire environment when Env is nil, which is exactly the
// failure this allowlist exists to prevent.
func TestDecoderEnvIsNeverNil(t *testing.T) {
	env := decoderEnv(func(string) (string, bool) { return "", false })
	if env == nil {
		t.Fatal("decoderEnv returned nil; exec would inherit the full parent environment")
	}
	if len(env) != 0 {
		t.Fatalf("expected an empty environment when nothing is set, got %v", env)
	}
}

// TestFFmpegDecoderSubprocessDoesNotSeeCredentials runs the real subprocess
// boundary end to end, using `env` as a stand-in for ffmpeg so its output is
// the child's actual environment.
func TestFFmpegDecoderSubprocessDoesNotSeeCredentials(t *testing.T) {
	envBinary, err := exec.LookPath("env")
	if err != nil {
		t.Skip("no env binary available to inspect the child environment")
	}
	t.Setenv("JETKVM_PASSWORD", "leaked-value-must-never-appear")
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "leaked-value-must-never-appear")

	d := &FFmpegDecoder{BinaryPath: envBinary}
	cmd := d.newFFmpegCmd(context.Background())
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("running env: %v", err)
	}

	if strings.Contains(stdout.String(), "leaked-value-must-never-appear") {
		t.Error("the decoder subprocess inherited a credential from this process")
	}
	if strings.Contains(stdout.String(), "JETKVM_") {
		t.Error("the decoder subprocess inherited JETKVM_* configuration")
	}
}

// TestFFmpegDecoderCancellationReapsSubprocess proves the decode is bounded
// and leaves nothing behind: a canceled context must return promptly with a
// cancellation error rather than blocking on a child that outlives it.
func TestFFmpegDecoderCancellationReapsSubprocess(t *testing.T) {
	sleepBinary, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary available")
	}

	d := &FFmpegDecoder{BinaryPath: sleepBinary, Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err = d.DecodeFrame(context.Background(), []byte("ignored"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a decode error when the subprocess is killed")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("decode took %v; cancellation must be bounded", elapsed)
	}
}

// TestFFmpegDecoderErrorsAreRedacted checks the decoder's error path cannot
// become a side channel for anything sensitive in its output.
func TestFFmpegDecoderErrorsAreRedacted(t *testing.T) {
	d := &FFmpegDecoder{BinaryPath: "/nonexistent/ffmpeg-binary"}
	err := d.CheckAvailable(context.Background())
	if err == nil {
		t.Fatal("expected an error for a missing binary")
	}
	if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "authToken") {
		t.Errorf("decoder error mentioned credential material: %v", err)
	}
}
