package mcpserver

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestRunCanceledContextPreservesStdoutDiscipline(t *testing.T) {
	tests := []struct {
		name        string
		httpTimeout time.Duration
	}{
		{name: "default timeout", httpTimeout: 0},
		{name: "configured timeout", httpTimeout: 250 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdin, stdinWriter, err := os.Pipe()
			if err != nil {
				t.Fatalf("os.Pipe stdin: %v", err)
			}
			stdoutReader, stdout, err := os.Pipe()
			if err != nil {
				stdin.Close()
				stdinWriter.Close()
				t.Fatalf("os.Pipe stdout: %v", err)
			}

			originalStdin, originalStdout := os.Stdin, os.Stdout
			os.Stdin, os.Stdout = stdin, stdout
			t.Cleanup(func() {
				os.Stdin, os.Stdout = originalStdin, originalStdout
				stdin.Close()
				stdinWriter.Close()
				stdout.Close()
				stdoutReader.Close()
			})

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err = Run(ctx, Options{HTTPTimeout: tt.httpTimeout})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run error = %v, want context.Canceled", err)
			}

			if err := stdout.Close(); err != nil {
				t.Fatalf("closing captured stdout: %v", err)
			}
			got, err := io.ReadAll(stdoutReader)
			if err != nil {
				t.Fatalf("reading captured stdout: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("Run wrote %q to stdout without a protocol request", got)
			}
		})
	}
}
