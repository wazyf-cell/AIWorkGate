<script setup lang="ts">
// task-63：六维上下文 Treemap（一期）。纯 CSS squarified——手写 Bruls 行打包
// 算法（约 40 行，无图表库），tile 几何经 :style 绑定输出百分比（style-debt
// rule 4 明确放行 data-driven geometry），色值走 var(--chart-*) class。
//
// 数据：task-62 端点的 sections（六桶固定序，est 本地口径 / allocated 上游
// 分摊口径——有 allocated 用它渲染，与 usage 口径对齐）+ items 条级明细（后端
// 已按 tokens 降序）。点类别块展开类内 Top-8；悬浮 title 给类内 Top 5。
import { computed, ref } from 'vue'
import type { GatewayCtxItem, GatewayCtxSection } from '../api'
import AppIcon from './AppIcon.vue'

const props = defineProps<{
  sections: GatewayCtxSection[]
  items?: GatewayCtxItem[]
}>()

const BUCKET_LABELS: Record<string, string> = {
  system: '系统提示词',
  tools: '工具定义',
  tool_results: '工具结果',
  conversation: '会话内容',
  memory: '记忆',
  other: '其它',
}

/** 六维专属图标名（task-68 修复 3，AppIcon.vue 提供，无图标库依赖）。 */
const BUCKET_ICONS: Record<string, string> = {
  system: 'ctx-system',
  tools: 'ctx-tools',
  tool_results: 'ctx-tool-results',
  conversation: 'ctx-conversation',
  memory: 'ctx-memory',
  other: 'ctx-other',
}

function iconOf(key: string): string {
  return BUCKET_ICONS[key] ?? 'ctx-other'
}

const TOP_TITLE = 5
/** tile 短边低于该百分比时不画文字（色块靠 title 表达）。 */
const TILE_TEXT_MIN = 16

const expanded = ref('')

/** 双口径视图（task-68 修复 2）：allocated = 上游分摊，est = 本地估算。 */
const viewMode = ref<'allocated' | 'est'>('allocated')

/** 渲染口径：allocated 视图下无上游口径的 section 回落 est。 */
function tileValue(section: GatewayCtxSection): number {
  if (viewMode.value === 'allocated') return section.allocated ?? section.est
  return section.est
}

/** item 无独立 allocated 字段——按所在 section 的分摊比例推导（后端
 * section 级 allocated 本就是比例分摊，同法推导到条级；est 视图直接用
 * item.tokens）。无上游口径（section.allocated 缺省）时回落 est。 */
function itemValue(item: GatewayCtxItem): number {
  if (viewMode.value === 'est') return item.tokens
  const section = sectionOf(item.bucket)
  if (!section || section.allocated === undefined || !section.est) return item.tokens
  return Math.round(item.tokens * (section.allocated / section.est))
}

/** item 的上游分摊推导值：按所在 section 的 allocated/est 比例放大；
 * section 无上游口径时返回 undefined（调用方回落 est）。 */
function itemAllocated(item: GatewayCtxItem): number | undefined {
  const section = sectionOf(item.bucket)
  if (!section || section.allocated === undefined || !section.est) return undefined
  return Math.round(item.tokens * (section.allocated / section.est))
}

/** 悬浮同显两值：est 与 allocated（缺省标「无上游口径」）。 */
function itemBothText(item: GatewayCtxItem): string {
  const allocated = itemAllocated(item)
  return allocated === undefined
    ? `本地估算 ${fmtCompact(item.tokens)}（无上游口径）`
    : `本地估算 ${fmtCompact(item.tokens)} · 上游分摊 ${fmtCompact(allocated)}`
}

interface CtxTile {
  key: string
  value: number
  x: number
  y: number
  w: number
  h: number
}

/** 逐行最坏长宽比（Bruls et al. squarified 的判据）。 */
function worstRatio(row: { area: number }[], side: number): number {
  const sum = row.reduce((s, e) => s + e.area, 0)
  const thickness = sum / side
  let worst = 0
  for (const entry of row) {
    const length = entry.area / thickness
    worst = Math.max(worst, thickness / length, length / thickness)
  }
  return worst
}

/** squarified 行打包：值域映射进 100×100 百分比坐标，输出每块的矩形。 */
function squarify(entries: { key: string; value: number }[], width: number, height: number): CtxTile[] {
  const total = entries.reduce((s, e) => s + e.value, 0)
  if (total <= 0 || width <= 0 || height <= 0) return []
  const scale = (width * height) / total
  let pending = entries
    .map((e) => ({ key: e.key, area: e.value * scale }))
    .filter((e) => e.area > 0)
    .sort((a, b) => b.area - a.area)
  const rects: CtxTile[] = []
  let x = 0
  let y = 0
  let w = width
  let h = height
  while (pending.length > 0) {
    const row = [pending[0]]
    let rest = pending.slice(1)
    const side = Math.min(w, h)
    while (rest.length > 0) {
      const candidate = [...row, rest[0]]
      if (worstRatio(candidate, side) <= worstRatio(row, side)) {
        row.push(rest[0])
        rest = rest.slice(1)
      } else break
    }
    const sum = row.reduce((s, e) => s + e.area, 0)
    const thickness = sum / side
    if (w >= h) {
      let yy = y
      for (const entry of row) {
        const eh = entry.area / thickness
        rects.push({ key: entry.key, value: entry.area, x, y: yy, w: thickness, h: eh })
        yy += eh
      }
      x += thickness
      w -= thickness
    } else {
      let xx = x
      for (const entry of row) {
        const ew = entry.area / thickness
        rects.push({ key: entry.key, value: entry.area, x: xx, y, w: ew, h: thickness })
        xx += ew
      }
      y += thickness
      h -= thickness
    }
    pending = rest
  }
  return rects
}

const tiles = computed<CtxTile[]>(() =>
  squarify(
    props.sections.map((section) => ({ key: section.key, value: tileValue(section) })),
    100,
    100,
  ),
)

function sectionOf(key: string): GatewayCtxSection | undefined {
  return props.sections.find((section) => section.key === key)
}

function labelOf(key: string): string {
  return BUCKET_LABELS[key] ?? key
}

/** token 数紧凑格式（块内与明细共用；<1000 原样，其余 K）。 */
function fmtCompact(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '0'
  if (value >= 1000) return `${(value / 1000).toFixed(1)}K`
  return String(Math.round(value))
}

function topItems(bucket: string, limit: number): GatewayCtxItem[] {
  return (props.items ?? []).filter((item) => item.bucket === bucket).slice(0, limit)
}

function tileTitle(key: string): string {
  const section = sectionOf(key)
  if (!section) return labelOf(key)
  const lines = [`类内 Top 项（${labelOf(key)}）· 本地估算 ${fmtCompact(section.est)}${section.allocated !== undefined ? ` · 上游分摊 ${fmtCompact(section.allocated)}` : ' · 无上游口径'}`]
  for (const item of topItems(key, TOP_TITLE)) {
    lines.push(`${item.label}：${itemBothText(item)}`)
  }
  if (lines.length === 1) lines.push('（无条级明细）')
  return lines.join('\n')
}

function toggle(key: string) {
  expanded.value = expanded.value === key ? '' : key
}

const detailBucket = computed<string>(() => expanded.value)
/** 修复 1（二期）：钻取展示该类**全部**条级明细（一期只 Top-8），
 * 每行带当前口径 tokens 与类内占比（分母 = 该类已加载明细的 est 合计）。 */
const detailItems = computed<GatewayCtxItem[]>(() =>
  detailBucket.value ? (props.items ?? []).filter((item) => item.bucket === detailBucket.value) : [],
)

const detailShareBase = computed<number>(() =>
  detailItems.value.reduce((sum, item) => sum + item.tokens, 0),
)

function itemShare(item: GatewayCtxItem): string {
  const base = detailShareBase.value
  if (base <= 0) return '—'
  return `${((item.tokens / base) * 100).toFixed(1)}%`
}
</script>

<template>
  <div class="ctx-treemap">
    <div class="row between">
      <span class="field-label">上下文构成（六维）</span>
      <!-- 双口径切换（task-68 修复 2） -->
      <div class="seg">
        <button
          type="button"
          class="seg-btn"
          :class="{ active: viewMode === 'allocated' }"
          @click="viewMode = 'allocated'"
        >上游分摊</button>
        <button
          type="button"
          class="seg-btn"
          :class="{ active: viewMode === 'est' }"
          @click="viewMode = 'est'"
        >本地估算</button>
      </div>
    </div>
    <div class="ctx-treemap-canvas">
      <div
        v-for="tile in tiles"
        :key="tile.key"
        class="ctx-tile"
        :class="`ctx-tile--${tile.key} ${expanded === tile.key ? 'ctx-tile--active' : ''}`"
        :style="{ left: `${tile.x}%`, top: `${tile.y}%`, width: `${tile.w}%`, height: `${tile.h}%` }"
        :title="tileTitle(tile.key)"
        role="button"
        tabindex="0"
        @click="toggle(tile.key)"
        @keydown.enter="toggle(tile.key)"
      >
        <span v-if="tile.w >= TILE_TEXT_MIN" class="ctx-tile-name">{{ labelOf(tile.key) }}</span>
        <span v-if="tile.h >= TILE_TEXT_MIN + 6" class="ctx-tile-tokens">
          {{ fmtCompact(tileValue(sectionOf(tile.key) ?? { key: tile.key, est: 0 })) }}
        </span>
      </div>
    </div>
    <div v-if="detailBucket" class="ctx-treemap-detail">
      <span class="field-label">
        {{ labelOf(detailBucket) }} · 条级明细（{{ detailItems.length }} 条 · 全量）
      </span>
      <div v-if="detailItems.length === 0" class="small muted">该类没有条级明细。</div>
      <div v-for="item in detailItems" :key="item.label" class="ctx-item-row" :title="itemBothText(item)">
        <span class="ctx-item-label mono" :title="item.label">{{ item.label }}</span>
        <span class="ctx-item-tokens">
          {{ fmtCompact(itemValue(item)) }} · {{ itemShare(item) }}
        </span>
      </div>
    </div>
    <div class="ctx-treemap-legend">
      <span v-for="section in sections" :key="section.key" class="ctx-legend-item">
        <AppIcon :name="iconOf(section.key)" :size="12" />
        <span class="ctx-legend-swatch" :class="`ctx-legend--${section.key}`"></span>
        {{ labelOf(section.key) }} {{ fmtCompact(tileValue(section)) }}
      </span>
    </div>
  </div>
</template>

<style scoped>
.ctx-treemap {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

/* 画布：百分比坐标的 absolute 矩形（squarify 输出 0-100）。 */
.ctx-treemap-canvas {
  position: relative;
  width: 100%;
  aspect-ratio: 3 / 2;
  min-height: 180px;
}

.ctx-tile {
  position: absolute;
  display: flex;
  flex-direction: column;
  justify-content: center;
  align-items: flex-start;
  gap: 2px;
  padding: 4px 6px;
  overflow: hidden;
  border: 1px solid var(--surface);
  border-radius: var(--radius-xs);
  cursor: pointer;
}

.ctx-tile:focus-visible {
  outline: 2px solid var(--accent);
}

.ctx-tile--active {
  outline: 2px solid var(--accent);
}

/* 六类色：项目图表色板变量 + color-mix 浅色填充（styles.css 先例同款，
 * 色值不硬编码，rule 1）；other 用中性面。 */
.ctx-tile--system {
  background: color-mix(in srgb, var(--chart-1) 18%, var(--surface));
}

.ctx-tile--tools {
  background: color-mix(in srgb, var(--chart-2) 18%, var(--surface));
}

.ctx-tile--tool_results {
  background: color-mix(in srgb, var(--chart-3) 18%, var(--surface));
}

.ctx-tile--conversation {
  background: color-mix(in srgb, var(--chart-4) 18%, var(--surface));
}

.ctx-tile--memory {
  background: color-mix(in srgb, var(--chart-5) 18%, var(--surface));
}

.ctx-tile--other {
  background: var(--surface-2);
}

.ctx-tile-name {
  font-size: var(--text-2xs);
  font-weight: 600;
}

.ctx-tile-tokens {
  font-size: var(--text-2xs);
  color: var(--muted);
}

.ctx-treemap-detail {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 8px 10px;
  background: var(--surface-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
}

.ctx-item-row {
  display: flex;
  justify-content: space-between;
  gap: 12px;
}

.ctx-item-label {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.ctx-item-tokens {
  flex: none;
  font-size: var(--text-xs);
  color: var(--muted);
}

.ctx-treemap-legend {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 14px;
}

.ctx-legend-item {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: var(--text-2xs);
  color: var(--muted);
}

.ctx-legend-swatch {
  width: 10px;
  height: 10px;
  border-radius: 3px;
  border: 1px solid var(--border);
}

.ctx-legend--system {
  background: color-mix(in srgb, var(--chart-1) 18%, var(--surface));
}

.ctx-legend--tools {
  background: color-mix(in srgb, var(--chart-2) 18%, var(--surface));
}

.ctx-legend--tool_results {
  background: color-mix(in srgb, var(--chart-3) 18%, var(--surface));
}

.ctx-legend--conversation {
  background: color-mix(in srgb, var(--chart-4) 18%, var(--surface));
}

.ctx-legend--memory {
  background: color-mix(in srgb, var(--chart-5) 18%, var(--surface));
}

.ctx-legend--other {
  background: var(--surface-2);
}
</style>
