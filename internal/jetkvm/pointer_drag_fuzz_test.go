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
		{MaxAbsoluteCoordinate, MaxAbsoluteCoordinate, 0, 0, 255, MaxDragSteps},
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
		startValid := ValidatePointer(x1, y1, button) == nil
		destinationValid := ValidatePointer(x2, y2, button) == nil
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
