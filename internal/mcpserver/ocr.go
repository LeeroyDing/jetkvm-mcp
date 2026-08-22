package mcpserver

import (
	"context"
	"fmt"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

// serialOCREngine bounds one MCP server to one active OCR recognition. Each
// Tesseract invocation is independently time- and output-bounded, but MCP tool
// calls are concurrent; without this shared admission gate, parallel
// read-text and wait-for-text calls could start an unbounded number of heavy
// subprocesses. Waiting for the slot remains bounded by the tool context.
type serialOCREngine struct {
	engine jetkvm.OCREngine
	slot   chan struct{}
}

func newSerialOCREngine(engine jetkvm.OCREngine) jetkvm.OCREngine {
	if engine == nil {
		return nil
	}
	return &serialOCREngine{
		engine: engine,
		slot:   make(chan struct{}, 1),
	}
}

func (e *serialOCREngine) CheckAvailable(ctx context.Context) error {
	return e.engine.CheckAvailable(ctx)
}

func (e *serialOCREngine) ReadText(ctx context.Context, imageData []byte) (string, error) {
	release, err := e.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return e.engine.ReadText(ctx, imageData)
}

func (e *serialOCREngine) acquire(ctx context.Context) (func(), error) {
	select {
	case e.slot <- struct{}{}:
		// Cancellation and an available slot can become ready together. Return
		// the slot rather than starting OCR for an abandoned tool call.
		if err := ctx.Err(); err != nil {
			<-e.slot
			return nil, fmt.Errorf("waiting for the MCP OCR worker: %w", err)
		}
		return func() { <-e.slot }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for the MCP OCR worker: %w", ctx.Err())
	}
}

var _ jetkvm.OCREngine = (*serialOCREngine)(nil)
