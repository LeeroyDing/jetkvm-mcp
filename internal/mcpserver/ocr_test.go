package mcpserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

type blockingOCREngine struct {
	started chan struct{}
	release chan struct{}

	mu        sync.Mutex
	active    int
	maxActive int
	readCalls int
}

func (*blockingOCREngine) CheckAvailable(context.Context) error { return nil }

func (e *blockingOCREngine) ReadText(ctx context.Context, _ []byte) (string, error) {
	e.mu.Lock()
	e.active++
	e.readCalls++
	if e.active > e.maxActive {
		e.maxActive = e.active
	}
	e.mu.Unlock()

	select {
	case e.started <- struct{}{}:
	default:
	}

	defer func() {
		e.mu.Lock()
		e.active--
		e.mu.Unlock()
	}()
	select {
	case <-e.release:
		return "recognized", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (e *blockingOCREngine) counts() (reads, maxActive int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.readCalls, e.maxActive
}

func TestSerialOCREngineBoundsConcurrencyAndCancelsWaiters(t *testing.T) {
	delegate := &blockingOCREngine{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	engine := newSerialOCREngine(delegate)

	firstDone := make(chan error, 1)
	go func() {
		_, err := engine.ReadText(context.Background(), []byte("first"))
		firstDone <- err
	}()
	select {
	case <-delegate.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first OCR call did not start")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := engine.ReadText(waitCtx, []byte("second")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting OCR call error = %v, want context deadline exceeded", err)
	}
	if reads, maxActive := delegate.counts(); reads != 1 || maxActive != 1 {
		t.Fatalf("delegate reads/max-active = %d/%d, want 1/1", reads, maxActive)
	}

	close(delegate.release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first OCR call failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first OCR call did not finish")
	}
}

func TestServerSharesOCRAdmissionAcrossTextTools(t *testing.T) {
	shot, _ := makeScreenshotFixture(t, 4, 4)
	delegate := &blockingOCREngine{
		started: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(delegate.release) }) }
	defer release()

	dev := &mockDevice{screenshotFunc: func(ctx context.Context) (jetkvm.Screenshot, error) {
		if err := ctx.Err(); err != nil {
			return jetkvm.Screenshot{}, err
		}
		return shot, nil
	}}
	cs := newReadTextToolTestSessionWithTimeout(t, dev, delegate, true, 2*time.Second)

	type callResult struct {
		result *mcp.CallToolResult
		err    error
	}
	firstDone := make(chan callResult, 1)
	go func() {
		result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "jetkvm_read_text",
			Arguments: map[string]any{},
		})
		firstDone <- callResult{result: result, err: err}
	}()
	select {
	case <-delegate.started:
	case <-time.After(2 * time.Second):
		t.Fatal("read-text OCR call did not start")
	}

	waitResult, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "jetkvm_wait_for_text",
		Arguments: map[string]any{
			"text":        "READY",
			"interval_ms": 100,
			"timeout_ms":  100,
		},
	})
	if err != nil {
		t.Fatalf("wait-for-text call failed: %v", err)
	}
	if waitResult.IsError {
		t.Fatalf("wait-for-text returned tool error: %+v", waitResult.Content)
	}
	metadata := decodeWaitForTextMetadata(t, waitResult)
	if !metadata.TimedOut || metadata.FrameCount != 1 {
		t.Fatalf("wait-for-text metadata = %+v, want one captured frame and a structured timeout", metadata)
	}
	if reads, maxActive := delegate.counts(); reads != 1 || maxActive != 1 {
		t.Fatalf("shared delegate reads/max-active = %d/%d, want 1/1", reads, maxActive)
	}

	release()
	select {
	case first := <-firstDone:
		if first.err != nil {
			t.Fatalf("read-text call failed: %v", first.err)
		}
		if first.result.IsError {
			t.Fatalf("read-text returned tool error: %+v", first.result.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read-text call did not finish")
	}
}
