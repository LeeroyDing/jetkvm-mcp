package jetkvm

import (
	"math"
	"testing"
)

// FuzzValidatePointer pins the adapter boundary before integer coordinates
// and the button bitmask are narrowed to their HID wire types. The validator
// accepts exactly the documented coordinate and button domains.
func FuzzValidatePointer(f *testing.F) {
	for _, seed := range []struct {
		x, y, buttons int
	}{
		{0, 0, 0},
		{MaxAbsoluteCoordinate, MaxAbsoluteCoordinate, 255},
		{-1, 0, 1},
		{0, -1, 1},
		{MaxAbsoluteCoordinate + 1, 0, 1},
		{0, MaxAbsoluteCoordinate + 1, 1},
		{0, 0, -1},
		{0, 0, 256},
		{math.MinInt, math.MaxInt, math.MaxInt},
	} {
		f.Add(seed.x, seed.y, seed.buttons)
	}

	f.Fuzz(func(t *testing.T, x, y, buttons int) {
		wantValid := x >= 0 && x <= MaxAbsoluteCoordinate &&
			y >= 0 && y <= MaxAbsoluteCoordinate &&
			buttons >= 0 && buttons <= 255

		err := ValidatePointer(x, y, buttons)
		if wantValid && err != nil {
			t.Fatalf("ValidatePointer(%d, %d, %d) rejected valid input: %v", x, y, buttons, err)
		}
		if !wantValid && err == nil {
			t.Fatalf("ValidatePointer(%d, %d, %d) accepted invalid input", x, y, buttons)
		}
	})
}
