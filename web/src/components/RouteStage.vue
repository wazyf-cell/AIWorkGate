<script setup lang="ts">
// task-53 阶段 2/3：Gateway 实时路由舞台（三 SVG 层 + 订阅循环）。
//
// 数据源：api.gatewayTrace（task-52 契约层，本组件不重复实现 HTTP）。
// 节点标定：props.groups / props.autoGroups（Groups.vue 已有数据源，组件
// 不自己拉路由组）；成员泳道按 trace 事件的 member（"插件/账号"）分列，
// group 字段现语义=逻辑模型，不当路由组 id 用（硬约束 4）。
//
// 订阅循环（硬约束 1/2/5）：
//   首屏 recent(50) 即激活后端 sticky 采集 → 游标 = 响应 last_seq →
//   页面可见时循环 poll(cursor, true)；游标推进用响应 last_seq（空返回
//   也推进）；last_seq < 本地游标 ⇒ 网关重启，清屏后 recent() 全量重放；
//   页面隐藏即停循环（不 abort 在途请求——api 层无 signal 透传，用世代
//   令牌丢弃过期响应；后端 25s 自清，无 goroutine 泄漏）；环形 500 条
//   重放兜底：ingest 按 seq 去重。
//
// 舞台三层（magpie client.js:59-84 结构，性能教训 :1820-1846）：
//   rs-stage-wires 静态连线层 / rs-stage-flows 流动虚线副本层（只给处于
//   live 窗口的线画副本，副本数 = 活跃线数，个位数）/ rs-stage-sky 飞行
//   光点层（上限 12 个，命令式 DOM，不进 Vue 响应式）。性能护栏：
//   ① getPointAtLength 按 path 缓存（WeakMap，一条线一生采样一次）；
//   ② 单 rAF 驱动全部飞行、无飞行即停帧；③ 无限动画仅 hub 一个元素 +
//   活跃线副本（个位数）；④ prefers-reduced-motion 时不生成飞行且 CSS 全灭。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api, type GatewayTraceEvent, type GatewayTracePage } from '../api'

interface StageGroupLike {
  id: string
  display?: string
  members?: string[]
}

interface NodePos {
  id: string
  x: number
  y: number
}

interface Flight {
  pts: [number, number][]
  cum: number[]
  total: number
  el: SVGCircleElement
  back: SVGCircleElement | null
  start: number
  duration: number
}

const props = withDefaults(
  defineProps<{
    /** 已保存路由组（Groups.vue 数据源；只取 members 标定泳道）。 */
    groups?: StageGroupLike[]
    /** 自动分组（快照求解结果；只取 members 标定泳道）。 */
    autoGroups?: StageGroupLike[]
    /** 成员泳道上限，超出部分不显示（事件仍计入状态）。 */
    maxLanes?: number
  }>(),
  { groups: () => [], autoGroups: () => [], maxLanes: 12 },
)

const emit = defineEmits<{
  /** 点击一条事件行，把原始事件交给父级展示详情。 */
  (e: 'select', event: GatewayTraceEvent): void
  /** 后端未接线（501 not_implemented）：调用方应呈现「功能未就绪」。 */
  (e: 'unavailable', message: string): void
  /** 游标重置 / 断线重连等状态说明。 */
  (e: 'status', message: string): void
}>()

/* ── 订阅状态 ── */

const events = ref<GatewayTraceEvent[]>([])
/** poll 游标：永远取响应的 last_seq（硬约束 2）。 */
const cursor = ref(0)
const disabled = ref(false)
const starting = ref(true)
const live = ref(false)
const statusNote = ref('')

const STAGE_EVENT_CAP = 200
const seenSeqs = new Set<number>()

function ingest(list: GatewayTraceEvent[]) {
  for (const ev of list) {
    if (seenSeqs.has(ev.seq)) continue // 环形重放兜底：按 seq 去重
    seenSeqs.add(ev.seq)
    events.value.push(ev)
  }
  events.value.sort((a, b) => a.seq - b.seq)
  if (events.value.length > STAGE_EVENT_CAP) {
    const dropped = events.value.splice(0, events.value.length - STAGE_EVENT_CAP)
    for (const ev of dropped) seenSeqs.delete(ev.seq)
  }
}

/** 事件按 req_id 归并：一个 agent 请求多次尝试共享一条展示行（最新覆盖）。 */
const requestRows = computed<GatewayTraceEvent[]>(() => {
  const byReq = new Map<string, GatewayTraceEvent>()
  for (const ev of events.value) {
    const prev = byReq.get(ev.req_id)
    if (!prev || ev.seq >= prev.seq) byReq.set(ev.req_id, ev)
  }
  return [...byReq.values()].sort((a, b) => b.seq - a.seq)
})

/* ── 线状态（硬约束 3：失败判据 = err_kind 非空，而非 status>=400）── */

/** 最近 5 条事件的 seq：处于窗口内的线 = lit（高亮），其余 rest（降透明）。 */
const recentSeqs = computed(() => {
  const tail = events.value.slice(-5)
  return new Set(tail.map((ev) => ev.seq))
})

function outcomeClass(ev: GatewayTraceEvent | undefined): string {
  if (!ev) return 'rs-stage-rest'
  const kind = ev.err_kind ?? ''
  if (kind === 'canceled') return 'rs-stage-canceled' // 客户端取消：灰色弱化，不是故障
  if (kind) return 'rs-stage-failed' // transport / upstream / request → 红
  return recentSeqs.value.has(ev.seq) ? 'rs-stage-lit' : 'rs-stage-rest'
}

const memberLast = computed(() => {
  const m = new Map<string, GatewayTraceEvent>()
  for (let i = events.value.length - 1; i >= 0; i--) {
    const ev = events.value[i]
    if (ev.member && !m.has(ev.member)) m.set(ev.member, ev)
  }
  return m
})

const agentLast = computed(() => {
  const m = new Map<string, GatewayTraceEvent>()
  for (let i = events.value.length - 1; i >= 0; i--) {
    const ev = events.value[i]
    const key = ev.agent || AGENT_FALLBACK
    if (!m.has(key)) m.set(key, ev)
  }
  return m
})

/* ── 舞台节点：agent 列（左）→ hub → 成员泳道（右） ── */

const AGENT_FALLBACK = '未知 agent'

const agentIds = computed<string[]>(() => {
  const ids = new Set<string>()
  for (const ev of events.value) {
    if (ev.agent) ids.add(ev.agent)
  }
  const list = [...ids]
  return list.length > 0 ? list.slice(0, 8) : [AGENT_FALLBACK]
})

/** 泳道：先 props 标定的已知成员，再补事件里新出现的；超上限截断。 */
const laneIds = computed<{ id: string; label: string; fresh: boolean }[]>(() => {
  const out: { id: string; label: string; fresh: boolean }[] = []
  const seen = new Set<string>()
  const push = (id: string, label: string, fresh: boolean) => {
    const key = id.trim()
    if (!key || seen.has(key)) return
    seen.add(key)
    out.push({ id: key, label, fresh })
  }
  for (const g of props.groups) {
    for (const m of g.members ?? []) push(m, m, false)
  }
  for (const g of props.autoGroups) {
    for (const m of g.members ?? []) push(m, m, false)
  }
  for (const ev of events.value) {
    if (ev.member) push(ev.member, ev.member, true)
  }
  return out.slice(0, props.maxLanes)
})

/** 首次测量时已存在的泳道不算「新发现」；之后新出现的泳道亮 rtfound 扫过。 */
const knownLanes = new Set<string>()
watch(
  laneIds,
  (lanes) => {
    if (!measured.value) {
      for (const lane of lanes) knownLanes.add(lane.id)
    }
  },
  { immediate: true },
)

/* ── 几何：DOM 量测（相对舞台容器）→ S 曲线 ── */

const stageEl = ref<HTMLElement | null>(null)
const agentEls = new Map<string, HTMLElement>()
const laneEls = new Map<string, HTMLElement>()
const wireEls = new Map<string, SVGPathElement>()
const hubEl = ref<HTMLElement | null>(null)
const skyEl = ref<SVGSVGElement | null>(null)
const measured = ref(false)
const stageSize = ref({ w: 0, h: 0 })
const agentPos = ref<NodePos[]>([])
const lanePos = ref<NodePos[]>([])
const hubPos = ref({ x: 0, y: 0 })

function setAgentEl(id: string, el: unknown) {
  if (el instanceof HTMLElement) agentEls.set(id, el)
  else agentEls.delete(id)
}

function setLaneEl(id: string, el: unknown) {
  if (el instanceof HTMLElement) laneEls.set(id, el)
  else laneEls.delete(id)
}

function setWireEl(key: string, el: unknown) {
  if (el instanceof SVGPathElement) wireEls.set(key, el)
  else wireEls.delete(key)
}

function centerOf(el: HTMLElement, stageRect: DOMRect): { x: number; y: number; right: number; left: number } {
  const r = el.getBoundingClientRect()
  return {
    x: r.left + r.width / 2 - stageRect.left,
    y: r.top + r.height / 2 - stageRect.top,
    right: r.right - stageRect.left,
    left: r.left - stageRect.left,
  }
}

/** 量测合帧：列表变化与 resize 都汇入一次 rAF（性能护栏：合帧）。 */
let measureQueued = false
function queueMeasure() {
  if (measureQueued) return
  measureQueued = true
  requestAnimationFrame(() => {
    measureQueued = false
    measure()
  })
}

function measure() {
  const stage = stageEl.value
  if (!stage) return
  const stageRect = stage.getBoundingClientRect()
  stageSize.value = { w: stageRect.width, h: stageRect.height }
  const hub = hubEl.value ? centerOf(hubEl.value, stageRect) : { x: stageRect.width / 2, y: stageRect.height / 2, right: 0, left: 0 }
  hubPos.value = { x: hub.x, y: hub.y }
  agentPos.value = agentIds.value.flatMap((id) => {
    const el = agentEls.get(id)
    if (!el) return []
    const c = centerOf(el, stageRect)
    return [{ id, x: c.right, y: c.y }]
  })
  lanePos.value = laneIds.value.flatMap((lane) => {
    const el = laneEls.get(lane.id)
    if (!el) return []
    const c = centerOf(el, stageRect)
    return [{ id: lane.id, x: c.left, y: c.y }]
  })
  measured.value = true
}

let resizeObserver: ResizeObserver | null = null
watch([agentIds, laneIds], () => {
  void nextTick(queueMeasure)
})

/** S 型贝塞尔（magpie client.js:1816 curve 的同形简化）。 */
function curve(x0: number, y0: number, x1: number, y1: number): string {
  const mx = (x0 + x1) / 2
  return `M${x0} ${y0} C${mx} ${y0} ${mx} ${y1} ${x1} ${y1}`
}

const agentWires = computed(() =>
  agentPos.value.map((a) => ({
    id: a.id,
    d: curve(a.x, a.y, hubPos.value.x, hubPos.value.y),
    cls: outcomeClass(agentLast.value.get(a.id)),
  })),
)
const laneWires = computed(() =>
  lanePos.value.map((l) => ({
    id: l.id,
    d: curve(hubPos.value.x, hubPos.value.y, l.x, l.y),
    cls: outcomeClass(memberLast.value.get(l.id)),
  })),
)

/** 流动虚线副本：只画「最近有成功流量」的线（硬约束下副本数 = 活跃线数，个位数）。 */
const flowCopies = computed(() => {
  const out: { key: string; d: string }[] = []
  for (const w of agentWires.value) {
    if (w.cls === 'rs-stage-lit') out.push({ key: `f-a-${w.id}`, d: w.d })
  }
  for (const w of laneWires.value) {
    if (w.cls === 'rs-stage-lit') out.push({ key: `f-l-${w.id}`, d: w.d })
  }
  return out
})

/* ── getPointAtLength 缓存（性能护栏：一条线只采样一次，8px 步长）── */

const pathSamples = new WeakMap<SVGPathElement, { len: number; pts: [number, number][] }>()

function samplePath(path: SVGPathElement): { len: number; pts: [number, number][] } {
  const cached = pathSamples.get(path)
  if (cached) return cached
  const len = path.getTotalLength()
  const pts: [number, number][] = []
  const step = 8
  for (let d = 0; d <= len; d += step) {
    const p = path.getPointAtLength(d)
    pts.push([p.x, p.y])
  }
  const end = path.getPointAtLength(len)
  pts.push([end.x, end.y])
  const entry = { len, pts }
  pathSamples.set(path, entry)
  return entry
}

/* ── 飞行光点（rs-stage-sky）：请求（前）+ 响应（回）双色，命令式 DOM ── */

const prefersReduced =
  typeof window !== 'undefined' &&
  typeof window.matchMedia === 'function' &&
  window.matchMedia('(prefers-reduced-motion: reduce)').matches

const FLIGHT_CAP = 12
const FLIGHT_MS = 900
const flights: Flight[] = []
let flightRaf = 0

function posAt(f: Flight, t: number): [number, number] {
  const d = Math.max(0, Math.min(1, t)) * f.total
  let lo = 0
  let hi = f.cum.length - 1
  while (lo < hi) {
    const mid = (lo + hi) >> 1
    if (f.cum[mid] < d) lo = mid + 1
    else hi = mid
  }
  const i = Math.max(1, lo)
  const seg = f.cum[i] - f.cum[i - 1] || 1
  const k = (d - f.cum[i - 1]) / seg
  const [x0, y0] = f.pts[i - 1]
  const [x1, y1] = f.pts[i]
  return [x0 + (x1 - x0) * k, y0 + (y1 - y0) * k]
}

function removeFlight(f: Flight) {
  f.el.remove()
  f.back?.remove()
}

function flightFrame(now: number) {
  for (let i = flights.length - 1; i >= 0; i--) {
    const f = flights[i]
    const t = (now - f.start) / f.duration
    if (t >= 1) {
      removeFlight(f)
      flights.splice(i, 1)
      continue
    }
    const [fx, fy] = posAt(f, t)
    f.el.setAttribute('cx', String(fx))
    f.el.setAttribute('cy', String(fy))
    // 回程光点在前程 55% 后出发、反向走完剩余 45%。
    if (f.back) {
      const bt = t <= 0.55 ? -1 : (t - 0.55) / 0.45
      if (bt < 0) f.back.setAttribute('display', 'none')
      else {
        f.back.removeAttribute('display')
        const [bx, by] = posAt(f, 1 - bt)
        f.back.setAttribute('cx', String(bx))
        f.back.setAttribute('cy', String(by))
      }
    }
  }
  if (flights.length > 0) flightRaf = requestAnimationFrame(flightFrame)
  else flightRaf = 0
}

function ensureFlightLoop() {
  if (!flightRaf) flightRaf = requestAnimationFrame(flightFrame)
}

/** 一次 live 事件 = 一对光点：agent→hub→member（请求），随后反向（响应）。 */
function spawnFlight(ev: GatewayTraceEvent) {
  const sky = skyEl.value
  if (!sky || prefersReduced) return
  const aw = wireEls.get(`a:${ev.agent || AGENT_FALLBACK}`)
  const lw = ev.member ? wireEls.get(`l:${ev.member}`) : null
  if (!aw || !lw) return
  if (flights.length >= FLIGHT_CAP) {
    removeFlight(flights[0])
    flights.shift()
  }
  const fwd = [...samplePath(aw).pts, ...samplePath(lw).pts]
  const pts: [number, number][] = fwd.map(([x, y]) => [x, y])
  const cum: number[] = [0]
  for (let i = 1; i < pts.length; i++) {
    const dx = pts[i][0] - pts[i - 1][0]
    const dy = pts[i][1] - pts[i - 1][1]
    cum.push(cum[i - 1] + Math.hypot(dx, dy))
  }
  const el = document.createElementNS('http://www.w3.org/2000/svg', 'circle')
  el.setAttribute('r', '3.5')
  el.setAttribute('class', 'rs-stage-dot')
  const kind = ev.err_kind ?? ''
  const back = document.createElementNS('http://www.w3.org/2000/svg', 'circle')
  back.setAttribute('r', '3')
  back.setAttribute('class', kind === 'canceled' ? 'rs-stage-dot rs-stage-dot-muted' : kind ? 'rs-stage-dot rs-stage-dot-fail' : 'rs-stage-dot rs-stage-dot-back')
  sky.append(el, back)
  const flight: Flight = { pts, cum, total: cum[cum.length - 1] || 1, el, back, start: performance.now(), duration: FLIGHT_MS }
  flights.push(flight)
  ensureFlightLoop()
}

/* ── 订阅循环控制：页面可见才轮询；世代令牌丢弃过期响应 ── */

let generation = 0
let backoffMs = 1000
const BACKOFF_MAX_MS = 15000

function applyPage(page: GatewayTracePage, animate: boolean) {
  // 硬约束 2：游标用响应 last_seq；回退 ⇒ 网关重启，全量重放（不播动画）。
  if (page.last_seq < cursor.value) {
    emit('status', '检测到网关重启，已重放最近事件。')
    statusNote.value = '检测到网关重启，已重放最近事件。'
    events.value = []
    seenSeqs.clear()
  }
  ingest(page.events ?? [])
  cursor.value = page.last_seq
  if (animate) {
    for (const ev of page.events ?? []) spawnFlight(ev)
  }
}

async function initialLoad() {
  starting.value = true
  try {
    // 硬约束 1：recent(50) 一经调用即激活 sticky 采集，之后循环 poll。
    const page = await api.gatewayTrace.recent(50)
    applyPage(page, false) // 首屏回放不播飞行（一次性最多 50 条，不刷屏）
    backoffMs = 1000
  } catch (err) {
    handleTraceError(err)
  } finally {
    starting.value = false
  }
}

function handleTraceError(err: unknown) {
  const status = (err as { status?: number }).status ?? 0
  if (status === 501) {
    // 未接线：这是「功能未就绪」，不能吞成空页——停循环并让父级呈现。
    disabled.value = true
    live.value = false
    generation += 1
    emit('unavailable', '网关 trace 接口未就绪（501）：需要重启 AIWorkGate 让新后端生效。')
    return
  }
  emit('status', `trace 连接失败（HTTP ${status}），${Math.round(backoffMs / 1000)}s 后重试。`)
}

async function pollLoop(myGeneration: number) {
  while (live.value && generation === myGeneration && !disabled.value) {
    try {
      const page = await api.gatewayTrace.poll(cursor.value, true)
      if (generation !== myGeneration) return // 停止/重启期间返回的过期响应，丢弃
      applyPage(page, true)
      backoffMs = 1000
    } catch (err) {
      if (generation !== myGeneration) return
      handleTraceError(err)
      if (disabled.value) return
      // 退避重试：挂起中的 poll 出错后等一会儿再开下一轮。
      await new Promise((resolve) => setTimeout(resolve, backoffMs))
      backoffMs = Math.min(backoffMs * 2, BACKOFF_MAX_MS)
    }
  }
}

function startLoop() {
  if (live.value || disabled.value || starting.value) return
  live.value = true
  void pollLoop(generation)
}

function stopLoop() {
  // 不 abort 在途 fetch（api 层无 signal 透传）：世代令牌使其结果被丢弃，
  // 后端长轮询 25s 自行超时清理（硬约束 5：abort 安全，不必高频 abort）。
  live.value = false
  generation += 1
}

function onVisibility() {
  if (document.hidden) stopLoop()
  else startLoop()
}

onMounted(async () => {
  document.addEventListener('visibilitychange', onVisibility)
  await initialLoad()
  await nextTick()
  measure()
  if (!document.hidden) startLoop()
  if (typeof ResizeObserver !== 'undefined' && stageEl.value) {
    resizeObserver = new ResizeObserver(queueMeasure)
    resizeObserver.observe(stageEl.value)
  }
})

onBeforeUnmount(() => {
  document.removeEventListener('visibilitychange', onVisibility)
  stopLoop()
  resizeObserver?.disconnect()
  resizeObserver = null
  if (flightRaf) cancelAnimationFrame(flightRaf)
  for (const f of flights) removeFlight(f)
  flights.length = 0
})

defineExpose({
  /** 供测试/父级探针：当前游标、事件数与泳道 id。 */
  cursor,
  eventCount: computed(() => events.value.length),
  lanes: computed(() => laneIds.value.map((lane) => lane.id)),
})
</script>

<template>
  <section class="card rs-stage-card">
    <div class="card-head">
      <h2>
        <span v-if="live" class="rs-dot" aria-hidden="true"></span>
        路由实时舞台
        <span class="badge plain" :class="live ? 'ok' : 'muted'">{{ live ? '实时' : '已暂停' }}</span>
      </h2>
      <span v-if="statusNote" class="sub">{{ statusNote }}</span>
    </div>
    <div class="card-body stack">
      <span class="small muted">
        这是什么：网关每次真实转发的观察流——请求从哪个 agent 进来、落在哪个「插件/账号」成员上、成功还是失败。页面切到后台会自动暂停轮询。
      </span>

      <div v-if="starting" class="empty">连接网关 trace 中…</div>
      <div v-else-if="disabled" class="empty">
        <strong>trace 接口未就绪</strong>
        <span>重启 AIWorkGate 后刷新本页。</span>
      </div>

      <!-- 三层舞台：agent（左）→ hub → 成员泳道（右）。
           三张 SVG 绝对定位铺满舞台；HTML 节点行浮在上层供量测与点击。 -->
      <div v-show="!starting && !disabled" ref="stageEl" class="rs-stage">
        <svg class="rs-stage-wires" :width="stageSize.w" :height="stageSize.h" aria-hidden="true">
          <path
            v-for="w in agentWires"
            :key="`a-${w.id}`"
            :ref="(el) => setWireEl(`a:${w.id}`, el)"
            :d="w.d"
            :class="w.cls"
          />
          <path
            v-for="w in laneWires"
            :key="`l-${w.id}`"
            :ref="(el) => setWireEl(`l:${w.id}`, el)"
            :d="w.d"
            :class="w.cls"
          />
        </svg>
        <!-- 流动虚线副本层：只承载 live 线的副本（个位数），与静态层分离，
             避免动画期间整层重绘（magpie client.js:1844-1846 的教训）。 -->
        <svg class="rs-stage-flows" :width="stageSize.w" :height="stageSize.h" aria-hidden="true">
          <path v-for="f in flowCopies" :key="f.key" :d="f.d" />
        </svg>
        <svg ref="skyEl" class="rs-stage-sky" :width="stageSize.w" :height="stageSize.h" aria-hidden="true"></svg>

        <div class="rs-stage-col rs-stage-col-left">
          <div
            v-for="agent in agentIds"
            :key="agent"
            :ref="(el) => setAgentEl(agent, el)"
            class="rs-stage-node rs-stage-agent"
            :class="{ 'rs-stage-lit-node': outcomeClass(agentLast.get(agent)) === 'rs-stage-lit' }"
          >
            <span class="rs-stage-node-name mono">{{ agent }}</span>
          </div>
        </div>

        <div
          ref="hubEl"
          class="rs-stage-hub"
          :class="{ 'rs-stage-hub-live': live }"
        >
          <span class="rs-stage-hub-name">AIWorkGate</span>
          <span class="small muted">网关</span>
        </div>

        <div class="rs-stage-col rs-stage-col-right">
          <div
            v-for="lane in laneIds"
            :key="lane.id"
            :ref="(el) => setLaneEl(lane.id, el)"
            class="rs-stage-node rs-stage-lane"
            :class="{
              'rs-found': lane.fresh && !knownLanes.has(lane.id),
              'rs-stage-lit-node': outcomeClass(memberLast.get(lane.id)) === 'rs-stage-lit',
              'rs-stage-node-failed': outcomeClass(memberLast.get(lane.id)) === 'rs-stage-failed',
            }"
          >
            <span class="rs-stage-node-name mono" :title="lane.id">{{ lane.label }}</span>
          </div>
          <div v-if="laneIds.length === 0" class="small muted">还没有已知的成员泳道。</div>
        </div>
      </div>

      <!-- 观察流列表：点击交给父级（select 事件）。 -->
      <div v-if="!starting && !disabled && requestRows.length === 0" class="empty">
        <strong>还没有请求事件</strong>
        <span>发一条真实请求（任意 agent 指向本网关），事件会实时出现在这里。</span>
      </div>
      <div v-else-if="!starting && !disabled" class="rs-stage-list">
        <button
          v-for="row in requestRows.slice(0, 30)"
          :key="row.req_id + row.seq"
          type="button"
          class="rule-item"
          @click="emit('select', row)"
        >
          <span class="rule-item-head">
            <strong class="mono">{{ row.agent || '未知 agent' }}</strong>
            <span class="badge plain" :class="row.err_kind === 'canceled' ? 'muted' : row.err_kind ? 'warn' : 'ok'">
              {{ row.err_kind ? row.err_kind : `HTTP ${row.status}` }}
            </span>
          </span>
          <span class="small muted mono">{{ row.member || '—' }} · {{ row.logical_model || row.group || '—' }} · {{ row.latency_ms }} ms</span>
        </button>
      </div>
    </div>
  </section>
</template>

<style scoped>
.rs-stage-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
  max-height: 420px;
  overflow: auto;
}
</style>
