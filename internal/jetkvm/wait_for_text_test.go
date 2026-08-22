package jetkvm

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

type waitForTextTestEngine struct {
	check      func(context.Context) error
	read       func(context.Context, []byte) (string, error)
	checkCalls int
	readCalls  int
}

func (e *waitForTextTestEngine) CheckAvailable(ctx context.Context) error {
	e.checkCalls++
	if e.check != nil {
		return e.check(ctx)
	}
	return nil
}

func (e *waitForTextTestEngine) ReadText(ctx context.Context, image []byte) (string, error) {
	e.readCalls++
	if e.read != nil {
		return e.read(ctx, image)
	}
	return "", nil
}

func TestWaitForTextLiteralMatch(t *testing.T) {
	interval := MinWaitForTextInterval
	timeout := time.Second
	events := make([]string, 0, 5)
	captures := 0
	capture := func(context.Context) (Screenshot, error) {
		events = append(events, "capture")
		captures++
		return Screenshot{PNG: []byte{byte(captures)}}, nil
	}
	recognized := []string{"firmware booting", "console: READY for login"}
	engine := &waitForTextTestEngine{
		check: func(context.Context) error {
			events = append(events, "check")
			return nil
		},
		read: func(context.Context, []byte) (string, error) {
			events = append(events, "read")
			return recognized[len(events)/2-1], nil
		},
	}

	result, err := WaitForText(context.Background(), WaitForTextOptions{
		Text:     "READY",
		Interval: &interval,
		Timeout:  &timeout,
	}, capture, engine)
	if err != nil {
		t.Fatalf("WaitForText failed: %v", err)
	}
	if !result.Matched || result.TimedOut {
		t.Fatalf("result = %+v, want a non-timeout match", result)
	}
	if result.Match != "READY" {
		t.Fatalf("Match = %q, want only recognized matching substring %q", result.Match, "READY")
	}
	if result.FrameCount != 2 || captures != 2 || engine.readCalls != 2 {
		t.Fatalf("counts: result=%d captures=%d OCR=%d, want all 2", result.FrameCount, captures, engine.readCalls)
	}
	if engine.checkCalls != 1 {
		t.Fatalf("OCR availability checks = %d, want 1", engine.checkCalls)
	}
	wantEvents := []string{"check", "capture", "read", "capture", "read"}
	if !slices.Equal(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if result.Elapsed < interval {
		t.Fatalf("Elapsed = %s, want at least one %s poll interval", result.Elapsed, interval)
	}
}

func TestWaitForTextRegexMatch(t *testing.T) {
	engine := &waitForTextTestEngine{
		read: func(context.Context, []byte) (string, error) {
			return "login prompt for user-4821 is ready", nil
		},
	}
	captures := 0
	result, err := WaitForText(context.Background(), WaitForTextOptions{
		Text:  `user-[0-9]+`,
		Regex: true,
	}, func(context.Context) (Screenshot, error) {
		captures++
		return Screenshot{PNG: []byte("png")}, nil
	}, engine)
	if err != nil {
		t.Fatalf("WaitForText failed: %v", err)
	}
	if !result.Matched || result.TimedOut || result.Match != "user-4821" {
		t.Fatalf("result = %+v, want recognized regex substring %q", result, "user-4821")
	}
	if result.FrameCount != 1 || captures != 1 || engine.readCalls != 1 {
		t.Fatalf("first-poll counts: result=%d captures=%d OCR=%d, want all 1",
			result.FrameCount, captures, engine.readCalls)
	}
}

func TestWaitForTextTimeoutIsStructured(t *testing.T) {
	interval := MinWaitForTextInterval
	timeout := 230 * time.Millisecond
	engine := &waitForTextTestEngine{
		read: func(context.Context, []byte) (string, error) {
			return "still booting", nil
		},
	}
	captures := 0
	result, err := WaitForText(context.Background(), WaitForTextOptions{
		Text:     "READY",
		Interval: &interval,
		Timeout:  &timeout,
	}, func(context.Context) (Screenshot, error) {
		captures++
		return Screenshot{PNG: []byte("png")}, nil
	}, engine)
	if err != nil {
		t.Fatalf("WaitForText timeout returned an error: %v", err)
	}
	if result.Matched || !result.TimedOut || result.Match != "" {
		t.Fatalf("result = %+v, want structured timeout without a match", result)
	}
	if result.FrameCount == 0 || result.FrameCount != captures || result.FrameCount != engine.readCalls {
		t.Fatalf("counts: result=%d captures=%d OCR=%d, want equal positive counts",
			result.FrameCount, captures, engine.readCalls)
	}
	if result.Elapsed < timeout || result.Elapsed > 2*time.Second {
		t.Fatalf("Elapsed = %s, want timeout near %s", result.Elapsed, timeout)
	}
}

func TestWaitForTextParentDeadlineIsStructured(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	interval := MinWaitForTextInterval
	timeout := time.Second
	engine := &waitForTextTestEngine{}
	result, err := WaitForText(ctx, WaitForTextOptions{
		Text:     "READY",
		Interval: &interval,
		Timeout:  &timeout,
	}, func(context.Context) (Screenshot, error) {
		return Screenshot{PNG: []byte("png")}, nil
	}, engine)
	if err != nil {
		t.Fatalf("parent deadline returned an error: %v", err)
	}
	if !result.TimedOut || result.Matched {
		t.Fatalf("result = %+v, want structured parent-context timeout", result)
	}
}

func TestWaitForTextValidationDoesNoWork(t *testing.T) {
	invalidUTF8 := string([]byte{0xff, 0xfe})
	tooManyRunes := strings.Repeat("界", MaxWaitForTextTextRunes+1)
	zero := time.Duration(0)
	negative := -time.Nanosecond
	belowInterval := MinWaitForTextInterval - time.Nanosecond
	aboveInterval := MaxWaitForTextInterval + time.Nanosecond
	belowTimeout := MinWaitForTextTimeout - time.Nanosecond
	aboveTimeout := MaxWaitForTextTimeout + time.Nanosecond
	shortTimeout := MinWaitForTextTimeout
	longerInterval := 2 * MinWaitForTextInterval
	minimumInterval := MinWaitForTextInterval
	invalidPattern := `(?P<REGEX-SECRET-CANARY>`

	tests := []struct {
		name string
		opts WaitForTextOptions
	}{
		{name: "empty text", opts: WaitForTextOptions{}},
		{name: "invalid UTF-8", opts: WaitForTextOptions{Text: invalidUTF8}},
		{name: "too many runes", opts: WaitForTextOptions{Text: tooManyRunes}},
		{name: "invalid regex", opts: WaitForTextOptions{Text: invalidPattern, Regex: true}},
		{name: "zero interval", opts: WaitForTextOptions{Text: "x", Interval: &zero}},
		{name: "negative interval", opts: WaitForTextOptions{Text: "x", Interval: &negative}},
		{name: "interval below minimum", opts: WaitForTextOptions{Text: "x", Interval: &belowInterval}},
		{name: "interval above maximum", opts: WaitForTextOptions{Text: "x", Interval: &aboveInterval}},
		{name: "zero timeout", opts: WaitForTextOptions{Text: "x", Interval: &minimumInterval, Timeout: &zero}},
		{name: "negative timeout", opts: WaitForTextOptions{Text: "x", Interval: &minimumInterval, Timeout: &negative}},
		{name: "timeout below minimum", opts: WaitForTextOptions{Text: "x", Interval: &minimumInterval, Timeout: &belowTimeout}},
		{name: "timeout above maximum", opts: WaitForTextOptions{Text: "x", Interval: &minimumInterval, Timeout: &aboveTimeout}},
		{name: "interval exceeds timeout", opts: WaitForTextOptions{Text: "x", Interval: &longerInterval, Timeout: &shortTimeout}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := &waitForTextTestEngine{}
			captures := 0
			result, err := WaitForText(context.Background(), test.opts, func(context.Context) (Screenshot, error) {
				captures++
				return Screenshot{}, nil
			}, engine)
			if err == nil {
				t.Fatalf("WaitForText accepted invalid options %+v with result %+v", test.opts, result)
			}
			if captures != 0 || engine.checkCalls != 0 || engine.readCalls != 0 {
				t.Fatalf("invalid options performed work: captures=%d checks=%d reads=%d",
					captures, engine.checkCalls, engine.readCalls)
			}
			validateErr := ValidateWaitForTextOptions(test.opts)
			if validateErr == nil {
				t.Fatal("ValidateWaitForTextOptions accepted options rejected by WaitForText")
			}
			if test.name == "invalid regex" &&
				(strings.Contains(err.Error(), invalidPattern) || strings.Contains(validateErr.Error(), invalidPattern)) {
				t.Fatalf("invalid regex was reflected in an error: wait=%q validate=%q", err, validateErr)
			}
		})
	}
}

func TestValidateWaitForTextOptionsAcceptsBoundariesAndDefaults(t *testing.T) {
	if err := ValidateWaitForTextOptions(WaitForTextOptions{Text: "READY"}); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	minimumInterval := MinWaitForTextInterval
	minimumTimeout := MinWaitForTextTimeout
	if err := ValidateWaitForTextOptions(WaitForTextOptions{
		Text:     strings.Repeat("界", MaxWaitForTextTextRunes),
		Regex:    false,
		Interval: &minimumInterval,
		Timeout:  &minimumTimeout,
	}); err != nil {
		t.Fatalf("inclusive minimums and maximum text length rejected: %v", err)
	}
	maximumInterval := MaxWaitForTextInterval
	maximumTimeout := MaxWaitForTextTimeout
	if err := ValidateWaitForTextOptions(WaitForTextOptions{
		Text:     `ready(?: now)?`,
		Regex:    true,
		Interval: &maximumInterval,
		Timeout:  &maximumTimeout,
	}); err != nil {
		t.Fatalf("inclusive maximum durations rejected: %v", err)
	}
}

func TestWaitForTextDependencyFailuresAndCancellation(t *testing.T) {
	t.Run("OCR preflight before capture", func(t *testing.T) {
		wantErr := errors.New("OCR unavailable")
		engine := &waitForTextTestEngine{check: func(context.Context) error { return wantErr }}
		captures := 0
		result, err := WaitForText(context.Background(), WaitForTextOptions{Text: "x"}, func(context.Context) (Screenshot, error) {
			captures++
			return Screenshot{}, nil
		}, engine)
		if !errors.Is(err, wantErr) || result.TimedOut || captures != 0 || engine.readCalls != 0 {
			t.Fatalf("result=%+v err=%v captures=%d reads=%d", result, err, captures, engine.readCalls)
		}
	})

	t.Run("capture failure", func(t *testing.T) {
		wantErr := errors.New("capture failed")
		engine := &waitForTextTestEngine{}
		result, err := WaitForText(context.Background(), WaitForTextOptions{Text: "x"}, func(context.Context) (Screenshot, error) {
			return Screenshot{}, wantErr
		}, engine)
		if !errors.Is(err, wantErr) || result.TimedOut || result.FrameCount != 0 || engine.readCalls != 0 {
			t.Fatalf("result=%+v err=%v reads=%d", result, err, engine.readCalls)
		}
	})

	t.Run("OCR failure", func(t *testing.T) {
		wantErr := errors.New("OCR failed")
		engine := &waitForTextTestEngine{read: func(context.Context, []byte) (string, error) {
			return "", wantErr
		}}
		result, err := WaitForText(context.Background(), WaitForTextOptions{Text: "x"}, func(context.Context) (Screenshot, error) {
			return Screenshot{PNG: []byte("png")}, nil
		}, engine)
		if !errors.Is(err, wantErr) || result.TimedOut || result.FrameCount != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	t.Run("independent OCR deadline is an error", func(t *testing.T) {
		engine := &waitForTextTestEngine{read: func(context.Context, []byte) (string, error) {
			return "", context.DeadlineExceeded
		}}
		result, err := WaitForText(context.Background(), WaitForTextOptions{Text: "x"}, func(context.Context) (Screenshot, error) {
			return Screenshot{PNG: []byte("png")}, nil
		}, engine)
		if !errors.Is(err, context.DeadlineExceeded) || result.TimedOut {
			t.Fatalf("result=%+v err=%v, want operational deadline error", result, err)
		}
	})

	t.Run("explicit cancellation is an error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		engine := &waitForTextTestEngine{read: func(readCtx context.Context, _ []byte) (string, error) {
			cancel()
			<-readCtx.Done()
			return "", readCtx.Err()
		}}
		result, err := WaitForText(ctx, WaitForTextOptions{Text: "x"}, func(context.Context) (Screenshot, error) {
			return Screenshot{PNG: []byte("png")}, nil
		}, engine)
		if !errors.Is(err, context.Canceled) || result.TimedOut {
			t.Fatalf("result=%+v err=%v, want cancellation error", result, err)
		}
	})
}

func TestWaitForTextRejectsCancellationAfterSuccessfulDependencyStep(t *testing.T) {
	t.Run("OCR availability check", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		engine := &waitForTextTestEngine{check: func(context.Context) error {
			cancel()
			return nil
		}}
		captures := 0

		result, err := WaitForText(ctx, WaitForTextOptions{Text: "READY"}, func(context.Context) (Screenshot, error) {
			captures++
			return Screenshot{PNG: []byte("png")}, nil
		}, engine)
		if !errors.Is(err, context.Canceled) || result.TimedOut || result.Matched {
			t.Fatalf("result=%+v err=%v, want cancellation without a match", result, err)
		}
		if result.FrameCount != 0 || captures != 0 || engine.checkCalls != 1 || engine.readCalls != 0 {
			t.Fatalf("work after canceled preflight: result frames=%d captures=%d checks=%d reads=%d",
				result.FrameCount, captures, engine.checkCalls, engine.readCalls)
		}
	})

	t.Run("screenshot capture", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		engine := &waitForTextTestEngine{}

		result, err := WaitForText(ctx, WaitForTextOptions{Text: "READY"}, func(context.Context) (Screenshot, error) {
			cancel()
			return Screenshot{PNG: []byte("png")}, nil
		}, engine)
		if !errors.Is(err, context.Canceled) || result.TimedOut || result.Matched {
			t.Fatalf("result=%+v err=%v, want cancellation without a match", result, err)
		}
		if result.FrameCount != 1 || engine.checkCalls != 1 || engine.readCalls != 0 {
			t.Fatalf("work after canceled capture: result frames=%d checks=%d reads=%d",
				result.FrameCount, engine.checkCalls, engine.readCalls)
		}
	})

	t.Run("OCR recognition", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		engine := &waitForTextTestEngine{read: func(context.Context, []byte) (string, error) {
			cancel()
			return "READY", nil
		}}

		result, err := WaitForText(ctx, WaitForTextOptions{Text: "READY"}, func(context.Context) (Screenshot, error) {
			return Screenshot{PNG: []byte("png")}, nil
		}, engine)
		if !errors.Is(err, context.Canceled) || result.TimedOut || result.Matched || result.Match != "" {
			t.Fatalf("result=%+v err=%v, want cancellation without the successful OCR match", result, err)
		}
		if result.FrameCount != 1 || engine.checkCalls != 1 || engine.readCalls != 1 {
			t.Fatalf("canceled OCR counts: result frames=%d checks=%d reads=%d",
				result.FrameCount, engine.checkCalls, engine.readCalls)
		}
	})
}

func TestWaitForTextRejectsNilDependencies(t *testing.T) {
	engine := &waitForTextTestEngine{}
	if _, err := WaitForText(context.Background(), WaitForTextOptions{Text: "x"}, nil, engine); err == nil {
		t.Fatal("WaitForText accepted a nil capture function")
	}
	if _, err := WaitForText(context.Background(), WaitForTextOptions{Text: "x"}, func(context.Context) (Screenshot, error) {
		return Screenshot{}, nil
	}, nil); err == nil {
		t.Fatal("WaitForText accepted a nil OCR engine")
	}
}
