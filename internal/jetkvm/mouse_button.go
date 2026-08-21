package jetkvm

import "errors"

// USB HID mouse-button bits used by the named mouse-button resolver.
const (
	MouseButtonLeft byte = 1 << iota
	MouseButtonRight
	MouseButtonMiddle
)

var (
	errUnknownMouseButton = errors.New("unknown mouse button; valid buttons: left, right, middle")
	errUnknownMouseAction = errors.New("unknown mouse button action; valid actions: press, release")
	errInvalidMouseButton = errors.New("invalid mouse button mask; valid masks: 1 (left), 2 (right), 4 (middle)")
)

// ValidateMouseButton accepts exactly one supported named-button bit. Combined
// masks and zero are invalid because one mouse-button action targets one
// button. The error is static and never reflects the caller-controlled byte.
func ValidateMouseButton(button byte) error {
	switch button {
	case MouseButtonLeft, MouseButtonRight, MouseButtonMiddle:
		return nil
	default:
		return errInvalidMouseButton
	}
}

// ResolveMouseButton resolves the strict named-button/action contract shared
// by the MCP and CLI adapters. Names are deliberately exact and
// case-sensitive so callers cannot accidentally broaden their input contract.
// Errors never include either caller-controlled value.
func ResolveMouseButton(button, action string) (mask byte, pressed bool, err error) {
	switch button {
	case "left":
		mask = MouseButtonLeft
	case "right":
		mask = MouseButtonRight
	case "middle":
		mask = MouseButtonMiddle
	default:
		return 0, false, errUnknownMouseButton
	}
	if err := ValidateMouseButton(mask); err != nil {
		return 0, false, err
	}

	switch action {
	case "press":
		return mask, true, nil
	case "release":
		return mask, false, nil
	default:
		return 0, false, errUnknownMouseAction
	}
}
