package main

import "math"

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

			dHub := math.Sqrt((fx-11.0)*(fx-11.0) + (fy-cy)*(fy-cy))
			if dHub <= 4.0 {
				alpha = 1.0
			} else if dHub <= 4.8 {
				alpha = (4.8 - dHub) / 0.8
			}

			dLeft := math.Sqrt((fx-3.0)*(fx-3.0) + (fy-cy)*(fy-cy))
			if dLeft <= 2.0 {
				alpha = 1.0
			} else if dLeft <= 2.6 {
				alpha = math.Max(alpha, (2.6-dLeft)/0.6)
			}

			dRight := math.Sqrt((fx-19.0)*(fx-19.0) + (fy-cy)*(fy-cy))
			if dRight <= 2.0 {
				alpha = 1.0
			} else if dRight <= 2.6 {
				alpha = math.Max(alpha, (2.6-dRight)/0.6)
			}

			if fx >= 5.0 && fx <= 7.0 && fy >= 10.0 && fy <= 12.0 {
				alpha = 1.0
			}

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
