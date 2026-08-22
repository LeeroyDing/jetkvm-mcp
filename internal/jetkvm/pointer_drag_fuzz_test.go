package jetkvm

import (
	"math"
	"testing"
)

// FuzzBuildPointerDragReports pins the complete adapter-side drag sequence:
// valid full-width inputs produce a bounded, fully validated press/move/release
// gesture, while invalid endpoint, button, or step values produce no reports.
func FuzzBuildPointerDragReports(f *testing.F) {
	for _, seed := range []struct {
		x1, y1, x2, y2 int
		button, steps  int
	}{
		{0, 0, 0, 0, 0, 0},
		{0, 0, 9, 6, 1, 2},
		{MaxAbsoluteCoordinate, MaxAbsoluteCoordinate, 0, 0, MaxPointerButtonMask, MaxDragSteps},
		{-1, 0, 0, 0, 1, 0},
		{0, 0, MaxAbsoluteCoordinate + 1, 0, 1, 0},
		{0, 0, 0, 0, -1, 0},
		{0, 0, 0, 0, 256, 0},
		{0, 0, 0, 0, 1, -1},
		{0, 0, 0, 0, 1, MaxDragSteps + 1},
		{math.MinInt, math.MaxInt, math.MaxInt, math.MinInt, math.MaxInt, math.MinInt},
	} {
		f.Add(seed.x1, seed.y1, seed.x2, seed.y2, seed.button, seed.steps)
	}

	f.Fuzz(func(t *testing.T, x1, y1, x2, y2, button, steps int) {
		startValid := ValidatePointerGesture(x1, y1, button) == nil
		destinationValid := ValidatePointerGesture(x2, y2, button) == nil
		stepsValid := steps >= 0 && steps <= MaxDragSteps
		wantValid := startValid && destinationValid && stepsValid

		reports, err := BuildPointerDragReports(x1, y1, x2, y2, button, steps)
		if !wantValid {
			if err == nil {
				t.Fatalf("BuildPointerDragReports accepted invalid input: start=(%d,%d) destination=(%d,%d) button=%d steps=%d", x1, y1, x2, y2, button, steps)
			}
			if len(reports) != 0 {
				t.Fatalf("invalid input returned %d reports, want none", len(reports))
			}
			return
		}

		if err != nil {
			t.Fatalf("BuildPointerDragReports rejected valid input: %v", err)
		}
		assertPointerDragReportInvariants(t, reports, x1, y1, x2, y2, button, steps)
	})
}

// FuzzValidatePointerDragReports drives the operation-boundary validator with
// arbitrary prebuilt report slices. A sequence is accepted exactly when it is
// non-empty, every full-width report is in range, and at least one report has
// a nonzero button state; a terminal release is not required by this helper.
func FuzzValidatePointerDragReports(f *testing.F) {
	for _, seed := range []struct {
		x1, y1, buttons1 int
		x2, y2, buttons2 int
		shape            uint8
	}{
		{shape: 0},
		{shape: 1},
		{x1: 1, y1: 2, buttons1: 1, shape: 2},
		{x1: 1, y1: 2, x2: 3, y2: 4, shape: 3},
		{x1: 1, y1: 2, x2: 3, y2: 4, buttons2: 1, shape: 4},
		{x1: 1, y1: 2, buttons1: 1, x2: 3, y2: 4, shape: 5},
		{x1: MaxAbsoluteCoordinate, y1: MaxAbsoluteCoordinate, buttons1: MaxPointerButtonMask, shape: 2},
		{x1: -1, buttons1: 1, shape: 2},
		{x1: MaxAbsoluteCoordinate + 1, buttons1: 1, shape: 2},
		{x1: 1, y1: 2, buttons1: -1, shape: 2},
		{x1: 1, y1: 2, buttons1: MaxPointerButtonMask + 1, shape: 2},
		{x1: math.MinInt, y1: math.MaxInt, buttons1: math.MaxInt, x2: math.MaxInt, y2: math.MinInt, buttons2: math.MinInt, shape: 3},
	} {
		f.Add(seed.x1, seed.y1, seed.buttons1, seed.x2, seed.y2, seed.buttons2, seed.shape)
	}

	f.Fuzz(func(t *testing.T, x1, y1, buttons1, x2, y2, buttons2 int, shape uint8) {
		first := PointerDragReport{X: x1, Y: y1, Buttons: buttons1}
		second := PointerDragReport{X: x2, Y: y2, Buttons: buttons2}
		var reports []PointerDragReport
		switch shape % 6 {
		case 0:
			reports = nil
		case 1:
			reports = []PointerDragReport{}
		case 2:
			reports = []PointerDragReport{first}
		case 3:
			reports = []PointerDragReport{first, second}
		case 4:
			first.Buttons = 0
			reports = []PointerDragReport{first, second}
		case 5:
			second.Buttons = 0
			reports = []PointerDragReport{first, second}
		}

		wantValid := len(reports) > 0
		hasButtonState := false
		for _, report := range reports {
			if report.X < 0 || report.X > MaxAbsoluteCoordinate ||
				report.Y < 0 || report.Y > MaxAbsoluteCoordinate ||
				report.Buttons < 0 || report.Buttons > MaxPointerButtonMask {
				wantValid = false
			}
			if report.Buttons != 0 {
				hasButtonState = true
			}
		}
		wantValid = wantValid && hasButtonState

		err := ValidatePointerDragReports(reports)
		if wantValid && err != nil {
			t.Fatalf("ValidatePointerDragReports(%+v) rejected valid input: %v", reports, err)
		}
		if !wantValid && err == nil {
			t.Fatalf("ValidatePointerDragReports(%+v) accepted invalid input", reports)
		}
	})
}
