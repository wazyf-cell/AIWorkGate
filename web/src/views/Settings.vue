<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  api,
  DEFAULT_SETTINGS,
  errorMessage,
  type ClientKey,
  type DiagnosticsKind,
  type DiagnosticsProbe,
  type NotifyChannel,
  type NotifyEventKind,
  type NotifyStatus,
  type NotifyUpdate,
} from '../api'
import { formatLatency, formatTimestamp } from '../format'
import CustomModelsPanel from '../components/CustomModelsPanel.vue'

const defaults = DEFAULT_SETTINGS

/* Top-level modules. Cards stay mounted so unsaved edits survive a tab
 * switch; the active module lives in ?tab= so reloads and links keep it. */
const SETTINGS_TABS = [
  { id: 'gateway', label: '接入与网关', hint: '监听端口、A 接入、上游代理与跨源' },
  { id: 'client', label: '客户端', hint: '客户端 Key、自定义接入与模型' },
  { id: 'plugin', label: '插件与签到', hint: '插件运行参数、签到时间与重试' },
  { id: 'ops', label: '通知与日志', hint: '系统通知、日志保留与连通性诊断' },
] as const
type SettingsTabId = (typeof SETTINGS_TABS)[number]['id']
const settingsTabIds = SETTINGS_TABS.map((tab) => tab.id) as SettingsTabId[]

function settingsTabFromQuery(raw: unknown): SettingsTabId {
  const value = Array.isArray(raw) ? raw[0] : raw
  return settingsTabIds.includes(value as SettingsTabId) ? (value as SettingsTabId) : 'gateway'
}

const route = useRoute()
const router = useRouter()
const activeTab = ref<SettingsTabId>(settingsTabFromQuery(route.query.tab))
const activeTabHint = computed(() => SETTINGS_TABS.find((tab) => tab.id === activeTab.value)?.hint ?? '')

watch(
  () => route.query.tab,
  (raw) => {
    const next = settingsTabFromQuery(raw)
    if (next !== activeTab.value) activeTab.value = next
  },
)

function selectTab(id: SettingsTabId) {
  if (activeTab.value === id) return
  activeTab.value = id
  void router.replace({ query: { ...route.query, tab: id } })
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

const form = reactive({
  port: defaults.port,
  listen_lan: defaults.listen_lan,
  autostart: defaults.autostart,
  a_base_url: defaults.a.base_url,
  a_exe_path: defaults.a.exe_path,
  a_auto_start: defaults.a.auto_start,
  a_model_cache_minutes: defaults.a.model_cache_minutes,
  proxy_enabled: defaults.proxy.enabled,
  proxy_http: defaults.proxy.http,
  proxy_https: defaults.proxy.https,
  proxy_no_proxy: defaults.proxy.no_proxy.join(', '),
  egress_direct_logging: defaults.egress.direct_logging,
  plugins_concurrency: defaults.plugins.concurrency,
  plugins_startup_mode: defaults.plugins.startup_mode,
  plugins_strict_egress: defaults.plugins.strict_egress,
  checkin_default_time: defaults.checkin.default_time,
  checkin_retry_count: defaults.checkin.retry_count,
  checkin_retry_backoff: defaults.checkin.retry_backoff_seconds.join(', '),
  retention_api_request_days: defaults.retention.api_request_days,
  retention_body_preview_days: defaults.retention.body_preview_days,
  retention_checkin_days: defaults.retention.checkin_days,
  retention_host_log_days: defaults.retention.host_log_days,
  retention_proxy_log_days: defaults.retention.proxy_log_days,
  logging_debug_body: defaults.logging.debug_body,
  gateway_cors: '',
})

/** Last raw payload from the host; keeps unknown keys for the round trip. */
const loaded = ref<Record<string, unknown>>({})
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const message = ref('')
/** Inline failure of the retry-backoff field; the global 保存设置 sits remotely. */
const backoffError = ref('')

const aKeyConfigured = ref(false)
const aKeyAvailable = ref(true)
const aKeyInput = ref('')
const aKeyBusy = ref(false)

const lanPassword = ref('')
const lanBusy = ref(false)

const clientKeys = ref<ClientKey[]>([])
const pluginIDs = ref<string[]>([])
const newKeyName = ref('')
const newKeyScope = ref('')
const newKeyBindPool = ref('')
const newKeyQuota = ref(0)
const keyBusy = ref(false)
const createdSecret = ref('')
const hostPort = ref(7865)

/** 通知（F-19）：策略走 /api/notify，URL 是只写密钥，读回只有 url_set。 */
const NOTIFY_CHANNELS: { value: NotifyChannel; label: string; hint: string; placeholder: string }[] = [
  {
    value: 'generic',
    label: '通用 Webhook（JSON）',
    hint: '向地址 POST {"title","body","kind","at"}，适合自建接收端。',
    placeholder: 'https://example.com/hooks/aiworkgate',
  },
  {
    value: 'bark',
    label: 'Bark（iOS）',
    hint: '填写 https://api.day.app/<device_key>，标题与内容会拼在地址路径中。',
    placeholder: 'https://api.day.app/你的DeviceKey',
  },
  {
    value: 'serverchan',
    label: 'Server酱',
    hint: '填写完整端点 https://sctapi.ftqq.com/<sendkey>.send。',
    placeholder: 'https://sctapi.ftqq.com/你的SendKey.send',
  },
  {
    value: 'wecom',
    label: '企业微信群机器人',
    hint: '填写群机器人 Webhook 地址，以文本消息发送。',
    placeholder: 'https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...',
  },
]

const NOTIFY_EVENTS: { value: NotifyEventKind; label: string }[] = [
  { value: 'checkin_fail', label: '签到失败' },
  { value: 'checkin_done', label: '签到完成' },
  { value: 'account_dead', label: '账号失效' },
  { value: 'quota_low', label: '额度不足' },
]

const notifyForm = reactive({
  enabled: false,
  channel: 'generic' as NotifyChannel,
  events: [] as NotifyEventKind[],
  min_interval_seconds: 600,
})
const notifyURL = ref('')
const notifyURLSet = ref(false)
const notifyLastSentAt = ref(0)
const notifyLastError = ref('')
const notifySaving = ref(false)
const notifyTesting = ref(false)

/* The page-wide save lives in the module bar now, so track whether the form
 * differs from the last loaded/saved payload. Notify has its own save flow. */
const savedSignature = ref('')

function settingsSignature(): string {
  return JSON.stringify(form)
}

const settingsDirty = computed(
  () => savedSignature.value !== '' && settingsSignature() !== savedSignature.value,
)

const notifyChannel = computed(
  () => NOTIFY_CHANNELS.find((item) => item.value === notifyForm.channel) ?? NOTIFY_CHANNELS[0],
)
const notifyChannelHint = computed(() => notifyChannel.value.hint)
const notifyURLPlaceholder = computed(() =>
  notifyURLSet.value
    ? '已保存通知地址（留空保持不变）'
    : notifyChannel.value.placeholder,
)

function notifyConfigFromForm() {
  return {
    enabled: notifyForm.enabled,
    channel: notifyForm.channel,
    events: [...notifyForm.events],
    min_interval_seconds: Number(notifyForm.min_interval_seconds) || 600,
  }
}

function applyNotifyStatus(status: NotifyStatus) {
  notifyForm.enabled = Boolean(status?.enabled)
  notifyForm.channel = status?.channel ?? 'generic'
  notifyForm.events = Array.isArray(status?.events) ? [...status.events] : []
  notifyForm.min_interval_seconds = Number(status?.min_interval_seconds) || 600
  notifyURLSet.value = Boolean(status?.url_set)
  notifyLastSentAt.value = Number(status?.last_sent_at) || 0
  notifyLastError.value = status?.last_error ?? ''
}

async function loadNotify() {
  try {
    applyNotifyStatus(await api.notify.get())
  } catch (e) {
    error.value = errorMessage(e)
  }
}

/** 只刷新“最近发送/错误/是否已存地址”，避免覆盖尚未保存的表单。 */
async function refreshNotifyState() {
  try {
    const status = await api.notify.get()
    notifyURLSet.value = Boolean(status?.url_set)
    notifyLastSentAt.value = Number(status?.last_sent_at) || 0
    notifyLastError.value = status?.last_error ?? ''
  } catch {
    // 状态刷新失败不打断主流程，下一次保存或进入页面会重试。
  }
}

async function saveNotify() {
  const interval = Number(notifyForm.min_interval_seconds)
  if (!Number.isInteger(interval) || interval < 60 || interval > 86400) {
    error.value = '最小发送间隔必须是 60–86400 秒之间的整数。'
    return
  }
  const update: NotifyUpdate = {
    enabled: notifyForm.enabled,
    channel: notifyForm.channel,
    events: [...notifyForm.events],
    min_interval_seconds: interval,
  }
  const url = notifyURL.value.trim()
  if (url) {
    try {
      const parsed = new URL(url)
      if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
        error.value = '通知地址必须是完整的 http(s) URL。'
        return
      }
    } catch {
      error.value = '通知地址必须是完整的 http(s) URL。'
      return
    }
    update.url = url
  }
  notifySaving.value = true
  error.value = ''
  message.value = ''
  try {
    applyNotifyStatus(await api.notify.update(update))
    // Keep the round-trip payload in sync so the page-wide 保存设置 does not
    // write a stale notify block back to config.json.
    loaded.value = { ...loaded.value, notify: notifyConfigFromForm() }
    notifyURL.value = ''
    message.value = '通知设置已保存，下一次事件推送即按新配置发送。'
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    notifySaving.value = false
  }
}

async function clearNotifyURL() {
  notifySaving.value = true
  error.value = ''
  message.value = ''
  try {
    applyNotifyStatus(await api.notify.update({ url: '' }))
    notifyURL.value = ''
    message.value = '已清除保存的通知地址。'
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    notifySaving.value = false
  }
}

async function testNotify() {
  notifyTesting.value = true
  error.value = ''
  message.value = ''
  try {
    await api.notify.test()
    message.value = '测试通知已发送：桌面系统通知应已弹出，已配置地址时请一并检查接收端。'
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    await refreshNotifyState()
    notifyTesting.value = false
  }
}

/** Scope/quota editor (plan 2026-10-21 §1). */
const editingKey = ref<ClientKey | null>(null)
const keyEditBusy = ref(false)
const keyDraft = reactive({
  name: '',
  enabled: true,
  model_scope: '',
  bind_pool: '',
  daily_quota: 0,
})

async function loadClientKeys() {
  try {
    clientKeys.value = (await api.keys.list()) ?? []
  } catch (e) {
    error.value = errorMessage(e)
  }
}

/** Installed plugin ids feed the "绑定插件" selects. */
async function loadPluginIDs() {
  try {
    pluginIDs.value = ((await api.plugins.list()) ?? [])
      .map((plugin) => plugin.manifest?.id ?? '')
      .filter((id): id is string => Boolean(id))
  } catch {
    pluginIDs.value = []
  }
}

/** One scope entry per line or comma-separated; empty means every model. */
function parseModelScope(text: string): string[] {
  return text
    .split(/[\n,]/)
    .map((entry) => entry.trim())
    .filter(Boolean)
}

function normalizeQuota(value: unknown): number {
  const quota = Math.floor(Number(value) || 0)
  return quota < 0 ? 0 : quota
}

function keyScopeLabel(item: ClientKey): string {
  const parts: string[] = []
  const scope = item.model_scope ?? []
  if (scope.length) parts.push(scope.join('、'))
  if (item.bind_pool) parts.push(`仅插件 ${item.bind_pool}`)
  return parts.length ? parts.join('；') : '不限'
}

function keyUsageLabel(item: ClientKey): string {
  const used = item.used_today ?? 0
  const quota = item.daily_quota ?? 0
  return quota > 0 ? `${used} / ${quota}` : `${used} / 不限`
}

function openKeyEditor(item: ClientKey) {
  editingKey.value = item
  keyDraft.name = item.name
  keyDraft.enabled = item.enabled
  keyDraft.model_scope = (item.model_scope ?? []).join('\n')
  keyDraft.bind_pool = item.bind_pool ?? ''
  keyDraft.daily_quota = item.daily_quota ?? 0
}

function closeKeyEditor() {
  if (!keyEditBusy.value) editingKey.value = null
}

async function saveKeyEdit() {
  if (!editingKey.value) return
  const name = keyDraft.name.trim()
  if (!name) {
    error.value = 'Key 名称不能为空。'
    return
  }
  keyEditBusy.value = true
  error.value = ''
  try {
    await api.keys.update(editingKey.value.id, {
      name,
      enabled: keyDraft.enabled,
      model_scope: parseModelScope(keyDraft.model_scope),
      bind_pool: keyDraft.bind_pool,
      daily_quota: normalizeQuota(keyDraft.daily_quota),
    })
    editingKey.value = null
    message.value = '客户端 Key 已更新。'
    await loadClientKeys()
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    keyEditBusy.value = false
  }
}

async function createClientKey() {
  const name = newKeyName.value.trim()
  if (!name) {
    error.value = '请先给 Key 起个名字。'
    return
  }
  keyBusy.value = true
  error.value = ''
  try {
    const created = await api.keys.create({
      name,
      model_scope: parseModelScope(newKeyScope.value),
      bind_pool: newKeyBindPool.value,
      daily_quota: normalizeQuota(newKeyQuota.value),
    })
    createdSecret.value = created.secret || ''
    newKeyName.value = ''
    newKeyScope.value = ''
    newKeyBindPool.value = ''
    newKeyQuota.value = 0
    await loadClientKeys()
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    keyBusy.value = false
  }
}

async function removeClientKey(item: ClientKey) {
  keyBusy.value = true
  error.value = ''
  try {
    await api.keys.remove(item.id)
    if (createdSecret.value) {
      createdSecret.value = ''
    }
    await loadClientKeys()
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    keyBusy.value = false
  }
}

async function copyCreatedKey() {
  try {
    await navigator.clipboard.writeText(createdSecret.value)
    message.value = 'Key 已复制到剪贴板。'
  } catch {
    message.value = '浏览器拒绝了剪贴板写入，请手动选中复制。'
  }
}

/* ------------------------------------------------------------------ */
/* 连通性诊断记录 (GET /api/diagnostics/probes)                         */
/* ------------------------------------------------------------------ */

const DIAGNOSTICS_KIND_LABELS: Record<DiagnosticsKind, string> = {
  a: 'A',
  plugin: '插件模型',
  custom_model: '自定义模型',
}

const probeKindFilter = ref<'' | DiagnosticsKind>('')
const probes = ref<DiagnosticsProbe[]>([])
const probesLoading = ref(false)
const probesError = ref('')

function probeKindLabel(kind: DiagnosticsKind): string {
  return DIAGNOSTICS_KIND_LABELS[kind] ?? kind
}

function probeTargetLabel(probe: DiagnosticsProbe): string {
  if (!probe.target_id || probe.target_id === probe.target) return probe.target || '—'
  return `${probe.target} · ${probe.target_id}`
}

async function loadProbes() {
  probesLoading.value = true
  try {
    probes.value = await api.diagnostics.probes({
      limit: 20,
      kind: probeKindFilter.value || undefined,
    })
    probesError.value = ''
  } catch (e) {
    probesError.value = errorMessage(e)
  } finally {
    probesLoading.value = false
  }
}

async function refreshProbes() {
  await loadProbes()
}

watch(probeKindFilter, () => void loadProbes())

function asNumber(value: unknown, fallback: number): number {
  const parsed = typeof value === 'number' ? value : Number(value)
  return Number.isFinite(parsed) ? parsed : fallback
}

function asString(value: unknown, fallback: string): string {
  return typeof value === 'string' ? value : fallback
}

function asBoolean(value: unknown, fallback: boolean): boolean {
  return typeof value === 'boolean' ? value : fallback
}

function asObject(value: unknown): Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {}
}

function asStringList(value: unknown, fallback: string[]): string[] {
  if (!Array.isArray(value)) return fallback
  return value.filter((item): item is string => typeof item === 'string')
}

/** Integer list from the host; a missing/non-array field keeps the default. */
function asNumberList(value: unknown, fallback: number[]): number[] {
  if (!Array.isArray(value)) return fallback
  return value
    .map((item) => Number(item))
    .filter((item) => Number.isInteger(item) && item >= 0)
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/** Recursively merges the edited fields over the raw host payload. */
function deepMerge(base: Record<string, unknown>, patch: Record<string, unknown>): Record<string, unknown> {
  const merged: Record<string, unknown> = { ...base }
  for (const [key, value] of Object.entries(patch)) {
    const current = merged[key]
    merged[key] = isPlainObject(current) && isPlainObject(value) ? deepMerge(current, value) : value
  }
  return merged
}

function parseNoProxy(input: string): string[] {
  const seen = new Set<string>()
  const entries: string[] = []
  for (const part of input.split(/[,，]/)) {
    const value = part.trim()
    if (!value || seen.has(value)) continue
    seen.add(value)
    entries.push(value)
  }
  return entries
}

/**
 * Retry backoff is a list of whole seconds. The manual (§8.3) promises this is
 * editable here, so it takes the same shape the host stores: 0–10 comma
 * separated entries, each an integer ≥ 0. An empty field is a valid empty list.
 */
function parseRetryBackoff(input: string): { values: number[]; error: string } {
  const text = input.trim()
  if (!text) return { values: [], error: '' }
  const parts = text.split(/[,，]/).map((part) => part.trim())
  if (parts.length > 10) {
    return { values: [], error: '重试退避最多 10 个值，用逗号分隔（如 5,20）。' }
  }
  for (const [index, part] of parts.entries()) {
    if (!/^\d+$/.test(part)) {
      return {
        values: [],
        error: `重试退避第 ${index + 1} 项「${part || '空'}」不是 ≥0 的整数秒。`,
      }
    }
  }
  return { values: parts.map((part) => Number(part)), error: '' }
}

/**
 * CORS origins are entered one per line. Blank lines are ignored, entries are
 * trimmed and de-duplicated. "跟随为空" (an empty textarea) means the gateway
 * allows no browser cross-origin access at all.
 */
function parseCorsOrigins(input: string): string[] {
  const seen = new Set<string>()
  const entries: string[] = []
  for (const part of input.split(/\r?\n/)) {
    const value = part.trim()
    if (!value || seen.has(value)) continue
    seen.add(value)
    entries.push(value)
  }
  return entries
}

/**
 * validateCorsOrigins mirrors internal/config: each entry is "*" or an absolute
 * http(s) origin with a host. Returns an error message or "" when valid.
 */
function validateCorsOrigins(entries: string[]): string {
  for (const origin of entries) {
    if (origin === '*') continue
    let parsed: URL
    try {
      parsed = new URL(origin)
    } catch {
      return `CORS 来源「${origin}」无效：每行需填写一个完整的 http(s) 来源（如 http://localhost:5173）或 *。`
    }
    if ((parsed.protocol !== 'http:' && parsed.protocol !== 'https:') || !parsed.host) {
      return `CORS 来源「${origin}」无效：仅允许 http(s) 来源或 *。`
    }
  }
  return ''
}

const PROXY_SCHEMES = new Set(['http:', 'https:', 'socks5:'])

function proxyURLError(label: string, value: string): string {
  const text = value.trim()
  if (!text) return ''
  let parsed: URL
  try {
    parsed = new URL(text)
  } catch {
    return `${label}必须是完整地址，例如 http://127.0.0.1:7890。`
  }
  if (!PROXY_SCHEMES.has(parsed.protocol) || !parsed.hostname) {
    return `${label}仅支持 http、https 或 socks5 协议，且必须包含主机名。`
  }
  return ''
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const raw = asObject((await api.getSettings()) ?? {})
    loaded.value = raw

    const a = asObject(raw.a)
    const proxy = asObject(raw.proxy)
    const egress = asObject(raw.egress)
    const plugins = asObject(raw.plugins)
    const checkin = asObject(raw.checkin)
    const retention = asObject(raw.retention)
    const logging = asObject(raw.logging)
    const gateway = asObject(raw.gateway)

    form.port = asNumber(raw.port, defaults.port)
    form.listen_lan = asBoolean(raw.listen_lan, defaults.listen_lan)
    form.autostart = asBoolean(raw.autostart, defaults.autostart)

    form.a_base_url = asString(a.base_url, defaults.a.base_url)
    form.a_exe_path = asString(a.exe_path, defaults.a.exe_path)
    form.a_auto_start = asBoolean(a.auto_start, defaults.a.auto_start)
    form.a_model_cache_minutes = asNumber(a.model_cache_minutes, defaults.a.model_cache_minutes)

    form.proxy_enabled = asBoolean(proxy.enabled, defaults.proxy.enabled)
    form.proxy_http = asString(proxy.http, defaults.proxy.http)
    form.proxy_https = asString(proxy.https, defaults.proxy.https)
    form.proxy_no_proxy = asStringList(proxy.no_proxy, defaults.proxy.no_proxy).join(', ')
    form.egress_direct_logging = asBoolean(egress.direct_logging, defaults.egress.direct_logging)

    form.plugins_concurrency = asNumber(plugins.concurrency, defaults.plugins.concurrency)
    form.plugins_startup_mode =
      asString(plugins.startup_mode, defaults.plugins.startup_mode) === 'on_demand' ? 'on_demand' : 'always'
    form.plugins_strict_egress = asBoolean(plugins.strict_egress, defaults.plugins.strict_egress)

    form.checkin_default_time = asString(checkin.default_time, defaults.checkin.default_time)
    form.checkin_retry_count = asNumber(checkin.retry_count, defaults.checkin.retry_count)
    form.checkin_retry_backoff = asNumberList(
      checkin.retry_backoff_seconds,
      defaults.checkin.retry_backoff_seconds,
    ).join(', ')
    backoffError.value = ''

    form.retention_api_request_days = asNumber(retention.api_request_days, defaults.retention.api_request_days)
    form.retention_body_preview_days = asNumber(
      retention.body_preview_days,
      defaults.retention.body_preview_days,
    )
    form.retention_checkin_days = asNumber(retention.checkin_days, defaults.retention.checkin_days)
    form.retention_host_log_days = asNumber(retention.host_log_days, defaults.retention.host_log_days)
    form.retention_proxy_log_days = asNumber(
      retention.proxy_log_days,
      defaults.retention.proxy_log_days,
    )

    form.logging_debug_body = asBoolean(logging.debug_body, defaults.logging.debug_body)
    form.gateway_cors = asStringList(gateway.cors_allow_origins, defaults.gateway.cors_allow_origins ?? []).join('\n')
    savedSignature.value = settingsSignature()
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    loading.value = false
  }
}

async function loadAKeyStatus() {
  try {
    const status = await api.aKey.status()
    aKeyConfigured.value = Boolean(status?.configured)
    aKeyAvailable.value = true
  } catch {
    aKeyAvailable.value = false
    aKeyConfigured.value = false
  }
}

onMounted(async () => {
  await Promise.all([
    load(),
    loadAKeyStatus(),
    loadClientKeys(),
    loadPluginIDs(),
    loadNotify(),
    loadProbes(),
  ])
  try {
    const info = await api.status()
    hostPort.value = info?.port || 7865
  } catch {
    // The empty-state hint falls back to the default port.
  }
})

async function save() {
  const port = Number(form.port)
  if (!Number.isInteger(port) || port < 1024 || port > 65535) {
    error.value = '监听端口必须是 1024–65535 之间的整数。'
    return
  }
  const concurrency = Number(form.plugins_concurrency)
  if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 32) {
    error.value = '默认并发数必须是 1–32 之间的整数。'
    return
  }
  const modelCache = Number(form.a_model_cache_minutes)
  if (!Number.isInteger(modelCache) || modelCache < 1 || modelCache > 1440) {
    error.value = 'A 模型缓存时间必须是 1–1440 之间的整数（分钟）。'
    return
  }
  const retryCount = Number(form.checkin_retry_count)
  if (!Number.isInteger(retryCount) || retryCount < 0 || retryCount > 5) {
    error.value = '签到失败重试次数必须是 0–5 之间的整数。'
    return
  }

  const backoff = parseRetryBackoff(form.checkin_retry_backoff)
  backoffError.value = backoff.error
  if (backoff.error) {
    // The field sits under 插件与签到 while 保存设置 is page-wide; mirror the
    // inline message into the notice so a save on another tab is not a no-op.
    error.value = backoff.error
    return
  }

  const time = form.checkin_default_time.trim()
  if (!/^([01][0-9]|2[0-3]):[0-5][0-9](:[0-5][0-9])?$/.test(time)) {
    error.value = '默认签到时间必须为 HH:MM 格式（24 小时制）。'
    return
  }

  const apiDays = Number(form.retention_api_request_days)
  const bodyDays = Number(form.retention_body_preview_days)
  const checkinDays = Number(form.retention_checkin_days)
  const hostDays = Number(form.retention_host_log_days)
  const proxyDays = Number(form.retention_proxy_log_days)
  const retentionError =
    (Number.isInteger(apiDays) && apiDays >= 1 ? '' : 'API 请求日志保留天数必须是不小于 1 的整数。') ||
    (Number.isInteger(bodyDays) && bodyDays >= 1 ? '' : '请求正文预览保留天数必须是不小于 1 的整数。') ||
    (Number.isInteger(checkinDays) && checkinDays >= 1 ? '' : '签到记录保留天数必须是不小于 1 的整数。') ||
    (Number.isInteger(hostDays) && hostDays >= 1 ? '' : '宿主日志保留天数必须是不小于 1 的整数。') ||
    (Number.isInteger(proxyDays) && proxyDays >= 1 ? '' : '代理日志保留天数必须是不小于 1 的整数。')
  if (retentionError) {
    error.value = retentionError
    return
  }

  const httpProxy = form.proxy_http.trim()
  const httpsProxy = form.proxy_https.trim()
  const proxyError =
    proxyURLError('HTTP 代理地址', httpProxy) || proxyURLError('HTTPS 代理地址', httpsProxy)
  if (proxyError) {
    error.value = proxyError
    return
  }

  const corsOrigins = parseCorsOrigins(form.gateway_cors)
  const corsError = validateCorsOrigins(corsOrigins)
  if (corsError) {
    error.value = corsError
    return
  }

  const notifyInterval = Number(notifyForm.min_interval_seconds)
  if (!Number.isInteger(notifyInterval) || notifyInterval < 60 || notifyInterval > 86400) {
    error.value = '通知的最小发送间隔必须是 60–86400 秒之间的整数。'
    return
  }

  const patch: Record<string, unknown> = {
    version: asNumber(loaded.value.version, defaults.version),
    port,
    listen_lan: form.listen_lan,
    autostart: form.autostart,
    a: {
      base_url: form.a_base_url.trim(),
      exe_path: form.a_exe_path.trim(),
      auto_start: form.a_auto_start,
      model_cache_minutes: modelCache,
    },
    proxy: {
      enabled: form.proxy_enabled,
      http: httpProxy,
      https: httpsProxy,
      no_proxy: parseNoProxy(form.proxy_no_proxy),
    },
    // Host egress accounting: sending the key is what persists a toggle.
    egress: {
      direct_logging: form.egress_direct_logging,
    },
    plugins: {
      concurrency,
      startup_mode: form.plugins_startup_mode,
      strict_egress: form.plugins_strict_egress,
    },
    checkin: {
      default_time: time.slice(0, 5),
      retry_count: retryCount,
      retry_backoff_seconds: backoff.values,
    },
    retention: {
      api_request_days: apiDays,
      body_preview_days: bodyDays,
      checkin_days: checkinDays,
      host_log_days: hostDays,
      proxy_log_days: proxyDays,
    },
    logging: {
      debug_body: form.logging_debug_body,
    },
    gateway: {
      cors_allow_origins: corsOrigins,
    },
    // The notification policy lives in the same config.json block; sending it
    // here keeps a page-wide save from reverting a card edit. The webhook URL
    // is never part of this payload.
    notify: notifyConfigFromForm(),
  }

  saving.value = true
  error.value = ''
  message.value = ''
  try {
    const merged = deepMerge(loaded.value, patch)
    await api.saveSettings(merged)
    loaded.value = merged
    savedSignature.value = settingsSignature()
    message.value = '设置已保存。端口与局域网等变更通常在宿主重启后完全生效。'
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    saving.value = false
  }
}

async function saveAKey() {
  const key = aKeyInput.value.trim()
  if (!key) {
    error.value = '请填写 A 的 Key。'
    return
  }
  aKeyBusy.value = true
  error.value = ''
  message.value = ''
  try {
    await api.aKey.set(key)
    aKeyInput.value = ''
    aKeyConfigured.value = true
    aKeyAvailable.value = true
    message.value = 'A Key 已加密保存。'
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    aKeyBusy.value = false
  }
}

async function saveLanPassword() {
  lanBusy.value = true
  error.value = ''
  message.value = ''
  const clearing = lanPassword.value.trim() === ''
  try {
    await api.setLANPassword(lanPassword.value)
    lanPassword.value = ''
    message.value = clearing ? '局域网密码已清除。' : '局域网密码已更新。'
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    lanBusy.value = false
  }
}
</script>

<template>
  <div class="page">
    <div v-if="error" class="notice error">{{ error }}</div>
    <div v-if="message" class="notice ok">{{ message }}</div>

    <div class="settings-bar">
      <nav class="settings-tabs" role="tablist" aria-label="设置分区">
        <button
          v-for="tab in SETTINGS_TABS"
          :key="tab.id"
          type="button"
          role="tab"
          class="settings-tab"
          :class="{ active: activeTab === tab.id }"
          :aria-selected="activeTab === tab.id"
          :tabindex="activeTab === tab.id ? 0 : -1"
          @click="selectTab(tab.id)"
        >
          {{ tab.label }}
        </button>
      </nav>
      <div class="settings-actions">
        <span v-if="settingsDirty" class="settings-dirty">有未保存的更改</span>
        <button type="button" class="btn" :disabled="loading" @click="load">重新读取</button>
        <button type="button" class="btn primary" :disabled="saving || loading" @click="save">
          {{ saving ? '保存中…' : '保存设置' }}
        </button>
      </div>
    </div>
    <p class="settings-tab-hint">{{ activeTabHint }}</p>

    <section v-show="activeTab === 'gateway'" class="card">
      <div class="card-head">
        <h2>宿主</h2>
        <span class="sub">{{ loading ? '读取中…' : '本机网页免密，局域网模式需密码' }}</span>
      </div>
      <div class="card-body stack">
        <div class="form-grid">
          <div class="field">
            <label for="set-port">监听端口 <span class="badge plain warn">需重启生效</span></label>
            <input id="set-port" v-model.number="form.port" class="input" type="number" min="1024" max="65535" />
            <span class="help">默认 7865，允许范围 1024–65535；被占用时宿主会在 7866–7875 顺延。修改后需重启宿主才切换监听地址。</span>
          </div>
        </div>

        <div class="field">
          <label class="switch">
            <input v-model="form.listen_lan" type="checkbox" />
            <span class="switch-track"></span>
            允许局域网访问 <span class="badge plain warn">需重启生效</span>
          </label>
          <span class="help">开启后其他设备可访问本页；必须设置网页密码，API 仍然必须携带客户端 Key。绑定地址在宿主重启后重新生效。</span>
        </div>

        <div class="field">
          <label class="switch">
            <input v-model="form.autostart" type="checkbox" />
            <span class="switch-track"></span>
            开机自启
          </label>
          <span class="help">关闭后宿主不再写入开机自启项（下次启动生效）。</span>
        </div>

        <div v-if="form.listen_lan" class="stack lan-panel">
          <div class="notice warn">
            局域网模式会扩大暴露面：请仅在可信网络使用，并同时配置防火墙。网页密码使用 Argon2id 哈希保存在本机。
          </div>
          <div class="row" style="align-items: flex-end">
            <div class="field grow">
              <label for="set-lan-password">局域网密码</label>
              <input
                id="set-lan-password"
                v-model="lanPassword"
                class="input"
                type="password"
                autocomplete="new-password"
                placeholder="留空保存即清除密码"
              />
            </div>
            <button type="button" class="btn" :disabled="lanBusy" @click="saveLanPassword">
              {{ lanBusy ? '保存中…' : '保存密码' }}
            </button>
          </div>
        </div>
      </div>
    </section>

    <section v-show="activeTab === 'gateway'" class="card">
      <div class="card-head">
        <h2>A 接入</h2>
        <span class="sub">A 保持独立，宿主只注入 Key 并转发裸模型名</span>
      </div>
      <div class="card-body stack">
        <div class="form-grid">
          <div class="field span-2">
            <label for="set-a-url">A 基础地址</label>
            <input id="set-a-url" v-model="form.a_base_url" class="input mono" type="text" placeholder="http://127.0.0.1:7864" />
            <span class="help">裸模型名（不含插件前缀）会转发到该地址的 /v1。</span>
          </div>
          <div class="field span-2">
            <label for="set-a-exe">A 可执行文件路径（可选）</label>
            <input
              id="set-a-exe"
              v-model="form.a_exe_path"
              class="input mono"
              type="text"
              placeholder="D:\AIWork\AIWork.exe"
              spellcheck="false"
            />
            <span class="help">用于宿主拉起 A；留空表示 A 由用户自行启动。</span>
          </div>
          <div class="field">
            <span class="field-label">随宿主启动 A <span class="badge plain warn">需重启生效</span></span>
            <label class="switch">
              <input v-model="form.a_auto_start" type="checkbox" />
              <span class="switch-track"></span>
              {{ form.a_auto_start ? '自动启动' : '不自动启动' }}
            </label>
            <span class="help">是否在宿主启动时拉起 A 的进程；仅在下次宿主重启后生效。</span>
          </div>
          <div class="field">
            <label for="set-a-cache">A 模型缓存（分钟） <span class="badge plain warn">需重启生效</span></label>
            <input
              id="set-a-cache"
              v-model.number="form.a_model_cache_minutes"
              class="input"
              type="number"
              min="1"
              max="1440"
            />
            <span class="help">默认 10，范围 1–1440；缓存期内不重新拉取模型列表。新的缓存时长在宿主重启后应用。</span>
          </div>
        </div>

        <div class="field" style="max-width: 560px">
          <label for="set-a-key">A Key</label>
          <div class="row" style="align-items: center">
            <input
              id="set-a-key"
              v-model="aKeyInput"
              class="input"
              type="password"
              autocomplete="new-password"
              placeholder="写入后不再回显"
              style="flex: 1"
            />
            <button type="button" class="btn primary" :disabled="aKeyBusy" @click="saveAKey">
              {{ aKeyBusy ? '保存中…' : '保存 Key' }}
            </button>
            <span class="badge" :class="aKeyConfigured ? 'ok' : 'muted'">
              {{ aKeyConfigured ? '已保存' : '未保存' }}
            </span>
          </div>
          <span class="help">只写字段：宿主使用 DPAPI 加密保存，接口从不返回明文。</span>
          <span v-if="!aKeyAvailable" class="help help-warn">
            宿主尚未提供 A Key 状态接口，无法确认是否已保存。
          </span>
        </div>
      </div>
    </section>

    <section v-show="activeTab === 'client'" class="card">
      <div class="card-head">
        <h2>客户端 Key</h2>
        <span class="sub">给 IDE / agent 使用；可限制模型范围、绑定插件与每日配额</span>
      </div>
      <div class="card-body stack">
        <div class="row key-create-row">
          <div class="field key-name-field">
            <label for="new-client-key">新建一把（起个名字）</label>
            <input
              id="new-client-key"
              v-model="newKeyName"
              class="input"
              type="text"
              placeholder="例如 codex / cherry-studio"
              maxlength="40"
              :disabled="keyBusy"
              @keyup.enter="createClientKey"
            />
          </div>
          <button type="button" class="btn primary" :disabled="keyBusy" @click="createClientKey">
            {{ keyBusy ? '创建中…' : '创建 Key' }}
          </button>
          <button type="button" class="btn ghost" :disabled="keyBusy" @click="loadClientKeys">
            刷新列表
          </button>
        </div>

        <div class="form-grid">
          <div class="field">
            <label for="new-key-scope">模型范围（可选）</label>
            <input
              id="new-key-scope"
              v-model="newKeyScope"
              class="input"
              type="text"
              placeholder="留空 = 不限；例如 trae/*, gpt-4o"
              :disabled="keyBusy"
            />
            <span class="help">
              一行一个或用逗号分隔。可写完整模型名（<code>插件id/模型</code>）或 <code>插件id/*</code> 前缀；留空表示可用全部模型。
            </span>
          </div>
          <div class="field">
            <label for="new-key-pool">绑定插件（可选）</label>
            <select id="new-key-pool" v-model="newKeyBindPool" class="select" :disabled="keyBusy">
              <option value="">不绑定（跟随路由）</option>
              <option v-for="id in pluginIDs" :key="id" :value="id">{{ id }}</option>
            </select>
            <span class="help">绑定后裸模型名只走该插件，带其他插件前缀的模型会被拒绝（404）。</span>
          </div>
          <div class="field">
            <label for="new-key-quota">每日配额（可选）</label>
            <input
              id="new-key-quota"
              v-model.number="newKeyQuota"
              class="input"
              type="number"
              min="0"
              max="1000000"
              step="1"
              :disabled="keyBusy"
            />
            <span class="help">0 = 不限；按宿主本地时间的自然日计数，次日 00:00 重置。</span>
          </div>
        </div>

        <div v-if="createdSecret" class="notice warn">
          <div class="row between" style="align-items: center">
            <span>
              明文只显示这一次，立刻复制走；关掉本提示后无法再取回，只能删掉重建。
              <code class="mono">{{ createdSecret }}</code>
            </span>
            <div class="btn-row">
              <button type="button" class="btn sm" @click="copyCreatedKey">复制</button>
              <button type="button" class="btn sm ghost" @click="createdSecret = ''">我已复制</button>
            </div>
          </div>
        </div>

        <div v-if="clientKeys.length === 0" class="empty">
          <strong>还没有客户端 Key</strong>
          <span>没有 Key 时 <code>/v1/*</code> 一律返回 401。创建后填进客户端的 api_key，base_url 用
            <code>http://127.0.0.1:{{ hostPort }}/v1</code>。</span>
        </div>
        <div v-else class="table-wrap">
          <table class="table">
            <thead>
              <tr>
                <th>名称</th>
                <th>前缀</th>
                <th>状态</th>
                <th>今日用量 / 配额</th>
                <th>范围</th>
                <th>创建</th>
                <th>最近使用</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="item in clientKeys" :key="item.id">
                <td class="title-cell">{{ item.name || item.id }}</td>
                <td class="mono">{{ item.prefix }}</td>
                <td>
                  <span class="badge plain" :class="item.enabled ? 'ok' : 'muted'">
                    {{ item.enabled ? '启用' : '已停用' }}
                  </span>
                </td>
                <td class="small mono">{{ keyUsageLabel(item) }}</td>
                <td class="small muted">{{ keyScopeLabel(item) }}</td>
                <td class="small muted num">{{ formatTimestamp(item.created_at) }}</td>
                <td class="small muted">{{ item.last_used_at ? formatTimestamp(item.last_used_at) : '从未' }}</td>
                <td>
                  <div class="btn-row" style="justify-content: flex-end">
                    <button type="button" class="btn sm" :disabled="keyBusy" @click="openKeyEditor(item)">
                      编辑
                    </button>
                    <button
                      type="button"
                      class="btn sm danger ghost"
                      :disabled="keyBusy"
                      @click="removeClientKey(item)"
                    >
                      删除
                    </button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <span class="help">
          宿主只保存 Key 的 SHA-256；删除后使用它的客户端立刻 401。管理接口只接受本机浏览器的同源请求，
          因此不能再用 curl/PowerShell 创建 Key。
        </span>
      </div>
    </section>

    <CustomModelsPanel v-show="activeTab === 'client'" />

    <section v-show="activeTab === 'plugin'" class="card">
      <div class="card-head">
        <h2>插件</h2>
        <span class="sub">插件进程的默认并发与启动方式</span>
      </div>
      <div class="card-body stack">
        <div class="notice muted">
          下方并发与严格出网是全局<b>默认值</b>：插件「设置」里显式填写的插件级覆盖优先于此，未设置（跟随全局）时才使用这里的值。
        </div>
        <div class="form-grid">
          <div class="field">
            <label for="set-concurrency">默认并发数</label>
            <input
              id="set-concurrency"
              v-model.number="form.plugins_concurrency"
              class="input"
              type="number"
              min="1"
              max="32"
            />
            <span class="help">全局默认值，范围 1–32。插件未在其「设置」中显式覆盖时使用；插件级覆盖优先。</span>
          </div>
          <div class="field">
            <label for="set-startup-mode">插件启动方式</label>
            <select id="set-startup-mode" v-model="form.plugins_startup_mode" class="select">
              <option value="always">随宿主启动</option>
              <option value="on_demand">按需启动</option>
            </select>
            <span class="help">全局默认值。插件未在其「设置」中选择启动方式时使用；插件级覆盖优先。</span>
          </div>
        </div>

        <div class="field">
          <label class="switch">
            <input v-model="form.plugins_strict_egress" type="checkbox" />
            <span class="switch-track"></span>
            默认严格出网模式
          </label>
          <span class="help">全局默认值。开启后，未在其「设置」中显式选择的插件仅能经由上方配置的上游代理出网；插件级「严格出网」覆盖优先于此。</span>
        </div>
      </div>
    </section>

    <section v-show="activeTab === 'plugin'" class="card">
      <div class="card-head">
        <h2>签到</h2>
        <span class="sub">每日签到的默认调度参数</span>
      </div>
      <div class="card-body stack">
        <div class="form-grid">
          <div class="field">
            <label for="set-checkin-time">默认签到时间</label>
            <input id="set-checkin-time" v-model="form.checkin_default_time" class="input" type="time" step="60" />
            <span class="help">24 小时制 HH:MM；插件未指定时间时使用。</span>
          </div>
          <div class="field">
            <label for="set-checkin-retry">失败重试次数 <span class="badge plain warn">需重启生效</span></label>
            <input
              id="set-checkin-retry"
              v-model.number="form.checkin_retry_count"
              class="input"
              type="number"
              min="0"
              max="5"
            />
            <span class="help">范围 0–5；重试间隔见「重试退避（秒）」。新的重试次数在宿主重启后应用。</span>
          </div>
          <div class="field">
            <label for="set-checkin-backoff">重试退避（秒） <span class="badge plain warn">需重启生效</span></label>
            <input
              id="set-checkin-backoff"
              v-model="form.checkin_retry_backoff"
              class="input mono"
              type="text"
              placeholder="5,20"
              spellcheck="false"
              @input="backoffError = ''"
            />
            <span v-if="backoffError" class="help help-warn">{{ backoffError }}</span>
            <span v-else class="help">
              逗号分隔的整数秒（如 5,20），最多 10 项；留空按 20 秒，条目不足时重复最后一个值。
            </span>
          </div>
        </div>
      </div>
    </section>

    <section v-show="activeTab === 'ops'" class="card">
      <div class="card-head">
        <h2>通知</h2>
        <span class="sub">签到与额度事件推送到手机、群机器人，并在桌面弹出系统通知</span>
      </div>
      <div class="card-body stack">
        <div class="field">
          <label class="switch">
            <input v-model="notifyForm.enabled" type="checkbox" />
            <span class="switch-track"></span>
            启用通知
          </label>
          <span class="help">关闭后停止所有渠道推送；启用时事件会同时以 Windows 系统通知弹出，未填写通知地址时仅显示系统通知。</span>
        </div>

        <div class="form-grid">
          <div class="field">
            <label for="set-notify-channel">渠道</label>
            <select id="set-notify-channel" v-model="notifyForm.channel" class="select">
              <option v-for="item in NOTIFY_CHANNELS" :key="item.value" :value="item.value">
                {{ item.label }}
              </option>
            </select>
            <span class="help">{{ notifyChannelHint }}</span>
          </div>
          <div class="field">
            <label for="set-notify-interval">最小发送间隔（秒）</label>
            <input
              id="set-notify-interval"
              v-model.number="notifyForm.min_interval_seconds"
              class="input"
              type="number"
              min="60"
              max="86400"
              step="60"
            />
            <span class="help">同一类事件对同一插件的最短间隔，范围 60–86400；重试失败不会刷屏。</span>
          </div>
          <div class="field span-2">
            <label for="set-notify-url">
              通知地址
              <span v-if="notifyURLSet" class="badge plain ok">已保存</span>
            </label>
            <input
              id="set-notify-url"
              v-model="notifyURL"
              class="input mono"
              type="password"
              autocomplete="new-password"
              spellcheck="false"
              :placeholder="notifyURLPlaceholder"
            />
            <span class="help">
              地址以 DPAPI 加密保存在本机，不会写入 config.json 或日志；留空表示保持已保存的地址，未配置地址时只发送桌面系统通知。
            </span>
            <div v-if="notifyURLSet" class="row">
              <button type="button" class="btn ghost" :disabled="notifySaving" @click="clearNotifyURL">
                清除已保存地址
              </button>
            </div>
          </div>
        </div>

        <div class="field">
          <span class="field-label">推送事件</span>
          <div class="notify-events">
            <label v-for="item in NOTIFY_EVENTS" :key="item.value">
              <input v-model="notifyForm.events" type="checkbox" :value="item.value" />
              {{ item.label }}
            </label>
          </div>
          <span class="help">
            签到失败按每次签到轮次汇总为一条（含首个失败原因）；签到完成每个插件每天最多一条；账号失效会附上处理建议。
          </span>
        </div>

        <div v-if="notifyLastSentAt || notifyLastError" class="notify-state">
          <span v-if="notifyLastSentAt">最近成功发送：{{ formatTimestamp(notifyLastSentAt) }}</span>
          <span v-if="notifyLastError" class="notify-error">最近错误：{{ notifyLastError }}</span>
        </div>

        <div class="row">
          <button type="button" class="btn primary" :disabled="notifySaving || loading" @click="saveNotify">
            {{ notifySaving ? '保存中…' : '保存' }}
          </button>
          <button type="button" class="btn" :disabled="notifyTesting" @click="testNotify">
            {{ notifyTesting ? '发送中…' : '发送测试' }}
          </button>
        </div>
      </div>
    </section>

    <section v-show="activeTab === 'gateway'" class="card">
      <div class="card-head">
        <h2>上游代理</h2>
        <span class="sub">插件出网可选的 HTTP / HTTPS / SOCKS5 代理</span>
      </div>
      <div class="card-body stack">
        <div class="field">
          <label class="switch">
            <input v-model="form.proxy_enabled" type="checkbox" />
            <span class="switch-track"></span>
            启用上游代理
          </label>
          <span class="help">关闭时插件直连出网；地址留空表示该协议不使用代理。</span>
        </div>

        <div class="form-grid">
          <div class="field">
            <label for="set-proxy-http">HTTP 代理</label>
            <input
              id="set-proxy-http"
              v-model="form.proxy_http"
              class="input mono"
              type="text"
              placeholder="http://127.0.0.1:7890"
              spellcheck="false"
            />
          </div>
          <div class="field">
            <label for="set-proxy-https">HTTPS 代理</label>
            <input
              id="set-proxy-https"
              v-model="form.proxy_https"
              class="input mono"
              type="text"
              placeholder="http://127.0.0.1:7890"
              spellcheck="false"
            />
          </div>
          <div class="field span-2">
            <label for="set-proxy-no-proxy">不走代理的地址</label>
            <input
              id="set-proxy-no-proxy"
              v-model="form.proxy_no_proxy"
              class="input mono"
              type="text"
              placeholder="127.0.0.1, localhost"
              spellcheck="false"
            />
            <span class="help">多个地址用逗号分隔，匹配的主机将直接连接。</span>
          </div>
        </div>

        <div class="field">
          <label class="switch">
            <input v-model="form.egress_direct_logging" type="checkbox" />
            <span class="switch-track"></span>
            直连流量也记入代理日志
          </label>
          <span class="help">开启后直连账号的流量也会先经过本机监听（多一跳），但能记进代理日志；默认关。</span>
        </div>
      </div>
    </section>

    <section v-show="activeTab === 'gateway'" class="card">
      <div class="card-head">
        <h2>网关 / 跨源 (CORS)</h2>
        <span class="sub">允许浏览器从其它来源调用 /v1 公开接口</span>
      </div>
      <div class="card-body stack">
        <div class="field">
          <label for="set-cors">允许的跨源来源</label>
          <textarea
            id="set-cors"
            v-model="form.gateway_cors"
            class="input mono"
            rows="4"
            spellcheck="false"
            placeholder="http://localhost:5173&#10;https://app.example.com&#10;*"
          ></textarea>
          <span class="help">
            每行一个来源，取值须为 <code>*</code> 或完整的 http(s) 来源（含协议与主机，不含路径）。留空表示不允许任何浏览器跨源访问。
          </span>
          <span class="help">
            未列入本列表的来源，其浏览器预检请求会被网关以 405 <code>cors_not_allowed</code> 拒绝；非浏览器客户端（不发送 Origin）不受此限制。保存后即时生效，无需重启。
          </span>
        </div>
      </div>
    </section>

    <section v-show="activeTab === 'ops'" class="card">
      <div class="card-head">
        <h2>日志与保留</h2>
        <span class="sub">请求日志、正文预览与签到记录按天保留并定期清理</span>
      </div>
      <div class="card-body stack">
        <div class="form-grid">
          <div class="field">
            <label for="set-request-days">API 请求日志保留（天）</label>
            <input
              id="set-request-days"
              v-model.number="form.retention_api_request_days"
              class="input"
              type="number"
              min="1"
            />
          </div>
          <div class="field">
            <label for="set-body-days">请求正文预览保留（天）</label>
            <input
              id="set-body-days"
              v-model.number="form.retention_body_preview_days"
              class="input"
              type="number"
              min="1"
            />
            <span class="help">默认 3 天，正文体积较大，建议短于请求日志。</span>
          </div>
          <div class="field">
            <label for="set-checkin-days">签到记录保留（天）</label>
            <input
              id="set-checkin-days"
              v-model.number="form.retention_checkin_days"
              class="input"
              type="number"
              min="1"
            />
            <span class="help">契约建议 180 天，占用空间很小。</span>
          </div>
          <div class="field">
            <label for="set-host-days">宿主日志保留（天）</label>
            <input
              id="set-host-days"
              v-model.number="form.retention_host_log_days"
              class="input"
              type="number"
              min="1"
            />
          </div>
          <div class="field">
            <label for="set-proxy-days">代理日志保留（天）</label>
            <input
              id="set-proxy-days"
              v-model.number="form.retention_proxy_log_days"
              class="input"
              type="number"
              min="1"
            />
            <span class="help">
              每条经代理建立的隧道记一行，量比请求日志大，所以默认只留 7 天。
            </span>
          </div>
        </div>

        <div class="field">
          <span class="help">
            Debug（记录请求 / 响应正文，并让管理操作显示真实 id）已移到「日志」页顶部工具栏，
            在那裡随点随开；本页保存时沿用当前开关状态，不会把它改回去。
          </span>
        </div>

        <span class="help">本页所有分区共用一个「保存设置」，入口在顶部模块栏右侧。</span>
      </div>
    </section>

    <section v-show="activeTab === 'ops'" class="card">
      <div class="card-head">
        <h2>连通性诊断记录</h2>
        <div class="row">
          <select v-model="probeKindFilter" class="select select-compact" aria-label="按类型筛选诊断记录">
            <option value="">全部</option>
            <option value="a">A</option>
            <option value="plugin">插件模型</option>
            <option value="custom_model">自定义模型</option>
          </select>
          <button type="button" class="btn sm" :disabled="probesLoading" @click="refreshProbes">
            {{ probesLoading ? '刷新中…' : '刷新' }}
          </button>
        </div>
      </div>
      <div v-if="probesError" class="notice error probes-error">{{ probesError }}</div>
      <div v-if="probesLoading && probes.length === 0" class="log-skeleton" aria-hidden="true">
        <span v-for="n in 5" :key="n" class="skeleton skeleton-line"></span>
      </div>
      <div v-else-if="probes.length === 0" class="empty">
        <strong>暂无诊断记录</strong>
        <span>在模型目录或自定义模型里点「检测」，最近 20 条结果会显示在这里。</span>
      </div>
      <div v-else class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>时间</th>
              <th>类型</th>
              <th>目标</th>
              <th>结果</th>
              <th>延迟</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="probe in probes" :key="probe.id">
              <td class="small muted mono">{{ formatTimestamp(probe.at) }}</td>
              <td>
                <span class="badge plain muted">{{ probeKindLabel(probe.kind) }}</span>
              </td>
              <td class="small mono truncate" :title="probeTargetLabel(probe)">
                {{ probeTargetLabel(probe) }}
              </td>
              <td>
                <span class="badge plain" :class="probe.ok ? 'ok' : 'danger'" :title="probe.detail || undefined">
                  {{ probe.ok ? '连通' : '失败' }}
                </span>
                <span v-if="!probe.ok && probe.detail" class="probe-detail" :title="probe.detail">
                  {{ probe.detail }}
                </span>
              </td>
              <td class="num">{{ probe.latency_ms ? formatLatency(probe.latency_ms) : '—' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <!-- 编辑客户端 Key：改名、启停、模型范围、绑定插件、每日配额 -->
    <div v-if="editingKey" class="drawer-backdrop" @click.self="closeKeyEditor">
      <aside class="drawer" role="dialog" aria-modal="true" aria-labelledby="key-edit-title">
        <header class="drawer-head">
          <div>
            <h2 id="key-edit-title">编辑客户端 Key</h2>
            <div class="small muted mono">{{ editingKey.prefix }}</div>
          </div>
          <button type="button" class="btn sm ghost" :disabled="keyEditBusy" @click="closeKeyEditor">
            关闭
          </button>
        </header>
        <div class="drawer-body">
          <p class="small muted plain-p">
            明文 Key 不会再显示。这里的改动（名称、启停、范围、配额）对下一次请求立即生效。
          </p>
          <div class="field">
            <label for="key-edit-name">名称</label>
            <input
              id="key-edit-name"
              v-model="keyDraft.name"
              class="input"
              type="text"
              maxlength="64"
              :disabled="keyEditBusy"
            />
          </div>
          <div class="field">
            <label class="switch">
              <input v-model="keyDraft.enabled" type="checkbox" :disabled="keyEditBusy" />
              <span class="switch-track"></span>
              启用
            </label>
            <span class="help">停用后该 Key 的下一次请求返回 401，正在进行的请求不受影响。</span>
          </div>
          <div class="field">
            <label for="key-edit-scope">模型范围</label>
            <textarea
              id="key-edit-scope"
              v-model="keyDraft.model_scope"
              class="input mono"
              rows="4"
              spellcheck="false"
              placeholder="留空 = 不限&#10;trae/*&#10;trae/gpt-4o"
              :disabled="keyEditBusy"
            ></textarea>
            <span class="help">
              一行一个或用逗号分隔。可写完整模型名（<code>插件id/模型</code>）或 <code>插件id/*</code> 前缀；
              范围外的模型返回 404 <code>model_not_found</code>。
            </span>
          </div>
          <div class="field">
            <label for="key-edit-pool">绑定插件</label>
            <select id="key-edit-pool" v-model="keyDraft.bind_pool" class="select" :disabled="keyEditBusy">
              <option value="">不绑定（跟随路由）</option>
              <option v-for="id in pluginIDs" :key="id" :value="id">{{ id }}</option>
            </select>
            <span class="help">绑定后裸模型名只走该插件；其他插件前缀的模型返回 404。</span>
          </div>
          <div class="field">
            <label for="key-edit-quota">每日配额</label>
            <input
              id="key-edit-quota"
              v-model.number="keyDraft.daily_quota"
              class="input"
              type="number"
              min="0"
              max="1000000"
              step="1"
              :disabled="keyEditBusy"
            />
            <span class="help">
              0 = 不限。今日已用 {{ editingKey.used_today ?? 0 }} 次，剩余
              {{ (editingKey.quota_left ?? -1) < 0 ? '不限' : editingKey.quota_left }}。配额用尽返回 429
              <code>key_quota_exceeded</code>，次日 00:00（宿主本地时间）重置。
            </span>
          </div>
          <div class="row">
            <button type="button" class="btn primary" :disabled="keyEditBusy" @click="saveKeyEdit">
              {{ keyEditBusy ? '保存中…' : '保存' }}
            </button>
            <button type="button" class="btn ghost" :disabled="keyEditBusy" @click="closeKeyEditor">取消</button>
          </div>
        </div>
      </aside>
    </div>
  </div>
</template>

<style scoped>
/* Text-only warning state on a .help line (no global .text-warn utility).
   Doubled class beats the global `.field .help` colour rule. */
.help.help-warn {
  color: var(--warn);
}

.plain-p {
  margin: 0;
}

/* Client-key toolbar: the create field aligns with its add button. */
.key-create-row {
  align-items: flex-end;
}

.key-name-field {
  margin: 0;
  min-width: 240px;
}

.probes-error {
  margin: var(--space-3) var(--space-4);
}

.notify-events {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 20px;
}

.notify-events label {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: var(--text-sm);
  color: var(--text-soft);
  cursor: pointer;
}

.notify-state {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: var(--text-xs);
  color: var(--text-soft);
}

.notify-error {
  color: var(--danger);
  word-break: break-all;
}

/* LAN access sub-panel: the warning notice plus the password row only render
   when the listener is exposed to the network, so they sit in one bordered box. */
.lan-panel {
  gap: var(--space-2);
  padding: var(--space-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  background: var(--surface);
}

/* Redacted failure text in the diagnostics table. */
.probe-detail {
  display: inline-block;
  max-width: 260px;
  margin-left: 8px;
  overflow: hidden;
  vertical-align: middle;
  color: var(--danger);
  font-size: var(--text-xs);
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* Top module bar. Four groups keep the eleven cards from reading as one
   long form; unselected cards stay mounted (v-show) so edits survive. */
.settings-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.settings-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}

.settings-dirty {
  color: var(--warn);
  font-size: var(--text-xs);
  white-space: nowrap;
}

.settings-tabs {
  display: flex;
  flex-wrap: wrap;
  gap: 3px;
  padding: 4px;
  border: 1px solid var(--border);
  border-radius: 10px;
  background: var(--surface-2);
}

.settings-tab {
  padding: 7px 14px;
  border: none;
  border-radius: 7px;
  background: transparent;
  color: var(--muted);
  font: inherit;
  font-size: var(--text-sm);
  font-weight: 500;
  white-space: nowrap;
  cursor: pointer;
  transition: background 0.15s ease, color 0.15s ease;
}

.settings-tab:hover {
  color: var(--text);
  background: color-mix(in srgb, var(--surface) 55%, transparent);
}

.settings-tab.active {
  background: var(--surface);
  color: var(--text);
  box-shadow: var(--shadow-sm);
}

.settings-tab:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: 1px;
}

.settings-tab-hint {
  margin: -10px 2px 0;
  color: var(--muted);
  font-size: var(--text-xs);
}
</style>

