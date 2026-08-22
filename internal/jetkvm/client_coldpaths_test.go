package jetkvm

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type coldPathDecoder struct {
	image  image.Image
	err    error
	cancel context.CancelFunc
}

func (d coldPathDecoder) DecodeFrame(context.Context, []byte) (image.Image, error) {
	if d.cancel != nil {
		d.cancel()
	}
	return d.image, d.err
}

func newColdPathScreenshotClient(decoder Decoder) (*Client, *frameCapture) {
	diag := newVideoDiagnostics()
	video := newFrameCapture(diag)
	return &Client{
		decoder: decoder,
		cmdMu:   make(chan struct{}, 1),
		sess: &session{
			video: video,
			diag:  diag,
		},
	}, video
}

func deliverColdPathFrame(video *frameCapture) {
	video.ingest(buildAnnexB(
		[]byte{0x67, 0x01},
		[]byte{0x68, 0x01},
		[]byte{0x65, 0x01},
	))
}

// coldPathFrameContext publishes a fresh frame when CaptureScreenshot reaches
// frameCapture's wait select. Client.lock performs the first Done observation;
// the frame wait performs the second. The hook runs after the request boundary
// was recorded and closes the exact update channel the waiter selected on.
type coldPathFrameContext struct {
	context.Context
	video     *frameCapture
	doneCalls int
}

type cancelBeforeScreenshotCommitContext struct {
	done     chan struct{}
	errCalls int
}

func newCancelBeforeScreenshotCommitContext() *cancelBeforeScreenshotCommitContext {
	return &cancelBeforeScreenshotCommitContext{done: make(chan struct{})}
}

func (*cancelBeforeScreenshotCommitContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelBeforeScreenshotCommitContext) Done() <-chan struct{}     { return c.done }
func (*cancelBeforeScreenshotCommitContext) Value(any) any               { return nil }

func (c *cancelBeforeScreenshotCommitContext) Err() error {
	c.errCalls++
	if c.errCalls < 2 {
		return nil
	}
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return context.Canceled
}

func (c *coldPathFrameContext) Done() <-chan struct{} {
	c.doneCalls++
	if c.doneCalls == 2 {
		deliverColdPathFrame(c.video)
	}
	return c.Context.Done()
}

func captureColdPathFrame(
	t *testing.T,
	ctx context.Context,
	client *Client,
	video *frameCapture,
) (Screenshot, error) {
	t.Helper()
	return client.CaptureScreenshot(&coldPathFrameContext{Context: ctx, video: video})
}

func saveColdPathFrame(
	t *testing.T,
	client *Client,
	video *frameCapture,
	outputPath string,
) (ScreenshotResult, error) {
	t.Helper()
	ctx := &coldPathFrameContext{Context: context.Background(), video: video}
	return client.SaveScreenshot(ctx, outputPath)
}

// cancelAfterClientLockCheckContext cancels exactly after Client.lock's fail-fast
// check. Done remains open while the select chooses the free lock slot, then
// the post-acquisition Err check closes it. This deterministically exercises
// the cancellation race without relying on scheduler timing.
type cancelAfterClientLockCheckContext struct {
	done     chan struct{}
	errCalls int
}

func newCancelAfterClientLockCheckContext() *cancelAfterClientLockCheckContext {
	return &cancelAfterClientLockCheckContext{done: make(chan struct{})}
}

func (*cancelAfterClientLockCheckContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterClientLockCheckContext) Done() <-chan struct{}     { return c.done }
func (*cancelAfterClientLockCheckContext) Value(any) any               { return nil }

func (c *cancelAfterClientLockCheckContext) Err() error {
	c.errCalls++
	if c.errCalls == 1 {
		return nil
	}
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return context.Canceled
}

func TestClientLockReturnsSlotWhenCancellationRacesAcquisition(t *testing.T) {
	client := &Client{cmdMu: make(chan struct{}, 1)}
	ctx := newCancelAfterClientLockCheckContext()

	unlock, err := client.lock(ctx)
	if unlock != nil {
		unlock()
		t.Fatal("lock returned an unlock function after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lock error = %v, want context.Canceled", err)
	}
	if len(client.cmdMu) != 0 {
		t.Fatal("canceled acquisition retained the command lock slot")
	}
}

func TestClientOperationsRejectCanceledCommandLock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "status",
			run: func() error {
				_, err := (&Client{cmdMu: make(chan struct{}, 1)}).Status(ctx)
				return err
			},
		},
		{
			name: "scroll",
			run: func() error {
				return (&Client{
					allowControl: true,
					control:      newControlLease(nil),
					cmdMu:        make(chan struct{}, 1),
				}).Scroll(ctx, 0, 1)
			},
		},
		{
			name: "capture screenshot",
			run: func() error {
				_, err := (&Client{
					decoder: coldPathDecoder{},
					cmdMu:   make(chan struct{}, 1),
				}).CaptureScreenshot(ctx)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, context.Canceled) {
				t.Fatalf("operation error = %v, want context.Canceled", err)
			}
		})
	}
}

func TestConnectReturnsSignalingUpgradeFailure(t *testing.T) {
	mode := "noPassword"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/status":
			_ = json.NewEncoder(w).Encode(DeviceStatus{IsSetup: true})
		case "/device":
			_ = json.NewEncoder(w).Encode(LocalDevice{AuthMode: &mode, DeviceID: "cold-path-device"})
		case "/webrtc/signaling/client":
			http.Error(w, "signaling unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := Connect(context.Background(), Options{
		BaseURL:     srv.URL,
		HTTPTimeout: time.Second,
	})
	if client != nil {
		_ = client.Close(context.Background())
		t.Fatal("Connect returned a client after signaling upgrade rejection")
	}
	if kind := ErrorKindOf(err); kind != ErrorKindUnreachable {
		t.Fatalf("Connect error kind = %q, want %q: %v", kind, ErrorKindUnreachable, err)
	}
}

func TestCaptureScreenshotColdFailures(t *testing.T) {
	t.Run("terminal video error without context cancellation", func(t *testing.T) {
		wantErr := errors.New("synthetic video stream failure")
		client, video := newColdPathScreenshotClient(coldPathDecoder{})
		video.fail(wantErr)

		_, err := client.CaptureScreenshot(context.Background())
		if !errors.Is(err, wantErr) {
			t.Fatalf("CaptureScreenshot error = %v, want %v", err, wantErr)
		}
		if !strings.Contains(err.Error(), "no video frame available") {
			t.Fatalf("CaptureScreenshot error lacks frame context: %v", err)
		}
	})

	t.Run("decoder error", func(t *testing.T) {
		wantErr := errors.New("synthetic decoder failure")
		client, video := newColdPathScreenshotClient(coldPathDecoder{err: wantErr})

		_, err := captureColdPathFrame(t, context.Background(), client, video)
		if !errors.Is(err, wantErr) {
			t.Fatalf("CaptureScreenshot error = %v, want %v", err, wantErr)
		}
		if got := client.VideoDiagnostics().DecodeFailure; got == "" {
			t.Fatal("decoder failure was not recorded in video diagnostics")
		}
	})

	t.Run("context canceled by decoder", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		client, video := newColdPathScreenshotClient(coldPathDecoder{
			image:  image.NewRGBA(image.Rect(0, 0, 1, 1)),
			cancel: cancel,
		})

		_, err := captureColdPathFrame(t, ctx, client, video)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CaptureScreenshot error = %v, want context.Canceled", err)
		}
		if !strings.Contains(err.Error(), "canceled after decode") {
			t.Fatalf("CaptureScreenshot error lacks decode-stage context: %v", err)
		}
	})
}

func TestSaveScreenshotColdFailures(t *testing.T) {
	t.Run("capture failure", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client := &Client{decoder: coldPathDecoder{}, cmdMu: make(chan struct{}, 1)}

		_, err := client.SaveScreenshot(ctx, filepath.Join(t.TempDir(), "shot.png"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SaveScreenshot error = %v, want context.Canceled", err)
		}
	})

	t.Run("cancellation before atomic commit", func(t *testing.T) {
		root := t.TempDir()
		outputPath := filepath.Join(root, "shot.png")
		ctx := newCancelBeforeScreenshotCommitContext()
		shot := Screenshot{PNG: []byte("complete encoded image")}

		_, err := writeScreenshot(ctx, outputPath, shot)
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "before commit") {
			t.Fatalf("writeScreenshot cancellation error = %v, want pre-commit cancellation", err)
		}
		if _, statErr := os.Stat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("canceled screenshot commit created output: %v", statErr)
		}
		entries, readErr := os.ReadDir(root)
		if readErr != nil {
			t.Fatalf("reading output directory: %v", readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("canceled screenshot commit left files: %v", entries)
		}
	})

	t.Run("output directory blocked by file", func(t *testing.T) {
		root := t.TempDir()
		blocker := filepath.Join(root, "not-a-directory")
		if err := os.WriteFile(blocker, []byte("block"), 0o600); err != nil {
			t.Fatalf("creating directory blocker: %v", err)
		}
		client, video := newColdPathScreenshotClient(coldPathDecoder{
			image: image.NewRGBA(image.Rect(0, 0, 1, 1)),
		})

		_, err := saveColdPathFrame(t, client, video, filepath.Join(blocker, "nested", "shot.png"))
		if err == nil || !strings.Contains(err.Error(), "creating output directory") {
			t.Fatalf("SaveScreenshot directory error = %v", err)
		}
	})

	t.Run("rename onto directory", func(t *testing.T) {
		root := t.TempDir()
		outputDir := filepath.Join(root, "existing-directory")
		if err := os.Mkdir(outputDir, 0o755); err != nil {
			t.Fatalf("creating output directory: %v", err)
		}
		client, video := newColdPathScreenshotClient(coldPathDecoder{
			image: image.NewRGBA(image.Rect(0, 0, 1, 1)),
		})

		_, err := saveColdPathFrame(t, client, video, outputDir)
		if err == nil || !strings.Contains(err.Error(), "saving screenshot") {
			t.Fatalf("SaveScreenshot rename error = %v", err)
		}
		entries, readErr := os.ReadDir(root)
		if readErr != nil {
			t.Fatalf("reading output parent: %v", readErr)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".jetkvm-screenshot-") {
				t.Fatalf("failed SaveScreenshot left temporary file %q", entry.Name())
			}
		}
	})
}

func TestVideoDiagnosticsWithoutSessionIsUndetermined(t *testing.T) {
	got := (&Client{}).VideoDiagnostics()
	if got.FailureBoundary != BoundaryUndetermined {
		t.Fatalf("VideoDiagnostics failure boundary = %q, want %q", got.FailureBoundary, BoundaryUndetermined)
	}
}
