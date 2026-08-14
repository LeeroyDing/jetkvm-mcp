package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxMCPMessageBytes         = 1 << 20
	batchProhibitedFromVersion = "2025-06-18"
	legacyProtocol20241105     = "2024-11-05"
	legacyProtocol20250326     = "2025-03-26"
)

// newRedactingIOTransport owns the outer NDJSON boundaries. Its connection
// adapter intentionally hides the SDK ioConn's unexported sessionUpdated hook:
// go-sdk v1.4.1 writes and reads ioConn.protocolVersion concurrently without
// synchronization. versionAwareReadCloser enforces the same protocol-version
// batch rule before SDK parsing, without that race, and caps each request line.
func newRedactingIOTransport(reader io.ReadCloser, writer io.WriteCloser) mcp.Transport {
	return &safeIOTransport{inner: &mcp.IOTransport{
		Reader: newVersionAwareReadCloser(reader),
		Writer: &redactingWriteCloser{next: writer},
	}}
}

func newStdioTransport() mcp.Transport {
	// This is the package's only stdout reference. Every SDK byte passes
	// through redactingWriteCloser before reaching the protocol-owned stream.
	return newRedactingIOTransport(os.Stdin, nonClosingWriter{Writer: os.Stdout})
}

type safeIOTransport struct {
	inner *mcp.IOTransport
}

func (t *safeIOTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, errors.New("mcpserver: protocol transport unavailable")
	}
	// Embedding only the exported interface prevents the SDK from finding its
	// racy package-private sessionUpdated method. Batch semantics are enforced
	// at the byte boundary below.
	return &safeConnection{Connection: conn}, nil
}

type safeConnection struct {
	mcp.Connection
}

type versionAwareReadCloser struct {
	next        io.ReadCloser
	reader      *bufio.Reader
	pending     []byte
	terminalErr error
	protocol    string
}

func newVersionAwareReadCloser(next io.ReadCloser) *versionAwareReadCloser {
	return &versionAwareReadCloser{
		next:     next,
		reader:   bufio.NewReaderSize(next, maxMCPMessageBytes+1),
		protocol: "2025-03-26",
	}
}

func (r *versionAwareReadCloser) Read(p []byte) (int, error) {
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	if r.terminalErr != nil {
		return 0, r.terminalErr
	}
	line, err := r.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		r.terminalErr = errors.New("mcpserver: protocol request exceeded the size limit")
		return 0, r.terminalErr
	}
	if err != nil && !errors.Is(err, io.EOF) {
		r.terminalErr = errors.New("mcpserver: protocol input unavailable")
		return 0, r.terminalErr
	}
	if len(line) == 0 {
		if err == nil {
			err = io.EOF
		}
		r.terminalErr = err
		return 0, err
	}
	if validationErr := r.validateLine(line); validationErr != nil {
		r.terminalErr = validationErr
		return 0, validationErr
	}
	// ReadSlice's storage is reused on the next call, so retain this line until
	// callers with small read buffers have consumed it completely.
	r.pending = append(r.pending[:0], line...)
	if errors.Is(err, io.EOF) {
		r.terminalErr = io.EOF
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *versionAwareReadCloser) validateLine(line []byte) error {
	raw := bytes.TrimSpace(line)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '[' {
		protocol := r.protocol
		var batch []json.RawMessage
		if json.Unmarshal(raw, &batch) == nil {
			for _, message := range batch {
				if requested, initialize := initializeProtocol(message); initialize {
					protocol = negotiatedBatchProtocol(requested)
					break
				}
			}
		}
		if protocol >= batchProhibitedFromVersion {
			return errors.New("mcpserver: JSON-RPC batching is not supported for the negotiated protocol")
		}
		return nil
	}
	if requested, initialize := initializeProtocol(raw); initialize {
		r.protocol = negotiatedBatchProtocol(requested)
	}
	return nil
}

// negotiatedBatchProtocol mirrors go-sdk v1.4.1's negotiatedVersion for the
// only distinction this adapter needs. Exactly the two known legacy versions
// permit batching. Current, newer-known, empty, and every unsupported value
// negotiate to current behavior and therefore reject it.
func negotiatedBatchProtocol(requested string) string {
	switch requested {
	case legacyProtocol20241105, legacyProtocol20250326:
		return requested
	default:
		return batchProhibitedFromVersion
	}
}

func initializeProtocol(raw []byte) (string, bool) {
	var request struct {
		Method string `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &request) != nil || request.Method != "initialize" {
		return "", false
	}
	return request.Params.ProtocolVersion, true
}

func (r *versionAwareReadCloser) Close() error {
	if err := r.next.Close(); err != nil {
		return errors.New("mcpserver: protocol input cleanup failed")
	}
	return nil
}

type redactingWriteCloser struct {
	next io.WriteCloser
	mu   sync.Mutex
}

// Write receives one complete newline-delimited JSON value per call from the
// SDK's serialized ioConn writer. Any top-level JSON-RPC error is rewritten to
// a fixed message, covering SDK decode/schema errors that occur before our MCP
// middleware or tool handlers can run. Invalid server output fails closed and
// is never forwarded as protocol-corrupting or potentially sensitive bytes.
func (w *redactingWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	newline := bytes.HasSuffix(p, []byte{'\n'})
	raw := bytes.TrimSuffix(p, []byte{'\n'})
	var payload any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return 0, errors.New("mcpserver: refusing invalid protocol output")
	}
	sanitizeProtocolErrors(payload)
	safe, err := json.Marshal(payload)
	if err != nil {
		return 0, errors.New("mcpserver: refusing unencodable protocol output")
	}
	if newline {
		safe = append(safe, '\n')
	}
	n, err := w.next.Write(safe)
	if err != nil || n != len(safe) {
		return 0, errors.New("mcpserver: protocol output unavailable")
	}
	// io.Writer requires the input length on success, even though the redacted
	// representation can be shorter than the SDK-provided bytes.
	return len(p), nil
}

func sanitizeProtocolErrors(payload any) {
	switch value := payload.(type) {
	case []any:
		for _, item := range value {
			sanitizeProtocolErrors(item)
		}
	case map[string]any:
		if value["jsonrpc"] != "2.0" {
			return
		}
		errValue, ok := value["error"].(map[string]any)
		if !ok {
			return
		}
		code, ok := errValue["code"].(json.Number)
		if !ok {
			code = json.Number("-32603")
		}
		value["error"] = map[string]any{
			"code":    code,
			"message": "request rejected",
		}
	}
}

func (w *redactingWriteCloser) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.next.Close(); err != nil {
		return errors.New("mcpserver: protocol output cleanup failed")
	}
	return nil
}

type nonClosingWriter struct {
	io.Writer
}

func (nonClosingWriter) Close() error { return nil }
