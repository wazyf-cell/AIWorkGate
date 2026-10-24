// Style-debt gate: keeps the token contract from drifting back.
//
// Rules (view/component code only; src/styles.css owns the raw values):
//   1. no hex colors
//   2. no inline style carrying color / font-size / spacing
//   3. no raw px font-size inside a scoped <style> block; use var(--text-*)
//   4. data-driven geometry (bar height, swatch color) is allowed only through
//      :style bindings, which rule 2 deliberately skips.
//
// Run: npm run check:style (also part of npm run build).

import { readdirSync, readFileSync } from 'node:fs'
import path from 'node:path'

const roots = ['src/views', 'src/components']
const HEX = /#[0-9a-fA-F]{3,8}\b/g
// Hyphenated longhands must be listed: \bpadding\s*: never matches
// padding-bottom:, and a gate that misses 8 of the real violations is worse
// than no gate, because it reports green.
const PROPS =
  '(?:color|background|background-color|font-size|padding(?:-(?:top|right|bottom|left))?|margin(?:-(?:top|right|bottom|left))?|gap|row-gap|column-gap|border(?:-(?:color|width|top|right|bottom|left))?|border-radius)'
const INLINE = new RegExp(`(?<![:\\w])style="[^"]*\\b${PROPS}\\s*:`, 'g')
const RAW_PX = /font-size:\s*\d*\.?\d+px/g

function files(dir) {
  return readdirSync(dir, { withFileTypes: true })
    .filter((e) => e.isFile() && e.name.endsWith('.vue'))
    .map((e) => path.join(dir, e.name))
}

const problems = []

for (const root of roots) {
  for (const file of files(root)) {
    const lines = readFileSync(file, 'utf8').split(/\r?\n/)
    lines.forEach((line, i) => {
      const at = `${file}:${i + 1}`
      const hex = line.match(HEX)
      if (hex && !/var\(--[\w-]+,\s*#/.test(line)) problems.push(`${at} 硬编码色值 ${hex.join(' ')}`)
      if (INLINE.test(line)) problems.push(`${at} 内联样式承载颜色/间距/字号`)
      if (RAW_PX.test(line)) problems.push(`${at} 未用字阶 token 的 font-size`)
      INLINE.lastIndex = 0
      RAW_PX.lastIndex = 0
    })
  }
}

if (problems.length) {
  console.error(`check-style-debt: ${problems.length} 处设计债\n` + problems.slice(0, 40).join('\n'))
  if (problems.length > 40) console.error(`…另有 ${problems.length - 40} 处`)
  process.exit(1)
}
console.log('check-style-debt: 0 处，视图层无硬编码色值/内联样式/裸字号')
