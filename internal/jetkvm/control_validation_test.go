package jetkvm

import (
	"math"
	"testing"
)

func TestValidateKeypressBounds(t *testing.T) {
	for _, tc := range []struct {
		key, modifier int
		valid         bool
	}{
		{0, 0, true},
		{255, 255, true},
		{-1, 0, false},
		{256, 0, false},
		{0, -1, false},
		{0, 256, false},
		{math.MaxInt, math.MaxInt, false},
	} {
		if err := ValidateKeypress(tc.key, tc.modifier); (err == nil) != tc.valid {
			t.Errorf("ValidateKeypress(%d, %d) error = %v, valid=%v", tc.key, tc.modifier, err, tc.valid)
		}
	}
}

func TestValidatePointerBounds(t *testing.T) {
	for _, tc := range []struct {
		x, y, buttons int
		valid         bool
	}{
		{0, 0, 0, true},
		{MaxAbsoluteCoordinate, MaxAbsoluteCoordinate, 255, true},
		{-1, 0, 0, false},
		{0, -1, 0, false},
		{MaxAbsoluteCoordinate + 1, 0, 0, false},
		{0, MaxAbsoluteCoordinate + 1, 0, false},
		{0, 0, -1, false},
		{0, 0, 256, false},
		{math.MaxInt, math.MaxInt, math.MaxInt, false},
	} {
		if err := ValidatePointer(tc.x, tc.y, tc.buttons); (err == nil) != tc.valid {
			t.Errorf("ValidatePointer(%d, %d, %d) error = %v, valid=%v", tc.x, tc.y, tc.buttons, err, tc.valid)
		}
	}
}
