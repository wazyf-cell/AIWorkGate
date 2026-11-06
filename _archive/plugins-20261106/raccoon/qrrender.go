// Package main — SVG rendering of a QR matrix (reference renderQrSvg).
package main

import (
	"fmt"
	"strings"
)

// renderQRSVG renders text as an inline SVG QR image.
func renderQRSVG(text string, px, margin int) (string, error) {
	modules, err := buildQRMatrix(text)
	if err != nil {
		return "", err
	}
	size := len(modules)
	dim := size + margin*2
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img">`, px, px, dim, dim)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/><path d="`, dim, dim)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if modules[y][x] {
				fmt.Fprintf(&b, "M%d,%dh1v1h-1z", x+margin, y+margin)
			}
		}
	}
	b.WriteString(`" fill="#000000"/></svg>`)
	return b.String(), nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
