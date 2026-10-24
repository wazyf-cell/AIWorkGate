<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import {
  api,
  errorMessage,
  normalizeAccounts,
  type Account,
  type AccountGroup,
  type PluginStatus,
  type UsageAccountCredit,
  type UsageFilterParams,
  type UsageKeyRow,
  type UsageKpis,
  type UsageLatencyDay,
  type UsageModelLatencyRow,
  type UsageModelRow,
  type UsageSeriesPoint,
  type UsageSession,
  type UsageSessionsParams,
  type UsageSummary,
} from '../api'
import { dateISO, formatDate, formatLatency, formatNumber, formatTimestamp, toDate } from '../format'

type Preset = 'today' | '7d' | '30d' | 'custom'
type Tab = 'credits' | 'token' | 'latency' | 'keys' | 'models' | 'expiry' | 'sessions'
type Dimension = 'plugin' | 'group' | 'account'
type TokenSeries = 'in' | 'out'

interface FilterOption {
  value: string
  label: string
  hint?: string
}

interface ToastItem {
  id: number
  text: string
  tone: 'ok' | 'error'
}

interface PluginTotal {
  plugin: string
  requests: number
  prompt: number
  completion: number
  credits: number
}

interface ChartSegment {
  key: string
  label: string
  value: number
  color: string
  plugin: string
}

interface ChartDay {
  day: string
  label: string
  total: number
  segments: ChartSegment[]
  /** Daily request count, drawn as the line over the token bars. */
  requests?: number
}

const PAGE_SIZE = 50
const DIMENSIONS: Dimension[] = ['plugin', 'group', 'account']
const MENU_LABELS: Record<Dimension, string> = { plugin: '插件', group: '分组', account: '账号' }
const ALL_LABELS: Record<Dimension, string> = { plugin: '全部插件', group: '全部分组', account: '全部账号' }
const PRESETS: { key: Preset; label: string }[] = [
  { key: 'today', label: '今天' },
  { key: '7d', label: '7天' },
  { key: '30d', label: '30天' },
  { key: 'custom', label: '自定义' },
]
const TABS: { key: Tab; label: string }[] = [
  { key: 'credits', label: '积分' },
  { key: 'token', label: 'Token' },
  { key: 'latency', label: '延迟分位' },
  { key: 'keys', label: '按 Key' },
  { key: 'models', label: '模型' },
  { key: 'expiry', label: '到期' },
  { key: 'sessions', label: '会话明细' },
]

/** Windows offered by the two observability blocks (API accepts 1–90). */
const OBS_DAY_OPTIONS = [7, 14, 30, 90]

/* ------------------------------------------------------------------ */
/* Filters                                                             */
/* ------------------------------------------------------------------ */

const today = new Date()
const preset = ref<Preset>('7d')
const from = ref(dateISO(new Date(today.getFullYear(), today.getMonth(), today.getDate() - 6)))
const to = ref(dateISO(today))
const tab = ref<Tab>('credits')

const selected = reactive<Record<Dimension, string[]>>({ plugin: [], group: [], account: [] })
const openMenu = ref<Dimension | ''>('')

const plugins = ref<PluginStatus[]>([])
const groups = ref<AccountGroup[]>([])
const accounts = ref<Account[]>([])

const pluginOptions = computed<FilterOption[]>(() =>
  plugins.value
    .map((plugin) => ({
      value: plugin.manifest?.id ?? '',
      label: plugin.manifest?.name || plugin.manifest?.id || '—',
    }))
    .filter((option) => option.value !== ''),
)

const groupOptions = computed<FilterOption[]>(() =>
  groups.value.map((group) => ({
    value: group.id,
    label: group.name || group.id,
    hint: `${group.account_count ?? 0} 个账号`,
  })),
)

const accountOptions = computed<FilterOption[]>(() =>
  accounts.value.flatMap((account) => {
    const pluginId = account.plugin_id ?? ''
    if (!pluginId) return []
    return [
      {
        value: `${pluginId}:${account.id}`,
        label: account.display_name || account.alias || account.label || account.id,
        hint: pluginId,
      },
    ]
  }),
)

function optionsFor(dimension: Dimension): FilterOption[] {
  if (dimension === 'plugin') return pluginOptions.value
  if (dimension === 'group') return groupOptions.value
  return accountOptions.value
}

function toggleOption(dimension: Dimension, value: string) {
  const list = selected[dimension]
  const index = list.indexOf(value)
  if (index >= 0) list.splice(index, 1)
  else list.push(value)
}

function clearDimension(dimension: Dimension) {
  selected[dimension].splice(0)
}

const filterParams = computed<UsageFilterParams>(() => ({
  from: from.value || undefined,
  to: to.value || undefined,
  plugin: selected.plugin.join(',') || undefined,
  group: selected.group.join(',') || undefined,
  account: selected.account.join(',') || undefined,
}))

const filterKey = computed(() => JSON.stringify(filterParams.value))
const exportUrl = computed(() => api.usage.exportUrl(filterParams.value))

function applyPreset(next: Preset) {
  preset.value = next
  openMenu.value = ''
  if (next === 'custom') return
  const base = new Date()
  const days = next === 'today' ? 1 : next === '7d' ? 7 : 30
  from.value = dateISO(new Date(base.getFullYear(), base.getMonth(), base.getDate() - (days - 1)))
  to.value = dateISO(base)
}

function resetFilters() {
  const hadDrill = drill.value !== null
  selected.plugin.splice(0)
  selected.group.splice(0)
  selected.account.splice(0)
  drill.value = null
  applyPreset('7d')
  if (hadDrill && tab.value === 'sessions') void loadSessions()
}

/* ------------------------------------------------------------------ */
/* Data                                                                */
/* ------------------------------------------------------------------ */

const summary = ref<UsageSummary>({})
const summaryLoaded = ref(false)
const credits = ref<UsageAccountCredit[]>([])
const creditsLoaded = ref(false)
const expiry = ref<UsageAccountCredit[]>([])
const expiryLoaded = ref(false)
const sessions = ref<UsageSession[]>([])
const sessionTotal = ref(0)
const sessionPage = ref(1)
const sessionsLoaded = ref(false)
// Observability blocks read raw api_logs / client_key_usage, so they carry
// their own day windows instead of the from/to filter above.
const latencyDays = ref(14)
const keyDays = ref(30)
// The model tab ranks consumption and, when api_logs has samples, joins the
// per-model percentiles over the same window.
const modelDays = ref(30)
const latencyRows = ref<UsageLatencyDay[]>([])
const keyRows = ref<UsageKeyRow[]>([])
const modelRows = ref<UsageModelRow[]>([])
const modelLatencyRows = ref<UsageModelLatencyRow[]>([])
const latencyLoaded = ref(false)
const keysLoaded = ref(false)
const modelsLoaded = ref(false)
const drill = ref<{ day: string; plugin: string } | null>(null)

const loading = reactive({
  summary: true,
  credits: false,
  expiry: false,
  sessions: false,
  latency: false,
  keys: false,
  models: false,
})
const error = ref('')

let summarySeq = 0
let creditsSeq = 0
let expirySeq = 0
let sessionsSeq = 0
let latencySeq = 0
let keysSeq = 0
let modelsSeq = 0

const sessionParams = computed<UsageSessionsParams>(() => {
  const active = drill.value
  return {
    from: active?.day ?? filterParams.value.from,
    to: active?.day ?? filterParams.value.to,
    plugin: active?.plugin || filterParams.value.plugin,
    group: filterParams.value.group,
    account: filterParams.value.account,
    page: sessionPage.value,
    page_size: PAGE_SIZE,
  }
})

async function loadSummary() {
  const token = ++summarySeq
  loading.summary = true
  summaryLoaded.value = false
  try {
    const data = await api.usage.summary(filterParams.value)
    if (token !== summarySeq) return
    summary.value = data ?? {}
    summaryLoaded.value = true
    error.value = ''
  } catch (e) {
    if (token !== summarySeq) return
    summary.value = {}
    error.value = errorMessage(e)
  } finally {
    if (token === summarySeq) loading.summary = false
  }
}

async function loadCredits() {
  const token = ++creditsSeq
  loading.credits = true
  try {
    const data = await api.usage.credits(filterParams.value)
    if (token !== creditsSeq) return
    credits.value = data?.accounts ?? []
    creditsLoaded.value = true
    error.value = ''
  } catch (e) {
    if (token !== creditsSeq) return
    credits.value = []
    error.value = errorMessage(e)
  } finally {
    if (token === creditsSeq) loading.credits = false
  }
}

async function loadExpiry() {
  const token = ++expirySeq
  loading.expiry = true
  try {
    const data = await api.usage.expiry(filterParams.value)
    if (token !== expirySeq) return
    expiry.value = data?.accounts ?? []
    expiryLoaded.value = true
    error.value = ''
  } catch (e) {
    if (token !== expirySeq) return
    expiry.value = []
    error.value = errorMessage(e)
  } finally {
    if (token === expirySeq) loading.expiry = false
  }
}

async function loadSessions() {
  const token = ++sessionsSeq
  loading.sessions = true
  try {
    const data = await api.usage.sessions(sessionParams.value)
    if (token !== sessionsSeq) return
    sessions.value = data?.items ?? []
    sessionTotal.value = data?.total ?? sessions.value.length
    sessionsLoaded.value = true
    error.value = ''
  } catch (e) {
    if (token !== sessionsSeq) return
    sessions.value = []
    sessionTotal.value = 0
    error.value = errorMessage(e)
  } finally {
    if (token === sessionsSeq) loading.sessions = false
  }
}

async function loadLatency() {
  const token = ++latencySeq
  loading.latency = true
  try {
    const data = await api.usage.latency(latencyDays.value)
    if (token !== latencySeq) return
    latencyRows.value = data ?? []
    latencyLoaded.value = true
    error.value = ''
  } catch (e) {
    if (token !== latencySeq) return
    latencyRows.value = []
    error.value = errorMessage(e)
  } finally {
    if (token === latencySeq) loading.latency = false
  }
}

async function loadKeys() {
  const token = ++keysSeq
  loading.keys = true
  try {
    const data = await api.usage.keys(keyDays.value)
    if (token !== keysSeq) return
    keyRows.value = data ?? []
    keysLoaded.value = true
    error.value = ''
  } catch (e) {
    if (token !== keysSeq) return
    keyRows.value = []
    error.value = errorMessage(e)
  } finally {
    if (token === keysSeq) loading.keys = false
  }
}

function setLatencyDays(days: number) {
  if (latencyDays.value === days) return
  latencyDays.value = days
  latencyLoaded.value = false
  void loadLatency()
}

async function loadModels() {
  const token = ++modelsSeq
  loading.models = true
  try {
    // The percentile block is optional: an older host answers 404/501 and the
    // ranking still renders without the latency columns.
    const [usage, latency] = await Promise.all([
      api.usage.models(modelDays.value),
      api.usage.modelLatency(modelDays.value).catch(() => [] as UsageModelLatencyRow[]),
    ])
    if (token !== modelsSeq) return
    modelRows.value = usage ?? []
    modelLatencyRows.value = latency ?? []
    modelsLoaded.value = true
    error.value = ''
  } catch (e) {
    if (token !== modelsSeq) return
    modelRows.value = []
    modelLatencyRows.value = []
    error.value = errorMessage(e)
  } finally {
    if (token === modelsSeq) loading.models = false
  }
}

function setModelDays(days: number) {
  if (modelDays.value === days) return
  modelDays.value = days
  modelsLoaded.value = false
  void loadModels()
}

function setKeyDays(days: number) {
  if (keyDays.value === days) return
  keyDays.value = days
  keysLoaded.value = false
  void loadKeys()
}

function loadActiveTab() {
  if (tab.value === 'credits') return loadCredits()
  if (tab.value === 'expiry') return loadExpiry()
  if (tab.value === 'sessions') return loadSessions()
  if (tab.value === 'latency') return loadLatency()
  if (tab.value === 'keys') return loadKeys()
  if (tab.value === 'models') return loadModels()
  return Promise.resolve()
}

async function refresh() {
  await Promise.all([loadSummary(), loadActiveTab()])
}

function retry() {
  void refresh()
}

function setTab(next: Tab) {
  tab.value = next
  openMenu.value = ''
  if (next === 'credits' && !creditsLoaded.value) void loadCredits()
  else if (next === 'expiry' && !expiryLoaded.value) void loadExpiry()
  else if (next === 'sessions' && !sessionsLoaded.value) void loadSessions()
  else if (next === 'latency' && !latencyLoaded.value) void loadLatency()
  else if (next === 'keys' && !keysLoaded.value) void loadKeys()
  else if (next === 'models' && !modelsLoaded.value) void loadModels()
}

watch(filterKey, () => {
  drill.value = null
  sessionPage.value = 1
  creditsLoaded.value = false
  expiryLoaded.value = false
  sessionsLoaded.value = false
  sessions.value = []
  sessionTotal.value = 0
  void refresh()
})

/* ------------------------------------------------------------------ */
/* Chart data                                                          */
/* ------------------------------------------------------------------ */

const series = computed<UsageSeriesPoint[]>(() => summary.value.series ?? [])
const kpis = computed<UsageKpis>(() => summary.value.kpis ?? {})
const days = computed(() => Array.from(new Set(series.value.map((point) => point.day))).sort())

const pluginIds = computed(() =>
  Array.from(new Set(series.value.map((point) => point.plugin_id || 'unknown'))),
)

const pluginNames = computed<Record<string, string>>(() => {
  const map: Record<string, string> = {}
  for (const plugin of plugins.value) {
    const id = plugin.manifest?.id
    if (id) map[id] = plugin.manifest?.name || id
  }
  return map
})

function pluginLabel(id: string): string {
  if (!id || id === 'unknown') return '未上报插件'
  if (id === 'a') return 'A'
  const raw = id.startsWith('plugin:') ? id.slice(7) : id
  return pluginNames.value[raw] || raw
}

function pluginColor(id: string): string {
  const index = pluginIds.value.indexOf(id)
  return `var(--chart-${(Math.max(0, index) % 8) + 1})`
}

const pluginTotals = computed<PluginTotal[]>(() => {
  const map = new Map<string, PluginTotal>()
  for (const point of series.value) {
    const id = point.plugin_id || 'unknown'
    const row =
      map.get(id) ?? ({ plugin: id, requests: 0, prompt: 0, completion: 0, credits: 0 } as PluginTotal)
    row.requests += point.requests ?? 0
    row.prompt += point.prompt_tokens ?? 0
    row.completion += point.completion_tokens ?? 0
    row.credits += point.credits ?? 0
    map.set(id, row)
  }
  return Array.from(map.values()).sort((a, b) => b.requests - a.requests)
})

const hasCredits = computed(
  () => (kpis.value.credits ?? 0) > 0 || series.value.some((point) => (point.credits ?? 0) > 0),
)

/** KPI credits: prefer the summary value, fall back to summing the daily series. */
const creditTotal = computed(() => {
  if (kpis.value.credits !== undefined) return kpis.value.credits
  return series.value.reduce((sum, point) => sum + (point.credits ?? 0), 0)
})

const hiddenPlugins = ref<Set<string>>(new Set())

function togglePlugin(id: string) {
  const next = new Set(hiddenPlugins.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  hiddenPlugins.value = next
}

const creditChart = computed<ChartDay[]>(() => {
  const byDay = new Map<string, ChartDay>()
  for (const day of days.value) byDay.set(day, { day, label: day.slice(5), total: 0, segments: [] })
  for (const point of series.value) {
    const plugin = point.plugin_id || 'unknown'
    if (hiddenPlugins.value.has(plugin)) continue
    const value = point.credits ?? 0
    if (value <= 0) continue
    const entry = byDay.get(point.day)
    if (!entry) continue
    entry.segments.push({
      key: `${point.day}:${plugin}`,
      label: pluginLabel(plugin),
      value,
      color: pluginColor(plugin),
      plugin,
    })
    entry.total += value
  }
  for (const entry of byDay.values()) {
    entry.segments.sort((a, b) => pluginIds.value.indexOf(a.plugin) - pluginIds.value.indexOf(b.plugin))
  }
  return Array.from(byDay.values())
})

const creditMax = computed(() => Math.max(0, ...creditChart.value.map((day) => day.total)))
const creditEmpty = computed(() => creditChart.value.every((day) => day.segments.length === 0))

const tokenDays = computed(() =>
  days.value.map((day) => {
    let input = 0
    let output = 0
    let requests = 0
    for (const point of series.value) {
      if (point.day !== day) continue
      input += point.prompt_tokens ?? 0
      output += point.completion_tokens ?? 0
      requests += point.requests ?? 0
    }
    return { day, input, output, requests }
  }),
)

const tokenTotals = computed(() => {
  let input = 0
  let output = 0
  for (const point of series.value) {
    input += point.prompt_tokens ?? 0
    output += point.completion_tokens ?? 0
  }
  return { input, output, total: input + output }
})

const hiddenTokenSeries = ref<Set<TokenSeries>>(new Set())

function toggleTokenSeries(seriesName: TokenSeries) {
  const next = new Set(hiddenTokenSeries.value)
  if (next.has(seriesName)) next.delete(seriesName)
  else next.add(seriesName)
  hiddenTokenSeries.value = next
}

const tokenChart = computed<ChartDay[]>(() =>
  tokenDays.value.map((day) => {
    const segments: ChartSegment[] = []
    if (!hiddenTokenSeries.value.has('in') && day.input > 0) {
      segments.push({
        key: `${day.day}:in`,
        label: '输入',
        value: day.input,
        color: 'var(--chart-1)',
        plugin: '',
      })
    }
    if (!hiddenTokenSeries.value.has('out') && day.output > 0) {
      segments.push({
        key: `${day.day}:out`,
        label: '输出',
        value: day.output,
        color: 'var(--chart-2)',
        plugin: '',
      })
    }
    return {
      day: day.day,
      label: day.day.slice(5),
      total: segments.reduce((sum, segment) => sum + segment.value, 0),
      segments,
      requests: day.requests,
    }
  }),
)

const tokenMax = computed(() => Math.max(0, ...tokenChart.value.map((day) => day.total)))
const tokenEmpty = computed(() => tokenChart.value.every((day) => day.segments.length === 0))

/* Daily call counts are a second measure with its own scale: a few hundred
 * calls and a few million tokens would make one of them a flat line if they
 * shared an axis. The bars keep the left axis, the line gets the right one. */

const CHART_HEIGHT = 180
const CHART_GAP = 4
const CHART_TOP_PAD = 6
/** Mirrors `.chart-col { max-width }` in styles.css: the columns are centred
 * once they hit this width, and the line follows the same geometry. */
const CHART_COLUMN_MAX = 180

const callsMax = computed(() => Math.max(0, ...tokenDays.value.map((day) => day.requests)))

const callsAxis = computed(() => {
  const max = callsMax.value
  return [max, Math.round(max / 2), 0]
})

/** Width of the bar area in px, measured so the line lands on bar centres. */
const plotWidth = ref(0)
const plotEl = ref<HTMLElement | null>(null)
let plotObserver: ResizeObserver | null = null

watch(plotEl, (el) => {
  plotObserver?.disconnect()
  plotObserver = null
  if (!el || typeof ResizeObserver === 'undefined') return
  plotObserver = new ResizeObserver((entries) => {
    const width = entries[0]?.contentRect.width ?? 0
    plotWidth.value = Math.round(width)
  })
  plotObserver.observe(el)
  plotWidth.value = Math.round(el.clientWidth)
})

onBeforeUnmount(() => {
  plotObserver?.disconnect()
  plotObserver = null
})

const callLine = computed(() => {
  const count = tokenDays.value.length
  if (count === 0 || plotWidth.value <= 0 || callsMax.value <= 0) return null
  const columnWidth = Math.min(
    CHART_COLUMN_MAX,
    Math.max(6, (plotWidth.value - CHART_GAP * (count - 1)) / count),
  )
  const totalWidth = columnWidth * count + CHART_GAP * (count - 1)
  const offset = Math.max(0, (plotWidth.value - totalWidth) / 2)
  const scale = (CHART_HEIGHT - CHART_TOP_PAD) / callsMax.value
  const dots = tokenDays.value.map((day, index) => ({
    x: offset + index * (columnWidth + CHART_GAP) + columnWidth / 2,
    y: CHART_HEIGHT - day.requests * scale,
    value: day.requests,
    day: day.day,
  }))
  return {
    points: dots.map((dot) => `${dot.x.toFixed(1)},${dot.y.toFixed(1)}`).join(' '),
    dots,
  }
})

function segmentHeight(value: number, max: number): string {
  if (value <= 0 || max <= 0) return '0%'
  return `${Math.max(0.6, (value / max) * 100)}%`
}

/* ------------------------------------------------------------------ */
/* Formatting helpers                                                  */
/* ------------------------------------------------------------------ */

function compact(value?: number | null): string {
  if (value === undefined || value === null || Number.isNaN(value)) return '—'
  const abs = Math.abs(value)
  if (abs >= 1_000_000) return `${(value / 1_000_000).toFixed(abs >= 10_000_000 ? 0 : 1)}M`
  if (abs >= 10_000) return `${Math.round(value / 1_000)}K`
  return value.toLocaleString('zh-CN', { maximumFractionDigits: 2 })
}

function exact(value?: number | null): string {
  return formatNumber(value)
}

function statusTone(status?: number): string {
  if (status === undefined || status === 0) return 'muted'
  if (status >= 500) return 'danger'
  if (status >= 400) return 'warn'
  if (status >= 300) return 'muted'
  return 'ok'
}

function expiryDays(row: UsageAccountCredit): number | null {
  if (typeof row.days_left === 'number' && Number.isFinite(row.days_left)) return row.days_left
  const date = toDate(row.expires_at)
  if (!date) return null
  return Math.ceil((date.getTime() - Date.now()) / 86_400_000)
}

function expiryTone(row: UsageAccountCredit): 'past' | 'soon' | 'unknown' | '' {
  const daysLeft = expiryDays(row)
  if (daysLeft === null) return 'unknown'
  if (daysLeft < 0) return 'past'
  if (daysLeft <= 7) return 'soon'
  return ''
}

const hasModelLatency = computed(() => modelLatencyRows.value.length > 0)

/** Percentiles join the ranking on an exact model id, a missing row is "—". */
const modelLatencyByModel = computed(() => {
  const map = new Map<string, UsageModelLatencyRow>()
  for (const row of modelLatencyRows.value) map.set(row.model, row)
  return map
})

function modelLatencyOf(model: string): UsageModelLatencyRow | undefined {
  return modelLatencyByModel.value.get(model)
}

/* ------------------------------------------------------------------ */
/* Drill-down + detail panel                                           */
/* ------------------------------------------------------------------ */

function drillInto(day: string, plugin: string) {
  drill.value = { day, plugin }
  sessionPage.value = 1
  tab.value = 'sessions'
  sessionsLoaded.value = false
  void loadSessions()
}

function clearDrill() {
  if (!drill.value) return
  drill.value = null
  sessionPage.value = 1
  void loadSessions()
}

const activeSession = ref<UsageSession | null>(null)

function openSession(item: UsageSession) {
  activeSession.value = item
}

function closeSession() {
  activeSession.value = null
}

const deviceSessionPages = computed(() => Math.max(1, Math.ceil(sessionTotal.value / PAGE_SIZE)))

function goPage(next: number) {
  const clamped = Math.min(Math.max(1, next), deviceSessionPages.value)
  if (clamped === sessionPage.value) return
  sessionPage.value = clamped
  void loadSessions()
}

/* ------------------------------------------------------------------ */
/* Copy + toasts                                                       */
/* ------------------------------------------------------------------ */

const toasts = ref<ToastItem[]>([])
let toastSeq = 0
const toastTimers = new Set<number>()

function toast(text: string, tone: ToastItem['tone'] = 'ok') {
  const id = ++toastSeq
  toasts.value.push({ id, text, tone })
  const timer = window.setTimeout(() => {
    toastTimers.delete(timer)
    toasts.value = toasts.value.filter((item) => item.id !== id)
  }, 3600)
  toastTimers.add(timer)
}

async function copyText(text: string, label = '已复制') {
  if (!text) {
    toast('没有可复制的内容', 'error')
    return
  }
  try {
    await navigator.clipboard.writeText(text)
    toast(label)
  } catch {
    toast('复制失败，请手动选择文本', 'error')
  }
}

/* ------------------------------------------------------------------ */
/* Lifecycle                                                           */
/* ------------------------------------------------------------------ */

function onDocumentClick(event: MouseEvent) {
  const target = event.target as HTMLElement | null
  if (!target) return
  if (target.closest('.ms-pop') || target.closest('.ms-btn')) return
  openMenu.value = ''
}

function onKeydown(event: KeyboardEvent) {
  if (event.key !== 'Escape') return
  if (activeSession.value) {
    closeSession()
    return
  }
  openMenu.value = ''
}

async function loadOptions() {
  try {
    plugins.value = (await api.plugins.list()) ?? []
  } catch {
    plugins.value = []
  }
  try {
    const data = await api.groups.list()
    groups.value = data?.groups ?? []
  } catch {
    groups.value = []
  }
  try {
    accounts.value = normalizeAccounts(await api.accounts.query())
  } catch {
    accounts.value = []
  }
}

onMounted(() => {
  document.addEventListener('click', onDocumentClick)
  document.addEventListener('keydown', onKeydown)
  void loadOptions()
  void refresh()
})

onBeforeUnmount(() => {
  document.removeEventListener('click', onDocumentClick)
  document.removeEventListener('keydown', onKeydown)
  for (const timer of toastTimers) window.clearTimeout(timer)
  toastTimers.clear()
})
</script>

<template>
  <div class="page usage-page">
    <div v-if="error" class="notice error">
      <span>读取用量数据失败：{{ error }}</span>
      <button type="button" class="btn sm ghost" @click="retry">重试</button>
    </div>

    <section class="card">
      <div class="card-body tight">
        <div class="filterbar">
          <div class="seg" role="group" aria-label="日期范围">
            <button
              v-for="item in PRESETS"
              :key="item.key"
              type="button"
              class="seg-btn"
              :class="{ active: preset === item.key }"
              :aria-pressed="preset === item.key"
              @click="applyPreset(item.key)"
            >
              {{ item.label }}
            </button>
          </div>

          <div v-if="preset === 'custom'" class="date-range">
            <input v-model="from" class="input input-date" type="date" aria-label="开始日期" />
            <span class="muted small">至</span>
            <input v-model="to" class="input input-date" type="date" aria-label="结束日期" />
          </div>

          <div v-for="dimension in DIMENSIONS" :key="dimension" class="ms">
            <button
              type="button"
              class="btn ms-btn"
              :class="{ 'is-active': selected[dimension].length > 0, open: openMenu === dimension }"
              :aria-expanded="openMenu === dimension"
              @click="openMenu = openMenu === dimension ? '' : dimension"
            >
              {{ MENU_LABELS[dimension] }}
              <span v-if="selected[dimension].length > 0" class="ms-count">{{ selected[dimension].length }}</span>
              <span class="ms-caret" aria-hidden="true">▾</span>
            </button>
            <div v-if="openMenu === dimension" class="ms-pop" role="group" :aria-label="MENU_LABELS[dimension]">
              <label class="ms-option ms-option-all">
                <input
                  type="checkbox"
                  :checked="selected[dimension].length === 0"
                  @change="clearDimension(dimension)"
                />
                <span class="ms-option-label">{{ ALL_LABELS[dimension] }}</span>
                <span class="ms-option-hint">不筛选</span>
              </label>
              <div v-if="optionsFor(dimension).length === 0" class="ms-empty">暂无可选项</div>
              <label v-for="option in optionsFor(dimension)" :key="option.value" class="ms-option">
                <input
                  type="checkbox"
                  :checked="selected[dimension].includes(option.value)"
                  @change="toggleOption(dimension, option.value)"
                />
                <span class="ms-option-label">{{ option.label }}</span>
                <span v-if="option.hint" class="ms-option-hint">{{ option.hint }}</span>
              </label>
              <div v-if="selected[dimension].length > 0 || openMenu === dimension" class="ms-foot">
                <button type="button" class="btn sm ghost" :disabled="selected[dimension].length === 0" @click="clearDimension(dimension)">
                  清空
                </button>
                <button type="button" class="btn sm ghost" @click="openMenu = ''">完成</button>
              </div>
            </div>
          </div>

          <span class="spacer"></span>
          <button type="button" class="btn" @click="resetFilters">重置</button>
          <a class="btn primary" :href="exportUrl" download="usage.csv">导出 CSV</a>
        </div>
      </div>
    </section>

    <div class="stat-grid kpi-grid">
      <section class="stat">
        <span class="stat-label">请求数</span>
        <span v-if="loading.summary && !summaryLoaded" class="skeleton skeleton-value"></span>
        <span v-else class="stat-value">{{ kpis.requests === undefined ? '—' : formatNumber(kpis.requests) }}</span>
        <span class="stat-sub">{{ from }} ~ {{ to }}</span>
      </section>
      <section class="stat">
        <span class="stat-label">错误数</span>
        <span v-if="loading.summary && !summaryLoaded" class="skeleton skeleton-value"></span>
        <span v-else class="stat-value" :class="{ 'value-danger': (kpis.errors ?? 0) > 0 }">
          {{ kpis.errors === undefined ? '—' : formatNumber(kpis.errors) }}
        </span>
        <span class="stat-sub">失败或 4xx/5xx 请求</span>
      </section>
      <section class="stat">
        <span class="stat-label">输入 tokens</span>
        <span v-if="loading.summary && !summaryLoaded" class="skeleton skeleton-value"></span>
        <span v-else class="stat-value" :title="exact(kpis.prompt_tokens)">{{ compact(kpis.prompt_tokens) }}</span>
        <span class="stat-sub">prompt tokens</span>
      </section>
      <section class="stat">
        <span class="stat-label">输出 tokens</span>
        <span v-if="loading.summary && !summaryLoaded" class="skeleton skeleton-value"></span>
        <span v-else class="stat-value" :title="exact(kpis.completion_tokens)">{{ compact(kpis.completion_tokens) }}</span>
        <span class="stat-sub">completion tokens</span>
      </section>
      <section class="stat">
        <span class="stat-label">积分消耗</span>
        <span v-if="loading.summary && !summaryLoaded" class="skeleton skeleton-value"></span>
        <span
          v-else
          class="stat-value"
          :title="hasCredits ? exact(creditTotal) : '平台未上报每请求积分'"
        >
          {{ hasCredits ? exact(creditTotal) : '—' }}
        </span>
        <span class="stat-sub">
          {{ hasCredits ? '按额度快照差分汇总' : '平台未上报每请求积分' }}
        </span>
      </section>
      <section class="stat">
        <span class="stat-label">活跃账号</span>
        <span v-if="loading.summary && !summaryLoaded" class="skeleton skeleton-value"></span>
        <span v-else class="stat-value">{{ kpis.active_accounts === undefined ? '—' : formatNumber(kpis.active_accounts) }}</span>
        <span class="stat-sub">区间内产生请求的账号</span>
      </section>
    </div>

    <div class="tabs" role="tablist" aria-label="用量视图">
      <button
        v-for="item in TABS"
        :key="item.key"
        type="button"
        role="tab"
        class="tab"
        :class="{ active: tab === item.key }"
        :aria-selected="tab === item.key"
        @click="setTab(item.key)"
      >
        {{ item.label }}
      </button>
    </div>

    <!-- 积分 -->
    <template v-if="tab === 'credits'">
      <section class="card">
        <div class="card-head">
          <h2>按日积分</h2>
          <span class="sub">点击色块下钻会话 · 点击图例隐藏插件</span>
        </div>
        <div class="card-body">
          <div v-if="loading.summary && !summaryLoaded" class="chart-skeleton" aria-hidden="true">
            <span
              v-for="n in 16"
              :key="n"
              class="skeleton skeleton-bar"
              :style="{ height: `${28 + ((n * 19) % 68)}%` }"
            ></span>
          </div>
          <div v-else-if="series.length === 0" class="empty">
            <strong>暂无积分数据</strong>
            <span>调整筛选条件，或等待首日汇总后再查看。</span>
          </div>
          <div v-else-if="!hasCredits" class="empty">
            <strong>平台未上报每请求积分</strong>
            <span>插件未回传 credits 字段，积分图表暂时为空。</span>
          </div>
          <div v-else-if="creditEmpty" class="empty">
            <strong>图表已被图例全部隐藏</strong>
            <span>点击下方图例恢复插件。</span>
          </div>
          <template v-else>
            <div class="chart">
              <div class="chart-y" aria-hidden="true">
                <span>{{ compact(creditMax) }}</span>
                <span>{{ compact(creditMax / 2) }}</span>
                <span>0</span>
              </div>
              <div class="chart-bars">
                <div
                  v-for="day in creditChart"
                  :key="day.day"
                  class="chart-col"
                  :title="`${day.day} · ${formatNumber(day.total)} 积分`"
                >
                  <div class="chart-stack">
                    <button
                      v-for="segment in day.segments"
                      :key="segment.key"
                      type="button"
                      class="chart-seg"
                      :style="{ height: segmentHeight(segment.value, creditMax), background: segment.color }"
                      :title="`${segment.label} · ${formatNumber(segment.value)}`"
                      :aria-label="`${day.day} ${segment.label} ${formatNumber(segment.value)} 积分，查看会话`"
                      @click="drillInto(day.day, segment.plugin)"
                    ></button>
                  </div>
                  <span class="chart-x">{{ day.label }}</span>
                </div>
              </div>
            </div>
            <div class="legend chart-legend">
              <button
                v-for="pluginId in pluginIds"
                :key="pluginId"
                type="button"
                class="legend-toggle"
                :class="{ off: hiddenPlugins.has(pluginId) }"
                :aria-pressed="!hiddenPlugins.has(pluginId)"
                @click="togglePlugin(pluginId)"
              >
                <span class="swatch" :style="{ background: pluginColor(pluginId) }"></span>
                {{ pluginLabel(pluginId) }}
              </button>
            </div>
          </template>
        </div>
      </section>

      <section class="card">
        <div class="card-head">
          <h2>按插件汇总</h2>
          <span class="sub">{{ from }} ~ {{ to }}</span>
        </div>
        <div class="table-wrap">
          <table class="table usage-table">
            <thead>
              <tr>
                <th>插件</th>
                <th>请求数</th>
                <th>输入 tokens</th>
                <th>输出 tokens</th>
                <th>积分</th>
              </tr>
            </thead>
            <tbody>
              <template v-if="loading.summary && !summaryLoaded">
                <tr v-for="n in 3" :key="`sk-${n}`">
                  <td colspan="5"><span class="skeleton skeleton-line"></span></td>
                </tr>
              </template>
              <template v-else>
                <tr v-for="row in pluginTotals" :key="row.plugin">
                  <td class="title-cell">
                    <span class="swatch" :style="{ background: pluginColor(row.plugin) }"></span>
                    {{ pluginLabel(row.plugin) }}
                  </td>
                  <td class="num">{{ formatNumber(row.requests) }}</td>
                  <td class="num">{{ formatNumber(row.prompt) }}</td>
                  <td class="num">{{ formatNumber(row.completion) }}</td>
                  <td class="num">
                    <template v-if="row.credits > 0">{{ formatNumber(row.credits) }}</template>
                    <span v-else class="muted" title="平台未上报每请求积分">—</span>
                  </td>
                </tr>
                <tr v-if="pluginTotals.length === 0">
                  <td colspan="5">
                    <div class="empty">
                      <strong>暂无汇总数据</strong>
                      <span>调整筛选条件，或等待首日汇总。</span>
                    </div>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
      </section>
    </template>

    <!-- Token -->
    <template v-else-if="tab === 'token'">
      <section class="card">
        <div class="card-head">
          <h2>按日 Token</h2>
          <span class="sub">输入 / 输出 柱状（左轴）+ 每日调用次数 折线（右轴）· 点击图例切换柱序列</span>
        </div>
        <div class="card-body">
          <div class="total-strip" v-if="series.length > 0">
            <div class="total-item">
              <span class="stat-label">输入合计</span>
              <strong :title="exact(tokenTotals.input)">{{ compact(tokenTotals.input) }}</strong>
            </div>
            <div class="total-item">
              <span class="stat-label">输出合计</span>
              <strong :title="exact(tokenTotals.output)">{{ compact(tokenTotals.output) }}</strong>
            </div>
            <div class="total-item">
              <span class="stat-label">Token 合计</span>
              <strong :title="exact(tokenTotals.total)">{{ compact(tokenTotals.total) }}</strong>
            </div>
          </div>

          <div v-if="loading.summary && !summaryLoaded" class="chart-skeleton" aria-hidden="true">
            <span
              v-for="n in 16"
              :key="n"
              class="skeleton skeleton-bar"
              :style="{ height: `${32 + ((n * 23) % 60)}%` }"
            ></span>
          </div>
          <div v-else-if="series.length === 0" class="empty">
            <strong>暂无 Token 数据</strong>
            <span>调整筛选条件，或等待首日汇总后再查看。</span>
          </div>
          <div v-else-if="tokenTotals.total === 0" class="empty">
            <strong>区间内没有 Token 记录</strong>
            <span>插件尚未回传 prompt / completion tokens。</span>
          </div>
          <div v-else-if="tokenEmpty" class="empty">
            <strong>图表已被图例全部隐藏</strong>
            <span>点击下方图例恢复序列。</span>
          </div>
          <template v-else>
            <div class="chart" :class="{ 'chart-with-line': callLine }">
              <div class="chart-y" aria-hidden="true">
                <span>{{ compact(tokenMax) }}</span>
                <span>{{ compact(tokenMax / 2) }}</span>
                <span>0</span>
              </div>
              <div class="chart-plot">
                <div ref="plotEl" class="chart-bars">
                  <div
                    v-for="day in tokenChart"
                    :key="day.day"
                    class="chart-col"
                    :title="`${day.day} · 输入 ${formatNumber(tokenDays.find((item) => item.day === day.day)?.input)} · 输出 ${formatNumber(tokenDays.find((item) => item.day === day.day)?.output)} · 调用 ${formatNumber(day.requests)} 次`"
                  >
                    <div class="chart-stack">
                      <button
                        v-for="segment in day.segments"
                        :key="segment.key"
                        type="button"
                        class="chart-seg"
                        :style="{ height: segmentHeight(segment.value, tokenMax), background: segment.color }"
                        :title="`${segment.label} · ${formatNumber(segment.value)} tokens`"
                        :aria-label="`${day.day} ${segment.label} ${formatNumber(segment.value)} tokens，查看会话`"
                        @click="drillInto(day.day, '')"
                      ></button>
                    </div>
                    <span class="chart-x">{{ day.label }}</span>
                  </div>
                </div>
                <svg
                  v-if="callLine"
                  class="chart-line"
                  :width="plotWidth"
                  :height="CHART_HEIGHT"
                  aria-hidden="true"
                >
                  <polyline
                    :points="callLine.points"
                    fill="none"
                    stroke="var(--chart-3)"
                    stroke-width="2"
                    stroke-linejoin="round"
                    stroke-linecap="round"
                  />
                  <circle
                    v-for="dot in callLine.dots"
                    :key="dot.day"
                    :cx="dot.x"
                    :cy="dot.y"
                    r="2.5"
                    fill="var(--chart-3)"
                  />
                </svg>
              </div>
              <div v-if="callLine" class="chart-y chart-y-right" aria-hidden="true">
                <span>{{ compact(callsAxis[0]) }}</span>
                <span>{{ compact(callsAxis[1]) }}</span>
                <span>0</span>
              </div>
            </div>
            <div class="legend chart-legend">
              <button
                type="button"
                class="legend-toggle"
                :class="{ off: hiddenTokenSeries.has('in') }"
                :aria-pressed="!hiddenTokenSeries.has('in')"
                @click="toggleTokenSeries('in')"
              >
                <span class="swatch swatch-input"></span>
                输入 tokens
              </button>
              <button
                type="button"
                class="legend-toggle"
                :class="{ off: hiddenTokenSeries.has('out') }"
                :aria-pressed="!hiddenTokenSeries.has('out')"
                @click="toggleTokenSeries('out')"
              >
                <span class="swatch swatch-output"></span>
                输出 tokens
              </button>
              <span v-if="callLine" class="legend-note">
                <span class="swatch-line"></span>
                调用次数（右轴）
              </span>
            </div>
          </template>
        </div>
      </section>
    </template>

    <!-- 延迟分位 -->
    <template v-else-if="tab === 'latency'">
      <section class="card">
        <div class="card-head">
          <h2>延迟分位</h2>
          <span class="sub">仅统计成功请求（status=200）· TTFT 仅统计有首字时间的请求</span>
        </div>
        <div class="card-body tight">
          <div class="seg" role="group" aria-label="延迟统计窗口">
            <button
              v-for="days in OBS_DAY_OPTIONS"
              :key="days"
              type="button"
              class="seg-btn"
              :class="{ active: latencyDays === days }"
              :aria-pressed="latencyDays === days"
              @click="setLatencyDays(days)"
            >
              最近 {{ days }} 天
            </button>
          </div>
        </div>
        <div class="table-wrap">
          <table class="table usage-table">
            <thead>
              <tr>
                <th>日期</th>
                <th>请求数</th>
                <th>P50</th>
                <th>P95</th>
                <th>TTFT P50</th>
                <th>TTFT P95</th>
              </tr>
            </thead>
            <tbody>
              <template v-if="loading.latency && !latencyLoaded">
                <tr v-for="n in 4" :key="`sk-${n}`">
                  <td colspan="6"><span class="skeleton skeleton-line"></span></td>
                </tr>
              </template>
              <template v-else>
                <tr v-for="row in latencyRows" :key="row.day">
                  <td class="title-cell">{{ row.day }}</td>
                  <td class="num">{{ formatNumber(row.requests) }}</td>
                  <td class="num">{{ formatLatency(row.p50_ms) }}</td>
                  <td class="num">{{ formatLatency(row.p95_ms) }}</td>
                  <td class="num">{{ formatLatency(row.p50_ttfb_ms) }}</td>
                  <td class="num">{{ formatLatency(row.p95_ttfb_ms) }}</td>
                </tr>
                <tr v-if="latencyRows.length === 0">
                  <td colspan="6">
                    <div class="empty">
                      <strong>暂无延迟数据</strong>
                      <span>请求日志里还没有成功的调用，或所选窗口内没有记录。</span>
                    </div>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
      </section>
    </template>

    <!-- 按 Key -->
    <template v-else-if="tab === 'keys'">
      <section class="card">
        <div class="card-head">
          <h2>按 Key 用量</h2>
          <span class="sub">数据来自客户端 Key 的每日计数 · 已删除的 Key 仍保留历史并显示 ID</span>
        </div>
        <div class="card-body tight">
          <div class="seg" role="group" aria-label="Key 统计窗口">
            <button
              v-for="days in OBS_DAY_OPTIONS"
              :key="days"
              type="button"
              class="seg-btn"
              :class="{ active: keyDays === days }"
              :aria-pressed="keyDays === days"
              @click="setKeyDays(days)"
            >
              最近 {{ days }} 天
            </button>
          </div>
        </div>
        <div class="table-wrap">
          <table class="table usage-table">
            <thead>
              <tr>
                <th>Key 名</th>
                <th>请求数</th>
                <th>prompt tokens</th>
                <th>completion tokens</th>
                <th>缓存命中</th>
                <th>积分</th>
              </tr>
            </thead>
            <tbody>
              <template v-if="loading.keys && !keysLoaded">
                <tr v-for="n in 4" :key="`sk-${n}`">
                  <td colspan="6"><span class="skeleton skeleton-line"></span></td>
                </tr>
              </template>
              <template v-else>
                <tr v-for="row in keyRows" :key="row.key_id">
                  <td class="title-cell">
                    {{ row.name || row.key_id }}
                    <span v-if="row.name && row.name !== row.key_id" class="muted small block mono">{{ row.key_id }}</span>
                  </td>
                  <td class="num">{{ formatNumber(row.requests) }}</td>
                  <td class="num">{{ formatNumber(row.prompt_tokens) }}</td>
                  <td class="num">{{ formatNumber(row.completion_tokens) }}</td>
                  <td class="num">{{ formatNumber(row.cached_tokens) }}</td>
                  <td class="num">{{ row.credits === undefined || row.credits === null ? '—' : formatNumber(row.credits) }}</td>
                </tr>
                <tr v-if="keyRows.length === 0">
                  <td colspan="6">
                    <div class="empty">
                      <strong>暂无 Key 用量</strong>
                      <span>所选窗口内还没有客户端 Key 发起请求。</span>
                    </div>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
      </section>
    </template>

    <!-- 模型 -->
    <template v-else-if="tab === 'models'">
      <section class="card">
        <div class="card-head">
          <h2>模型消耗排行</h2>
          <span class="sub">按请求数从高到低 · 数据来自每日汇总；延迟列仅统计 status=200 的请求</span>
        </div>
        <div class="card-body tight">
          <div class="seg" role="group" aria-label="模型统计窗口">
            <button
              v-for="days in OBS_DAY_OPTIONS"
              :key="days"
              type="button"
              class="seg-btn"
              :class="{ active: modelDays === days }"
              :aria-pressed="modelDays === days"
              @click="setModelDays(days)"
            >
              最近 {{ days }} 天
            </button>
          </div>
        </div>
        <div class="table-wrap">
          <table class="table usage-table">
            <thead>
              <tr>
                <th class="col-rank">排名</th>
                <th>模型</th>
                <th>请求数</th>
                <th>输入 tokens</th>
                <th>输出 tokens</th>
                <th>积分</th>
                <template v-if="hasModelLatency">
                  <th>P50</th>
                  <th>P95</th>
                  <th>TTFT P50</th>
                  <th>TTFT P95</th>
                </template>
              </tr>
            </thead>
            <tbody>
              <template v-if="loading.models && !modelsLoaded">
                <tr v-for="n in 4" :key="`sk-${n}`">
                  <td :colspan="hasModelLatency ? 10 : 6"><span class="skeleton skeleton-line"></span></td>
                </tr>
              </template>
              <template v-else>
                <tr v-for="(row, index) in modelRows" :key="row.model || index">
                  <td class="num">
                    <span class="badge plain" :class="index === 0 ? 'accent' : 'muted'">{{ index + 1 }}</span>
                  </td>
                  <td class="title-cell mono">{{ row.model || '未上报' }}</td>
                  <td class="num">{{ formatNumber(row.requests) }}</td>
                  <td class="num">{{ formatNumber(row.prompt_tokens) }}</td>
                  <td class="num">{{ formatNumber(row.completion_tokens) }}</td>
                  <td class="num">
                    <template v-if="row.credits === undefined || row.credits === null">
                      <span class="muted" title="平台未上报每请求积分">—</span>
                    </template>
                    <template v-else>{{ formatNumber(row.credits) }}</template>
                  </td>
                  <template v-if="hasModelLatency">
                    <td class="num">{{ formatLatency(modelLatencyOf(row.model)?.p50_ms) }}</td>
                    <td class="num">{{ formatLatency(modelLatencyOf(row.model)?.p95_ms) }}</td>
                    <td class="num">{{ formatLatency(modelLatencyOf(row.model)?.p50_ttfb_ms) }}</td>
                    <td class="num">{{ formatLatency(modelLatencyOf(row.model)?.p95_ttfb_ms) }}</td>
                  </template>
                </tr>
                <tr v-if="modelRows.length === 0">
                  <td :colspan="hasModelLatency ? 10 : 6">
                    <div class="empty">
                      <strong>暂无模型用量</strong>
                      <span>所选窗口内还没有完成汇总的请求。</span>
                    </div>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
      </section>
    </template>

    <!-- 到期 -->
    <template v-else-if="tab === 'expiry'">
      <section class="card">
        <div class="card-head">
          <h2>账号到期情况</h2>
          <span class="sub">7 天内到期标黄，已过期标红，未知到期置灰</span>
        </div>
        <div class="table-wrap">
          <table class="table usage-table expiry-table">
            <thead>
              <tr>
                <th>账号</th>
                <th>插件</th>
                <th>套餐</th>
                <th>剩余</th>
                <th>到期日</th>
                <th>日消耗</th>
                <th>预计可用天数</th>
              </tr>
            </thead>
            <tbody>
              <template v-if="loading.expiry && !expiryLoaded">
                <tr v-for="n in 4" :key="`sk-${n}`">
                  <td colspan="7"><span class="skeleton skeleton-line"></span></td>
                </tr>
              </template>
              <template v-else>
                <tr v-for="row in expiry" :key="`${row.plugin_id}:${row.account_id}`" :class="`tone-${expiryTone(row) || 'ok'}`">
                  <td class="title-cell">{{ row.display_name || row.account_id || '—' }}</td>
                  <td>{{ pluginLabel(row.plugin_id ?? '') }}</td>
                  <td>{{ row.plan || '—' }}</td>
                  <td class="num">
                    {{ formatNumber(row.remaining) }}
                    <span v-if="row.total !== undefined" class="muted small">/ {{ formatNumber(row.total) }}</span>
                  </td>
                  <td class="num expiry-date">{{ row.expires_at ? formatDate(row.expires_at) : '—' }}</td>
                  <td class="num">{{ row.daily_credits === undefined ? '—' : formatNumber(row.daily_credits) }}</td>
                  <td class="num">
                    <template v-if="expiryDays(row) === null">—</template>
                    <template v-else>
                      {{ expiryDays(row) }} 天
                      <span v-if="expiryTone(row) === 'past'" class="badge danger plain">已过期</span>
                      <span v-else-if="expiryTone(row) === 'soon'" class="badge warn plain">临期</span>
                    </template>
                  </td>
                </tr>
                <tr v-if="expiry.length === 0">
                  <td colspan="7">
                    <div class="empty">
                      <strong>暂无到期信息</strong>
                      <span>账号额度尚未回传到期时间，或筛选条件不匹配。</span>
                    </div>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
      </section>
    </template>

    <!-- 会话明细 -->
    <template v-else>
      <section class="card">
        <div class="card-head">
          <div class="row">
            <h2>会话明细</h2>
            <span v-if="drill" class="drill-chip">
              下钻：{{ drill.day }}<template v-if="drill.plugin"> · {{ pluginLabel(drill.plugin) }}</template>
              <button type="button" class="drill-clear" aria-label="清除下钻" @click="clearDrill">×</button>
            </span>
          </div>
          <span class="sub">共 {{ formatNumber(sessionTotal) }} 条 · 每页 {{ PAGE_SIZE }} 条 · 点击行查看详情</span>
        </div>
        <div class="table-wrap">
          <table class="table sessions-table">
            <thead>
              <tr>
                <th>时间</th>
                <th>插件</th>
                <th>账号</th>
                <th>模型</th>
                <th>输入</th>
                <th>输出</th>
                <th>积分</th>
                <th>首字</th>
                <th>耗时</th>
                <th>状态</th>
                <th>会话</th>
              </tr>
            </thead>
            <tbody>
              <template v-if="loading.sessions && !sessionsLoaded">
                <tr v-for="n in 6" :key="`sk-${n}`">
                  <td colspan="11"><span class="skeleton skeleton-line"></span></td>
                </tr>
              </template>
              <template v-else>
                <tr
                  v-for="(item, index) in sessions"
                  :key="item.req_id || `${item.ts}-${index}`"
                  class="row-click"
                  tabindex="0"
                  @click="openSession(item)"
                  @keydown.enter="openSession(item)"
                  @keydown.space.prevent="openSession(item)"
                >
                  <td class="num small">{{ formatTimestamp(item.ts) }}</td>
                  <td class="small">{{ pluginLabel(item.plugin_id ?? '') }}</td>
                  <td class="small">
                    {{ item.account_name || item.account_id || '—' }}
                    <span v-if="item.account_name && item.account_id" class="muted small block">{{ item.account_id }}</span>
                  </td>
                  <td class="small">
                    {{ item.model || '—' }}
                    <span v-if="item.logical_model && item.logical_model !== item.model" class="muted small block">
                      → {{ item.logical_model }}
                    </span>
                  </td>
                  <td class="num small">{{ formatNumber(item.prompt_tokens) }}</td>
                  <td class="num small">{{ formatNumber(item.completion_tokens) }}</td>
                  <td class="num small">{{ formatNumber(item.credits) }}</td>
                  <td class="num small">{{ formatLatency(item.ttfb_ms) }}</td>
                  <td class="num small">{{ formatLatency(item.latency_ms) }}</td>
                  <td>
                    <span class="badge plain" :class="statusTone(item.status)">
                      {{ item.status ?? '—' }}
                    </span>
                  </td>
                  <td class="small mono conv-cell truncate" :title="item.conversation_id || ''">
                    {{ item.conversation_id || '—' }}
                  </td>
                </tr>
                <tr v-if="sessions.length === 0">
                  <td colspan="11">
                    <div class="empty">
                      <strong>暂无会话记录</strong>
                      <span>调整筛选条件，或等待客户端产生请求。</span>
                    </div>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
        <div class="pager">
          <span>第 {{ sessionPage }} / {{ deviceSessionPages }} 页</span>
          <button type="button" class="btn sm" :disabled="sessionPage <= 1 || loading.sessions" @click="goPage(sessionPage - 1)">
            上一页
          </button>
          <button
            type="button"
            class="btn sm"
            :disabled="sessionPage >= deviceSessionPages || loading.sessions"
            @click="goPage(sessionPage + 1)"
          >
            下一页
          </button>
        </div>
      </section>
    </template>

    <div v-if="activeSession" class="drawer-backdrop" @click.self="closeSession">
      <aside class="drawer" role="dialog" aria-modal="true" aria-labelledby="session-panel-title">
        <div class="drawer-head">
          <div>
            <h2 id="session-panel-title">请求详情</h2>
            <span class="muted small">{{ formatTimestamp(activeSession.ts) }}</span>
          </div>
          <button type="button" class="btn sm ghost" @click="closeSession">关闭</button>
        </div>
        <div class="drawer-body">
          <dl class="kv session-kv">
            <dt>时间</dt>
            <dd>{{ formatTimestamp(activeSession.ts) }}</dd>
            <dt>req_id</dt>
            <dd class="mono">{{ activeSession.req_id || '—' }}</dd>
            <dt>插件</dt>
            <dd>{{ pluginLabel(activeSession.plugin_id ?? '') }}</dd>
            <dt>账号</dt>
            <dd>{{ activeSession.account_name || '—' }}</dd>
            <dt>账号 ID</dt>
            <dd class="mono">{{ activeSession.account_id || '—' }}</dd>
            <dt>模型</dt>
            <dd>{{ activeSession.model || '—' }}</dd>
            <dt>逻辑模型</dt>
            <dd>{{ activeSession.logical_model || '—' }}</dd>
            <dt>会话 ID</dt>
            <dd class="mono">{{ activeSession.conversation_id || '—' }}</dd>
            <dt>输入 tokens</dt>
            <dd>{{ formatNumber(activeSession.prompt_tokens) }}</dd>
            <dt>输出 tokens</dt>
            <dd>{{ formatNumber(activeSession.completion_tokens) }}</dd>
            <dt>积分</dt>
            <dd>{{ formatNumber(activeSession.credits) }}</dd>
            <dt>首字延迟</dt>
            <dd>{{ formatLatency(activeSession.ttfb_ms) }}</dd>
            <dt>耗时</dt>
            <dd>{{ formatLatency(activeSession.latency_ms) }}</dd>
            <dt>状态</dt>
            <dd>
              <span class="badge plain" :class="statusTone(activeSession.status)">{{ activeSession.status ?? '—' }}</span>
            </dd>
          </dl>
          <div class="row">
            <button type="button" class="btn" :disabled="!activeSession.req_id" @click="copyText(activeSession.req_id || '')">
              复制 req_id
            </button>
            <button
              v-if="activeSession.conversation_id"
              type="button"
              class="btn"
              @click="copyText(activeSession.conversation_id || '')"
            >
              复制会话 ID
            </button>
          </div>
        </div>
      </aside>
    </div>

    <div class="toasts" role="status" aria-live="polite">
      <div v-for="item in toasts" :key="item.id" class="toast" :class="item.tone">{{ item.text }}</div>
    </div>
  </div>
</template>

<style scoped>
/* Fixed legend colours for the two token series; per-plugin swatches stay
   data-driven through :style. */
.swatch-input {
  background: var(--chart-1);
}

.swatch-output {
  background: var(--chart-2);
}
</style>
