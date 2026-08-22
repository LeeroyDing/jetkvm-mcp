package jetkvm

import (
	"context"
	"errors"
	"fmt"
)

// ErrorKind is the stable failure taxonomy exposed to CLI and MCP callers.
// Callers can distinguish these values without matching dependency-specific
// transport strings.
type ErrorKind string

const (
	ErrorKindAuthFailed  ErrorKind = "auth-failed"
	ErrorKindUnreachable ErrorKind = "unreachable"
	ErrorKindTimeout     ErrorKind = "timeout"
	ErrorKindBadFrame    ErrorKind = "bad-frame"
)

// DeviceError is a privacy-safe, classified device failure. Detail must
// already be redacted; constructors inside this package enforce that rule,
// and the MCP boundary applies RedactError once more before returning it.
type DeviceError struct {
	Kind      ErrorKind
	Operation string
	Detail    string
	sentinel  error
}

func (e *DeviceError) Error() string {
	description := map[ErrorKind]string{
		ErrorKindAuthFailed:  "device authentication failed",
		ErrorKindUnreachable: "device is unreachable",
		ErrorKindTimeout:     "device operation timed out",
		ErrorKindBadFrame:    "device returned a malformed or oversized protocol frame",
	}[e.Kind]
	if description == "" {
		description = "device operation failed"
	}

	message := fmt.Sprintf("jetkvm: %s: %s", e.Kind, description)
	if e.Operation != "" {
		message += " during " + e.Operation
	}
	if e.Detail != "" {
		message += ": " + e.Detail
	}
	return message
}

// Unwrap is reserved for package-controlled, privacy-safe sentinels. Raw
// dependency errors are flattened by newDeviceError so callers cannot recover
// credential-bearing transport objects through errors.As.
func (e *DeviceError) Unwrap() error {
	return e.sentinel
}

// ErrorKindOf returns a classified kind, including through ordinary wrapping.
// A raw context deadline/cancellation is normalized to timeout at this public
// boundary so MCP callers never receive an undifferentiated context error.
func ErrorKindOf(err error) ErrorKind {
	var deviceErr *DeviceError
	if errors.As(err, &deviceErr) {
		return deviceErr.Kind
	}
	if errors.Is(err, ErrHIDClosed) {
		return ErrorKindUnreachable
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrorKindTimeout
	}
	return ""
}

func newDeviceError(kind ErrorKind, operation string, err error) error {
	var existing *DeviceError
	if errors.As(err, &existing) {
		return err
	}
	detail := ""
	if err != nil {
		detail = RedactError(err)
	}
	return &DeviceError{Kind: kind, Operation: operation, Detail: detail}
}

func timeoutError(operation string, err error) error {
	return &DeviceError{Kind: ErrorKindTimeout, Operation: operation, Detail: RedactError(err)}
}
