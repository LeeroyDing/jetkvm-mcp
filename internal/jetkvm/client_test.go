package jetkvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type unavailableDecoder struct {
	checkErr    error
	checkCalls  int
	decodeCalls int
}

func (d *unavailableDecoder) CheckAvailable(context.Context) error {
	d.checkCalls++
	return d.checkErr
}

func (d *unavailableDecoder) DecodeFrame(context.Context, []byte) (image.Image, error) {
	d.decodeCalls++
	return nil, errors.New("DecodeFrame must not run after a failed preflight")
}

func TestClientConnectStatusScreenshotNoPassword(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{DeviceVersion: "0.4.7+dev"})

	ctx := contextWithTimeout(t, connectTimeout(t, 15*time.Second))
	client, err := Connect(ctx, Options{BaseURL: fd.baseURL()})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close(context.Background())

	if client.DeviceID() != "fake-device-id" {
		t.Errorf("DeviceID() = %q, want fake-device-id", client.DeviceID())
	}
	if client.FirmwareVersion() != "0.4.7+dev" {
		t.Errorf("FirmwareVersion() = %q, want 0.4.7+dev", client.FirmwareVersion())
	}

	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if !status.RPCReachable {
		t.Error("expected RPCReachable = true")
	}

	outPath := filepath.Join(t.TempDir(), "shot.png")
	shot, err := client.SaveScreenshot(ctx, outPath)
	if err != nil {
		t.Fatalf("SaveScreenshot failed: %v", err)
	}
	if shot.Width != 32 || shot.Height != 32 {
		t.Errorf("screenshot size = %dx%d, want 32x32", shot.Width, shot.Height)
	}
	if !shot.Fresh {
		t.Error("expected freshly captured screenshot to be marked Fresh")
	}
	if shot.CapturedAt.IsZero() {
		t.Error("expected a non-zero CapturedAt")
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Errorf("expected screenshot file to exist: %v", err)
	}
}

func TestClientConnectRequiresAuthWhenPasswordSet(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{Password: "s3cret"})

	ctx := contextWithTimeout(t, 10*time.Second)
	_, err := Connect(ctx, Options{BaseURL: fd.baseURL()})
	if err == nil {
		t.Fatal("expected Connect to fail without credentials against a password-protected device")
	}
	if kind := ErrorKindOf(err); kind != ErrorKindAuthFailed {
		t.Fatalf("error kind = %q, want %q: %v", kind, ErrorKindAuthFailed, err)
	}
}

func TestClientConnectWithPassword(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{Password: "s3cret"})

	ctx := contextWithTimeout(t, connectTimeout(t, 15*time.Second))
	client, err := Connect(ctx, Options{
		BaseURL:     fd.baseURL(),
		Credentials: Credentials{Password: NewSecret("s3cret")},
	})
	if err != nil {
		t.Fatalf("Connect with password failed: %v", err)
	}
	defer client.Close(context.Background())

	if _, err := client.Status(ctx); err != nil {
		t.Fatalf("Status failed: %v", err)
	}
}

func TestStatusDoesNotRequireFFmpegAndScreenshotPreflights(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{})
	ctx := contextWithTimeout(t, connectTimeout(t, 15*time.Second))
	wantErr := errors.New("FFmpeg unavailable")
	decoder := &unavailableDecoder{checkErr: wantErr}
	client, err := Connect(ctx, Options{BaseURL: fd.baseURL(), Decoder: decoder})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close(context.Background())

	if _, err := client.Status(ctx); err != nil {
		t.Fatalf("Status required the screenshot decoder: %v", err)
	}
	if decoder.checkCalls != 0 || decoder.decodeCalls != 0 {
		t.Fatalf("Status touched decoder: checks=%d decodes=%d", decoder.checkCalls, decoder.decodeCalls)
	}
	if _, err := client.CaptureScreenshot(ctx); !errors.Is(err, wantErr) {
		t.Fatalf("CaptureScreenshot error = %v, want preflight error", err)
	}
	if decoder.checkCalls != 1 || decoder.decodeCalls != 0 {
		t.Fatalf("screenshot preflight/decode calls = %d/%d, want 1/0", decoder.checkCalls, decoder.decodeCalls)
	}
}

func TestClientControlDisabledByDefault(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{})
	ctx := contextWithTimeout(t, connectTimeout(t, 15*time.Second))
	client, err := Connect(ctx, Options{BaseURL: fd.baseURL()})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close(context.Background())

	if _, err := client.Control(); err == nil {
		t.Fatal("expected Control() to fail when AllowControl was not set")
	}
}

func TestClientControlEnabledOptIn(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{})
	ctx := contextWithTimeout(t, connectTimeout(t, 15*time.Second))
	client, err := Connect(ctx, Options{BaseURL: fd.baseURL(), AllowControl: true})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close(context.Background())

	lease, err := client.Control()
	if err != nil {
		t.Fatalf("Control() failed: %v", err)
	}
	held, err := lease.Acquire(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	_ = held.Release()
}

// TestCaptureScreenshotWritesNothing pins the property the MCP adapter
// depends on: capturing an image touches no filesystem path at all, so
// there is no caller-influenced path to attack.
func TestCaptureScreenshotWritesNothing(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{})
	ctx := contextWithTimeout(t, connectTimeout(t, 15*time.Second))
	client, err := Connect(ctx, Options{BaseURL: fd.baseURL()})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close(context.Background())

	workdir := t.TempDir()
	before, err := os.ReadDir(workdir)
	if err != nil {
		t.Fatal(err)
	}

	shot, err := client.CaptureScreenshot(ctx)
	if err != nil {
		t.Fatalf("CaptureScreenshot failed: %v", err)
	}
	if len(shot.PNG) == 0 {
		t.Fatal("CaptureScreenshot returned no image bytes")
	}
	if !bytes.HasPrefix(shot.PNG, []byte("\x89PNG\r\n\x1a\n")) {
		t.Error("CaptureScreenshot did not return a PNG")
	}
	if shot.Path != "" {
		t.Errorf("CaptureScreenshot reported a path %q; it must not write anywhere", shot.Path)
	}
	if shot.Width != 32 || shot.Height != 32 {
		t.Errorf("screenshot size = %dx%d, want 32x32", shot.Width, shot.Height)
	}

	after, err := os.ReadDir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("CaptureScreenshot created %d files, want 0", len(after)-len(before))
	}
}

func TestRapidScreenshotsEachUseANewerFrame(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{})
	ctx := contextWithTimeout(t, connectTimeout(t, 20*time.Second))
	client, err := Connect(ctx, Options{BaseURL: fd.baseURL()})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close(context.Background())

	first, err := client.CaptureScreenshot(ctx)
	if err != nil {
		t.Fatalf("first CaptureScreenshot failed: %v", err)
	}
	secondStarted := time.Now()
	second, err := client.CaptureScreenshot(ctx)
	if err != nil {
		t.Fatalf("second CaptureScreenshot failed: %v", err)
	}
	if !second.CapturedAt.After(first.CapturedAt) {
		t.Fatalf("second capturedAt = %v, want after first %v", second.CapturedAt, first.CapturedAt)
	}
	if second.CapturedAt.Before(secondStarted) {
		t.Fatalf("second screenshot used a frame from before its request: capturedAt=%v requestStarted=%v", second.CapturedAt, secondStarted)
	}
	if !first.Fresh || !second.Fresh {
		t.Fatalf("successful request-fresh screenshots must report fresh=true: first=%v second=%v", first.Fresh, second.Fresh)
	}
}

func TestScreenshotAfterControlUsesPostActionFrame(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{})
	ctx := contextWithTimeout(t, connectTimeout(t, 20*time.Second))
	client, err := Connect(ctx, Options{BaseURL: fd.baseURL(), AllowControl: true})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close(context.Background())

	lease, err := client.Control()
	if err != nil {
		t.Fatal(err)
	}
	held, err := lease.Acquire(ctx, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := held.SendKeyboardReport(ctx, 0, []byte{4}); err != nil {
		t.Fatalf("SendKeyboardReport failed: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
	actionCompleted := time.Now()

	shot, err := client.CaptureScreenshot(ctx)
	if err != nil {
		t.Fatalf("CaptureScreenshot failed: %v", err)
	}
	if shot.CapturedAt.Before(actionCompleted) {
		t.Fatalf("post-action screenshot predates completed action: capturedAt=%v actionCompleted=%v", shot.CapturedAt, actionCompleted)
	}
}

// TestSaveScreenshotIsAtomic checks the CLI's write path leaves no partial
// image and no stray temp file behind.
func TestSaveScreenshotIsAtomic(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{})
	ctx := contextWithTimeout(t, connectTimeout(t, 15*time.Second))
	client, err := Connect(ctx, Options{BaseURL: fd.baseURL()})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close(context.Background())

	dir := t.TempDir()
	outPath := filepath.Join(dir, "shot.png")
	if _, err := client.SaveScreenshot(ctx, outPath); err != nil {
		t.Fatalf("SaveScreenshot failed: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected exactly the screenshot in the output directory, got %v", names)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		t.Error("saved file is not a complete PNG")
	}
}

// TestScreenshotIsBoundedByContext proves a screenshot cannot hang past its
// deadline - the failure mode behind the unproven live-capture path, where
// a connected peer simply never produces a decodable frame.
func TestScreenshotIsBoundedByContext(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{WithoutVideo: true})
	ctx := contextWithTimeout(t, connectTimeout(t, 15*time.Second))
	client, err := Connect(ctx, Options{BaseURL: fd.baseURL()})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close(context.Background())

	shotCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = client.CaptureScreenshot(shotCtx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a screenshot against a frameless peer to fail, not hang")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("screenshot took %v; it must respect its deadline", elapsed)
	}
	if !strings.Contains(err.Error(), "no video frame available") {
		t.Errorf("error should explain that no frame arrived, got: %v", err)
	}
}

// TestScreenshotFailureNamesTheBoundary is the payoff of the diagnostic
// work against a real (fake-device) failure: the error must say which stage
// the pipeline stopped at, not merely that it timed out.
func TestScreenshotFailureNamesTheBoundary(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{WithoutVideo: true})
	ctx := contextWithTimeout(t, connectTimeout(t, 15*time.Second))
	client, err := Connect(ctx, Options{BaseURL: fd.baseURL()})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close(context.Background())

	shotCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = client.CaptureScreenshot(shotCtx)
	if err == nil {
		t.Fatal("expected the screenshot to fail against a peer that sends no media")
	}

	// A peer that negotiates but never sends media stops at one of these two
	// stages, depending on whether Pion surfaced the track at all.
	msg := err.Error()
	if !strings.Contains(msg, BoundaryNoRTP) && !strings.Contains(msg, BoundaryNegotiation) {
		t.Errorf("error did not localize the failure boundary: %v", err)
	}
	// The counts that make the boundary actionable have to be there too.
	for _, want := range []string{"pc=", "track=", "rtp=", "au=", "idr=", "pli=", "elapsed="} {
		if !strings.Contains(msg, want) {
			t.Errorf("error summary is missing %q: %v", want, err)
		}
	}

	// And it must not leak where the device is.
	host := strings.TrimPrefix(fd.baseURL(), "http://")
	if strings.Contains(msg, host) {
		t.Error("the screenshot error leaked the device address")
	}

	diag := client.VideoDiagnostics()
	if diag.FailureBoundary == BoundaryNone {
		t.Error("diagnostics reported success for a failed capture")
	}
	if diag.AnswerApplied != true {
		t.Error("expected the SDP answer to have been applied against the fake device")
	}
	encoded, err := json.Marshal(diag)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), host) {
		t.Error("the diagnostic snapshot leaked the device address")
	}
}

// TestSuccessfulScreenshotReportsCleanDiagnostics gives the failing-run
// numbers something to be compared against.
func TestSuccessfulScreenshotReportsCleanDiagnostics(t *testing.T) {
	fd := startFakeDevice(t, fakeDeviceOptions{})
	ctx := contextWithTimeout(t, connectTimeout(t, 20*time.Second))
	client, err := Connect(ctx, Options{BaseURL: fd.baseURL()})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close(context.Background())

	if _, err := client.CaptureScreenshot(ctx); err != nil {
		t.Fatalf("CaptureScreenshot failed: %v", err)
	}

	diag := client.VideoDiagnostics()
	if diag.FailureBoundary != BoundaryNone {
		t.Errorf("FailureBoundary = %q, want %q", diag.FailureBoundary, BoundaryNone)
	}
	if !diag.TrackObserved {
		t.Error("expected the video track to have been observed")
	}
	if diag.TrackMimeType != "video/H264" {
		t.Errorf("TrackMimeType = %q, want video/H264", diag.TrackMimeType)
	}
	if diag.RTPPackets == 0 {
		t.Error("expected RTP packets to have been counted")
	}
	if !diag.SawSPS || !diag.SawPPS || !diag.SawIDR {
		t.Errorf("expected SPS/PPS/IDR to have been seen: sps=%v pps=%v idr=%v",
			diag.SawSPS, diag.SawPPS, diag.SawIDR)
	}
	if diag.FramesAssembled == 0 {
		t.Error("expected at least one assembled frame")
	}
	if diag.DecodeFailure != "" {
		t.Errorf("unexpected decode failure category: %q", diag.DecodeFailure)
	}
}
