package jetkvm

import (
	"strings"
	"testing"
)

func TestBuildPointerDragReportsSequence(t *testing.T) {
	tests := []struct {
		name           string
		x1, y1, x2, y2 int
		button, steps  int
		want           []PointerDragReport
	}{
		{
			name:   "direct move",
			x1:     10,
			y1:     20,
			x2:     30,
			y2:     40,
			button: 1,
			want: []PointerDragReport{
				{X: 10, Y: 20, Buttons: 1},
				{X: 30, Y: 40, Buttons: 1},
				{X: 30, Y: 40, Buttons: 0},
			},
		},
		{
			name:   "interpolated move",
			x1:     0,
			y1:     0,
			x2:     9,
			y2:     6,
			button: 2,
			steps:  2,
			want: []PointerDragReport{
				{X: 0, Y: 0, Buttons: 2},
				{X: 3, Y: 2, Buttons: 2},
				{X: 6, Y: 4, Buttons: 2},
				{X: 9, Y: 6, Buttons: 2},
				{X: 9, Y: 6, Buttons: 0},
			},
		},
		{
			name:   "reverse interpolation at coordinate bounds",
			x1:     MaxAbsoluteCoordinate,
			y1:     MaxAbsoluteCoordinate,
			x2:     0,
			y2:     0,
			button: MaxPointerButtonMask,
			steps:  1,
			want: []PointerDragReport{
				{X: MaxAbsoluteCoordinate, Y: MaxAbsoluteCoordinate, Buttons: MaxPointerButtonMask},
				{X: MaxAbsoluteCoordinate / 2, Y: MaxAbsoluteCoordinate / 2, Buttons: MaxPointerButtonMask},
				{X: 0, Y: 0, Buttons: MaxPointerButtonMask},
				{X: 0, Y: 0, Buttons: 0},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reports, err := BuildPointerDragReports(tc.x1, tc.y1, tc.x2, tc.y2, tc.button, tc.steps)
			if err != nil {
				t.Fatalf("BuildPointerDragReports: %v", err)
			}
			if len(reports) != len(tc.want) {
				t.Fatalf("reports = %+v, want %+v", reports, tc.want)
			}
			for i := range tc.want {
				if reports[i] != tc.want[i] {
					t.Errorf("report %d = %+v, want %+v", i+1, reports[i], tc.want[i])
				}
			}
			assertPointerDragReportInvariants(t, reports, tc.x1, tc.y1, tc.x2, tc.y2, tc.button, tc.steps)
		})
	}
}

func TestBuildPointerDragReportsInclusiveBounds(t *testing.T) {
	reports, err := BuildPointerDragReports(
		0,
		MaxAbsoluteCoordinate,
		MaxAbsoluteCoordinate,
		0,
		MaxPointerButtonMask,
		MaxDragSteps,
	)
	if err != nil {
		t.Fatalf("BuildPointerDragReports at inclusive bounds: %v", err)
	}
	assertPointerDragReportInvariants(
		t,
		reports,
		0,
		MaxAbsoluteCoordinate,
		MaxAbsoluteCoordinate,
		0,
		MaxPointerButtonMask,
		MaxDragSteps,
	)
}

func TestBuildPointerDragReportsRejectsOutOfRangeInput(t *testing.T) {
	tests := []struct {
		name           string
		x1, y1, x2, y2 int
		button, steps  int
		wantError      string
	}{
		{name: "start x below minimum", x1: -1, wantError: "drag start"},
		{name: "start y above maximum", y1: MaxAbsoluteCoordinate + 1, wantError: "drag start"},
		{name: "destination x below minimum", x2: -1, wantError: "drag destination"},
		{name: "destination y above maximum", y2: MaxAbsoluteCoordinate + 1, wantError: "drag destination"},
		{name: "button below minimum", button: -1, wantError: "drag start"},
		{name: "button above maximum", button: MaxPointerButtonMask + 1, wantError: "drag start"},
		{name: "steps below minimum", steps: -1, wantError: "steps must be"},
		{name: "steps above maximum", steps: MaxDragSteps + 1, wantError: "steps must be"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reports, err := BuildPointerDragReports(tc.x1, tc.y1, tc.x2, tc.y2, tc.button, tc.steps)
			if err == nil {
				t.Fatalf("BuildPointerDragReports accepted invalid input and returned %+v", reports)
			}
			if len(reports) != 0 {
				t.Errorf("invalid input returned %d reports, want none", len(reports))
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("error = %q, want marker %q", err, tc.wantError)
			}
		})
	}
}

func assertPointerDragReportInvariants(
	t testing.TB,
	reports []PointerDragReport,
	x1, y1, x2, y2, button, steps int,
) {
	t.Helper()

	if len(reports) != steps+3 {
		t.Fatalf("report count = %d, want steps+3 = %d", len(reports), steps+3)
	}
	if got := reports[0]; got != (PointerDragReport{X: x1, Y: y1, Buttons: button}) {
		t.Errorf("first report = %+v, want pressed start", got)
	}
	if got := reports[len(reports)-2]; got != (PointerDragReport{X: x2, Y: y2, Buttons: button}) {
		t.Errorf("penultimate report = %+v, want held destination", got)
	}
	if got := reports[len(reports)-1]; got != (PointerDragReport{X: x2, Y: y2, Buttons: 0}) {
		t.Errorf("final report = %+v, want released destination", got)
	}

	for i, report := range reports {
		if err := ValidatePointer(report.X, report.Y, report.Buttons); err != nil {
			t.Errorf("report %d failed ValidatePointer: %+v: %v", i+1, report, err)
		}
		if i < len(reports)-1 && report.Buttons != button {
			t.Errorf("report %d buttons = %d, want held button %d", i+1, report.Buttons, button)
		}
	}
}
