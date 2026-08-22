package jetkvm

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"
)

func TestTesseractOCREngineDefaultsAndGenericProcessFailure(t *testing.T) {
	var nilEngine *TesseractOCREngine
	if got := nilEngine.binary(); got != "tesseract" {
		t.Fatalf("nil engine binary = %q, want tesseract", got)
	}
	if got := nilEngine.timeout(); got != defaultOCRTimeout {
		t.Fatalf("nil engine timeout = %v, want %v", got, defaultOCRTimeout)
	}

	engine := &TesseractOCREngine{Timeout: -time.Second}
	if got := engine.binary(); got != "tesseract" {
		t.Fatalf("empty binary override = %q, want tesseract", got)
	}
	if got := engine.timeout(); got != defaultOCRTimeout {
		t.Fatalf("negative timeout = %v, want %v", got, defaultOCRTimeout)
	}

	const pathCanary = "/private/OCR-PROCESS-PATH-CANARY/tesseract"
	err := ocrExitError(errors.New("starting " + pathCanary))
	if got, want := err.Error(), "jetkvm: tesseract OCR process failed"; got != want {
		t.Fatalf("generic OCR process error = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), pathCanary) {
		t.Fatalf("generic OCR process error leaked binary path: %v", err)
	}
}

func TestTesseractOCREngineReadTextColdFailures(t *testing.T) {
	t.Run("pre-canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := (&TesseractOCREngine{}).ReadText(ctx, []byte("image"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ReadText error = %v, want context.Canceled", err)
		}
	})

	t.Run("missing binary", func(t *testing.T) {
		engine := &TesseractOCREngine{BinaryPath: "/private/missing-tesseract"}
		_, err := engine.ReadText(context.Background(), []byte("image"))
		if !errors.Is(err, ErrOCRUnavailable) {
			t.Fatalf("ReadText error = %v, want ErrOCRUnavailable", err)
		}
	})
}

func TestDecodeFFmpegPNGColdPaths(t *testing.T) {
	encoded := encodeUtilityPNG(t)

	t.Run("malformed header", func(t *testing.T) {
		_, err := decodeFFmpegPNG(context.Background(), []byte("not a PNG"))
		if err == nil || !strings.Contains(err.Error(), "PNG header") {
			t.Fatalf("decodeFFmpegPNG error = %v, want PNG header failure", err)
		}
	})

	t.Run("canceled before pixel decode", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := decodeFFmpegPNG(ctx, encoded)
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "before PNG decode") {
			t.Fatalf("decodeFFmpegPNG error = %v, want pre-decode cancellation", err)
		}
	})

	t.Run("corrupt pixel data", func(t *testing.T) {
		corrupt := append([]byte(nil), encoded...)
		idat := bytes.Index(corrupt, []byte("IDAT"))
		if idat < 0 || idat+4 >= len(corrupt) {
			t.Fatal("encoded test PNG has no IDAT payload")
		}
		corrupt[idat+4] ^= 0xff
		_, err := decodeFFmpegPNG(context.Background(), corrupt)
		if err == nil || !strings.Contains(err.Error(), "PNG output") {
			t.Fatalf("decodeFFmpegPNG error = %v, want pixel decode failure", err)
		}
	})

	t.Run("canceled after pixel decode", func(t *testing.T) {
		ctx := newCancelOnErrCallContext(2)
		_, err := decodeFFmpegPNG(ctx, encoded)
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "after PNG decode") {
			t.Fatalf("decodeFFmpegPNG error = %v, want post-decode cancellation", err)
		}
	})

	t.Run("valid image", func(t *testing.T) {
		img, err := decodeFFmpegPNG(context.Background(), encoded)
		if err != nil {
			t.Fatalf("decodeFFmpegPNG: %v", err)
		}
		if got, want := img.Bounds(), image.Rect(0, 0, 2, 1); got != want {
			t.Fatalf("decoded bounds = %v, want %v", got, want)
		}
	})
}

func encodeUtilityPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 0xff, A: 0xff})
	img.Set(1, 0, color.RGBA{B: 0xff, A: 0xff})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return encoded.Bytes()
}

func TestSecretStringDistinguishesEmptyValue(t *testing.T) {
	if got := (Secret{}).String(); got != "<empty>" {
		t.Fatalf("empty Secret.String() = %q, want <empty>", got)
	}
	if got := (Secret{}).GoString(); got != "<empty>" {
		t.Fatalf("empty Secret.GoString() = %q, want <empty>", got)
	}
}

func TestRedactURLRejectsMalformedInput(t *testing.T) {
	if got := redactURL("http://%"); got != redactionPlaceholder {
		t.Fatalf("redactURL malformed input = %q, want %q", got, redactionPlaceholder)
	}
}

type utilityDecoderFunc func(context.Context, []byte) (image.Image, error)

func (f utilityDecoderFunc) DecodeFrame(ctx context.Context, annexB []byte) (image.Image, error) {
	return f(ctx, annexB)
}

type utilityCheckingDecoder struct {
	check func(context.Context) error
}

func (d utilityCheckingDecoder) CheckAvailable(ctx context.Context) error {
	return d.check(ctx)
}

func (utilityCheckingDecoder) DecodeFrame(context.Context, []byte) (image.Image, error) {
	panic("DecodeFrame called after failed preflight")
}

func TestWaitStableClassifiesPreflightAndLockCancellation(t *testing.T) {
	t.Run("decoder preflight", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		decoder := utilityCheckingDecoder{check: func(context.Context) error {
			cancel()
			return errors.New("preflight interrupted")
		}}
		result, err := (&Client{decoder: decoder}).WaitStable(ctx, WaitStableOptions{})
		assertWaitStableTimeout(t, result, err, "decoder preflight did not complete")
	})

	t.Run("command lock", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		decoder := utilityDecoderFunc(func(context.Context, []byte) (image.Image, error) {
			panic("DecodeFrame called after lock cancellation")
		})
		result, err := (&Client{decoder: decoder, cmdMu: make(chan struct{}, 1)}).WaitStable(ctx, WaitStableOptions{})
		assertWaitStableTimeout(t, result, err, "command lock was not acquired")
	})

	t.Run("poll interval", func(t *testing.T) {
		ctx := newCancelOnErrCallContext(3)
		decoder := utilityDecoderFunc(func(context.Context, []byte) (image.Image, error) {
			panic("DecodeFrame called after poll cancellation")
		})
		result, err := (&Client{decoder: decoder, cmdMu: make(chan struct{}, 1)}).WaitStable(ctx, WaitStableOptions{})
		assertWaitStableTimeout(t, result, err, "poll interval did not complete")
	})
}

func TestWaitStableReturnsTerminalVideoError(t *testing.T) {
	wantErr := errors.New("video stream ended")
	client, capture := newUtilityWaitStableClient(utilityDecoderFunc(func(context.Context, []byte) (image.Image, error) {
		panic("DecodeFrame called after terminal video error")
	}))
	capture.fail(wantErr)
	pollInterval := time.Duration(0)
	result, err := client.WaitStable(context.Background(), WaitStableOptions{PollInterval: &pollInterval})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WaitStable error = %v, want %v", err, wantErr)
	}
	if result.Settled || result.FramesSampled != 0 {
		t.Fatalf("WaitStable result = %+v, want no sampled frames", result)
	}
}

func TestWaitStableClassifiesFrameWaitCancellation(t *testing.T) {
	decoder := utilityDecoderFunc(func(context.Context, []byte) (image.Image, error) {
		panic("DecodeFrame called without a frame")
	})
	client, _ := newUtilityWaitStableClient(decoder)
	base, cancel := context.WithCancel(context.Background())
	ctx := &doneHookContext{
		Context: base,
		hook: func(call int) {
			if call == 2 {
				cancel()
			}
		},
	}
	pollInterval := time.Duration(0)
	result, err := client.WaitStable(ctx, WaitStableOptions{PollInterval: &pollInterval})
	assertWaitStableTimeout(t, result, err, "no video frame available")
}

func TestWaitStableDecoderColdFailures(t *testing.T) {
	t.Run("ordinary decoder error", func(t *testing.T) {
		wantErr := errors.New("decoder rejected frame")
		decoder := utilityDecoderFunc(func(context.Context, []byte) (image.Image, error) {
			return nil, wantErr
		})
		client, capture := newUtilityWaitStableClient(decoder)
		ctx := newFreshFrameContext(context.Background(), capture)
		result, err := waitStableWithoutPollDelay(client, ctx)
		if !errors.Is(err, wantErr) {
			t.Fatalf("WaitStable error = %v, want %v", err, wantErr)
		}
		if result.FramesSampled != 0 {
			t.Fatalf("FramesSampled = %d, want 0", result.FramesSampled)
		}
	})

	t.Run("decoder cancellation", func(t *testing.T) {
		base, cancel := context.WithCancel(context.Background())
		decoder := utilityDecoderFunc(func(context.Context, []byte) (image.Image, error) {
			cancel()
			return nil, errors.New("decoder interrupted")
		})
		client, capture := newUtilityWaitStableClient(decoder)
		ctx := newFreshFrameContext(base, capture)
		result, err := waitStableWithoutPollDelay(client, ctx)
		assertWaitStableTimeout(t, result, err, "video frame decode did not complete")
	})

	t.Run("nil image", func(t *testing.T) {
		decoder := utilityDecoderFunc(func(context.Context, []byte) (image.Image, error) {
			return nil, nil
		})
		client, capture := newUtilityWaitStableClient(decoder)
		ctx := newFreshFrameContext(context.Background(), capture)
		result, err := waitStableWithoutPollDelay(client, ctx)
		if err == nil || !strings.Contains(err.Error(), "decoder returned a nil image") {
			t.Fatalf("WaitStable error = %v, want nil-image failure", err)
		}
		if result.FramesSampled != 0 {
			t.Fatalf("FramesSampled = %d, want 0", result.FramesSampled)
		}
	})

	t.Run("canceled after decode", func(t *testing.T) {
		base, cancel := context.WithCancel(context.Background())
		decoder := utilityDecoderFunc(func(context.Context, []byte) (image.Image, error) {
			cancel()
			return image.NewRGBA(image.Rect(0, 0, 1, 1)), nil
		})
		client, capture := newUtilityWaitStableClient(decoder)
		ctx := newFreshFrameContext(base, capture)
		result, err := waitStableWithoutPollDelay(client, ctx)
		assertWaitStableTimeout(t, result, err, "call canceled after video frame decode")
	})
}

func TestWaitStableClassifiesCancellationDuringPixelComparison(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	decodeCalls := 0
	decoder := utilityDecoderFunc(func(context.Context, []byte) (image.Image, error) {
		decodeCalls++
		if decodeCalls == 1 {
			return image.NewGray(image.Rect(0, 0, 1, 1)), nil
		}
		return cancelingAtImage{cancel: cancel}, nil
	})
	client, capture := newUtilityWaitStableClient(decoder)
	ctx := newFreshFrameContext(base, capture)
	result, err := waitStableWithoutPollDelay(client, ctx)
	assertWaitStableTimeout(t, result, err, "pixel comparison did not complete")
	if result.FramesSampled != 2 {
		t.Fatalf("FramesSampled = %d, want 2", result.FramesSampled)
	}
}

type cancelingAtImage struct {
	cancel context.CancelFunc
}

func (cancelingAtImage) ColorModel() color.Model { return color.RGBAModel }
func (cancelingAtImage) Bounds() image.Rectangle { return image.Rect(0, 0, 1, 1) }
func (i cancelingAtImage) At(int, int) color.Color {
	i.cancel()
	return color.Black
}

func newUtilityWaitStableClient(decoder Decoder) (*Client, *frameCapture) {
	diag := newVideoDiagnostics()
	capture := newFrameCapture(diag)
	client := &Client{
		decoder: decoder,
		sess:    &session{video: capture, diag: diag},
		cmdMu:   make(chan struct{}, 1),
	}
	return client, capture
}

func waitStableWithoutPollDelay(client *Client, ctx context.Context) (WaitStableResult, error) {
	pollInterval := time.Duration(0)
	return client.WaitStable(ctx, WaitStableOptions{PollInterval: &pollInterval})
}

func assertWaitStableTimeout(t *testing.T, result WaitStableResult, err error, wantStage string) {
	t.Helper()
	if kind := ErrorKindOf(err); kind != ErrorKindTimeout {
		t.Fatalf("WaitStable error kind = %q, want %q: %v", kind, ErrorKindTimeout, err)
	}
	if !strings.Contains(err.Error(), wantStage) {
		t.Fatalf("WaitStable error = %v, want stage %q", err, wantStage)
	}
	if result.Settled {
		t.Fatalf("WaitStable result = %+v, want Settled=false", result)
	}
}

// doneHookContext uses the two existing select boundaries in Client.lock and
// frameCapture.waitForFrameAfter as deterministic synchronization points. The
// first Done call belongs to the command lock; later calls begin fresh-frame
// waits and can safely publish the next completed generation.
type doneHookContext struct {
	context.Context
	doneCalls int
	hook      func(int)
}

func (c *doneHookContext) Done() <-chan struct{} {
	c.doneCalls++
	if c.hook != nil {
		c.hook(c.doneCalls)
	}
	return c.Context.Done()
}

func newFreshFrameContext(base context.Context, capture *frameCapture) context.Context {
	return &doneHookContext{
		Context: base,
		hook: func(call int) {
			if call > 1 {
				capture.ingest(buildAnnexB(
					[]byte{0x67, 0x01},
					[]byte{0x68, 0x01},
					[]byte{0x65, 0x01},
				))
			}
		},
	}
}

type cancelOnErrCallContext struct {
	context.Context
	cancel   context.CancelFunc
	cancelAt int
	errCalls int
}

func newCancelOnErrCallContext(cancelAt int) context.Context {
	base, cancel := context.WithCancel(context.Background())
	return &cancelOnErrCallContext{
		Context:  base,
		cancel:   cancel,
		cancelAt: cancelAt,
	}
}

func (c *cancelOnErrCallContext) Err() error {
	c.errCalls++
	if c.errCalls == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}
