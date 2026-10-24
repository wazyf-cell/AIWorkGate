/**
 * Minimal PNG codec: decode to RGBA, encode from RGBA. Zero dependencies.
 *
 * Why hand-rolled: the visual regression gate must not add an npm dependency or
 * a browser download (the host has Chrome/Edge already). Node's built-in zlib
 * covers the compression, so the only real work left is IHDR parsing, the five
 * scanline filters, and the chunk CRCs.
 *
 * Scope is deliberately narrow — this decodes and writes what Chrome's
 * Page.captureScreenshot produces (8-bit, non-interlaced, RGB or RGBA) and the
 * diff images this tool writes. Palette, 16-bit, and interlaced PNGs are
 * rejected loudly instead of being silently mis-decoded.
 */

import { deflateSync, inflateSync } from 'node:zlib'

const SIGNATURE = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])

/** CRC-32 (ISO 3309), as required by every PNG chunk. */
const CRC_TABLE = (() => {
  const table = new Uint32Array(256)
  for (let n = 0; n < 256; n++) {
    let c = n
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
    table[n] = c >>> 0
  }
  return table
})()

function crc32(buf) {
  let c = 0xffffffff
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8)
  return (c ^ 0xffffffff) >>> 0
}

function paeth(a, b, c) {
  const p = a + b - c
  const pa = Math.abs(p - a)
  const pb = Math.abs(p - b)
  const pc = Math.abs(p - c)
  if (pa <= pb && pa <= pc) return a
  if (pb <= pc) return b
  return c
}

/**
 * Decodes a PNG buffer into { width, height, data } where data is RGBA
 * (4 bytes per pixel, row-major, no padding). Throws on formats it does not
 * implement rather than returning a wrong image.
 */
export function decodePng(buffer) {
  if (buffer.length < 8 || !buffer.subarray(0, 8).equals(SIGNATURE)) {
    throw new Error('not a PNG: bad signature')
  }
  let offset = 8
  let width = 0
  let height = 0
  let bitDepth = 0
  let colorType = 0
  let interlace = 0
  const idat = []

  while (offset + 8 <= buffer.length) {
    const length = buffer.readUInt32BE(offset)
    const type = buffer.toString('ascii', offset + 4, offset + 8)
    const body = buffer.subarray(offset + 8, offset + 8 + length)
    if (body.length !== length) throw new Error(`truncated PNG chunk ${type}`)
    if (type === 'IHDR') {
      width = body.readUInt32BE(0)
      height = body.readUInt32BE(4)
      bitDepth = body[8]
      colorType = body[9]
      interlace = body[12]
    } else if (type === 'IDAT') {
      idat.push(body)
    } else if (type === 'IEND') {
      break
    }
    offset += 12 + length
  }

  if (bitDepth !== 8) throw new Error(`unsupported PNG bit depth ${bitDepth}`)
  if (interlace !== 0) throw new Error('unsupported interlaced PNG')
  // 0 = grey, 2 = RGB, 4 = grey+alpha, 6 = RGBA
  const channels = { 0: 1, 2: 3, 4: 2, 6: 4 }[colorType]
  if (!channels) throw new Error(`unsupported PNG color type ${colorType}`)

  const raw = inflateSync(Buffer.concat(idat))
  const stride = width * channels
  const expected = height * (stride + 1)
  if (raw.length < expected) throw new Error(`PNG data short: ${raw.length} < ${expected}`)

  // Un-filter in place: each scanline is a filter byte followed by `stride`
  // bytes of filtered data, and each filter predicts from the pixel to its left
  // (a), above (b), and above-left (c) in the *already reconstructed* output.
  const pixels = Buffer.alloc(height * stride)
  for (let y = 0; y < height; y++) {
    const filter = raw[y * (stride + 1)]
    const src = y * (stride + 1) + 1
    const dst = y * stride
    const prev = dst - stride
    for (let x = 0; x < stride; x++) {
      const value = raw[src + x]
      const a = x >= channels ? pixels[dst + x - channels] : 0
      const b = y > 0 ? pixels[prev + x] : 0
      const c = y > 0 && x >= channels ? pixels[prev + x - channels] : 0
      let out
      switch (filter) {
        case 0: out = value; break
        case 1: out = value + a; break
        case 2: out = value + b; break
        case 3: out = value + ((a + b) >> 1); break
        case 4: out = value + paeth(a, b, c); break
        default: throw new Error(`unknown PNG filter ${filter} on row ${y}`)
      }
      pixels[dst + x] = out & 0xff
    }
  }

  // Everything downstream (diffing, diff images) works in RGBA, so expand the
  // narrower colour types once here instead of branching per pixel later.
  if (channels === 4) {
    return { width, height, data: pixels }
  }
  const rgba = Buffer.alloc(width * height * 4)
  for (let i = 0, j = 0; i < width * height; i++, j += channels) {
    if (channels === 3) {
      rgba[i * 4] = pixels[j]
      rgba[i * 4 + 1] = pixels[j + 1]
      rgba[i * 4 + 2] = pixels[j + 2]
      rgba[i * 4 + 3] = 255
    } else if (channels === 1) {
      rgba[i * 4] = rgba[i * 4 + 1] = rgba[i * 4 + 2] = pixels[j]
      rgba[i * 4 + 3] = 255
    } else {
      rgba[i * 4] = rgba[i * 4 + 1] = rgba[i * 4 + 2] = pixels[j]
      rgba[i * 4 + 3] = pixels[j + 1]
    }
  }
  return { width, height, data: rgba }
}

function chunk(type, body) {
  const head = Buffer.alloc(4)
  head.writeUInt32BE(body.length, 0)
  const typeBuf = Buffer.from(type, 'ascii')
  const crcBuf = Buffer.alloc(4)
  crcBuf.writeUInt32BE(crc32(Buffer.concat([typeBuf, body])), 0)
  return Buffer.concat([head, typeBuf, body, crcBuf])
}

/**
 * Encodes RGBA pixels as an 8-bit RGBA PNG using filter type 0 on every row.
 * Diffs are mostly large flat black areas, so the extra ratio a smarter filter
 * would buy is not worth the code.
 */
export function encodePng(width, height, rgba) {
  const stride = width * 4
  const raw = Buffer.alloc(height * (stride + 1))
  for (let y = 0; y < height; y++) {
    raw[y * (stride + 1)] = 0
    rgba.copy(raw, y * (stride + 1) + 1, y * stride, y * stride + stride)
  }
  const ihdr = Buffer.alloc(13)
  ihdr.writeUInt32BE(width, 0)
  ihdr.writeUInt32BE(height, 4)
  ihdr[8] = 8 // bit depth
  ihdr[9] = 6 // colour type: RGBA
  ihdr[10] = 0 // deflate
  ihdr[11] = 0 // adaptive filtering
  ihdr[12] = 0 // no interlace
  return Buffer.concat([
    SIGNATURE,
    chunk('IHDR', ihdr),
    chunk('IDAT', deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ])
}
