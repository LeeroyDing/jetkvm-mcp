package jetkvm

import (
	"fmt"

	"github.com/leeroyding/jetkvm-mcp/internal/hidproto"
)

// MaxAbsoluteCoordinate is the inclusive coordinate bound supported by the
// JetKVM absolute-pointer HID report.
const MaxAbsoluteCoordinate = hidproto.MaxAbsoluteCoordinate

// MaxScrollDelta is the inclusive magnitude accepted for either wheel axis.
// The JetKVM absolute- and relative-mouse HID descriptors both encode Wheel
// and AC Pan as signed 8-bit relative values with logical bounds -127..127.
// Keep -128 out even though it fits in int8: it is outside that HID contract.
const MaxScrollDelta = 127

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

// ValidateKeyCombo validates a resolved keyboard chord before its integer
// representation is narrowed to the HID wire bytes. A keyboard report can
// carry at most six non-modifier keys. Modifier-only chords are valid, but a
// report with neither a modifier nor a key is not a chord.
func ValidateKeyCombo(modifier int, keys []int) error {
	if modifier < 0 || modifier > 255 {
		return fmt.Errorf("modifier must be in [0,255], got %d", modifier)
	}
	if len(keys) > hidproto.HIDKeyBufferSize {
		return fmt.Errorf("key combo must contain at most %d keys, got %d", hidproto.HIDKeyBufferSize, len(keys))
	}
	if modifier == 0 && len(keys) == 0 {
		return fmt.Errorf("key combo must contain at least one modifier or key")
	}
	for i, key := range keys {
		if key < 0 || key > 255 {
			return fmt.Errorf("key at index %d must be in [0,255], got %d", i, key)
		}
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

// ValidateScroll validates integer adapter input before either wheel delta is
// narrowed to the firmware's int8 parameters. Like ValidatePointer, it rejects
// values outside the device contract rather than silently changing the
// caller's requested input. A zero/zero report is also rejected because both
// the reference UI and firmware treat it as a no-op.
func ValidateScroll(dx, dy int) error {
	if dx < -MaxScrollDelta || dx > MaxScrollDelta || dy < -MaxScrollDelta || dy > MaxScrollDelta {
		return fmt.Errorf("dx and dy must be in [%d,%d]", -MaxScrollDelta, MaxScrollDelta)
	}
	if dx == 0 && dy == 0 {
		return fmt.Errorf("dx and dy cannot both be zero; nothing would be scrolled")
	}
	return nil
}
