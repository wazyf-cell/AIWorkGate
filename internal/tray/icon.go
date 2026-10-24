//go:build windows

package tray

import (
	"encoding/binary"
	"math"
	"sync"
)

// The tray icon is generated in code (no asset files): a 16x16 32-bit ICO with
// a teal ring and a light centre dot, visible on both light and dark taskbars.
const (
	iconSize        = 16
	iconDirSize     = 6
	iconEntrySize   = 16
	iconHeaderSize  = 40
	iconXORBytes    = iconSize * iconSize * 4
	iconANDBytes    = ((iconSize + 31) / 32) * 4 * iconSize
	iconImageBytes  = iconHeaderSize + iconXORBytes + iconANDBytes
	iconImageOffset = iconDirSize + iconEntrySize
	iconTotalBytes  = iconImageOffset + iconImageBytes
)

var (
	iconOnce sync.Once
	iconData []byte
)

// IconBytes returns a fresh copy of the generated tray icon.
func IconBytes() []byte {
	iconOnce.Do(func() { iconData = buildIcon() })
	out := make([]byte, len(iconData))
	copy(out, iconData)
	return out
}

// buildIcon assembles an ICO holding one uncompressed 32bpp DIB. The DIB is
// stored bottom-up, as BMP/ICO require, and the 1bpp AND mask is left zero
// because every pixel carries an alpha channel.
func buildIcon() []byte {
	buf := make([]byte, 0, iconTotalBytes)

	// ICONDIR
	buf = binary.LittleEndian.AppendUint16(buf, 0) // reserved
	buf = binary.LittleEndian.AppendUint16(buf, 1) // type: icon
	buf = binary.LittleEndian.AppendUint16(buf, 1) // image count

	// ICONDIRENTRY
	buf = append(buf, iconSize, iconSize, 0, 0)     // width, height, palette, reserved
	buf = binary.LittleEndian.AppendUint16(buf, 1)  // planes
	buf = binary.LittleEndian.AppendUint16(buf, 32) // bits per pixel
	buf = binary.LittleEndian.AppendUint32(buf, iconImageBytes)
	buf = binary.LittleEndian.AppendUint32(buf, iconImageOffset)

	// BITMAPINFOHEADER: height is doubled to cover the XOR image plus AND mask.
	buf = binary.LittleEndian.AppendUint32(buf, iconHeaderSize)
	buf = binary.LittleEndian.AppendUint32(buf, iconSize)
	buf = binary.LittleEndian.AppendUint32(buf, iconSize*2)
	buf = binary.LittleEndian.AppendUint16(buf, 1)  // planes
	buf = binary.LittleEndian.AppendUint16(buf, 32) // bpp
	buf = binary.LittleEndian.AppendUint32(buf, 0)  // BI_RGB
	buf = binary.LittleEndian.AppendUint32(buf, iconXORBytes+iconANDBytes)
	buf = binary.LittleEndian.AppendUint32(buf, 0) // x pixels per metre
	buf = binary.LittleEndian.AppendUint32(buf, 0) // y pixels per metre
	buf = binary.LittleEndian.AppendUint32(buf, 0) // colours used
	buf = binary.LittleEndian.AppendUint32(buf, 0) // important colours

	// XOR image, bottom-up, straight (non-premultiplied) alpha, BGRA order.
	for y := iconSize - 1; y >= 0; y-- {
		for x := 0; x < iconSize; x++ {
			r, g, b, a := iconPixel(x, y)
			buf = append(buf, b, g, r, a)
		}
	}

	// AND mask: all zero (fully opaque through the mask, alpha decides).
	buf = append(buf, make([]byte, iconANDBytes)...)
	return buf
}

// iconPixel returns the antialiased colour of one icon pixel.
func iconPixel(x, y int) (r, g, b, a uint8) {
	const (
		centreX, centreY = 7.5, 7.5
		ringRadius       = 5.5
		ringHalfWidth    = 1.2
		dotRadius        = 1.7
	)
	d := math.Hypot(float64(x)-centreX, float64(y)-centreY)

	ring := clamp01(ringHalfWidth + 0.5 - math.Abs(d-ringRadius))
	dot := clamp01(dotRadius + 0.5 - d)

	if dot >= ring {
		return 0xF2, 0xFF, 0xFD, alphaByte(dot)
	}
	return 0x2F, 0xC0, 0xAE, alphaByte(ring)
}

func alphaByte(coverage float64) uint8 {
	return uint8(math.Round(clamp01(coverage) * 255))
}

func clamp01(v float64) float64 {
	switch {
	case v <= 0:
		return 0
	case v >= 1:
		return 1
	default:
		return v
	}
}
