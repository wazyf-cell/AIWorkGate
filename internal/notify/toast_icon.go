package notify

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
)

// pngSignature is the 8-byte PNG header.
var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// pngFromICO converts the single 32bpp DIB image stored in an ICO into PNG
// bytes. Windows toast logos only accept PNG/JPEG, so the tray icon (an ICO) is
// re-encoded once at first use rather than shipped as a second asset.
//
// ICO files may also embed a PNG payload directly; that case is returned as-is.
func pngFromICO(ico []byte) ([]byte, error) {
	const dirSize, entrySize = 6, 16
	if len(ico) < dirSize || binary.LittleEndian.Uint16(ico[0:]) != 0 || binary.LittleEndian.Uint16(ico[2:]) != 1 {
		return nil, errors.New("notify: 图标格式无效")
	}
	if binary.LittleEndian.Uint16(ico[4:]) < 1 || len(ico) < dirSize+entrySize {
		return nil, errors.New("notify: 图标不包含图像")
	}
	entry := ico[dirSize : dirSize+entrySize]
	payloadOffset := int(binary.LittleEndian.Uint32(entry[12:]))
	payloadSize := int(binary.LittleEndian.Uint32(entry[8:]))
	if payloadOffset < 0 || payloadSize < 0 || payloadOffset+payloadSize > len(ico) {
		return nil, errors.New("notify: 图标数据越界")
	}
	payload := ico[payloadOffset : payloadOffset+payloadSize]
	if bytes.HasPrefix(payload, pngSignature) {
		return append([]byte(nil), payload...), nil
	}
	return pngFromDIB(payload)
}

// pngFromDIB decodes the BITMAPINFOHEADER + bottom-up BGRA pixels of one ICO
// image. The AND mask is ignored: every pixel of a 32bpp icon carries alpha.
func pngFromDIB(payload []byte) ([]byte, error) {
	const headerMin = 40
	if len(payload) < headerMin {
		return nil, errors.New("notify: 图标数据不完整")
	}
	headerSize := int(binary.LittleEndian.Uint32(payload[0:]))
	width := int(int32(binary.LittleEndian.Uint32(payload[4:])))
	xorHeight := int(int32(binary.LittleEndian.Uint32(payload[8:])))
	bpp := int(binary.LittleEndian.Uint16(payload[14:]))
	compression := binary.LittleEndian.Uint32(payload[16:])
	// ICO stores the XOR image plus the AND mask, so biHeight is doubled.
	height := xorHeight / 2
	if headerSize < headerMin || headerSize > len(payload) || width <= 0 || height <= 0 ||
		width > 1024 || height > 1024 || bpp != 32 || compression != 0 {
		return nil, errors.New("notify: 不支持的图标编码")
	}
	stride := width * 4
	if headerSize+stride*height > len(payload) {
		return nil, errors.New("notify: 图标像素数据不完整")
	}

	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for row := 0; row < height; row++ {
		pixels := payload[headerSize+(height-1-row)*stride:]
		for x := 0; x < width; x++ {
			b, g, r, a := pixels[4*x], pixels[4*x+1], pixels[4*x+2], pixels[4*x+3]
			i := img.PixOffset(x, row)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = r, g, b, a
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
