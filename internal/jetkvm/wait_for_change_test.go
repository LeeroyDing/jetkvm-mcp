package jetkvm

import (
	"context"
	"image"
	"image/color"
	"math"
	"strings"
	"testing"
	"time"
)

func TestWaitForChangeComparesEveryFrameWithFixedBaseline(t *testing.T) {
	baseline := solidTestImage(image.Rect(0, 0, 2, 2), color.Black).(*image.RGBA)
	onePixel := solidTestImage(image.Rect(0, 0, 2, 2), color.Black).(*image.RGBA)
	onePixel.Set(0, 0, color.White)
	twoPixels := solidTestImage(image.Rect(0, 0, 2, 2), color.Black).(*image.RGBA)
	twoPixels.Set(0, 0, color.White)
	twoPixels.Set(1, 0, color.White)

	decoder := &scriptedImageDecoder{images: []image.Image{baseline, onePixel, twoPixels}}
	client := connectWaitStableTestClient(t, decoder)
	threshold := 0.25
	pollInterval := time.Duration(0)
	ctx := contextWithTimeout(t, 3*time.Second)

	result, err := client.WaitForChange(ctx, WaitForChangeOptions{
		Threshold:    &threshold,
		PollInterval: &pollInterval,
	})
	if err != nil {
		t.Fatalf("WaitForChange failed: %v", err)
	}
	if !result.Changed || result.FramesSampled != 3 {
		t.Fatalf("result = %+v, want change after baseline and two polls", result)
	}
	if result.FinalChangeFraction != 0.5 {
		t.Errorf("FinalChangeFraction = %v, want 0.5", result.FinalChangeFraction)
	}
	if result.Elapsed <= 0 {
		t.Errorf("Elapsed = %v, want a positive duration", result.Elapsed)
	}
	checks, decodes := decoder.calls()
	if checks != 1 || decodes != result.FramesSampled {
		t.Errorf("decoder checks/decodes = %d/%d, want 1/%d", checks, decodes, result.FramesSampled)
	}
}

func TestWaitForChangeResolutionMismatchCountsAtMaximumThreshold(t *testing.T) {
	decoder := &scriptedImageDecoder{images: []image.Image{
		solidTestImage(image.Rect(0, 0, 4, 4), color.Black),
		solidTestImage(image.Rect(0, 0, 2, 2), color.Black),
	}}
	client := connectWaitStableTestClient(t, decoder)
	threshold := 1.0
	pollInterval := time.Duration(0)
	ctx := contextWithTimeout(t, 3*time.Second)

	result, err := client.WaitForChange(ctx, WaitForChangeOptions{
		Threshold:    &threshold,
		PollInterval: &pollInterval,
	})
	if err != nil {
		t.Fatalf("WaitForChange failed: %v", err)
	}
	if !result.Changed || result.FramesSampled != 2 || result.FinalChangeFraction != 1 {
		t.Fatalf("result = %+v, want resolution change after two frames", result)
	}
}

func TestWaitForChangeTimesOutWithPartialObservations(t *testing.T) {
	decoder := &scriptedImageDecoder{
		images: []image.Image{solidTestImage(image.Rect(0, 0, 4, 4), color.Black)},
		repeat: true,
	}
	client := connectWaitStableTestClient(t, decoder)
	threshold := 0.0
	pollInterval := time.Duration(0)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	result, err := client.WaitForChange(ctx, WaitForChangeOptions{
		Threshold:    &threshold,
		PollInterval: &pollInterval,
	})
	if err == nil {
		t.Fatal("WaitForChange succeeded while every frame matched the baseline")
	}
	if kind := ErrorKindOf(err); kind != ErrorKindTimeout {
		t.Fatalf("error kind = %q, want %q: %v", kind, ErrorKindTimeout, err)
	}
	if result.Changed || result.FramesSampled < 1 || result.FinalChangeFraction != 0 || result.Elapsed <= 0 {
		t.Fatalf("partial result = %+v", result)
	}
	for _, want := range []string{"changed=false", "framesSampled=", "finalChangeFraction=", "elapsed="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout error is missing %q: %v", want, err)
		}
	}
}

func TestWaitForChangeExplicitCancellationIsTimeoutClassified(t *testing.T) {
	decoder := &scriptedImageDecoder{
		images: []image.Image{solidTestImage(image.Rect(0, 0, 4, 4), color.Black)},
		repeat: true,
	}
	client := connectWaitStableTestClient(t, decoder)
	pollInterval := time.Hour
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, decodes := decoder.calls()
			if decodes >= 1 {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()

	result, err := client.WaitForChange(ctx, WaitForChangeOptions{PollInterval: &pollInterval})
	<-done
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("WaitForChange error = %v, want cancellation detail", err)
	}
	if kind := ErrorKindOf(err); kind != ErrorKindTimeout {
		t.Fatalf("error kind = %q, want %q: %v", kind, ErrorKindTimeout, err)
	}
	if result.Changed {
		t.Fatalf("canceled result = %+v, want changed=false", result)
	}
}

func TestWaitForChangeRejectsInvalidOptionsBeforeDecoderWork(t *testing.T) {
	nan := math.NaN()
	positiveInfinity := math.Inf(1)
	negativeThreshold := -0.01
	tooLargeThreshold := 1.01
	negativePollInterval := -time.Nanosecond

	tests := []struct {
		name      string
		opts      WaitForChangeOptions
		wantField string
	}{
		{name: "negative threshold", opts: WaitForChangeOptions{Threshold: &negativeThreshold}, wantField: "threshold"},
		{name: "threshold above one", opts: WaitForChangeOptions{Threshold: &tooLargeThreshold}, wantField: "threshold"},
		{name: "NaN threshold", opts: WaitForChangeOptions{Threshold: &nan}, wantField: "threshold"},
		{name: "infinite threshold", opts: WaitForChangeOptions{Threshold: &positiveInfinity}, wantField: "threshold"},
		{name: "negative poll interval", opts: WaitForChangeOptions{PollInterval: &negativePollInterval}, wantField: "poll interval"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoder := &scriptedImageDecoder{images: []image.Image{
				solidTestImage(image.Rect(0, 0, 1, 1), color.Black),
			}}
			client := &Client{decoder: decoder}
			result, err := client.WaitForChange(context.Background(), test.opts)
			if err == nil || !strings.Contains(err.Error(), test.wantField) {
				t.Fatalf("WaitForChange error = %v, want %q validation error", err, test.wantField)
			}
			if result != (WaitForChangeResult{}) {
				t.Errorf("result = %+v, want zero result for validation failure", result)
			}
			checks, decodes := decoder.calls()
			if checks != 0 || decodes != 0 {
				t.Errorf("validation failure performed decoder work: checks=%d decodes=%d", checks, decodes)
			}
		})
	}
}

func TestWaitForChangeOptionDefaults(t *testing.T) {
	resolved, err := resolveWaitForChangeOptions(WaitForChangeOptions{})
	if err != nil {
		t.Fatalf("default options failed validation: %v", err)
	}
	if resolved.threshold != DefaultWaitForChangeThreshold ||
		resolved.pollInterval != DefaultWaitForChangePollInterval {
		t.Fatalf("resolved defaults = %+v, want threshold=%v pollInterval=%v",
			resolved, DefaultWaitForChangeThreshold, DefaultWaitForChangePollInterval)
	}
}
