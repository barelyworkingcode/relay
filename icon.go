package main

import "math"

// CreateIconRGBA generates a 22x22 RGBA byte slice for the tray icon.
// Draws a relay-node symbol: small circle -- line -- large circle -- line -- small circle.
// White on transparent background with antialiased edges.
func CreateIconRGBA() ([]byte, int, int) {
	const size = 22
	rgba := make([]byte, size*size*4)

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			idx := (y*size + x) * 4
			fx := float64(x) + 0.5
			fy := float64(y) + 0.5
			cy := 11.0

			alpha := 0.0

			// Central hub circle (filled, radius 4).
			d := math.Sqrt((fx-11.0)*(fx-11.0) + (fy-cy)*(fy-cy))
			if d <= 4.0 {
				alpha = 1.0
			} else if d <= 4.8 {
				alpha = (4.8 - d) / 0.8
			}

			// Left endpoint circle (filled, radius 2).
			dl := math.Sqrt((fx-3.0)*(fx-3.0) + (fy-cy)*(fy-cy))
			if dl <= 2.0 {
				alpha = 1.0
			} else if dl <= 2.6 {
				alpha = math.Max(alpha, (2.6-dl)/0.6)
			}

			// Right endpoint circle (filled, radius 2).
			dr := math.Sqrt((fx-19.0)*(fx-19.0) + (fy-cy)*(fy-cy))
			if dr <= 2.0 {
				alpha = 1.0
			} else if dr <= 2.6 {
				alpha = math.Max(alpha, (2.6-dr)/0.6)
			}

			// Left connecting line (from left dot to center hub).
			if fx >= 5.0 && fx <= 7.0 && fy >= 10.0 && fy <= 12.0 {
				alpha = 1.0
			}

			// Right connecting line (from center hub to right dot).
			if fx >= 15.0 && fx <= 17.0 && fy >= 10.0 && fy <= 12.0 {
				alpha = 1.0
			}

			if alpha > 0.0 {
				a := uint8(math.Min(alpha, 1.0) * 255.0)
				rgba[idx] = 255
				rgba[idx+1] = 255
				rgba[idx+2] = 255
				rgba[idx+3] = a
			}
		}
	}

	return rgba, size, size
}
