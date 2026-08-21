package jetkvm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultWaitForTextInterval is the minimum gap between the starts of two
	// successive screenshot/OCR polls when Interval is omitted.
	DefaultWaitForTextInterval = 500 * time.Millisecond

	// DefaultWaitForTextTimeout bounds a complete wait when Timeout is omitted.
	DefaultWaitForTextTimeout = 10 * time.Second

	MinWaitForTextInterval = 100 * time.Millisecond
	MaxWaitForTextInterval = 10 * time.Second
	MinWaitForTextTimeout  = 100 * time.Millisecond
	MaxWaitForTextTimeout  = 5 * time.Minute

	// MaxWaitForTextTextRunes bounds both literal needles and RE2 patterns.
	MaxWaitForTextTextRunes = 4096
)

// WaitForTextOptions configures a WaitForText call. Nil duration fields use
// the defaults above. Text is matched byte-for-byte unless Regex is true, in
// which case it is compiled with Go's RE2-syntax regexp package.
type WaitForTextOptions struct {
	Text     string
	Regex    bool
	Interval *time.Duration
	Timeout  *time.Duration
}

// WaitForTextResult reports the observations made while polling. A timeout is
// a normal structured outcome: TimedOut is true and the returned error is nil.
type WaitForTextResult struct {
	Matched    bool
	Match      string
	TimedOut   bool
	Elapsed    time.Duration
	FrameCount int
}

// ScreenshotCaptureFunc captures one request-fresh screenshot for an OCR poll.
type ScreenshotCaptureFunc func(context.Context) (Screenshot, error)

type resolvedWaitForTextOptions struct {
	text     string
	regexp   *regexp.Regexp
	interval time.Duration
	timeout  time.Duration
}

// ValidateWaitForTextOptions resolves omitted fields and validates all caller
// input without performing OCR or capturing a screenshot.
func ValidateWaitForTextOptions(opts WaitForTextOptions) error {
	_, err := resolveWaitForTextOptions(opts)
	return err
}

func resolveWaitForTextOptions(opts WaitForTextOptions) (resolvedWaitForTextOptions, error) {
	resolved := resolvedWaitForTextOptions{
		text:     opts.Text,
		interval: DefaultWaitForTextInterval,
		timeout:  DefaultWaitForTextTimeout,
	}
	if opts.Interval != nil {
		resolved.interval = *opts.Interval
	}
	if opts.Timeout != nil {
		resolved.timeout = *opts.Timeout
	}

	if !utf8.ValidString(opts.Text) {
		return resolvedWaitForTextOptions{}, errors.New("invalid Text: must be valid UTF-8")
	}
	if opts.Text == "" {
		return resolvedWaitForTextOptions{}, errors.New("invalid Text: must not be empty")
	}
	if runes := utf8.RuneCountInString(opts.Text); runes > MaxWaitForTextTextRunes {
		return resolvedWaitForTextOptions{}, fmt.Errorf(
			"invalid Text: must not exceed %d Unicode code points", MaxWaitForTextTextRunes)
	}
	if opts.Regex {
		compiled, err := regexp.Compile(opts.Text)
		if err != nil {
			// regexp's parse error includes the caller's pattern. Do not reflect
			// arbitrary text across the MCP boundary.
			return resolvedWaitForTextOptions{}, errors.New("invalid Text: must use valid RE2 syntax")
		}
		resolved.regexp = compiled
	}

	if resolved.interval < MinWaitForTextInterval || resolved.interval > MaxWaitForTextInterval {
		return resolvedWaitForTextOptions{}, fmt.Errorf(
			"invalid Interval: must be between %s and %s, got %s",
			MinWaitForTextInterval, MaxWaitForTextInterval, resolved.interval)
	}
	if resolved.timeout < MinWaitForTextTimeout || resolved.timeout > MaxWaitForTextTimeout {
		return resolvedWaitForTextOptions{}, fmt.Errorf(
			"invalid Timeout: must be between %s and %s, got %s",
			MinWaitForTextTimeout, MaxWaitForTextTimeout, resolved.timeout)
	}
	if resolved.interval > resolved.timeout {
		return resolvedWaitForTextOptions{}, fmt.Errorf(
			"invalid Interval: %s must not exceed Timeout %s", resolved.interval, resolved.timeout)
	}
	return resolved, nil
}

func (o resolvedWaitForTextOptions) findMatch(recognized string) (string, bool) {
	if o.regexp != nil {
		location := o.regexp.FindStringIndex(recognized)
		if location == nil {
			return "", false
		}
		return recognized[location[0]:location[1]], true
	}
	index := strings.Index(recognized, o.text)
	if index < 0 {
		return "", false
	}
	return recognized[index : index+len(o.text)], true
}

// WaitForText captures and OCRs request-fresh screenshots until Text matches
// or Timeout expires. The first poll starts immediately; later polls start no
// sooner than Interval after the preceding poll began. OCR availability is
// checked before the first screenshot is captured.
//
// Context deadlines, including Timeout, return a structured TimedOut result
// with a nil error. Explicit cancellation and dependency failures are errors.
func WaitForText(
	ctx context.Context,
	opts WaitForTextOptions,
	capture ScreenshotCaptureFunc,
	engine OCREngine,
) (WaitForTextResult, error) {
	resolved, err := resolveWaitForTextOptions(opts)
	if err != nil {
		return WaitForTextResult{}, err
	}

	started := time.Now()
	result := WaitForTextResult{}
	finish := func() WaitForTextResult {
		result.Elapsed = time.Since(started)
		return result
	}
	if capture == nil {
		return finish(), errors.New("jetkvm: wait-for-text screenshot capture function is nil")
	}
	if engine == nil {
		return finish(), errors.New("jetkvm: wait-for-text OCR engine is nil")
	}

	waitCtx, cancel := context.WithTimeout(ctx, resolved.timeout)
	defer cancel()
	terminal := func(operationErr error) (WaitForTextResult, error) {
		waitErr := waitCtx.Err()
		parentErr := ctx.Err()
		if errors.Is(waitErr, context.DeadlineExceeded) ||
			errors.Is(parentErr, context.DeadlineExceeded) {
			result.TimedOut = true
			return finish(), nil
		}
		if errors.Is(parentErr, context.Canceled) || errors.Is(waitErr, context.Canceled) {
			return finish(), fmt.Errorf("jetkvm: wait for text canceled: %w", context.Canceled)
		}
		return finish(), operationErr
	}

	if err := engine.CheckAvailable(waitCtx); err != nil {
		return terminal(err)
	}
	if err := waitCtx.Err(); err != nil {
		return terminal(err)
	}

	var previousPollStarted time.Time
	for {
		if err := waitForTextPoll(waitCtx, previousPollStarted, resolved.interval); err != nil {
			return terminal(err)
		}
		previousPollStarted = time.Now()
		if err := waitCtx.Err(); err != nil {
			return terminal(err)
		}

		shot, err := capture(waitCtx)
		if err != nil {
			return terminal(err)
		}
		result.FrameCount++
		if err := waitCtx.Err(); err != nil {
			return terminal(err)
		}

		recognized, err := engine.ReadText(waitCtx, shot.PNG)
		if err != nil {
			return terminal(err)
		}
		if err := waitCtx.Err(); err != nil {
			return terminal(err)
		}
		match, matched := resolved.findMatch(recognized)
		// Matching is synchronous but can still consume meaningful time for a
		// maximum-size RE2 pattern and OCR output. Keep the timeout authoritative
		// if the deadline or cancellation lands during that scan.
		if err := waitCtx.Err(); err != nil {
			return terminal(err)
		}
		if matched {
			result.Matched = true
			result.Match = match
			return finish(), nil
		}
	}
}

func waitForTextPoll(ctx context.Context, previousStart time.Time, interval time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if previousStart.IsZero() {
		return nil
	}
	remaining := interval - time.Since(previousStart)
	if remaining <= 0 {
		return nil
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
