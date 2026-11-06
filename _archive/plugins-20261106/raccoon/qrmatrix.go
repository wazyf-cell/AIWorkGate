// Package main — QR matrix layout: function patterns, masking and rendering.
//
// The companion of qr.go (Reed-Solomon + codeword packing). Also ported from the
// reference's raccoon-qr.ts, including its mask-penalty search, so the matrix
// this plugin draws is byte-identical to the one the reference draws.
package main

import "fmt"

func qrAlignmentPositions(version, size int) []int {
	if version == 1 {
		return nil
	}
	numAlign := version/7 + 2
	div := numAlign*2 - 2
	step := ((version*4 + 4 + div - 1) / div) * 2
	out := []int{6}
	for pos := size - 7; len(out) < numAlign; pos -= step {
		out = append(out[:1], append([]int{pos}, out[1:]...)...)
	}
	return out
}

func qrFinder(modules, isFunc [][]bool, size, x, y int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			xx, yy := x+dx, y+dy
			if xx < 0 || xx >= size || yy < 0 || yy >= size {
				continue
			}
			dist := absInt(dx)
			if absInt(dy) > dist {
				dist = absInt(dy)
			}
			modules[yy][xx] = dist != 2 && dist != 4
			isFunc[yy][xx] = true
		}
	}
}

func qrAlignment(modules, isFunc [][]bool, x, y int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			d := absInt(dx)
			if absInt(dy) > d {
				d = absInt(dy)
			}
			modules[y+dy][x+dx] = d != 1
			isFunc[y+dy][x+dx] = true
		}
	}
}

func qrFormatBits(modules, isFunc [][]bool, size, mask int) {
	data := mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*1335
	}
	bits := (data<<10 | rem) ^ 21522
	set := func(x, y int, dark bool) {
		modules[y][x] = dark
		isFunc[y][x] = true
	}
	bit := func(i int) bool { return bits>>i&1 == 1 }
	for i := 0; i <= 5; i++ {
		set(8, i, bit(i))
	}
	set(8, 7, bit(6))
	set(8, 8, bit(7))
	set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		set(size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		set(8, size-7+(i-8), bit(i))
	}
	set(8, size-8, true)
}

func qrFunctionPatterns(modules, isFunc [][]bool, size, version int) {
	for i := 0; i < size; i++ {
		dark := i%2 == 0
		modules[6][i], isFunc[6][i] = dark, true
		modules[i][6], isFunc[i][6] = dark, true
	}
	qrFinder(modules, isFunc, size, 3, 3)
	qrFinder(modules, isFunc, size, size-4, 3)
	qrFinder(modules, isFunc, size, 3, size-4)
	pos := qrAlignmentPositions(version, size)
	n := len(pos)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if (i == 0 && j == 0) || (i == 0 && j == n-1) || (i == n-1 && j == 0) {
				continue
			}
			qrAlignment(modules, isFunc, pos[i], pos[j])
		}
	}
	qrFormatBits(modules, isFunc, size, 0)
	if version >= 7 {
		rem := version
		for i := 0; i < 12; i++ {
			rem = rem<<1 ^ (rem>>11)*7973
		}
		bits := version<<12 | rem
		for i := 0; i < 18; i++ {
			dark := bits>>i&1 == 1
			a, b := size-11+i%3, i/3
			modules[b][a], isFunc[b][a] = dark, true
			modules[a][b], isFunc[a][b] = dark, true
		}
	}
}

func qrDrawCodewords(modules, isFunc [][]bool, size int, codewords []int) {
	i := 0
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := (right+1)&2 == 0
				y := vert
				if upward {
					y = size - 1 - vert
				}
				if isFunc[y][x] || i >= len(codewords)*8 {
					continue
				}
				modules[y][x] = codewords[i>>3]>>(7-(i&7))&1 == 1
				i++
			}
		}
	}
}

func qrMaskInvert(mask, x, y int) bool {
	switch mask {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (y/2+x/3)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	default:
		return ((x+y)%2+x*y%3)%2 == 0
	}
}

func qrApplyMask(modules, isFunc [][]bool, size, mask int) {
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if isFunc[y][x] {
				continue
			}
			if qrMaskInvert(mask, x, y) {
				modules[y][x] = !modules[y][x]
			}
		}
	}
}

func qrPenaltyForLine(line []bool) int {
	const n1, n3 = 3, 40
	result, run := 0, 1
	for i := 1; i < len(line); i++ {
		if line[i] == line[i-1] {
			run++
			continue
		}
		if run >= 5 {
			result += n1 + (run - 5)
		}
		run = 1
	}
	if run >= 5 {
		result += n1 + (run - 5)
	}
	patA := []bool{true, false, true, true, true, false, true, false, false, false, false}
	patB := []bool{false, false, false, false, true, false, true, true, true, false, true}
	for i := 0; i+11 <= len(line); i++ {
		matchA, matchB := true, true
		for j := 0; j < 11; j++ {
			if line[i+j] != patA[j] {
				matchA = false
			}
			if line[i+j] != patB[j] {
				matchB = false
			}
			if !matchA && !matchB {
				break
			}
		}
		if matchA {
			result += n3
		}
		if matchB {
			result += n3
		}
	}
	return result
}

func qrPenalty(modules [][]bool, size int) int {
	const n2, n4 = 3, 10
	result := 0
	for y := 0; y < size; y++ {
		result += qrPenaltyForLine(modules[y])
	}
	col := make([]bool, size)
	for x := 0; x < size; x++ {
		for y := 0; y < size; y++ {
			col[y] = modules[y][x]
		}
		result += qrPenaltyForLine(col)
	}
	for y := 0; y < size-1; y++ {
		for x := 0; x < size-1; x++ {
			c := modules[y][x]
			if c == modules[y][x+1] && c == modules[y+1][x] && c == modules[y+1][x+1] {
				result += n2
			}
		}
	}
	dark := 0
	for _, row := range modules {
		for _, cell := range row {
			if cell {
				dark++
			}
		}
	}
	total := size * size
	dev := dark*20 - total*10
	if dev < 0 {
		dev = -dev
	}
	k := (dev+total-1)/total - 1
	if k > 0 {
		result += k * n4
	}
	return result
}

// buildQRMatrix returns the module matrix for text (byte mode, ECC M).
func buildQRMatrix(text string) ([][]bool, error) {
	data := []byte(text)
	version := qrPickVersion(len(data))
	if version == 0 {
		return nil, fmt.Errorf("raccoon: QR payload too long (%d bytes, max 213)", len(data))
	}
	size := version*4 + 17
	modules := make([][]bool, size)
	isFunc := make([][]bool, size)
	for i := range modules {
		modules[i] = make([]bool, size)
		isFunc[i] = make([]bool, size)
	}
	qrFunctionPatterns(modules, isFunc, size, version)
	codewords, err := qrBuildCodewords(data, version)
	if err != nil {
		return nil, err
	}
	qrDrawCodewords(modules, isFunc, size, codewords)
	bestMask, bestPenalty := 0, -1
	for mask := 0; mask < 8; mask++ {
		qrApplyMask(modules, isFunc, size, mask)
		qrFormatBits(modules, isFunc, size, mask)
		if p := qrPenalty(modules, size); bestPenalty < 0 || p < bestPenalty {
			bestPenalty, bestMask = p, mask
		}
		qrApplyMask(modules, isFunc, size, mask)
	}
	qrApplyMask(modules, isFunc, size, bestMask)
	qrFormatBits(modules, isFunc, size, bestMask)
	return modules, nil
}
