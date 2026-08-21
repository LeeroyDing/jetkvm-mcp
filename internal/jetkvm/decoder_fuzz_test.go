package jetkvm

import "testing"

// FuzzValidateScreenshotDimensions pins the allocation boundary for arbitrary
// signed dimensions, including values whose product would overflow int.
func FuzzValidateScreenshotDimensions(f *testing.F) {
	for _, seed := range [][2]int{
		{1920, 1080},
		{3840, 2160},
		{4096, 4096},
		{8192, 2048},
		{8193, 1},
		{1, 8193},
		{4097, 4096},
		{0, 1},
		{-1, 1},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, width, height int) {
		wantValid := width > 0 && height > 0 &&
			width <= maxScreenshotDimension && height <= maxScreenshotDimension &&
			width <= maxScreenshotPixels/height
		err := ValidateScreenshotDimensions(width, height)
		if (err == nil) != wantValid {
			t.Fatalf("ValidateScreenshotDimensions(%d, %d) error = %v, want valid %t", width, height, err, wantValid)
		}
	})
}
