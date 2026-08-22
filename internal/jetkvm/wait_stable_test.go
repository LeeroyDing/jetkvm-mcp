package jetkvm

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

type scriptedImageDecoder struct {
	mu sync.Mutex

	images      []image.Image
	repeat      bool
	checkErr    error
	checkCalls  int
	decodeCalls int
}

type delayedImage struct {
	image.Image
	delay time.Duration
}

func (i delayedImage) At(x, y int) color.Color {
	time.Sleep(i.delay)
	return i.Image.At(x, y)
}

func (d *scriptedImageDecoder) CheckAvailable(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.checkCalls++
	return d.checkErr
}

func (d *scriptedImageDecoder) DecodeFrame(ctx context.Context, _ []byte) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.images) == 0 {
		return nil, errors.New("scripted decoder has no images")
	}
	index := d.decodeCalls
	d.decodeCalls++
	if d.repeat {
		index %= len(d.images)
	} else if index >= len(d.images) {
		index = len(d.images) - 1
	}
	return d.images[index], nil
}

func (d *scriptedImageDecoder) calls() (checks, decodes int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.checkCalls, d.decodeCalls
}

func solidTestImage(bounds image.Rectangle, fill color.Color) image.Image {
	img := image.NewRGBA(bounds)
	draw.Draw(img, bounds, image.NewUniform(fill), image.Point{}, draw.Src)
	return img
}

func connectWaitStableTestClient(t *testing.T, decoder Decoder) *Client {
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

func TestWaitStableSettlesAfterChangesStop(t *testing.T) {
	decoder := &scriptedImageDecoder{images: []image.Image{
		solidTestImage(image.Rect(0, 0, 32, 32), color.RGBA{R: 0xff, A: 0xff}),
		solidTestImage(image.Rect(0, 0, 32, 32), color.RGBA{B: 0xff, A: 0xff}),
		solidTestImage(image.Rect(0, 0, 32, 32), color.RGBA{B: 0xff, A: 0xff}),
		solidTestImage(image.Rect(0, 0, 32, 32), color.RGBA{B: 0xff, A: 0xff}),
	}}
	client := connectWaitStableTestClient(t, decoder)

	threshold := 0.0 // An explicit zero must not be replaced by the default.
	pollInterval := time.Duration(0)
	ctx := contextWithTimeout(t, 3*time.Second)
	result, err := client.WaitStable(ctx, WaitStableOptions{
		Threshold:    &threshold,
		PollInterval: &pollInterval,
		// StableFrames is omitted deliberately: its default is two stable
		// comparisons, requiring at least three frames and four for this script.
	})
	if err != nil {
		t.Fatalf("WaitStable failed: %v", err)
	}
	if !result.Settled {
		t.Fatal("WaitStable returned success without reporting settled=true")
	}
	if result.FramesSampled != 4 {
		t.Fatalf("FramesSampled = %d, want 4", result.FramesSampled)
	}
	if result.FinalChangeFraction != 0 {
		t.Errorf("FinalChangeFraction = %v, want 0", result.FinalChangeFraction)
	}
	if result.Elapsed <= 0 {
		t.Errorf("Elapsed = %v, want a positive duration", result.Elapsed)
	}
	checks, decodes := decoder.calls()
	if checks != 1 || decodes != result.FramesSampled {
		t.Errorf("decoder checks/decodes = %d/%d, want 1/%d", checks, decodes, result.FramesSampled)
	}
}

func TestWaitStableTimesOutWhileFramesKeepChanging(t *testing.T) {
	decoder := &scriptedImageDecoder{
		images: []image.Image{
			solidTestImage(image.Rect(0, 0, 32, 32), color.Black),
			solidTestImage(image.Rect(0, 0, 32, 32), color.White),
		},
		repeat: true,
	}
	client := connectWaitStableTestClient(t, decoder)

	threshold := 0.0
	pollInterval := time.Duration(0)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	result, err := client.WaitStable(ctx, WaitStableOptions{
		Threshold:    &threshold,
		PollInterval: &pollInterval,
	})
	if err == nil {
		t.Fatal("WaitStable settled while every consecutive frame changed")
	}
	if kind := ErrorKindOf(err); kind != ErrorKindTimeout {
		t.Fatalf("error kind = %q, want %q: %v", kind, ErrorKindTimeout, err)
	}
	if result.Settled {
		t.Fatal("timed-out result reported settled=true")
	}
	if result.FramesSampled < 2 {
		t.Fatalf("FramesSampled = %d, want at least 2 before timeout", result.FramesSampled)
	}
	if result.FinalChangeFraction != 1 {
		t.Errorf("FinalChangeFraction = %v, want 1 for fully changed frames", result.FinalChangeFraction)
	}
	if result.Elapsed <= 0 {
		t.Errorf("Elapsed = %v, want a positive partial-result duration", result.Elapsed)
	}
	for _, want := range []string{"settled=false", "framesSampled=", "finalChangeFraction=", "elapsed="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout error is missing %q: %v", want, err)
		}
	}
}

func TestWaitStableRejectsInvalidOptionsBeforeDecoderWork(t *testing.T) {
	nan := math.NaN()
	positiveInfinity := math.Inf(1)
	negativeThreshold := -0.01
	tooLargeThreshold := 1.01
	zeroStableFrames := 0
	negativeStableFrames := -1
	tooManyStableFrames := MaxWaitStableFrames + 1
	negativePollInterval := -time.Nanosecond

	tests := []struct {
		name      string
		opts      WaitStableOptions
		wantField string
	}{
		{name: "negative threshold", opts: WaitStableOptions{Threshold: &negativeThreshold}, wantField: "Threshold"},
		{name: "threshold above one", opts: WaitStableOptions{Threshold: &tooLargeThreshold}, wantField: "Threshold"},
		{name: "NaN threshold", opts: WaitStableOptions{Threshold: &nan}, wantField: "Threshold"},
		{name: "infinite threshold", opts: WaitStableOptions{Threshold: &positiveInfinity}, wantField: "Threshold"},
		{name: "zero stable frames", opts: WaitStableOptions{StableFrames: &zeroStableFrames}, wantField: "StableFrames"},
		{name: "negative stable frames", opts: WaitStableOptions{StableFrames: &negativeStableFrames}, wantField: "StableFrames"},
		{name: "too many stable frames", opts: WaitStableOptions{StableFrames: &tooManyStableFrames}, wantField: "StableFrames"},
		{name: "negative poll interval", opts: WaitStableOptions{PollInterval: &negativePollInterval}, wantField: "PollInterval"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoder := &scriptedImageDecoder{images: []image.Image{
				solidTestImage(image.Rect(0, 0, 1, 1), color.Black),
			}}
			// sess and cmdMu intentionally remain nil. Reaching either would panic,
			// proving validation was not completed before work began.
			client := &Client{decoder: decoder}
			result, err := client.WaitStable(context.Background(), test.opts)
			if err == nil {
				t.Fatal("WaitStable accepted invalid options")
			}
			if !strings.Contains(err.Error(), test.wantField) {
				t.Errorf("error %q does not name %s", err, test.wantField)
			}
			if result != (WaitStableResult{}) {
				t.Errorf("result = %+v, want zero result for validation failure", result)
			}
			checks, decodes := decoder.calls()
			if checks != 0 || decodes != 0 {
				t.Errorf("validation failure performed decoder work: checks=%d decodes=%d", checks, decodes)
			}
		})
	}
}

func TestWaitStableResolutionMismatchIsNeverStable(t *testing.T) {
	decoder := &scriptedImageDecoder{images: []image.Image{
		solidTestImage(image.Rect(0, 0, 32, 32), color.Black),
		solidTestImage(image.Rect(0, 0, 16, 16), color.Black),
		solidTestImage(image.Rect(0, 0, 16, 16), color.Black),
	}}
	client := connectWaitStableTestClient(t, decoder)

	threshold := 1.0
	stableFrames := 1
	pollInterval := time.Duration(0)
	ctx := contextWithTimeout(t, 3*time.Second)
	result, err := client.WaitStable(ctx, WaitStableOptions{
		Threshold:    &threshold,
		StableFrames: &stableFrames,
		PollInterval: &pollInterval,
	})
	if err != nil {
		t.Fatalf("WaitStable failed: %v", err)
	}
	if !result.Settled {
		t.Fatal("WaitStable did not settle after two equal-resolution identical frames")
	}
	if result.FramesSampled != 3 {
		t.Fatalf("FramesSampled = %d, want 3; a resolution mismatch must not settle even at threshold 1", result.FramesSampled)
	}
	if result.FinalChangeFraction != 0 {
		t.Errorf("FinalChangeFraction = %v, want 0 for the final identical comparison", result.FinalChangeFraction)
	}
}

func TestWaitStableOptionDefaultsAndDecoderPreflight(t *testing.T) {
	resolved, err := resolveWaitStableOptions(WaitStableOptions{})
	if err != nil {
		t.Fatalf("default options failed validation: %v", err)
	}
	if resolved.threshold != DefaultWaitStableThreshold ||
		resolved.stableFrames != DefaultWaitStableFrames ||
		resolved.pollInterval != DefaultWaitStablePollInterval {
		t.Fatalf("resolved defaults = %+v, want threshold=%v stableFrames=%d pollInterval=%v",
			resolved, DefaultWaitStableThreshold, DefaultWaitStableFrames, DefaultWaitStablePollInterval)
	}

	wantErr := errors.New("decoder preflight unavailable")
	decoder := &scriptedImageDecoder{checkErr: wantErr}
	client := &Client{decoder: decoder}
	result, err := client.WaitStable(context.Background(), WaitStableOptions{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WaitStable error = %v, want decoder preflight error", err)
	}
	if result.Elapsed <= 0 {
		t.Errorf("preflight failure result Elapsed = %v, want positive", result.Elapsed)
	}
	checks, decodes := decoder.calls()
	if checks != 1 || decodes != 0 {
		t.Errorf("decoder checks/decodes = %d/%d, want 1/0", checks, decodes)
	}
}

func TestChangedPixelFractionObservesContextAtComparisonBoundary(t *testing.T) {
	t.Run("already canceled empty images", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		empty := image.NewRGBA(image.Rect(0, 0, 0, 0))
		if _, _, err := changedPixelFraction(ctx, empty, empty); !errors.Is(err, context.Canceled) {
			t.Fatalf("changedPixelFraction error = %v, want context.Canceled", err)
		}
	})

	t.Run("deadline during final short row", func(t *testing.T) {
		base := solidTestImage(image.Rect(0, 0, 1, 1), color.Black)
		slow := delayedImage{Image: base, delay: 10 * time.Millisecond}
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		if _, _, err := changedPixelFraction(ctx, slow, slow); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("changedPixelFraction error = %v, want context.DeadlineExceeded", err)
		}
	})
}
