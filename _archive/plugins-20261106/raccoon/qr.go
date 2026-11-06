// Package main — QR encoder for the Raccoon scan-code login.
//
// Ported line for line from the reference's raccoon-qr.ts (byte mode, error
// correction M, versions 1..10, all eight masks with the standard penalty
// score). It exists because the host UI can only hand the user a link: the
// scan-code payload itself is a URL, so the plugin renders it as a QR on a
// loopback page instead of printing 32 hex characters nobody can scan.
//
// The matrices are pinned against the reference implementation in
// qr_test.go, so a porting mistake is a test failure rather than a QR that
// silently scans to garbage.
package main

import "fmt"

// qrDataCodewords is DATA_CODEWORDS (total data codewords per version).
var qrDataCodewords = [11]int{0, 16, 28, 44, 64, 86, 108, 124, 154, 182, 216}

// qrBlock is one EC_BLOCKS_M row: ecPerBlock plus [count, dataPerBlock] groups.
type qrBlock struct {
	ecPerBlock int
	groups     [][2]int
}

var qrECBlocksM = [11]*qrBlock{
	nil,
	{10, [][2]int{{1, 16}}},
	{16, [][2]int{{1, 28}}},
	{26, [][2]int{{1, 44}}},
	{18, [][2]int{{2, 32}}},
	{24, [][2]int{{2, 43}}},
	{16, [][2]int{{4, 27}}},
	{18, [][2]int{{4, 31}}},
	{22, [][2]int{{2, 38}, {2, 39}}},
	{22, [][2]int{{3, 36}, {2, 37}}},
	{26, [][2]int{{4, 43}, {1, 44}}},
}

var qrGFExp [512]byte
var qrGFLog [256]byte

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		qrGFExp[i] = byte(x)
		qrGFLog[x] = byte(i)
		x <<= 1
		if x&256 != 0 {
			x ^= 285
		}
	}
	for i := 255; i < 512; i++ {
		qrGFExp[i] = qrGFExp[i-255]
	}
}

func gfMul(a, b int) int {
	if a == 0 || b == 0 {
		return 0
	}
	return int(qrGFExp[(int(qrGFLog[a])+int(qrGFLog[b]))%255])
}

func polyMul(a, b []int) []int {
	out := make([]int, len(a)+len(b)-1)
	for i := range a {
		for j := range b {
			out[i+j] ^= gfMul(a[i], b[j])
		}
	}
	return out
}

func rsGeneratorPoly(degree int) []int {
	poly := []int{1}
	for i := 0; i < degree; i++ {
		poly = polyMul(poly, []int{1, int(qrGFExp[i])})
	}
	return poly
}

func rsEncode(data []int, ecCount int) []int {
	gen := rsGeneratorPoly(ecCount)
	buf := make([]int, len(data)+ecCount)
	copy(buf, data)
	for i := 0; i < len(data); i++ {
		coef := buf[i]
		if coef == 0 {
			continue
		}
		for j := range gen {
			buf[i+j] ^= gfMul(gen[j], coef)
		}
	}
	return buf[len(data):]
}

func qrPickVersion(byteLen int) int {
	for version := 1; version <= 10; version++ {
		capacityBits := qrDataCodewords[version] * 8
		overhead := 4
		if version <= 9 {
			overhead = 12
		} else {
			overhead = 20
		}
		if overhead+byteLen*8 <= capacityBits {
			return version
		}
	}
	return 0
}

func qrBuildCodewords(data []byte, version int) ([]int, error) {
	blocks := qrECBlocksM[version]
	if blocks == nil {
		return nil, fmt.Errorf("raccoon: unsupported QR version %d", version)
	}
	capacityBits := qrDataCodewords[version] * 8
	var bits []int
	pushBits := func(value, length int) {
		for i := length - 1; i >= 0; i-- {
			bits = append(bits, value>>i&1)
		}
	}
	pushBits(4, 4) // byte mode
	if version <= 9 {
		pushBits(len(data), 8)
	} else {
		pushBits(len(data), 16)
	}
	for _, b := range data {
		pushBits(int(b), 8)
	}
	terminator := 4
	if remain := capacityBits - len(bits); remain < terminator {
		terminator = remain
	}
	pushBits(0, terminator)
	for len(bits)%8 != 0 {
		bits = append(bits, 0)
	}
	for i := 0; len(bits) < capacityBits; i++ {
		pad := 236
		if i%2 == 1 {
			pad = 17
		}
		pushBits(pad, 8)
	}
	dataCodewords := make([]int, 0, len(bits)/8)
	for i := 0; i < len(bits); i += 8 {
		byteVal := 0
		for j := 0; j < 8; j++ {
			byteVal = byteVal<<1 | bits[i+j]
		}
		dataCodewords = append(dataCodewords, byteVal)
	}
	var dataBlocks, ecBlocks [][]int
	offset := 0
	for _, g := range blocks.groups {
		for b := 0; b < g[0]; b++ {
			block := dataCodewords[offset : offset+g[1]]
			offset += g[1]
			dataBlocks = append(dataBlocks, block)
			ecBlocks = append(ecBlocks, rsEncode(block, blocks.ecPerBlock))
		}
	}
	var out []int
	maxLen := 0
	for _, b := range dataBlocks {
		if len(b) > maxLen {
			maxLen = len(b)
		}
	}
	for i := 0; i < maxLen; i++ {
		for _, b := range dataBlocks {
			if i < len(b) {
				out = append(out, b[i])
			}
		}
	}
	for i := 0; i < blocks.ecPerBlock; i++ {
		for _, b := range ecBlocks {
			out = append(out, b[i])
		}
	}
	return out, nil
}
