<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  ApiError,
  api,
  errorMessage,
  normalizeRequestLogs,
  type APIRequestLog,
  type LogBodyWhich,
  type LogEvent,
  type LogProxyParams,
  type LogRawFile,
  type LogTimelineType,
  type ProxyLog,
  type RequestLogFilterParams,
  type RequestLogStatusClass,
} from '../api'
import { dateISO, formatLatency, formatNumber, formatTimestamp, pad, toDate } from '../format'

type Range = 'today' | 'hour' | 'custom'
type StreamState = 'idle' | 'connecting' | 'open' | 'reconnecting'
type LogsTab = 'requests' | 'proxy' | 'timeline'
type LogView = 'table' | 'text'

interface CategoryOption {
  value: string
  label: string
}

interface ToastItem {
  id: number
  text: string
  tone: 'ok' | 'error'
}

const LIVE_CAP = 500
/** Rows requested per page; selectable, persisted under PAGE_SIZE_KEY. */
const PAGE_SIZE_OPTIONS = [20, 50, 100]
const PAGE_SIZE_KEY = 'aiworkgate.logs.pageSize'
const DEFAULT_PAGE_SIZE = 50
/** Infinite scroll kicks in once the list is this close to its bottom edge. */
const SCROLL_TRIGGER_PX = 200

/** Tab order and labels: 请求日志 is the api_logs table, 代理日志 the egress
 * tunnels, 事件时间线 the merged stream. */
const TABS: { key: LogsTab; label: string }[] = [
  { key: 'requests', label: '请求日志' },
  { key: 'proxy', label: '代理日志' },
  { key: 'timeline', label: '事件时间线' },
]

const TAB_LABELS: Record<LogsTab, string> = Object.fromEntries(
  TABS.map((item) => [item.key, item.label]),
) as Record<LogsTab, string>

/** How a list renders: one row per record, or one plain-text line per event. */
const VIEWS: { key: LogView; label: string }[] = [
  { key: 'table', label: '表格' },
  { key: 'text', label: '文本' },
]
const VIEW_KEY = 'aiworkgate.logs.view'

const PROXY_RESULTS: { value: string; label: string }[] = [
  { value: '', label: '全部结果' },
  { value: 'ok', label: '成功' },
  { value: 'denied', label: '拒绝' },
  { value: 'failed', label: '失败' },
]

const PROXY_RESULT_LABELS: Record<string, string> = {
  ok: '成功',
  denied: '拒绝',
  failed: '失败',
}

const STATUS_CLASSES: { value: '' | RequestLogStatusClass; label: string }[] = [
  { value: '', label: '全部状态' },
  { value: '2xx', label: '2xx 成功' },
  { value: '4xx', label: '4xx 客户端' },
  { value: '5xx', label: '5xx 服务端' },
  { value: 'error', label: '失败（含无响应）' },
]

const STREAM_OPTIONS: { value: '' | 'true' | 'false'; label: string }[] = [
  { value: '', label: '全部流式' },
  { value: 'true', label: '流式' },
  { value: 'false', label: '非流式' },
]

const TYPES: { key: LogTimelineType; label: string }[] = [
  { key: 'all', label: '全部' },
  { key: 'api', label: 'API' },
  { key: 'proxy', label: '代理' },
  { key: 'audit', label: '审计' },
  { key: 'connectivity', label: '连通性' },
  { key: 'runtime', label: '运行' },
]

const RANGES: { key: Range; label: string }[] = [
  { key: 'today', label: '今天' },
  { key: 'hour', label: '1小时' },
  { key: 'custom', label: '自定义' },
]

const CATEGORIES: CategoryOption[] = [
  { value: 'request', label: '请求' },
  { value: 'checkin', label: '签到' },
  { value: 'scheduler', label: '调度' },
  { value: 'login', label: '登录' },
  { value: 'plugin', label: '插件' },
  { value: 'proxy', label: '代理' },
  { value: 'config', label: '配置' },
  { value: 'audit', label: '审计' },
  { value: 'action', label: '操作' },
  { value: 'connectivity', label: '连通性' },
  { value: 'error', label: '错误' },
  // runtime: host JSONL / plugin log lines that match no module rule
  // (internal/logging/timeline.go runtimeCategory default branch).
  { value: 'runtime', label: '运行' },
]

const CATEGORY_LABELS: Record<string, string> = Object.fromEntries(
  CATEGORIES.map((item) => [item.value, item.label]),
)

/* ------------------------------------------------------------------ */
/* Filters                                                             */
/* ------------------------------------------------------------------ */

const route = useRoute()
const router = useRouter()

/** Shared toolbar state: both tabs see the same day range and keyword. */
const tab = ref<LogsTab>('requests')
const range = ref<Range>('today')
const fromDate = ref(dateISO(new Date()))
const toDay = ref(dateISO(new Date()))
const q = ref('')
const keyword = ref('')
const pageSize = ref(readPageSize())

/** Timeline-only filters. */
const type = ref<LogTimelineType>('all')
const categories = ref<string[]>([])
const level = ref('')
const openCategory = ref(false)

/** Timeline paging cursor; empty means the next append restarts at the newest page. */
const nextCursor = ref('')

/** True while the timeline has a page behind the rows already loaded. */
const hasMore = ref(false)

/** A source hit its per-page read budget; the cursor still advances past it. */
const timelineTruncated = ref(false)

/** Bounds the empty-page auto-follow so a broken cursor cannot spin forever. */
let autoPageHops = 0

function queryText(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

function readPageSize(): number {
  try {
    const stored = Number(localStorage.getItem(PAGE_SIZE_KEY))
    if (PAGE_SIZE_OPTIONS.includes(stored)) return stored
  } catch {
    /* storage can be unavailable; fall back to the default */
  }
  return DEFAULT_PAGE_SIZE
}

function writePageSize(value: number) {
  try {
    localStorage.setItem(PAGE_SIZE_KEY, String(value))
  } catch {
    /* storage can be unavailable; the in-memory choice still applies */
  }
}

function readQuery() {
  const typeQuery = queryText(route.query.type)
  if (typeQuery === 'all' || typeQuery === 'api' || typeQuery === 'runtime') type.value = typeQuery

  const categoryQuery = queryText(route.query.category)
  if (categoryQuery) {
    categories.value = categoryQuery.split(',').filter((value) => value in CATEGORY_LABELS)
  }

  const levelQuery = queryText(route.query.level)
  if (levelQuery === 'info' || levelQuery === 'warn' || levelQuery === 'error') level.value = levelQuery

  const rangeQuery = queryText(route.query.range)
  if (rangeQuery === 'today' || rangeQuery === 'hour' || rangeQuery === 'custom') range.value = rangeQuery

  const fromQuery = queryText(route.query.from)
  if (fromQuery) fromDate.value = fromQuery
  const toQuery = queryText(route.query.to)
  if (toQuery) toDay.value = toQuery

  const keywordQuery = queryText(route.query.q)
  if (keywordQuery) {
    keyword.value = keywordQuery
    q.value = keywordQuery
  }
}

readQuery()

function syncQuery() {
  const query: Record<string, string> = {}
  if (type.value !== 'all') query.type = type.value
  if (categories.value.length > 0) query.category = categories.value.join(',')
  if (level.value) query.level = level.value
  if (range.value !== 'today') query.range = range.value
  if (range.value === 'custom') {
    if (fromDate.value) query.from = fromDate.value
    if (toDay.value) query.to = toDay.value
  }
  if (q.value) query.q = q.value
  // The page cursor is now internal (infinite scroll), so it is not mirrored
  // into the URL: a deep link should always start at the newest rows.
  void router.replace({ query })
}

/* ------------------------------------------------------------------ */
/* Data                                                                */
/* ------------------------------------------------------------------ */

const items = ref<LogEvent[]>([])
const loading = ref(true)
const loadingMore = ref(false)
const error = ref('')
let timelineSeq = 0

function rangeParams(): { from?: string; to?: string } {
  const now = new Date()
  if (range.value === 'today') {
    const day = dateISO(now)
    return { from: day, to: day }
  }
  if (range.value === 'hour') {
    return { from: new Date(now.getTime() - 3_600_000).toISOString(), to: now.toISOString() }
  }
  return { from: fromDate.value || undefined, to: toDay.value || undefined }
}

/** Timeline page load; `append` continues from the last page's cursor. */
async function loadTimeline(append = false) {
  if (append && !nextCursor.value) return
  const token = ++timelineSeq
  if (append) loadingMore.value = true
  else loading.value = true
  error.value = ''
  try {
    const data = await api.logs.timeline({
      ...rangeParams(),
      type: type.value,
      category: categories.value.join(',') || undefined,
      level: level.value || undefined,
      q: q.value || undefined,
      cursor: append ? nextCursor.value : undefined,
      page_size: pageSize.value,
    })
    if (token !== timelineSeq) return
    const rows = data?.items ?? []
    items.value = append ? [...items.value, ...rows] : rows
    nextCursor.value = data?.next_cursor ?? ''
    hasMore.value = Boolean(data?.has_more && nextCursor.value)
    timelineTruncated.value = Boolean(data?.truncated)
    // A page can come back empty while older events still exist (every source
    // spent its read budget on filtered-out rows). Follow the cursor instead of
    // showing "没有匹配的日志" over a page that is merely empty.
    if (rows.length === 0 && hasMore.value && autoPageHops < 5) {
      autoPageHops += 1
      void loadTimeline(true)
      return
    }
    autoPageHops = 0
  } catch (e) {
    if (token !== timelineSeq) return
    nextCursor.value = ''
    hasMore.value = false
    timelineTruncated.value = false
    if (!append) items.value = []
    error.value = errorMessage(e)
  } finally {
    if (token === timelineSeq) {
      loading.value = false
      loadingMore.value = false
    }
  }
}

/** Appends the next timeline page; the scroll handler and its guard call this. */
function loadMoreTimeline() {
  if (loading.value || loadingMore.value || !hasMore.value) return
  void loadTimeline(true)
}

/** Explicit refresh button: always restarts at the newest page. */
function reloadTimeline() {
  nextCursor.value = ''
  autoPageHops = 0
  void loadTimeline(false)
}

/** Refresh of the request table, same "back to page 1" semantics. */
function reloadRequestLogs() {
  rlogPage.value = 1
  void loadRequestLogs(1)
}

function applyFilters() {
  nextCursor.value = ''
  autoPageHops = 0
  liveEvents.value = []
  pendingEvents.value = []
  syncQuery()
  void loadTimeline(false)
  if (live.value) startStream()
}

/* ------------------------------------------------------------------ */
/* Request logs (filterable table + CSV export)                        */
/* ------------------------------------------------------------------ */

const rlogModel = ref('')
const rlogSource = ref('')
const rlogStatusClass = ref<'' | RequestLogStatusClass>('')
const rlogStream = ref<'' | 'true' | 'false'>('')
const rlogPage = ref(1)
const rlogItems = ref<APIRequestLog[]>([])
const rlogTotal = ref(0)
const rlogLoading = ref(true)
const rlogLoadingMore = ref(false)
const rlogLoaded = ref(false)
const rlogError = ref('')
const sourceOptions = ref<{ value: string; label: string }[]>([{ value: 'a', label: 'A 助手' }])
let rlogSeq = 0

/**
 * Filters of GET /api/logs/requests. The endpoint only knows the api_logs
 * columns, so type/category/level are deliberately absent here.
 */
function requestLogFilters(): RequestLogFilterParams {
  const filters: RequestLogFilterParams = { ...rangeParams(), q: q.value || undefined }
  const model = rlogModel.value.trim()
  if (model) filters.model = model
  if (rlogSource.value) filters.source = rlogSource.value
  if (rlogStatusClass.value) filters.status_class = rlogStatusClass.value
  if (rlogStream.value) filters.stream = rlogStream.value
  return filters
}

const rlogExportUrl = computed(() => api.logs.requestsExportUrl(requestLogFilters()))

/** True when another request-log page exists behind the rows already loaded. */
const rlogHasMore = computed(() => rlogItems.value.length < rlogTotal.value)

/** Request-log page load; `append` keeps the rows already on screen. */
async function loadRequestLogs(nextPage = 1, append = false) {
  const token = ++rlogSeq
  const pageNo = Math.max(1, Math.floor(nextPage))
  if (append) rlogLoadingMore.value = true
  else rlogLoading.value = true
  rlogError.value = ''
  try {
    const data = await api.logs.requests({
      ...requestLogFilters(),
      limit: pageSize.value,
      offset: (pageNo - 1) * pageSize.value,
    })
    if (token !== rlogSeq) return
    const rows = normalizeRequestLogs(data)
    rlogItems.value = append ? [...rlogItems.value, ...rows] : rows
    rlogPage.value = pageNo
    const total = data && !Array.isArray(data) && typeof data === 'object' ? Number(data.total) : NaN
    rlogTotal.value = Number.isFinite(total) ? total : rlogItems.value.length
    if (data && !Array.isArray(data) && typeof data === 'object' && data.error) {
      rlogError.value = String(data.error)
    }
  } catch (e) {
    if (token !== rlogSeq) return
    if (!append) {
      rlogItems.value = []
      rlogTotal.value = 0
    }
    rlogError.value = errorMessage(e)
  } finally {
    if (token === rlogSeq) {
      rlogLoading.value = false
      rlogLoadingMore.value = false
      rlogLoaded.value = true
    }
  }
}

/** Appends the next request-log page; the scroll handler and its guard call this. */
function loadMoreRequestLogs() {
  if (rlogLoading.value || rlogLoadingMore.value || !rlogHasMore.value) return
  void loadRequestLogs(rlogPage.value + 1, true)
}

function clearRequestLogFilters() {
  rlogModel.value = ''
  rlogSource.value = ''
  rlogStatusClass.value = ''
  rlogStream.value = ''
}

async function loadSourceOptions() {
  const options: { value: string; label: string }[] = [{ value: 'a', label: 'A 助手' }]
  try {
    const plugins = (await api.plugins.list()) ?? []
    for (const plugin of plugins) {
      options.push({ value: `plugin:${plugin.manifest.id}`, label: plugin.manifest.name || plugin.manifest.id })
    }
  } catch {
    /* plugin list unavailable: keep the "all sources" option */
  }
  try {
    const data = await api.customModels.list()
    for (const model of data?.models ?? []) {
      options.push({ value: `custom:${model.id}`, label: `自定义 · ${model.name || model.id}` })
    }
  } catch {
    /* custom models are optional */
  }
  sourceOptions.value = options
  if (rlogSource.value && !options.some((item) => item.value === rlogSource.value)) {
    rlogSource.value = ''
  }
}

function rlogKey(rec: APIRequestLog): string {
  return rec.key_name || rec.key_id || '—'
}

function rlogTokens(rec: APIRequestLog): string {
  return `${formatNumber(rec.prompt_tokens ?? 0)} / ${formatNumber(rec.completion_tokens ?? 0)}`
}

/* ------------------------------------------------------------------ */
/* Debug flag                                                          */
/* ------------------------------------------------------------------ */

/* ------------------------------------------------------------------ */
/* Proxy traffic log                                                   */
/* ------------------------------------------------------------------ */

const proxyResult = ref('')
const proxyItems = ref<ProxyLog[]>([])
const proxyTotal = ref(0)
const proxyPage = ref(1)
const proxyLoading = ref(false)
const proxyLoadingMore = ref(false)
const proxyLoaded = ref(false)
const proxyError = ref('')
let proxySeq = 0

function proxyFilters(): LogProxyParams {
  const filters: LogProxyParams = { ...rangeParams(), q: q.value || undefined }
  if (proxyResult.value) filters.result = proxyResult.value
  return filters
}

const proxyHasMore = computed(() => proxyItems.value.length < proxyTotal.value)

/** Proxy-log page load; `append` keeps the rows already on screen. */
async function loadProxyLogs(nextPage = 1, append = false) {
  const token = ++proxySeq
  const pageNo = Math.max(1, Math.floor(nextPage))
  if (append) proxyLoadingMore.value = true
  else proxyLoading.value = true
  proxyError.value = ''
  try {
    const data = await api.logs.proxy({
      ...proxyFilters(),
      page: pageNo,
      page_size: pageSize.value,
    })
    if (token !== proxySeq) return
    const rows = data?.items ?? []
    proxyItems.value = append ? [...proxyItems.value, ...rows] : rows
    proxyPage.value = pageNo
    proxyTotal.value = data?.total ?? proxyItems.value.length
  } catch (e) {
    if (token !== proxySeq) return
    if (!append) {
      proxyItems.value = []
      proxyTotal.value = 0
    }
    proxyError.value = errorMessage(e)
  } finally {
    if (token === proxySeq) {
      proxyLoading.value = false
      proxyLoadingMore.value = false
      proxyLoaded.value = true
    }
  }
}

function reloadProxyLogs() {
  void loadProxyLogs(1)
}

function loadMoreProxyLogs() {
  if (proxyLoading.value || proxyLoadingMore.value || !proxyHasMore.value) return
  void loadProxyLogs(proxyPage.value + 1, true)
}

function clearProxyFilters() {
  proxyResult.value = ''
}

function proxyIdentity(row: ProxyLog): string {
  const plugin = row.plugin_id || '—'
  return row.account_id ? `${plugin}/${row.account_id}` : plugin
}

function proxyTarget(row: ProxyLog): string {
  if (!row.target_host) return '—'
  return row.target_port ? `${row.target_host}:${row.target_port}` : row.target_host
}

function proxyResultLabel(value?: string): string {
  return PROXY_RESULT_LABELS[value ?? ''] ?? value ?? '—'
}

function proxyResultTone(value?: string): string {
  if (value === 'ok') return 'ok'
  if (value === 'denied') return 'warn'
  return 'error'
}

/** formatBytes keeps a tunnel row readable without pretending to be precise. */
function formatBytes(n?: number): string {
  const value = Number(n ?? 0)
  if (!Number.isFinite(value) || value <= 0) return '0 B'
  if (value < 1024) return `${value} B`
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`
  return `${(value / (1024 * 1024)).toFixed(1)} MB`
}

const proxyScroller = ref<HTMLElement | null>(null)

function onProxyScroll() {
  const el = proxyScroller.value
  if (!el) return
  if (el.scrollTop + el.clientHeight >= el.scrollHeight - SCROLL_TRIGGER_PX) loadMoreProxyLogs()
}

/* ------------------------------------------------------------------ */
/* Display format: table or plain text                                 */
/* ------------------------------------------------------------------ */

function readView(): LogView {
  try {
    const stored = localStorage.getItem(VIEW_KEY)
    if (stored === 'text' || stored === 'table') return stored
  } catch {
    /* storage can be unavailable; fall back to the table */
  }
  return 'table'
}

const view = ref<LogView>(readView())

watch(view, (value) => {
  try {
    localStorage.setItem(VIEW_KEY, value)
  } catch {
    /* ignore a private-mode storage failure */
  }
})

/** One line per request, in the shape the host log files already use. */
const requestsText = computed(() =>
  rlogItems.value
    .map((rec) => {
      const parts = [
        rec.model || '?',
        rec.account || '—',
        rec.key_name || rec.key_id || '—',
        String(rec.status ?? '—'),
        formatLatency(rec.latency_ms),
        `${rlogTokens(rec)} tok`,
      ]
      if (rec.stream) parts.push('流式')
      if (rec.error) parts.push(rec.error)
      return `${logTime(rec.ts)} [${rec.source || '—'}] ${parts.join(' · ')}`
    })
    .join('\n'),
)

const proxyText = computed(() =>
  proxyItems.value
    .map((row) => {
      const parts = [
        proxyTarget(row),
        proxyResultLabel(row.result),
        formatLatency(row.duration_ms),
        `↑${formatBytes(row.bytes_up)} ↓${formatBytes(row.bytes_down)}`,
      ]
      if (row.error) parts.push(row.error)
      return `${logTime(row.ts)} [${proxyIdentity(row)} · ${row.proxy_label || row.proxy_id || '—'}] ${parts.join(' · ')}`
    })
    .join('\n'),
)

const timelineText = computed(() =>
  displayItems.value
    .map((event) => `${logTime(event.ts)} [${event.source || event.type || '—'}] ${event.summary || ''}`)
    .join('\n'),
)

const currentText = computed(() => {
  if (tab.value === 'requests') return requestsText.value
  if (tab.value === 'proxy') return proxyText.value
  return timelineText.value
})

function copyCurrentText() {
  if (!currentText.value) {
    copyText('', '没有可复制的内容')
    return
  }
  void copyText(currentText.value, '已复制当前页')
}

/* ------------------------------------------------------------------ */
/* Debug flag                                                          */
/* ------------------------------------------------------------------ */

const debugEnabled = ref(false)
const debugKnown = ref(false)

async function loadSettings() {
  try {
    const raw = (await api.getSettings()) ?? {}
    const logging = raw.logging as { debug_body?: unknown } | undefined
    debugEnabled.value = Boolean(logging?.debug_body)
  } catch {
    debugEnabled.value = false
  } finally {
    debugKnown.value = true
  }
}

async function toggleDebug() {
  try {
    const settings = (await api.getSettings()) ?? {}
    const logging = (settings.logging ?? {}) as Record<string, unknown>
    const current = Boolean(logging.debug_body)
    await api.saveSettings({ ...settings, logging: { ...logging, debug_body: !current } })
    debugEnabled.value = !current
    toast(current ? 'Debug 已关闭' : 'Debug 已开启')
  } catch (e) {
    toast(errorMessage(e), 'error')
  }
}

/* ------------------------------------------------------------------ */
/* Realtime                                                            */
/* ------------------------------------------------------------------ */

const live = ref(false)
const paused = ref(false)
const liveEvents = ref<LogEvent[]>([])
const pendingEvents = ref<LogEvent[]>([])
const streamState = ref<StreamState>('idle')
const reconnecting = ref(false)

let stream: EventSource | null = null
let reconnectTimer: number | undefined
let reconnectAttempt = 0
let anchorKey = ''
let anchorOffset = 0

function eventKey(event: LogEvent): string {
  return `${event.ts ?? ''}|${event.type ?? ''}|${event.category ?? ''}|${event.req_id ?? ''}|${event.summary ?? ''}`
}

function pushLive(event: LogEvent) {
  if (paused.value) {
    pendingEvents.value = [event, ...pendingEvents.value].slice(0, LIVE_CAP)
    return
  }
  noteLiveBatch()
  liveEvents.value = [event, ...liveEvents.value].slice(0, LIVE_CAP)
}

function scheduleReconnect() {
  if (!live.value || reconnectTimer !== undefined) return
  const delay = Math.min(15000, 2000 * 2 ** Math.min(reconnectAttempt, 3))
  reconnectAttempt += 1
  reconnectTimer = window.setTimeout(() => {
    reconnectTimer = undefined
    if (live.value) startStream()
  }, delay)
}

function startStream() {
  stopStream()
  reconnecting.value = false
  streamState.value = 'connecting'
  const source = new EventSource(
    api.logs.streamUrl({
      type: type.value,
      category: categories.value.join(',') || undefined,
      level: level.value || undefined,
      q: q.value || undefined,
    }),
  )
  stream = source
  source.onopen = () => {
    reconnectAttempt = 0
    reconnecting.value = false
    streamState.value = 'open'
  }
  source.addEventListener('log', (event) => {
    const message = event as MessageEvent<string>
    try {
      const payload = JSON.parse(message.data) as LogEvent
      if (payload && typeof payload === 'object') pushLive(payload)
    } catch {
      /* heartbeat or malformed payload, ignore */
    }
  })
  source.onerror = () => {
    if (!live.value) return
    source.close()
    if (stream === source) stream = null
    streamState.value = 'reconnecting'
    reconnecting.value = true
    scheduleReconnect()
  }
}

function stopStream() {
  if (reconnectTimer !== undefined) {
    window.clearTimeout(reconnectTimer)
    reconnectTimer = undefined
  }
  if (stream) {
    stream.close()
    stream = null
  }
  streamState.value = 'idle'
  reconnecting.value = false
}

function toggleLive() {
  if (live.value) {
    live.value = false
    paused.value = false
    stopStream()
    return
  }
  live.value = true
  paused.value = false
  liveEvents.value = []
  pendingEvents.value = []
  startStream()
}

function captureAnchor() {
  const rows = document.querySelectorAll<HTMLElement>('[data-event-key]')
  for (const row of rows) {
    const rect = row.getBoundingClientRect()
    if (rect.bottom > 0) {
      anchorKey = row.dataset.eventKey ?? ''
      anchorOffset = rect.top
      return
    }
  }
  anchorKey = ''
}

function restoreAnchor() {
  if (!anchorKey) return
  const scroller = timelineScroller.value
  const rows = document.querySelectorAll<HTMLElement>('[data-event-key]')
  for (const row of rows) {
    if (row.dataset.eventKey !== anchorKey) continue
    const delta = row.getBoundingClientRect().top - anchorOffset
    // The timeline scrolls inside its own pane, not the document.
    if (scroller) scroller.scrollTop += delta
    else window.scrollBy({ top: delta })
    return
  }
}

function togglePause() {
  if (!live.value) return
  if (!paused.value) {
    paused.value = true
    captureAnchor()
    return
  }
  paused.value = false
  if (pendingEvents.value.length > 0) {
    liveEvents.value = [...pendingEvents.value, ...liveEvents.value].slice(0, LIVE_CAP)
    pendingEvents.value = []
    void nextTick(restoreAnchor)
  }
}

const displayItems = computed<LogEvent[]>(() => {
  if (!live.value) return items.value
  const seen = new Set<string>()
  const merged: LogEvent[] = []
  for (const event of [...liveEvents.value, ...items.value]) {
    const key = eventKey(event)
    if (seen.has(key)) continue
    seen.add(key)
    merged.push(event)
  }
  return merged
})

/* ------------------------------------------------------------------ */
/* Tabs + inner scrolling + infinite scroll                            */
/* ------------------------------------------------------------------ */

/** Scroll panes of the two tabs; both scroll independently of the page. */
const timelineScroller = ref<HTMLElement | null>(null)
const requestScroller = ref<HTMLElement | null>(null)

/**
 * True while the timeline is pinned to its top edge. Live events are
 * prepended, so a reader who scrolled down must not be dragged anywhere: we
 * only keep their visible rows anchored instead of jumping to the newest.
 */
const timelineStickTop = ref(true)

/** Height of the live batch before its DOM update, for anchor preservation. */
let liveBatchHeight = -1
let liveAnchorQueued = false

function applyLiveAnchor() {
  liveAnchorQueued = false
  const el = timelineScroller.value
  if (el && liveBatchHeight >= 0) el.scrollTop += el.scrollHeight - liveBatchHeight
  liveBatchHeight = -1
}

function noteLiveBatch() {
  const el = timelineScroller.value
  if (!el || timelineStickTop.value) return
  if (liveBatchHeight < 0) liveBatchHeight = el.scrollHeight
  if (liveAnchorQueued) return
  liveAnchorQueued = true
  void nextTick(applyLiveAnchor)
}

function onTimelineScroll() {
  const el = timelineScroller.value
  if (!el) return
  timelineStickTop.value = el.scrollTop <= 8
  if (el.scrollTop + el.clientHeight >= el.scrollHeight - SCROLL_TRIGGER_PX) loadMoreTimeline()
}

function onRequestScroll() {
  const el = requestScroller.value
  if (!el) return
  if (el.scrollTop + el.clientHeight >= el.scrollHeight - SCROLL_TRIGGER_PX) loadMoreRequestLogs()
}

function selectTab(next: LogsTab) {
  if (tab.value === next) return
  tab.value = next
  openCategory.value = false
  // The proxy list is only fetched once the user actually opens its tab.
  if (next === 'proxy' && !proxyLoaded.value) void loadProxyLogs(1)
}

/* ------------------------------------------------------------------ */
/* Detail + body                                                       */
/* ------------------------------------------------------------------ */

const expanded = ref('')

const bodyPanel = reactive({
  open: false,
  which: 'req' as LogBodyWhich,
  reqId: '',
  loading: false,
  error: '',
  content: '',
  truncated: false,
})

let bodySeq = 0

function toggleEvent(event: LogEvent) {
  const key = eventKey(event)
  openCategory.value = false
  if (expanded.value === key) {
    expanded.value = ''
    return
  }
  expanded.value = key
  bodyPanel.open = false
}

async function viewBody(event: LogEvent, which: LogBodyWhich) {
  if (!event.req_id || !debugEnabled.value) return
  const token = ++bodySeq
  bodyPanel.open = true
  bodyPanel.which = which
  bodyPanel.reqId = event.req_id
  bodyPanel.loading = true
  bodyPanel.error = ''
  bodyPanel.content = ''
  bodyPanel.truncated = false
  try {
    const data = await api.logs.body(event.req_id, which)
    if (token !== bodySeq) return
    bodyPanel.content = data?.body ?? ''
    bodyPanel.truncated = Boolean(data?.truncated)
    if (data && data.debug === false) bodyPanel.error = '点击顶部工具栏的 Debug 开启正文记录'
  } catch (e) {
    if (token !== bodySeq) return
    if (e instanceof ApiError && e.type === 'debug_disabled') {
      bodyPanel.error = '正文未记录：请用本页顶部工具栏的 Debug 按钮开启后再查看。'
      debugEnabled.value = false
    } else {
      bodyPanel.error = errorMessage(e)
    }
  } finally {
    if (token === bodySeq) bodyPanel.loading = false
  }
}

function closeBody() {
  bodyPanel.open = false
  bodyPanel.content = ''
  bodyPanel.error = ''
}

const bodyText = computed(() => {
  const raw = bodyPanel.content
  if (!raw) return ''
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
})

function downloadBody() {
  if (!bodyPanel.content) return
  const blob = new Blob([bodyPanel.content], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  const safeId = bodyPanel.reqId.replace(/[^a-zA-Z0-9_-]/g, '_') || 'body'
  anchor.href = url
  anchor.download = `req-${safeId}-${bodyPanel.which}.txt`
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  window.setTimeout(() => URL.revokeObjectURL(url), 1000)
}

/* ------------------------------------------------------------------ */
/* Raw line of a timeline event                                        */
/* ------------------------------------------------------------------ */

const rawPanel = reactive({
  open: false,
  key: '',
  /** Which row the panel belongs to: the event's source, or a table row's plugin. */
  label: '',
  loading: false,
  error: '',
  content: '',
})

let rawSeq = 0

/** Which day file holds the event: the local date of its timestamp. */
/** Which raw source holds the event: a database row, or one line of a file. */
function rawFileOf(event: LogEvent): LogRawFile | '' {
  switch (event.type) {
    case 'api':
    case 'audit':
    case 'proxy':
    case 'connectivity':
      return event.type
    default:
      break
  }
  const source = (event.source ?? '').trim()
  if (source.startsWith('plugin:')) return 'plugin'
  return 'host'
}

function rawIdOf(event: LogEvent, file: LogRawFile): string {
  switch (file) {
    case 'api':
      return (event.req_id ?? '').trim()
    case 'audit':
    case 'proxy':
    case 'connectivity':
      return event.row_id ? String(event.row_id) : ''
    case 'plugin':
      return (event.source ?? '').replace(/^plugin:/, '').trim()
    default:
      return (event.source ?? '').trim()
  }
}

/**
 * Reads the single source line behind a timeline event: the literal JSONL/text
 * line for a file source, the row as one JSON object for a database source.
 * A 404 means the row or the day file is gone, not that the page failed.
 */
async function viewRaw(event: LogEvent) {
  const file = rawFileOf(event)
  const id = file ? rawIdOf(event, file) : ''
  await loadRaw({
    key: eventKey(event),
    file,
    id,
    ts: event.ts === undefined ? undefined : String(event.ts),
    summary: file === 'host' || file === 'plugin' ? event.summary : undefined,
    label: event.source || '—',
  })
}

/**
 * loadRaw is the single fetch-and-panel path shared by all three log surfaces.
 *
 * The two table tabs already hold the exact row identifiers the endpoint wants
 * (the request log's `req_id`, the proxy log's `id`), so they can reuse this
 * instead of the timeline's event-derived lookup. Toggling the same key closes
 * the panel, which is what the timeline button already did.
 */
async function loadRaw(request: {
  key: string
  file: LogRawFile | ''
  id: string
  ts?: string
  summary?: string
  label: string
}) {
  if (rawPanel.open && rawPanel.key === request.key) {
    closeRaw()
    return
  }
  rawPanel.open = true
  rawPanel.key = request.key
  rawPanel.label = request.label
  rawPanel.error = ''
  rawPanel.content = ''
  if (!request.file || !request.id) {
    rawPanel.error = '该行没有可用于定位原始记录的标识。'
    return
  }
  const token = ++rawSeq
  rawPanel.loading = true
  try {
    const data = await api.logs.raw({
      file: request.file,
      id: request.id,
      ts: request.ts,
      summary: request.summary,
    })
    if (token !== rawSeq) return
    const line = data?.line ?? data?.text ?? data?.body ?? ''
    if (!line) rawPanel.error = '未找到对应的原始行。'
    else rawPanel.content = line
  } catch (e) {
    if (token !== rawSeq) return
    if (e instanceof ApiError && (e.status === 404 || e.status === 501)) {
      rawPanel.error = '未找到原始行：该行所在的日志文件或数据行已不在保留窗口内。'
    } else {
      rawPanel.error = errorMessage(e)
    }
  } finally {
    if (token === rawSeq) rawPanel.loading = false
  }
}

/** viewRequestRaw opens the raw row of one request-log line. */
function viewRequestRaw(rec: APIRequestLog) {
  void loadRaw({
    key: `api:${rec.req_id ?? ''}:${rec.ts ?? ''}`,
    file: 'api',
    id: (rec.req_id ?? '').trim(),
    ts: rec.ts,
    label: rec.source || '—',
  })
}

/** viewProxyRaw opens the raw row of one proxy-log line. */
function viewProxyRaw(row: ProxyLog) {
  void loadRaw({
    key: `proxy:${row.id ?? ''}:${row.ts ?? ''}`,
    file: 'proxy',
    id: row.id === undefined ? '' : String(row.id),
    ts: row.ts === undefined ? undefined : String(row.ts),
    label: row.plugin_id || '—',
  })
}

/** requestRawKey / proxyRawKey drive the per-row panel visibility check. */
function requestRawKey(rec: APIRequestLog): string {
  return `api:${rec.req_id ?? ''}:${rec.ts ?? ''}`
}

function proxyRawKey(row: ProxyLog): string {
  return `proxy:${row.id ?? ''}:${row.ts ?? ''}`
}

/** rawHint explains what "原始行" means for this event's source. */
function rawHint(event: LogEvent): string {
  const file = rawFileOf(event)
  if (file === 'host' || file === 'plugin') return '只读，取自该事件所在的日志文件'
  return '只读，该事件对应数据行的 JSON'
}

function closeRaw() {
  rawPanel.open = false
  rawPanel.key = ''
  rawPanel.label = ''
  rawPanel.content = ''
  rawPanel.error = ''
}

/* ------------------------------------------------------------------ */
/* Formatting                                                          */
/* ------------------------------------------------------------------ */

function categoryKey(category?: string): string {
  return category && category in CATEGORY_LABELS ? category : 'other'
}

function categoryLabel(category?: string): string {
  if (!category) return '其他'
  return CATEGORY_LABELS[category] || category
}

function statusTone(status?: number): string {
  if (status === undefined || status === 0) return 'muted'
  if (status >= 500) return 'danger'
  if (status >= 400) return 'warn'
  if (status >= 300) return 'muted'
  return 'ok'
}

function logTime(ts?: string | number): string {
  const date = toDate(ts)
  if (!date) return '—'
  const now = new Date()
  const clock = `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
  if (date.toDateString() === now.toDateString()) return clock
  return `${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${clock}`
}

function toggleCategory(value: string) {
  const index = categories.value.indexOf(value)
  if (index >= 0) categories.value.splice(index, 1)
  else categories.value.push(value)
}

function clearCategories() {
  categories.value.splice(0)
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

let keywordTimer: number | undefined

watch(keyword, (value) => {
  if (keywordTimer !== undefined) window.clearTimeout(keywordTimer)
  keywordTimer = window.setTimeout(() => {
    keywordTimer = undefined
    const next = value.trim()
    if (next === q.value) return
    q.value = next
  }, 300)
})

watch([type, level, range, fromDate, toDay, q, () => categories.value.join(',')], applyFilters)

let rlogModelTimer: number | undefined

watch(rlogModel, () => {
  if (rlogModelTimer !== undefined) window.clearTimeout(rlogModelTimer)
  rlogModelTimer = window.setTimeout(() => {
    rlogModelTimer = undefined
    void loadRequestLogs(1)
  }, 300)
})

watch([rlogSource, rlogStatusClass, rlogStream], () => {
  void loadRequestLogs(1)
})

// The request table reuses the shared date range and keyword.
watch([range, fromDate, toDay, q], () => {
  void loadRequestLogs(1)
})

// The proxy table shares the date range and keyword, and adds its own result.
watch([range, fromDate, toDay, q, proxyResult], () => {
  void loadProxyLogs(1)
})

// Changing the page size reloads page 1 of whichever list is on screen and
// resets both hidden cursors, so a list never mixes two page sizes.
watch(pageSize, (value) => {
  writePageSize(value)
  nextCursor.value = ''
  autoPageHops = 0
  void loadTimeline(false)
  void loadRequestLogs(1)
  void loadProxyLogs(1)
})

function onDocumentClick(event: MouseEvent) {
  const target = event.target as HTMLElement | null
  if (!target) return
  if (target.closest('.ms-pop') || target.closest('.ms-btn')) return
  openCategory.value = false
}

function onKeydown(event: KeyboardEvent) {
  if (event.key !== 'Escape') return
  if (bodyPanel.open) {
    closeBody()
    return
  }
  if (rawPanel.open) {
    closeRaw()
    return
  }
  if (expanded.value) {
    expanded.value = ''
    return
  }
  openCategory.value = false
}

onMounted(() => {
  document.addEventListener('click', onDocumentClick)
  document.addEventListener('keydown', onKeydown)
  void loadSettings()
  void loadTimeline(false)
  void loadRequestLogs(1)
  void loadSourceOptions()
})

onBeforeUnmount(() => {
  stopStream()
  document.removeEventListener('click', onDocumentClick)
  document.removeEventListener('keydown', onKeydown)
  if (keywordTimer !== undefined) window.clearTimeout(keywordTimer)
  if (rlogModelTimer !== undefined) window.clearTimeout(rlogModelTimer)
  for (const timer of toastTimers) window.clearTimeout(timer)
  toastTimers.clear()
})
</script>

<template>
  <div class="page logs-page">
    <div v-if="error" class="notice error">
      <span>读取日志失败：{{ error }}</span>
      <button type="button" class="btn sm ghost" @click="reloadTimeline">重试</button>
    </div>

    <div v-if="live && reconnecting" class="notice warn">
      <span>实时连接已断开，正在自动重连…</span>
      <button type="button" class="btn sm ghost" @click="startStream">立即重连</button>
    </div>

    <div v-if="debugKnown && debugEnabled" class="notice warn debug-body-warning" role="status">
      <span><strong>调试正文已开启</strong>：请求与响应正文会写进日志，管理操作也显示真实 id。排障完请关闭。</span>
      <button type="button" class="btn sm ghost" @click="toggleDebug">关闭 Debug</button>
    </div>

    <section class="card logs-card">
      <div class="card-head logs-head">
        <h2>{{ TAB_LABELS[tab] }}</h2>
        <span v-if="tab === 'requests'" class="sub">
          沿用上方时间范围与关键字 · 最多导出 5 万条
        </span>
        <span v-else-if="tab === 'proxy'" class="sub">
          每次经代理建立一条隧道记一行：谁（插件/账号）连了哪个目标、结果、耗时与字节
        </span>
        <span v-else class="sub">点击行展开详情 · 正文仅供排障</span>
      </div>

      <div class="card-body tight logs-toolbar">
        <div class="filterbar logs-sticky">
          <div class="seg" role="tablist" aria-label="日志视图">
            <button
              v-for="item in TABS"
              :key="item.key"
              type="button"
              class="seg-btn"
              :class="{ active: tab === item.key }"
              :aria-selected="tab === item.key"
              @click="selectTab(item.key)"
            >
              {{ item.label }}
            </button>
          </div>

          <div class="seg" role="group" aria-label="时间范围">
            <button
              v-for="item in RANGES"
              :key="item.key"
              type="button"
              class="seg-btn"
              :class="{ active: range === item.key }"
              :aria-pressed="range === item.key"
              @click="range = item.key"
            >
              {{ item.label }}
            </button>
          </div>

          <div v-if="range === 'custom'" class="date-range">
            <input v-model="fromDate" class="input input-date" type="date" aria-label="开始日期" />
            <span class="muted small">至</span>
            <input v-model="toDay" class="input input-date" type="date" aria-label="结束日期" />
          </div>

          <div class="field grow log-keyword">
            <input
              v-model="keyword"
              class="input"
              type="search"
              placeholder="关键字：路径 / 模型 / 账号 / req_id"
              aria-label="关键字"
            />
          </div>

          <label class="page-size">
            <span class="small muted">每页</span>
            <select v-model.number="pageSize" class="select select-compact" aria-label="每页条数">
              <option v-for="size in PAGE_SIZE_OPTIONS" :key="size" :value="size">{{ size }}</option>
            </select>
          </label>

          <div class="seg" role="group" aria-label="显示格式">
            <button
              v-for="item in VIEWS"
              :key="item.key"
              type="button"
              class="seg-btn"
              :class="{ active: view === item.key }"
              :aria-pressed="view === item.key"
              @click="view = item.key"
            >
              {{ item.label }}
            </button>
          </div>

          <button
            v-if="view === 'text'"
            type="button"
            class="btn sm ghost"
            :disabled="!currentText"
            :title="currentText ? '复制本页文本' : '本页没有内容，无法复制'"
            @click="copyCurrentText"
          >
            复制本页
          </button>

          <button
            type="button"
            class="btn ghost"
            :class="debugEnabled ? '' : 'muted'"
            :title="debugEnabled
              ? '正文已记录、管理操作显示真实 id（点击关闭）'
              : '点击开启：记录请求/响应正文，并在管理操作里显示真实 id'"
            @click="toggleDebug"
          >
            Debug {{ debugKnown ? (debugEnabled ? '开' : '关') : '…' }}
          </button>
        </div>

        <div v-if="tab === 'requests'" class="filterbar logs-subbar">
          <span class="logs-note small muted">本表仅含被代理的 API 请求；运行事件见「事件时间线」。</span>
          <div class="field grow log-keyword">
            <input
              v-model="rlogModel"
              class="input"
              type="search"
              placeholder="模型（模糊匹配）"
              aria-label="模型"
            />
          </div>

          <select v-model="rlogSource" class="select select-compact" aria-label="来源">
            <option value="">全部来源</option>
            <option v-for="item in sourceOptions" :key="item.value" :value="item.value">
              {{ item.label }}
            </option>
          </select>

          <select v-model="rlogStatusClass" class="select select-compact" aria-label="状态">
            <option v-for="item in STATUS_CLASSES" :key="item.value" :value="item.value">
              {{ item.label }}
            </option>
          </select>

          <select v-model="rlogStream" class="select select-compact" aria-label="流式">
            <option v-for="item in STREAM_OPTIONS" :key="item.value" :value="item.value">
              {{ item.label }}
            </option>
          </select>

          <span class="spacer"></span>
          <button
            type="button"
            class="btn"
            :disabled="rlogLoading"
            @click="reloadRequestLogs"
          >
            {{ rlogLoading ? '刷新中…' : '刷新' }}
          </button>
          <button type="button" class="btn ghost" @click="clearRequestLogFilters">清除筛选</button>
          <a class="btn primary" :href="rlogExportUrl" download>导出 CSV</a>
        </div>

        <div v-else-if="tab === 'proxy'" class="filterbar logs-subbar">
          <span class="logs-note small muted">
            隧道只记元数据：目标是域名与端口，被代理的 HTTPS 正文不可见，代理凭据也从不落盘。
          </span>
          <div class="field grow log-keyword">
            <input
              v-model="keyword"
              class="input"
              type="search"
              placeholder="目标域名 / 插件 / 账号 / 错误"
              aria-label="代理日志关键字"
            />
          </div>

          <select v-model="proxyResult" class="select select-compact" aria-label="结果">
            <option v-for="item in PROXY_RESULTS" :key="item.value" :value="item.value">
              {{ item.label }}
            </option>
          </select>

          <span class="spacer"></span>
          <button type="button" class="btn" :disabled="proxyLoading" @click="reloadProxyLogs">
            {{ proxyLoading ? '刷新中…' : '刷新' }}
          </button>
          <button type="button" class="btn ghost" @click="clearProxyFilters">清除筛选</button>
        </div>

        <div v-else class="filterbar logs-subbar">
          <div class="seg" role="group" aria-label="日志类型">
            <button
              v-for="item in TYPES"
              :key="item.key"
              type="button"
              class="seg-btn"
              :class="{ active: type === item.key }"
              :aria-pressed="type === item.key"
              @click="type = item.key"
            >
              {{ item.label }}
            </button>
          </div>

          <div class="ms">
            <button
              type="button"
              class="btn ms-btn"
              :class="{ 'is-active': categories.length > 0, open: openCategory }"
              :aria-expanded="openCategory"
              @click="openCategory = !openCategory"
            >
              分类
              <span v-if="categories.length > 0" class="ms-count">{{ categories.length }}</span>
              <span class="ms-caret" aria-hidden="true">▾</span>
            </button>
            <div v-if="openCategory" class="ms-pop" role="group" aria-label="日志分类">
              <label v-for="item in CATEGORIES" :key="item.value" class="ms-option">
                <input
                  type="checkbox"
                  :checked="categories.includes(item.value)"
                  @change="toggleCategory(item.value)"
                />
                <span class="ms-option-label">{{ item.label }}</span>
              </label>
              <div class="ms-foot">
                <button
                  type="button"
                  class="btn sm ghost"
                  :disabled="categories.length === 0"
                  @click="clearCategories"
                >
                  清空
                </button>
                <button type="button" class="btn sm ghost" @click="openCategory = false">完成</button>
              </div>
            </div>
          </div>

          <select v-model="level" class="select select-compact" aria-label="日志级别">
            <option value="">全部级别</option>
            <option value="info">info</option>
            <option value="warn">warn</option>
            <option value="error">error</option>
          </select>

          <span class="spacer"></span>
          <label class="switch live-switch">
            <input type="checkbox" :checked="live" @change="toggleLive" />
            <span class="switch-track"></span>
            实时
          </label>
          <button v-if="live" type="button" class="btn" @click="togglePause">
            {{ paused ? '继续' : '暂停' }}
          </button>
          <button type="button" class="btn" :disabled="loading" @click="reloadTimeline">
            {{ loading ? '刷新中…' : '刷新' }}
          </button>
        </div>

        <div class="toolbar-hint small muted">
          <template v-if="tab === 'requests'">
            <span>共 {{ formatNumber(rlogTotal) }} 条 · 已加载 {{ formatNumber(rlogItems.length) }} 条</span>
            <span v-if="rlogLoadingMore">· 加载中…</span>
          </template>
          <template v-else-if="tab === 'proxy'">
            <span>共 {{ formatNumber(proxyTotal) }} 条隧道 · 已加载 {{ formatNumber(proxyItems.length) }} 条</span>
            <span v-if="proxyLoadingMore">· 加载中…</span>
          </template>
          <template v-else>
            <span v-if="live" class="live-state">
              <span class="live-dot" :class="streamState"></span>
              {{
                streamState === 'open'
                  ? '实时接收中'
                  : streamState === 'connecting'
                    ? '连接中…'
                    : streamState === 'reconnecting'
                      ? '重连中…'
                      : '未连接'
              }}
              <template v-if="paused"> · 已暂停（缓冲 {{ pendingEvents.length }} 条）</template>
              <template v-else-if="liveEvents.length > 0"> · 内存 {{ liveEvents.length }} 条</template>
            </span>
            <span v-else>历史时间线 · 每页 {{ pageSize }} 条 · 已加载 {{ formatNumber(items.length) }} 条</span>
            <span v-if="timelineTruncated" class="muted">· 部分来源已读到本轮上限，继续滚动即可</span>
            <span v-if="loadingMore">· 加载中…</span>
          </template>
        </div>
      </div>

      <div v-if="tab === 'requests'" class="logs-body">
        <div v-if="rlogError" class="notice error">
          <span>读取请求日志失败：{{ rlogError }}</span>
          <button type="button" class="btn sm ghost" :disabled="rlogLoading" @click="reloadRequestLogs">重试</button>
        </div>

        <div v-if="rlogLoading && rlogItems.length === 0" class="log-skeleton" aria-hidden="true">
          <span v-for="n in 6" :key="n" class="skeleton skeleton-line"></span>
        </div>

        <div v-else-if="rlogLoaded && rlogItems.length === 0" class="empty">
          <strong>没有匹配的请求日志</strong>
          <span>调整时间范围、模型、来源、状态或流式筛选后再试。</span>
        </div>

        <div
          v-else-if="view === 'table'"
          ref="requestScroller"
          class="table-wrap logs-scroll"
          @scroll.passive="onRequestScroll"
        >
          <table class="table logs-table">
            <thead>
              <tr>
                <th>时间</th>
                <th>req_id</th>
                <th>来源</th>
                <th>模型</th>
                <th>账号</th>
                <th>Key</th>
                <th>流式</th>
                <th>状态</th>
                <th>延迟</th>
                <th v-if="debugEnabled">错误</th>
                <th class="col-raw">原始行</th>
              </tr>
            </thead>
            <tbody>
              <template v-for="rec in rlogItems" :key="`${rec.req_id}-${rec.ts}`">
                <tr>
                  <td class="num" :title="formatTimestamp(rec.ts)">{{ logTime(rec.ts) }}</td>
                  <td class="mono truncate col-reqid" :title="rec.req_id || ''">{{ rec.req_id || '—' }}</td>
                  <td class="mono truncate col-source" :title="rec.source || ''">{{ rec.source || '—' }}</td>
                  <td class="truncate" :title="rec.model || ''">{{ rec.model || '—' }}</td>
                  <td class="truncate" :title="rec.account || ''">{{ rec.account || '—' }}</td>
                  <td class="truncate col-key" :title="rlogKey(rec)">{{ rlogKey(rec) }}</td>
                  <td>{{ rec.stream ? '是' : '否' }}</td>
                  <td>
                    <span class="badge plain" :class="statusTone(rec.status)">{{ rec.status ?? '—' }}</span>
                  </td>
                  <td class="num">{{ formatLatency(rec.latency_ms) }}</td>
                  <td v-if="debugEnabled" class="truncate" :title="rec.error || ''">{{ rec.error || '—' }}</td>
                  <td class="col-raw">
                    <button
                      type="button"
                      class="btn sm ghost"
                      :title="rec.req_id ? '查看这条请求在 api 日志里的原始记录' : '这一行没有 req_id，无法定位原始记录'"
                      @click="viewRequestRaw(rec)"
                    >
                      {{ rawPanel.open && rawPanel.key === requestRawKey(rec) ? '收起' : '原始行' }}
                    </button>
                  </td>
                </tr>
                <tr v-if="rawPanel.open && rawPanel.key === requestRawKey(rec)">
                  <td :colspan="debugEnabled ? 11 : 10">
                    <div class="body-panel">
                      <div class="raw-metrics small muted">
                        <span>状态 {{ rec.status ?? '—' }}</span>
                        <span>延迟 {{ formatLatency(rec.latency_ms) }}</span>
                        <span>TTFB {{ rec.ttfb_ms === undefined || rec.ttfb_ms === 0 ? '—' : formatLatency(rec.ttfb_ms) }}</span>
                        <span>Token {{ rlogTokens(rec) }}</span>
                      </div>
                      <div class="row between">
                        <strong class="small">
                          原始行
                          <span class="mono muted small">· {{ rawPanel.label || rec.source || '—' }}</span>
                        </strong>
                        <div class="row">
                          <button
                            type="button"
                            class="btn sm"
                            :disabled="!rawPanel.content"
                            :title="rawPanel.loading ? '原始行读取中，请稍候' : '没有可复制的原始行内容'"
                            @click="copyText(rawPanel.content, '原始行已复制')"
                          >
                            复制
                          </button>
                          <button type="button" class="btn sm ghost" @click="closeRaw">收起</button>
                        </div>
                      </div>
                      <div v-if="rawPanel.loading" class="loading">读取原始行…</div>
                      <div v-else-if="rawPanel.error" class="notice error">{{ rawPanel.error }}</div>
                      <pre v-else class="log-view raw-view">{{ rawPanel.content || '（空行）' }}</pre>
                    </div>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
          <div v-if="rlogLoadingMore" class="logs-footer">加载中…</div>
          <div v-else-if="!rlogHasMore" class="logs-footer">没有更多了</div>
        </div>

        <pre v-else class="log-view text-dump">{{ requestsText || '（本页没有内容）' }}</pre>
      </div>

      <div v-else-if="tab === 'proxy'" class="logs-body">
        <div v-if="proxyError" class="notice error">
          <span>读取代理日志失败：{{ proxyError }}</span>
          <button type="button" class="btn sm ghost" :disabled="proxyLoading" @click="reloadProxyLogs">重试</button>
        </div>

        <div v-if="proxyLoading && proxyItems.length === 0" class="log-skeleton" aria-hidden="true">
          <span v-for="n in 6" :key="n" class="skeleton skeleton-line"></span>
        </div>

        <div v-else-if="proxyLoaded && proxyItems.length === 0" class="empty">
          <strong>还没有代理流量记录</strong>
          <span>
            只有「账号绑定了上游代理」时才会产生隧道记录：插件经本机 127.0.0.1 监听发起
            CONNECT，宿主为每个「插件 + 账号 + 代理」各开一个监听，因此每条记录都能归到具体账号。
            直连（不挂代理）的请求不会出现在这里，可在「事件时间线」的插件日志里查看。
          </span>
        </div>

        <div
          v-else-if="view === 'table'"
          ref="proxyScroller"
          class="table-wrap logs-scroll"
          @scroll.passive="onProxyScroll"
        >
          <table class="table">
            <thead>
              <tr>
                <th>时间</th>
                <th>插件 / 账号</th>
                <th>代理</th>
                <th>目标</th>
                <th>结果</th>
                <th>耗时</th>
                <th>上行 / 下行</th>
                <th>错误</th>
                <th class="col-raw">原始行</th>
              </tr>
            </thead>
            <tbody>
              <template v-for="row in proxyItems" :key="`${row.id}-${row.ts}`">
                <tr>
                  <td class="num" :title="formatTimestamp(row.ts)">{{ logTime(row.ts) }}</td>
                  <td class="mono truncate" :title="proxyIdentity(row)">{{ proxyIdentity(row) }}</td>
                  <td class="truncate" :title="row.proxy_label || row.proxy_id || ''">
                    {{ row.proxy_label || row.proxy_id || '—' }}
                  </td>
                  <td class="mono truncate" :title="proxyTarget(row)">{{ proxyTarget(row) }}</td>
                  <td>
                    <span class="badge plain" :class="proxyResultTone(row.result)">
                      {{ proxyResultLabel(row.result) }}
                    </span>
                  </td>
                  <td class="num">{{ formatLatency(row.duration_ms) }}</td>
                  <td class="num">
                    {{ formatBytes(row.bytes_up) }} / {{ formatBytes(row.bytes_down) }}
                  </td>
                  <td class="truncate" :title="row.error || ''">{{ row.error || '—' }}</td>
                  <td class="col-raw">
                    <button
                      type="button"
                      class="btn sm ghost"
                      :title="row.id !== undefined ? '查看这条隧道在 proxy 日志里的原始记录' : '这一行没有 id，无法定位原始记录'"
                      @click="viewProxyRaw(row)"
                    >
                      {{ rawPanel.open && rawPanel.key === proxyRawKey(row) ? '收起' : '原始行' }}
                    </button>
                  </td>
                </tr>
                <tr v-if="rawPanel.open && rawPanel.key === proxyRawKey(row)">
                  <td colspan="9">
                    <div class="body-panel">
                      <div class="row between">
                        <strong class="small">
                          原始行
                          <span class="mono muted small">· {{ rawPanel.label || row.plugin_id || '—' }}</span>
                        </strong>
                        <div class="row">
                          <button
                            type="button"
                            class="btn sm"
                            :disabled="!rawPanel.content"
                            :title="rawPanel.loading ? '原始行读取中，请稍候' : '没有可复制的原始行内容'"
                            @click="copyText(rawPanel.content, '原始行已复制')"
                          >
                            复制
                          </button>
                          <button type="button" class="btn sm ghost" @click="closeRaw">收起</button>
                        </div>
                      </div>
                      <div v-if="rawPanel.loading" class="loading">读取原始行…</div>
                      <div v-else-if="rawPanel.error" class="notice error">{{ rawPanel.error }}</div>
                      <pre v-else class="log-view raw-view">{{ rawPanel.content || '（空行）' }}</pre>
                    </div>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
          <div v-if="proxyLoadingMore" class="logs-footer">加载中…</div>
          <div v-else-if="!proxyHasMore" class="logs-footer">没有更多了</div>
        </div>

        <pre v-else class="log-view text-dump">{{ proxyText || '（本页没有内容）' }}</pre>
      </div>

      <div v-else class="logs-body">
        <div v-if="(loading || loadingMore) && displayItems.length === 0" class="log-skeleton" aria-hidden="true">
          <span v-for="n in 8" :key="n" class="skeleton skeleton-line"></span>
        </div>

        <div v-else-if="displayItems.length === 0 && !hasMore" class="empty">
          <strong>没有匹配的日志</strong>
          <span>调整时间范围、分类、级别或关键字后再试。</span>
        </div>

        <div
          v-else-if="view === 'table'"
          ref="timelineScroller"
          class="event-list logs-scroll"
          @scroll.passive="onTimelineScroll"
        >
          <div
            v-for="event in displayItems"
            :key="eventKey(event)"
            class="event"
            :class="{ open: expanded === eventKey(event) }"
          >
            <button
              type="button"
              class="event-row"
              :data-event-key="eventKey(event)"
              :aria-expanded="expanded === eventKey(event)"
              @click="toggleEvent(event)"
            >
              <span class="event-time" :title="formatTimestamp(event.ts)">{{ logTime(event.ts) }}</span>
              <span class="cat-chip" :class="`cat-${categoryKey(event.category)}`">
                {{ categoryLabel(event.category) }}
              </span>
              <span class="event-source" :title="event.source || ''">{{ event.source || '—' }}</span>
              <span class="event-summary" :title="event.summary || ''">{{ event.summary || '（无摘要）' }}</span>
              <span class="event-meta">
                <template v-if="event.type === 'api'">
                  <span class="badge plain" :class="statusTone(event.status)">{{ event.status ?? '—' }}</span>
                  <span v-if="event.latency_ms !== undefined">{{ formatLatency(event.latency_ms) }}</span>
                  <span v-if="event.account" class="event-account truncate" :title="event.account">{{ event.account }}</span>
                  <span v-if="event.model" class="event-model truncate" :title="event.model">{{ event.model }}</span>
                </template>
                <span
                  v-if="event.level && event.level !== 'info'"
                  class="badge plain"
                  :class="event.level === 'error' ? 'danger' : 'warn'"
                >
                  {{ event.level }}
                </span>
                <span class="event-caret" aria-hidden="true">›</span>
              </span>
            </button>

            <div v-if="expanded === eventKey(event)" class="event-detail">
              <dl class="log-kv">
                <dt>时间</dt>
                <dd class="num">{{ formatTimestamp(event.ts) }}</dd>
                <dt>类型</dt>
                <dd>
                  {{ event.type === 'api' ? 'API 请求' : event.type === 'runtime' ? '运行日志' : event.type || '—' }}
                </dd>
                <dt>分类</dt>
                <dd>{{ categoryLabel(event.category) }}</dd>
                <dt>级别</dt>
                <dd>{{ event.level || '—' }}</dd>
                <dt>来源</dt>
                <dd class="mono">{{ event.source || '—' }}</dd>
                <dt>req_id</dt>
                <dd class="mono">{{ event.req_id || '—' }}</dd>
                <dt>账号</dt>
                <dd>{{ event.account || '—' }}</dd>
                <dt>模型</dt>
                <dd>{{ event.model || '—' }}</dd>
                <dt>状态</dt>
                <dd>{{ event.status ?? '—' }}</dd>
                <dt>耗时</dt>
                <dd>{{ formatLatency(event.latency_ms) }}</dd>
              </dl>

              <p class="detail-summary">
                <span class="muted">摘要</span>
                {{ event.summary || '—' }}
              </p>

              <div v-if="event.req_id" class="body-actions">
                <button
                  type="button"
                  class="btn sm"
                  :disabled="!debugEnabled"
                  :title="debugEnabled ? '' : '点击顶部工具栏的 Debug 开启正文记录'"
                  @click="viewBody(event, 'req')"
                >
                  查看请求正文
                </button>
                <button
                  type="button"
                  class="btn sm"
                  :disabled="!debugEnabled"
                  :title="debugEnabled ? '' : '点击顶部工具栏的 Debug 开启正文记录'"
                  @click="viewBody(event, 'resp')"
                >
                  查看响应正文
                </button>
                <span v-if="!debugEnabled" class="muted small">本页工具栏的 Debug 开启后可查看正文</span>
              </div>

              <div v-if="rawFileOf(event)" class="body-actions">
                <button type="button" class="btn sm" @click="viewRaw(event)">查看原始行</button>
                <span class="muted small">{{ rawHint(event) }}</span>
              </div>

              <div v-if="rawPanel.open && rawPanel.key === eventKey(event)" class="body-panel">
                <div class="row between">
                  <strong class="small">
                    原始行
                    <span class="mono muted small">· {{ rawPanel.label || event.source || '—' }}</span>
                  </strong>
                  <div class="row">
                    <button
                      type="button"
                      class="btn sm"
                      :disabled="!rawPanel.content"
                      :title="rawPanel.loading ? '原始行读取中，请稍候' : '没有可复制的原始行内容'"
                      @click="copyText(rawPanel.content, '原始行已复制')"
                    >
                      复制
                    </button>
                    <button type="button" class="btn sm ghost" @click="closeRaw">收起</button>
                  </div>
                </div>
                <div v-if="rawPanel.loading" class="loading">读取原始行…</div>
                <div v-else-if="rawPanel.error" class="notice error">{{ rawPanel.error }}</div>
                <pre v-else class="log-view raw-view">{{ rawPanel.content || '（空行）' }}</pre>
              </div>

              <div v-if="bodyPanel.open && bodyPanel.reqId === event.req_id" class="body-panel">
                <div class="row between">
                  <strong class="small">
                    {{ bodyPanel.which === 'req' ? '请求正文' : '响应正文' }}
                    <span class="mono muted small">· {{ bodyPanel.reqId }}</span>
                  </strong>
                  <div class="row">
                    <span v-if="bodyPanel.truncated" class="badge warn plain">已截断</span>
                    <button
                      type="button"
                      class="btn sm"
                      :disabled="!bodyPanel.content"
                      :title="bodyPanel.loading
                        ? '正文读取中，请稍候'
                        : '没有可复制的正文：未开启 Debug 或该请求正文为空'"
                      @click="copyText(bodyText, '正文已复制')"
                    >
                      复制
                    </button>
                    <button type="button" class="btn sm" :disabled="!bodyPanel.content" @click="downloadBody">
                      下载
                    </button>
                    <button type="button" class="btn sm ghost" @click="closeBody">收起</button>
                  </div>
                </div>
                <div v-if="bodyPanel.loading" class="loading">读取正文…</div>
                <div v-else-if="bodyPanel.error" class="notice error">{{ bodyPanel.error }}</div>
                <pre v-else class="log-view body-view">{{ bodyText || '（空正文）' }}</pre>
              </div>
            </div>
          </div>
          <div v-if="loadingMore" class="logs-footer">加载中…</div>
          <div v-else-if="!live && !hasMore" class="logs-footer">没有更多了</div>
        </div>

        <pre v-else class="log-view text-dump">{{ timelineText || '（本页没有内容）' }}</pre>
      </div>
    </section>

    <div class="toasts" role="status" aria-live="polite">
      <div v-for="item in toasts" :key="item.id" class="toast" :class="item.tone">{{ item.text }}</div>
    </div>
  </div>
</template>
