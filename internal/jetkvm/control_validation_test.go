package jetkvm

import (
	"math"
	"strings"
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

func TestValidateKeyComboBounds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		modifier int
		keys     []int
		valid    bool
	}{
		{name: "single key", keys: []int{KeyUsageEnter}, valid: true},
		{name: "modifier and key", modifier: ModifierLeftControl, keys: []int{KeyUsageC}, valid: true},
		{name: "modifier only", modifier: ModifierLeftMeta, valid: true},
		{name: "modifier only with zero padding", modifier: ModifierLeftMeta, keys: []int{0, 0, 0, 0, 0, 0}, valid: true},
		{name: "key with zero padding", keys: []int{0, KeyUsageEnter, 0}, valid: true},
		{name: "inclusive boundaries", modifier: 255, keys: []int{0, 1, 2, 3, 4, 255}, valid: true},
		{name: "neutral empty chord"},
		{name: "neutral all-zero padded chord", keys: []int{0, 0, 0, 0, 0, 0}},
		{name: "too many keys", keys: []int{1, 2, 3, 4, 5, 6, 7}},
		{name: "negative modifier", modifier: -1, keys: []int{1}},
		{name: "oversized modifier", modifier: 256, keys: []int{1}},
		{name: "negative key", keys: []int{1, -1}},
		{name: "oversized key", keys: []int{1, 256}},
		{name: "maximum ints", modifier: math.MaxInt, keys: []int{math.MaxInt}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateKeyCombo(tc.modifier, tc.keys); (err == nil) != tc.valid {
				t.Errorf("ValidateKeyCombo(%d, %v) error = %v, valid=%v", tc.modifier, tc.keys, err, tc.valid)
			}
		})
	}
}

func TestValidateHoldMSBounds(t *testing.T) {
	for _, tc := range []struct {
		holdMS int
		valid  bool
	}{
		{holdMS: -1},
		{holdMS: 0},
		{holdMS: 1, valid: true},
		{holdMS: MaxHoldMS, valid: true},
		{holdMS: MaxHoldMS + 1},
		{holdMS: math.MaxInt},
	} {
		if err := ValidateHoldMS(tc.holdMS); (err == nil) != tc.valid {
			t.Errorf("ValidateHoldMS(%d) error = %v, valid=%v", tc.holdMS, err, tc.valid)
		}
	}
}

func TestValidateKeySequenceLength(t *testing.T) {
	for _, tc := range []struct {
		length int
		valid  bool
	}{
		{length: -1},
		{length: 0},
		{length: 1, valid: true},
		{length: MaxKeySequenceLength, valid: true},
		{length: MaxKeySequenceLength + 1},
	} {
		if err := ValidateKeySequenceLength(tc.length); (err == nil) != tc.valid {
			t.Errorf("ValidateKeySequenceLength(%d) error = %v, valid=%v", tc.length, err, tc.valid)
		}
	}
}

func TestValidatePointerBounds(t *testing.T) {
	for _, tc := range []struct {
		x, y, buttons int
		valid         bool
	}{
		{0, 0, 0, true},
		{MaxAbsoluteCoordinate, MaxAbsoluteCoordinate, MaxPointerButtonMask, true},
		{-1, 0, 0, false},
		{0, -1, 0, false},
		{MaxAbsoluteCoordinate + 1, 0, 0, false},
		{0, MaxAbsoluteCoordinate + 1, 0, false},
		{0, 0, -1, false},
		{0, 0, MaxPointerButtonMask + 1, false},
		{math.MaxInt, math.MaxInt, math.MaxInt, false},
	} {
		if err := ValidatePointer(tc.x, tc.y, tc.buttons); (err == nil) != tc.valid {
			t.Errorf("ValidatePointer(%d, %d, %d) error = %v, valid=%v", tc.x, tc.y, tc.buttons, err, tc.valid)
		}
	}
}

func TestValidateScrollBounds(t *testing.T) {
	for _, tc := range []struct {
		dx, dy int
		valid  bool
	}{
		{0, 1, true},
		{1, 0, true},
		{-MaxScrollDelta, MaxScrollDelta, true},
		{MaxScrollDelta, -MaxScrollDelta, true},
		{0, 0, false},
		{-MaxScrollDelta - 1, 0, false},
		{MaxScrollDelta + 1, 0, false},
		{0, -MaxScrollDelta - 1, false},
		{0, MaxScrollDelta + 1, false},
		{math.MinInt, math.MaxInt, false},
	} {
		if err := ValidateScroll(tc.dx, tc.dy); (err == nil) != tc.valid {
			t.Errorf("ValidateScroll(%d, %d) error = %v, valid=%v", tc.dx, tc.dy, err, tc.valid)
		}
	}
}

func TestPointerAndScrollRangeErrorsIncludeCallerValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "absolute pointer",
			err:  ValidatePointer(-1, MaxAbsoluteCoordinate+1, 0),
			want: []string{"x=-1", "y=32768"},
		},
		{
			name: "scroll",
			err:  ValidateScroll(-MaxScrollDelta-1, MaxScrollDelta+1),
			want: []string{"dx=-128", "dy=128"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil {
				t.Fatal("range validator accepted invalid input")
			}
			for _, marker := range tc.want {
				if !strings.Contains(tc.err.Error(), marker) {
					t.Errorf("range error %q does not include %q", tc.err, marker)
				}
			}
		})
	}
}
