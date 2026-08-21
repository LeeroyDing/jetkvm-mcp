package jetkvm

import (
	"context"
	"errors"
	"image"
	"image/color"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type captureTestDecoder struct {
	img image.Image
}

func (d captureTestDecoder) DecodeFrame(context.Context, []byte) (image.Image, error) {
	return d.img, nil
}

// observedImage stays lazy so tests can expose hostile dimensions without
// allocating the corresponding pixel backing store. An At call means the PNG
// encoder was entered.
type observedImage struct {
	bounds           image.Rectangle
	once             sync.Once
	onFirst          func()
	colorModelCalled bool
	atCalled         bool
}

func (i *observedImage) ColorModel() color.Model {
	i.colorModelCalled = true
	return color.NRGBAModel
}
func (i *observedImage) Bounds() image.Rectangle { return i.bounds }
func (i *observedImage) At(_, _ int) color.Color {
	i.atCalled = true
	i.once.Do(func() {
		if i.onFirst != nil {
			i.onFirst()
		}
	})
	return color.NRGBA{R: 0x4a, G: 0x78, B: 0xa8, A: 0xff}
}

func connectCaptureTestClient(t *testing.T, decoder Decoder) *Client {
	t.Helper()

	fd := startFakeDevice(t, fakeDeviceOptions{VideoInterval: 25 * time.Millisecond})
	ctx := contextWithTimeout(t, connectTimeout(t, 15*time.Second))
	client, err := Connect(ctx, Options{BaseURL: fd.baseURL(), Decoder: decoder})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client
}

func TestCaptureScreenshotRejectsOversizedDecodedDimensionsBeforeEncode(t *testing.T) {
	oversizedWidth := maxScreenshotDimension + 1
	img := &observedImage{bounds: image.Rect(0, 0, oversizedWidth, 1)}
	client := connectCaptureTestClient(t, captureTestDecoder{img: img})
	ctx := contextWithTimeout(t, 3*time.Second)

	shot, err := client.CaptureScreenshot(ctx)
	if err == nil {
		t.Fatalf("CaptureScreenshot returned a %dx%d image, want oversized dimensions rejected", shot.Width, shot.Height)
	}
	if img.colorModelCalled || img.atCalled {
		t.Fatal("CaptureScreenshot entered PNG encoding before rejecting oversized decoded dimensions")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(oversizedWidth)) {
		t.Fatalf("oversized-dimension error does not identify the rejected size: %v", err)
	}
}

func TestCaptureScreenshotChecksContextAfterPNGEncode(t *testing.T) {
	shotCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	img := &observedImage{
		bounds:  image.Rect(0, 0, 2, 2),
		onFirst: cancel,
	}
	client := connectCaptureTestClient(t, captureTestDecoder{img: img})

	shot, err := client.CaptureScreenshot(shotCtx)
	if !img.atCalled {
		t.Fatal("test image was not read during PNG encoding")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CaptureScreenshot error = %v, want context.Canceled after synchronous PNG encoding", err)
	}
	if len(shot.PNG) != 0 {
		t.Fatalf("canceled CaptureScreenshot returned %d PNG bytes", len(shot.PNG))
	}
}
