package jetkvm

import (
	"math"
	"slices"
	"testing"
)

// FuzzValidateKeypress drives ValidateKeypress across the full int space,
// including boundary and overflow shapes an adapter could produce from JSON
// or CLI parsing. The oracle is the documented contract: accept exactly when
// both key and modifier are in [0,255]. Properties pinned:
//
//   - no panic on any pair;
//   - acceptance matches the range oracle exactly (no wrap-around: a value
//     like 256 or math.MinInt must never validate as a wire byte).
//
// Under plain `go test ./...` only the seed corpus runs, so this stays
// CI-safe.
func FuzzValidateKeypress(f *testing.F) {
	for _, seed := range [][2]int{
		{0, 0}, {255, 255}, {-1, 0}, {256, 0}, {0, -1}, {0, 256},
		{math.MaxInt, math.MaxInt}, {math.MinInt, math.MinInt},
		{1 << 8, 1 << 16}, {255, 256}, {-256, 255},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, key, modifier int) {
		err := ValidateKeypress(key, modifier)
		valid := key >= 0 && key <= 255 && modifier >= 0 && modifier <= 255
		if (err == nil) != valid {
			t.Fatalf("ValidateKeypress(%d, %d) error = %v, oracle valid=%v", key, modifier, err, valid)
		}
	})
}

// FuzzValidateKeyCombo drives the full integer adapter domain, including the
// six-key report boundary and a seventh over-limit entry. Zero-valued key
// slots are HID padding, not pressed keys, so an all-zero report without a
// modifier must be rejected as an empty chord.
func FuzzValidateKeyCombo(f *testing.F) {
	for _, seed := range []struct {
		modifier int
		length   uint8
		keys     [7]int
	}{
		{modifier: ModifierLeftControl},
		{length: 1, keys: [7]int{KeyUsageC}},
		{length: 6, keys: [7]int{KeyUsageC, KeyUsageV, KeyUsageZ, KeyUsageT, KeyUsageEnter, KeyUsageEscape}},
		{length: 6},
		{length: 7, keys: [7]int{KeyUsageC, KeyUsageC, KeyUsageC, KeyUsageC, KeyUsageC, KeyUsageC, KeyUsageC}},
		{modifier: -1, length: 1, keys: [7]int{KeyUsageC}},
		{modifier: 256, length: 1, keys: [7]int{KeyUsageC}},
		{length: 1, keys: [7]int{-1}},
		{length: 1, keys: [7]int{256}},
		{modifier: math.MaxInt, length: 7, keys: [7]int{math.MinInt, math.MaxInt}},
	} {
		f.Add(
			seed.modifier, seed.length,
			seed.keys[0], seed.keys[1], seed.keys[2], seed.keys[3],
			seed.keys[4], seed.keys[5], seed.keys[6],
		)
	}

	f.Fuzz(func(
		t *testing.T,
		modifier int,
		length uint8,
		key0, key1, key2, key3, key4, key5, key6 int,
	) {
		keyCount := int(length)
		if keyCount > 7 {
			keyCount = 7
		}
		candidates := [...]int{key0, key1, key2, key3, key4, key5, key6}
		keys := slices.Clone(candidates[:keyCount])

		wantValid := modifier >= 0 && modifier <= 255 && len(keys) <= 6
		hasPressedKey := false
		for _, key := range keys {
			if key < 0 || key > 255 {
				wantValid = false
			}
			if key != 0 {
				hasPressedKey = true
			}
		}
		wantValid = wantValid && (modifier != 0 || hasPressedKey)

		err := ValidateKeyCombo(modifier, keys)
		if wantValid && err != nil {
			t.Fatalf("ValidateKeyCombo(%d, %v) rejected valid input: %v", modifier, keys, err)
		}
		if !wantValid && err == nil {
			t.Fatalf("ValidateKeyCombo(%d, %v) accepted invalid input", modifier, keys)
		}
	})
}

// FuzzValidatePointer pins both pointer adapter boundaries before integer
// coordinates and button masks are narrowed to their HID wire types. Movement
// accepts a zero mask, while click-like gestures require a nonzero mask.
func FuzzValidatePointer(f *testing.F) {
	for _, seed := range []struct {
		x, y, buttons int
	}{
		{0, 0, 0},
		{MaxAbsoluteCoordinate, MaxAbsoluteCoordinate, MaxPointerButtonMask},
		{-1, 0, 1},
		{0, -1, 1},
		{MaxAbsoluteCoordinate + 1, 0, 1},
		{0, MaxAbsoluteCoordinate + 1, 1},
		{0, 0, -1},
		{0, 0, MaxPointerButtonMask + 1},
		{math.MinInt, math.MaxInt, math.MaxInt},
	} {
		f.Add(seed.x, seed.y, seed.buttons)
	}

	f.Fuzz(func(t *testing.T, x, y, buttons int) {
		wantValid := x >= 0 && x <= MaxAbsoluteCoordinate &&
			y >= 0 && y <= MaxAbsoluteCoordinate &&
			buttons >= 0 && buttons <= MaxPointerButtonMask

		err := ValidatePointer(x, y, buttons)
		if wantValid && err != nil {
			t.Fatalf("ValidatePointer(%d, %d, %d) rejected valid input: %v", x, y, buttons, err)
		}
		if !wantValid && err == nil {
			t.Fatalf("ValidatePointer(%d, %d, %d) accepted invalid input", x, y, buttons)
		}

		wantGestureValid := x >= 0 && x <= MaxAbsoluteCoordinate &&
			y >= 0 && y <= MaxAbsoluteCoordinate &&
			buttons >= 1 && buttons <= MaxPointerButtonMask
		gestureErr := ValidatePointerGesture(x, y, buttons)
		if wantGestureValid && gestureErr != nil {
			t.Fatalf("ValidatePointerGesture(%d, %d, %d) rejected valid input: %v", x, y, buttons, gestureErr)
		}
		if !wantGestureValid && gestureErr == nil {
			t.Fatalf("ValidatePointerGesture(%d, %d, %d) accepted invalid input", x, y, buttons)
		}
	})
}

// FuzzValidateScroll pins the signed wheel-delta boundary before CLI or MCP
// integers are narrowed to the firmware's int8 wheelReport parameters.
func FuzzValidateScroll(f *testing.F) {
	for _, seed := range []struct {
		dx, dy int
	}{
		{0, 1},
		{1, 0},
		{-MaxScrollDelta, MaxScrollDelta},
		{MaxScrollDelta, -MaxScrollDelta},
		{0, 0},
		{-MaxScrollDelta - 1, 0},
		{MaxScrollDelta + 1, 0},
		{0, -MaxScrollDelta - 1},
		{0, MaxScrollDelta + 1},
		{math.MinInt, math.MaxInt},
	} {
		f.Add(seed.dx, seed.dy)
	}

	f.Fuzz(func(t *testing.T, dx, dy int) {
		wantValid := dx >= -MaxScrollDelta && dx <= MaxScrollDelta &&
			dy >= -MaxScrollDelta && dy <= MaxScrollDelta &&
			(dx != 0 || dy != 0)

		err := ValidateScroll(dx, dy)
		if wantValid && err != nil {
			t.Fatalf("ValidateScroll(%d, %d) rejected valid input: %v", dx, dy, err)
		}
		if !wantValid && err == nil {
			t.Fatalf("ValidateScroll(%d, %d) accepted invalid input", dx, dy)
		}
	})
}
