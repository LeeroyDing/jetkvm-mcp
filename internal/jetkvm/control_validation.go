package jetkvm

import (
	"fmt"

	"github.com/leeroyding/jetkvm-mcp/internal/hidproto"
)

// MaxAbsoluteCoordinate is the inclusive coordinate bound supported by the
// JetKVM absolute-pointer HID report.
const MaxAbsoluteCoordinate = hidproto.MaxAbsoluteCoordinate

// ValidateKeypress validates integer adapter input before any narrowing to a
// wire byte. CLI and MCP both call this exact function, so neither surface can
// wrap a negative or oversized modifier/key into an apparently valid report.
func ValidateKeypress(key, modifier int) error {
	if key < 0 || key > 255 {
		return fmt.Errorf("key must be in [0,255], got %d", key)
	}
	if modifier < 0 || modifier > 255 {
		return fmt.Errorf("modifier must be in [0,255], got %d", modifier)
	}
	return nil
}

// ValidatePointer validates integer adapter input before narrowing to int32
// and byte for the HID wire format.
func ValidatePointer(x, y, buttons int) error {
	if x < 0 || x > MaxAbsoluteCoordinate || y < 0 || y > MaxAbsoluteCoordinate {
		return fmt.Errorf("x and y must be in [0,%d]", MaxAbsoluteCoordinate)
	}
	if buttons < 0 || buttons > 255 {
		return fmt.Errorf("buttons must be in [0,255], got %d", buttons)
	}
	return nil
}
