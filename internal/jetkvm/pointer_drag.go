package jetkvm

import (
	"errors"
	"fmt"
)

// MaxDragSteps bounds the number of optional intermediate pointer reports in
// one drag gesture. The start, destination, and release reports are additional
// to this count.
const MaxDragSteps = 256

// PointerDragReport is one full-width absolute-pointer state in a drag
// gesture. Adapters keep these values as ints until validation is complete,
// then narrow them only at the HID send boundary.
type PointerDragReport struct {
	X       int
	Y       int
	Buttons int
}

// ValidatePointerDragReports validates an already-built drag sequence at an
// operation boundary. Release reports may use a zero button mask, but the
// sequence itself must include at least one nonzero button state so an empty
// or movement-only sequence cannot be reported as a successful drag.
func ValidatePointerDragReports(reports []PointerDragReport) error {
	if len(reports) == 0 {
		return errors.New("drag reports must not be empty")
	}

	hasButtonState := false
	for i, report := range reports {
		if err := ValidatePointer(report.X, report.Y, report.Buttons); err != nil {
			return fmt.Errorf("drag report %d: %w", i+1, err)
		}
		if report.Buttons != 0 {
			hasButtonState = true
		}
	}
	if !hasButtonState {
		return errors.New("drag reports must include a nonzero button mask")
	}
	return nil
}

// BuildPointerDragReports validates and constructs a complete drag gesture:
// press at the start, optionally move through interpolated positions, move to
// the destination, then release there. The operation-local button mask must be
// nonzero, while the generated terminal release remains a valid zero-button
// pointer report. Every generated coordinate is checked before any adapter can
// narrow it to HID wire types.
func BuildPointerDragReports(x1, y1, x2, y2, button, steps int) ([]PointerDragReport, error) {
	if err := ValidatePointerGesture(x1, y1, button); err != nil {
		return nil, fmt.Errorf("drag start: %w", err)
	}
	if err := ValidatePointerGesture(x2, y2, button); err != nil {
		return nil, fmt.Errorf("drag destination: %w", err)
	}
	if steps < 0 || steps > MaxDragSteps {
		return nil, fmt.Errorf("steps must be in [0,%d], got %d", MaxDragSteps, steps)
	}

	reports := make([]PointerDragReport, 0, steps+3)
	reports = append(reports, PointerDragReport{X: x1, Y: y1, Buttons: button})

	denominator := steps + 1
	for i := 1; i <= steps; i++ {
		// The weighted form keeps all arithmetic non-negative and produces a
		// point on the closed segment between the already-validated endpoints.
		x := (x1*(denominator-i) + x2*i) / denominator
		y := (y1*(denominator-i) + y2*i) / denominator
		reports = append(reports, PointerDragReport{X: x, Y: y, Buttons: button})
	}

	reports = append(reports,
		PointerDragReport{X: x2, Y: y2, Buttons: button},
		PointerDragReport{X: x2, Y: y2, Buttons: 0},
	)
	if err := ValidatePointerDragReports(reports); err != nil {
		return nil, err
	}
	return reports, nil
}
