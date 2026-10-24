<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import {
  api,
  errorMessage,
  normalizeCatalog,
  normalizeModelOverrides,
  normalizePoolDetail,
  normalizePoolSettings,
  samePoolSettings,
  POOL_STRATEGY_KEYS,
  POOL_STRATEGY_LABELS,
  type ModelOverridePatch,
  type ModelOverrideRecord,
  type PluginStatus,
  type PoolCatalogModel,
  type PoolDetail,
  type PoolSettings,
} from '../api'
import { formatLatency, formatNumber, formatTimestamp } from '../format'

interface ToastItem {
  id: number
  text: string
  tone: 'ok' | 'warn' | 'error'
}

const route = useRoute()
const pluginId = computed(() => String(route.params.id ?? ''))

/* Pool list (no :id) */
const plugins = ref<PluginStatus[]>([])
const pluginLoading = ref(false)
const pluginError = ref('')

/* Single pool */
const detail = ref<PoolDetail | null>(null)
const loading = ref(false)
const error = ref('')
const settingsDraft = ref<PoolSettings>(normalizePoolSettings(null))
const savingSettings = ref(false)
const upstreamBusy = ref(false)
const selected = ref<string[]>([])
const savingMembers = ref(false)
const syncing = ref(false)
const syncAt = ref<number | null>(null)
const syncNote = ref('')

/* Toasts */
let toastSeq = 0
const toasts = ref<ToastItem[]>([])
const toastTimers = new Set<number>()

function toast(text: string, tone: ToastItem['tone'] = 'ok') {
  const id = ++toastSeq
  toasts.value.push({ id, text, tone })
  const timer = window.setTimeout(() => {
    toastTimers.delete(timer)
    toasts.value = toasts.value.filter((item) => item.id !== id)
  }, tone === 'error' ? 6000 : 4200)
  toastTimers.add(timer)
}

/* ------------------------------------------------------------------ */
/* Helpers                                                             */
/* ------------------------------------------------------------------ */

function settingsOf(settings: PoolSettings): PoolSettings {
  return {
    strategy: settings.strategy,
    account_concurrency: settings.account_concurrency,
    pool_sticky_ttl: settings.pool_sticky_ttl,
    conv_sticky_ttl: settings.conv_sticky_ttl,
    hedge_ms: settings.hedge_ms,
    default_reasoning: settings.default_reasoning,
    background_downgrade: settings.background_downgrade,
    longctx_downgrade: settings.longctx_downgrade,
    model_sync_time: settings.model_sync_time,
  }
}

function validateSettings(settings: PoolSettings): string {
  const concurrency = Number(settings.account_concurrency)
  if (!Number.isInteger(concurrency) || concurrency < 0 || concurrency > 32) {
    return '并发需为 0–32 的整数（0 表示不限）。'
  }
  const poolTtl = Number(settings.pool_sticky_ttl)
  if (!Number.isInteger(poolTtl) || poolTtl < 0 || poolTtl > 86400) {
    return '池粘性需为 0–86400 秒的整数。'
  }
  const convTtl = Number(settings.conv_sticky_ttl)
  if (!Number.isInteger(convTtl) || convTtl < 0 || convTtl > 86400) {
    return '会话粘性需为 0–86400 秒的整数。'
  }
  const hedge = Number(settings.hedge_ms)
  if (!(hedge === 0 || (Number.isInteger(hedge) && hedge >= 1000 && hedge <= 8000))) {
    return '对冲需为 0（关闭）或 1000–8000 毫秒。'
  }
  const syncTime = settings.model_sync_time.trim()
  if (syncTime && !/^([01]\d|2[0-3]):[0-5]\d$/.test(syncTime)) {
    return '模型同步时刻需为 HH:MM（例如 05:45），留空表示不自动同步。'
  }
  return ''
}

function contextLabel(value?: number): string {
  if (!value || value <= 0) return '—'
  if (value >= 1_000_000) {
    const millions = value / 1_000_000
    return `${Number.isInteger(millions) ? millions : millions.toFixed(1)}M`
  }
  if (value >= 1000) return `${Math.round(value / 1000)}k`
  return String(value)
}

// Tier names stay in the protocol spelling (minimal/low/medium/high/xhigh/max)
// so the UI matches the values users see in API requests and plugin manifests.
const THINKING_LABELS: Record<string, string> = {
  off: '关',
  on: '开',
  none: '关',
  disabled: '关',
  auto: '自动',
}

function thinkingLabel(value?: string[]): string {
  if (!value || value.length === 0) return '—'
  return value.map((level) => THINKING_LABELS[level] ?? level).join(' / ')
}

function multiplierLabel(value?: number): string {
  if (typeof value !== 'number' || !Number.isFinite(value) || value <= 0) return '—'
  return `×${value.toFixed(2).replace(/\.?0+$/, '') || '0'}`
}

const settingsDirty = computed(() => {
  const current = detail.value
  if (!current) return false
  return !samePoolSettings(current.settings, settingsDraft.value)
})

const membersDirty = computed(() => {
  const current = detail.value
  if (!current) return false
  const saved = current.members
    .filter((member) => member.member)
    .map((member) => member.account_id)
    .sort()
  const draft = [...selected.value].sort()
  return saved.length !== draft.length || saved.some((id, index) => id !== draft[index])
})

/* ------------------------------------------------------------------ */
/* Plugin list                                                         */
/* ------------------------------------------------------------------ */

function statusLabel(plugin: PluginStatus): string {
  if (plugin.failed) return '异常'
  if (!plugin.enabled) return '已停用'
  if (plugin.running && plugin.healthy) return '运行中'
  if (plugin.running) return '运行中 · 未就绪'
  return '已停止'
}

/** One line the API-level badge can show in a tooltip; see the hint under the table. */
function apiLevelLabel(level?: number): string {
  if (level === 2) return 'v2：插件接受宿主指定账号（X-Aiworkgate-Account）'
  if (level === 1) return 'v1：只做整池调度，宿主不指定账号'
  return '清单未声明 api_level'
}

function statusTone(plugin: PluginStatus): string {
  if (plugin.failed) return 'danger'
  if (!plugin.enabled) return 'muted'
  if (plugin.running && plugin.healthy) return 'ok'
  return 'warn'
}

function upstreamLabel(plugin: PluginStatus): string {
  if (plugin.upstream_enabled === undefined) return '—'
  return plugin.upstream_enabled ? '开' : '关'
}

function upstreamTone(plugin: PluginStatus): string {
  if (plugin.upstream_enabled === undefined) return 'muted'
  return plugin.upstream_enabled ? 'ok' : 'warn'
}

async function loadPlugins() {
  pluginLoading.value = true
  try {
    plugins.value = (await api.plugins.list()) ?? []
    pluginError.value = ''
  } catch (e) {
    pluginError.value = errorMessage(e)
  } finally {
    pluginLoading.value = false
  }
}

/* ------------------------------------------------------------------ */
/* Pool detail                                                         */
/* ------------------------------------------------------------------ */

async function loadDetail() {
  if (!pluginId.value) return
  loading.value = true
  try {
    const pool = await api.pools.get(pluginId.value)
    const next = normalizePoolDetail(pool, pluginId.value)
    detail.value = next
    settingsDraft.value = normalizePoolSettings(next.settings)
    selected.value = next.members
      .filter((member) => member.member)
      .map((member) => member.account_id)
    await loadOverrides()
    error.value = ''
  } catch (e) {
    error.value = errorMessage(e)
    detail.value = null
  } finally {
    loading.value = false
  }
}

async function saveSettings() {
  const problem = validateSettings(settingsDraft.value)
  if (problem) {
    toast(problem, 'error')
    return
  }
  savingSettings.value = true
  try {
    await api.pools.save(pluginId.value, settingsOf(settingsDraft.value))
    await loadDetail()
    toast('调度参数已保存，立即生效。')
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    savingSettings.value = false
  }
}

function onUpstreamChange(event: Event) {
  const checked = (event.target as HTMLInputElement).checked
  void saveUpstream(checked)
}

async function saveUpstream(next: boolean) {
  const current = detail.value
  if (!current) return
  upstreamBusy.value = true
  try {
    await api.pools.save(pluginId.value, {
      ...settingsOf(current.settings),
      upstream_enabled: next,
    })
    current.upstream_enabled = next
    toast(
      next ? '上游已开启，模型恢复对外可见。' : '上游已关闭，模型将从 /v1/models 隐藏。',
      next ? 'ok' : 'warn',
    )
  } catch (e) {
    toast(errorMessage(e), 'error')
    await loadDetail()
  } finally {
    upstreamBusy.value = false
  }
}

function toggleMember(accountId: string) {
  const index = selected.value.indexOf(accountId)
  if (index >= 0) selected.value.splice(index, 1)
  else selected.value.push(accountId)
}

function selectAllMembers() {
  selected.value = (detail.value?.members ?? []).map((member) => member.account_id)
}

function clearMembers() {
  selected.value = []
}

async function saveMembers() {
  const count = selected.value.length
  savingMembers.value = true
  try {
    await api.pools.setMembers(pluginId.value, [...selected.value])
    await loadDetail()
    toast(count === 0 ? '池成员已清空：全部含凭据账号自动入池。' : `池成员已保存（${count} 个）。`)
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    savingMembers.value = false
  }
}

async function syncCatalog() {
  syncing.value = true
  try {
    const result = await api.pools.sync(pluginId.value)
    syncAt.value = Date.now()
    const count = result?.model_count ?? result?.count
    if (result?.error) {
      syncNote.value = `失败：${result.error}`
    } else if (result?.message) {
      syncNote.value = result.message
    } else if (typeof count === 'number') {
      syncNote.value = `已同步 ${count} 个模型`
    } else {
      syncNote.value = '同步完成'
    }
    await loadDetail()
    toast(syncNote.value, result?.error ? 'error' : 'ok')
  } catch (e) {
    syncAt.value = Date.now()
    syncNote.value = errorMessage(e)
    toast(syncNote.value, 'error')
  } finally {
    syncing.value = false
  }
}

const togglingModel = ref('')
const enabledCount = computed(
  () => (detail.value?.catalog ?? []).filter((model) => model.enabled !== false).length,
)

/* ------------------------------------------------------------------ */
/* Per-model connectivity probe (POST /api/diagnostics/probe)          */
/* ------------------------------------------------------------------ */

/** Last probe of one catalog model, rendered inline next to 检测. */
interface CatalogProbe {
  ok: boolean
  status: number
  latency_ms: number
  detail: string
  at: number
}

const catalogProbe = ref<Record<string, CatalogProbe>>({})
const probingModel = ref('')

function probeTone(modelId: string): string {
  const row = catalogProbe.value[modelId]
  if (!row) return ''
  return row.ok ? 'ok' : 'danger'
}

function probeLabel(modelId: string): string {
  const row = catalogProbe.value[modelId]
  if (!row) return ''
  if (row.ok) return formatLatency(row.latency_ms)
  return row.detail || '检测失败'
}

function probeTitle(modelId: string): string {
  const row = catalogProbe.value[modelId]
  if (!row) return ''
  const parts = [row.ok ? '连通正常' : row.detail || '检测失败']
  if (row.status) parts.push(`HTTP ${row.status}`)
  parts.push(formatTimestamp(row.at))
  return parts.join(' · ')
}

function rememberProbe(modelId: string, row: CatalogProbe) {
  catalogProbe.value = { ...catalogProbe.value, [modelId]: row }
}

async function probeCatalogModel(model: PoolCatalogModel) {
  const modelId = model.model_id
  probingModel.value = modelId
  try {
    const result = await api.diagnostics.probe('plugin', {
      plugin_id: pluginId.value,
      model: modelId,
    })
    rememberProbe(modelId, {
      ok: result.ok,
      status: result.status,
      latency_ms: result.latency_ms,
      detail: result.detail,
      at: result.at,
    })
  } catch (e) {
    // Host unreachable: surface it like a failed probe instead of a toast.
    rememberProbe(modelId, {
      ok: false,
      status: 0,
      latency_ms: 0,
      detail: errorMessage(e),
      at: Math.floor(Date.now() / 1000),
    })
  } finally {
    probingModel.value = ''
  }
}

const lastSyncAt = computed(() => {
  let newest = 0
  for (const model of detail.value?.catalog ?? []) {
    newest = Math.max(newest, model.synced_at ?? 0)
  }
  return newest > 0 ? newest * 1000 : 0
})

/**
 * Sync lifecycle from the host (A: ApiService.tsx:855-916). The host's last
 * success and the catalog timestamps are merged with the manual result of this
 * session, so a manual sync is reflected even when only the scheduled path
 * writes the lifecycle key.
 */
const modelSync = computed(() => detail.value?.model_sync ?? null)

const lastSyncLabel = computed(() => {
  const synced = modelSync.value?.last_success_at ?? 0
  const stamp = Math.max(synced * 1000, syncAt.value || 0, lastSyncAt.value || 0)
  return stamp > 0 ? formatTimestamp(stamp) : ''
})

const syncFailures = computed(() => modelSync.value?.failure_count ?? 0)

const nextRetryLabel = computed(() => {
  const at = modelSync.value?.next_retry_at ?? 0
  return at > 0 ? `预计 ${formatTimestamp(at)} 重试` : ''
})

async function toggleModel(model: PoolCatalogModel, event: Event) {
  const input = event.target as HTMLInputElement
  const enabled = input.checked
  togglingModel.value = model.model_id
  try {
    const data = await api.pools.setModelEnabled(pluginId.value, model.model_id, enabled)
    const list = normalizeCatalog(data)
    if (detail.value) detail.value.catalog = list
    toast(enabled ? `已接入 ${model.model_id}` : `已取消接入 ${model.model_id}`)
  } catch (e) {
    input.checked = !enabled
    toast(errorMessage(e), 'error')
  } finally {
    togglingModel.value = ''
  }
}

/* ------------------------------------------------------------------ */
/* Model metadata overrides (人工覆盖层)                                */
/* ------------------------------------------------------------------ */

const OVERRIDE_FIELD_LABELS: Record<string, string> = {
  display_name: '展示名',
  vendor: '厂商',
  context_length: '上下文',
  vision: '视觉',
  thinking: '思考',
  multiplier: '倍率',
  free: '免费',
}

/** One catalog row with the human override applied on top (人工 > 插件上报). */
interface CatalogRow {
  model: PoolCatalogModel
  override?: ModelOverrideRecord
  displayName: string
  vendor: string
  contextLength: number
  vision: boolean
  thinking: string[]
  multiplier: number
  free: boolean
}

interface OverrideDraft {
  display_name: string
  vendor: string
  context_length: string
  multiplier: string
  vision: '' | 'true' | 'false'
  thinking: string
  free: '' | 'true' | 'false'
}

const overrides = ref<Record<string, ModelOverrideRecord>>({})
const editingModel = ref<PoolCatalogModel | null>(null)
const overrideDraft = ref<OverrideDraft>(emptyOverrideDraft())
const overrideBusy = ref(false)

function emptyOverrideDraft(): OverrideDraft {
  return {
    display_name: '',
    vendor: '',
    context_length: '',
    multiplier: '',
    vision: '',
    thinking: '',
    free: '',
  }
}

const catalogRows = computed<CatalogRow[]>(() =>
  (detail.value?.catalog ?? []).map((model) => {
    const override = overrides.value[model.model_id]
    return {
      model,
      override,
      displayName: override?.display_name ?? model.display_name ?? model.model_id,
      vendor: override?.vendor ?? model.vendor ?? '',
      contextLength: override?.context_length ?? model.context_length ?? 0,
      vision: override?.vision ?? Boolean(model.vision),
      thinking: override?.thinking ?? model.thinking ?? [],
      multiplier: override?.multiplier ?? model.multiplier ?? 0,
      free: override?.free ?? Boolean(model.free),
    }
  }),
)

const editingOverride = computed(() =>
  editingModel.value ? overrides.value[editingModel.value.model_id] : undefined,
)

function overriddenLabels(entry?: ModelOverrideRecord): string[] {
  return (entry?.overridden ?? []).map((field) => OVERRIDE_FIELD_LABELS[field] ?? field)
}

async function loadOverrides() {
  if (!pluginId.value) {
    overrides.value = {}
    return
  }
  try {
    overrides.value = normalizeModelOverrides(await api.pools.overrides(pluginId.value))
  } catch {
    // The catalog stays usable without the override layer (old host, 501).
    overrides.value = {}
  }
}

function openOverrideEditor(model: PoolCatalogModel) {
  const override = overrides.value[model.model_id]
  const boolChoice = (value?: boolean | null): '' | 'true' | 'false' => {
    if (value === true) return 'true'
    if (value === false) return 'false'
    return ''
  }
  editingModel.value = model
  overrideDraft.value = {
    display_name: override?.display_name ?? '',
    vendor: override?.vendor ?? '',
    context_length: override?.context_length ? String(override.context_length) : '',
    multiplier: typeof override?.multiplier === 'number' ? String(override.multiplier) : '',
    vision: boolChoice(override?.vision),
    thinking: (override?.thinking ?? []).join(', '),
    free: boolChoice(override?.free),
  }
}

function closeOverrideEditor() {
  if (overrideBusy.value) return
  editingModel.value = null
}

function parseThinkingLevels(raw: string): string[] {
  const seen = new Set<string>()
  const levels: string[] = []
  for (const part of raw.split(/[,，]/)) {
    const level = part.trim()
    if (!level || seen.has(level)) continue
    seen.add(level)
    levels.push(level)
  }
  return levels
}

/** Validates the draft and turns it into the PUT body (empty = null = 清除). */
function buildOverridePatch(draft: OverrideDraft): { patch?: ModelOverridePatch; problem?: string } {
  const patch: ModelOverridePatch = {}

  const name = draft.display_name.trim()
  if ([...name].length > 128) return { problem: '展示名最多 128 个字符。' }
  patch.display_name = name || null

  const vendor = draft.vendor.trim()
  if ([...vendor].length > 64) return { problem: '厂商最多 64 个字符。' }
  patch.vendor = vendor || null

  const contextRaw = draft.context_length.trim()
  if (contextRaw) {
    const context = Number(contextRaw)
    if (!Number.isInteger(context) || context <= 0) {
      return { problem: '上下文需为正整数；留空表示跟随插件上报。' }
    }
    patch.context_length = context
  } else {
    patch.context_length = null
  }

  const multiplierRaw = draft.multiplier.trim()
  if (multiplierRaw) {
    const multiplier = Number(multiplierRaw)
    if (!Number.isFinite(multiplier) || multiplier < 0) {
      return { problem: '倍率需为不小于 0 的数值；留空表示跟随插件上报。' }
    }
    patch.multiplier = multiplier
  } else {
    patch.multiplier = null
  }

  const levels = parseThinkingLevels(draft.thinking)
  const tooLong = levels.find((level) => [...level].length > 32)
  if (tooLong) return { problem: `思考档位「${tooLong}」超过 32 个字符。` }
  if (levels.length > 12) return { problem: '思考档位最多 12 个。' }
  patch.thinking = levels.length ? levels : null

  patch.vision = draft.vision === '' ? null : draft.vision === 'true'
  patch.free = draft.free === '' ? null : draft.free === 'true'
  return { patch }
}

async function saveOverride() {
  const model = editingModel.value
  if (!model) return
  const built = buildOverridePatch(overrideDraft.value)
  if (!built.patch) {
    toast(built.problem || '请检查输入。', 'error')
    return
  }
  overrideBusy.value = true
  try {
    await api.pools.saveOverride(pluginId.value, model.model_id, built.patch)
    // loadDetail refreshes the catalog and the override layer together.
    await loadDetail()
    toast(`已保存 ${model.model_id} 的人工元数据。`)
    editingModel.value = null
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    overrideBusy.value = false
  }
}

async function clearOverride() {
  const model = editingModel.value
  if (!model) return
  overrideBusy.value = true
  try {
    const result = await api.pools.clearOverride(pluginId.value, model.model_id)
    await loadDetail()
    toast(result?.removed === false ? '该模型没有人工覆盖。' : '已清除人工覆盖，回落到插件上报值。')
    editingModel.value = null
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    overrideBusy.value = false
  }
}

/* ------------------------------------------------------------------ */
/* Lifecycle                                                           */
/* ------------------------------------------------------------------ */

watch(pluginId, () => {
  detail.value = null
  error.value = ''
  syncAt.value = null
  syncNote.value = ''
  overrides.value = {}
  editingModel.value = null
  catalogProbe.value = {}
  probingModel.value = ''
  if (pluginId.value) void loadDetail()
  else void loadPlugins()
})

onMounted(() => {
  if (pluginId.value) void loadDetail()
  else void loadPlugins()
})

onBeforeUnmount(() => {
  for (const timer of toastTimers) window.clearTimeout(timer)
  toastTimers.clear()
})
</script>

<template>
  <div class="page">
    <!-- ---------------------------------------------------------- -->
    <!-- Plugin list (/pools)                                       -->
    <!-- ---------------------------------------------------------- -->
    <template v-if="!pluginId">
      <div v-if="pluginError" class="notice error">
        <div class="row between">
          <span>{{ pluginError }}</span>
          <button type="button" class="btn sm" :disabled="pluginLoading" @click="loadPlugins">重试</button>
        </div>
      </div>

      <div class="row between">
        <p class="small muted intro-note">
          每个插件是一个独立资源池：积分、账号池与模型目录互不影响。点「管理」进入单插件的调度参数与账号池。
        </p>
        <div class="row">
          <button type="button" class="btn" :disabled="pluginLoading" @click="loadPlugins">
            {{ pluginLoading ? '刷新中…' : '刷新' }}
          </button>
          <RouterLink class="btn ghost" to="/scheduler">调度策略中心</RouterLink>
        </div>
      </div>

      <section class="card">
        <div class="card-head">
          <h2>资源池</h2>
          <span class="badge plain muted">{{ plugins.length }} 个插件</span>
        </div>
        <div v-if="pluginLoading && plugins.length === 0" class="loading">正在加载插件…</div>
        <div v-else-if="plugins.length === 0" class="empty">
          <strong>还没有安装插件</strong>
          <span>安装并启用插件后，这里会出现对应的资源池入口。</span>
          <RouterLink class="btn sm" to="/plugins">前往插件页</RouterLink>
        </div>
        <div v-else class="table-wrap">
          <table class="table">
            <thead>
              <tr>
                <th>插件</th>
                <th>API 级别</th>
                <th>运行状态</th>
                <th>上游</th>
                <th>模型数</th>
                <th style="text-align: right">操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="plugin in plugins" :key="plugin.manifest?.id">
                <td class="title-cell">
                  {{ plugin.manifest?.name || plugin.manifest?.id }}
                  <div class="small muted mono">{{ plugin.manifest?.id }}</div>
                </td>
                <td class="num" :title="apiLevelLabel(plugin.manifest?.api_level)">
                  {{ plugin.manifest?.api_level ? `v${plugin.manifest.api_level}` : '—' }}
                </td>
                <td>
                  <span class="badge" :class="statusTone(plugin)">{{ statusLabel(plugin) }}</span>
                </td>
                <td>
                  <span class="badge" :class="upstreamTone(plugin)">{{ upstreamLabel(plugin) }}</span>
                </td>
                <td class="num">{{ plugin.manifest?.models?.length ?? 0 }}</td>
                <td>
                  <div class="btn-row" style="justify-content: flex-end">
                    <RouterLink class="btn sm" :to="`/pools/${plugin.manifest?.id}`">管理</RouterLink>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <p class="small muted api-level-hint">
          API 级别是插件与宿主之间的契约代次：<strong>v2</strong> 表示插件接受宿主指定账号（<code>X-Aiworkgate-Account</code>），
          调度器才能把逻辑模型或会话固定到某个账号；<strong>v1</strong> 只能整池调度，宿主不指定账号。
        </p>
      </section>
    </template>

    <!-- ---------------------------------------------------------- -->
    <!-- Single pool (/pools/:id)                                   -->
    <!-- ---------------------------------------------------------- -->
    <template v-else>
      <div v-if="loading && !detail" class="loading">正在加载资源池…</div>

      <div v-else-if="error && !detail" class="notice error">
        <div class="row between">
          <span>{{ error }}</span>
          <div class="btn-row">
            <button type="button" class="btn sm" :disabled="loading" @click="loadDetail">重试</button>
            <RouterLink class="btn sm ghost" to="/pools">返回资源列表</RouterLink>
          </div>
        </div>
      </div>

      <template v-else-if="detail">
        <section class="card">
          <div class="card-body stack">
            <div class="row between">
              <div class="row">
                <strong class="pool-name">{{ detail.name }}</strong>
                <span class="small muted mono">{{ detail.plugin_id }}</span>
                <span class="badge plain muted" :title="apiLevelLabel(detail.api_level)">API {{ detail.api_level ? `v${detail.api_level}` : '—' }}</span>
              </div>
              <div class="row">
                <label class="switch">
                  <input
                    type="checkbox"
                    :checked="detail.upstream_enabled"
                    :disabled="upstreamBusy"
                    @change="onUpstreamChange"
                  />
                  <span class="switch-track"></span>
                  <span class="small">{{ detail.upstream_enabled ? '上游已启用' : '上游已关闭' }}</span>
                </label>
                <button type="button" class="btn" :disabled="syncing" @click="syncCatalog">
                  {{ syncing ? '同步中…' : '立即同步' }}
                </button>
              </div>
            </div>
            <div class="row small muted hint-row">
              <span>模型目录按需同步；设了同步时刻则每天到点自动同步一次</span>
              <span v-if="lastSyncLabel">
                最后同步 {{ lastSyncLabel }}<template v-if="syncNote"> · {{ syncNote }}</template>
              </span>
              <span v-else>从未同步</span>
              <span v-if="modelSync?.running" class="badge plain">同步中</span>
              <span
                v-if="syncFailures > 0"
                class="badge warn plain"
                :title="modelSync?.last_error || '上次自动同步失败'"
              >
                自动同步失败 {{ syncFailures }} 次<template v-if="nextRetryLabel"> · {{ nextRetryLabel }}</template>
              </span>
              <RouterLink class="btn sm ghost" to="/scheduler">返回调度策略中心</RouterLink>
            </div>
          </div>
        </section>

        <div v-if="!detail.upstream_enabled" class="notice warn">
          上游已关闭：该插件的模型将从 /v1/models 隐藏，直接请求会返回 503。恢复可见请重新开启上游。
        </div>

        <div class="stat-grid kpi-grid">
          <div class="stat">
            <span class="stat-label">积分总余额</span>
            <span class="stat-value">{{ formatNumber(detail.credits.total) }}</span>
            <span class="stat-sub">池内账号剩余积分之和</span>
          </div>
          <div class="stat">
            <span class="stat-label">健康账号</span>
            <span class="stat-value small-value">
              {{ detail.credits.healthy }}<span class="muted"> / {{ detail.members.length }}</span>
            </span>
            <span class="stat-sub">可用且未过期的账号</span>
          </div>
          <div class="stat">
            <span class="stat-label">今日使用账号</span>
            <span class="stat-value">{{ detail.credits.today_used }}</span>
            <span class="stat-sub">今日已有请求的账号</span>
          </div>
          <div class="stat">
            <span class="stat-label">池内账号</span>
            <span class="stat-value">{{ detail.credits.pool_size }}</span>
            <span class="stat-sub">{{ detail.credits.pool_size === 0 ? '未设置成员时视为全部账号' : '参与调度的账号数' }}</span>
          </div>
        </div>

        <section class="card">
          <div class="card-head">
            <h2>调度参数</h2>
            <span class="sub">保存后立即生效，进行中的请求不受影响</span>
          </div>
          <div class="card-body stack">
            <div class="form-grid">
              <div class="field">
                <label for="pool-strategy">池内策略</label>
                <select id="pool-strategy" v-model="settingsDraft.strategy" class="select">
                  <option v-for="key in POOL_STRATEGY_KEYS" :key="key" :value="key">
                    {{ POOL_STRATEGY_LABELS[key] }}
                  </option>
                </select>
                <span class="help">决定同一池内选哪个账号。</span>
              </div>
              <div class="field">
                <label for="pool-concurrency">账号并发</label>
                <input
                  id="pool-concurrency"
                  v-model.number="settingsDraft.account_concurrency"
                  class="input"
                  type="number"
                  min="0"
                  max="32"
                  step="1"
                />
                <span class="help">0 = 不限；达到上限的账号让位。</span>
              </div>
              <div class="field">
                <label for="pool-ttl">池粘性（秒）</label>
                <input
                  id="pool-ttl"
                  v-model.number="settingsDraft.pool_sticky_ttl"
                  class="input"
                  type="number"
                  min="0"
                  max="86400"
                  step="1"
                />
                <span class="help">同一模型 + 客户端固定到同一账号。</span>
              </div>
              <div class="field">
                <label for="pool-conv-ttl">会话粘性（秒）</label>
                <input
                  id="pool-conv-ttl"
                  v-model.number="settingsDraft.conv_sticky_ttl"
                  class="input"
                  type="number"
                  min="0"
                  max="86400"
                  step="1"
                />
                <span class="help">优先于池粘性，按会话 ID 绑定账号。</span>
              </div>
              <div class="field">
                <label for="pool-hedge">对冲（毫秒）</label>
                <input
                  id="pool-hedge"
                  v-model.number="settingsDraft.hedge_ms"
                  class="input"
                  type="number"
                  min="0"
                  max="8000"
                  step="100"
                />
                <span class="help">0 = 关闭；开启需 1000–8000 ms。</span>
              </div>
              <div class="field">
                <label for="pool-sync-time">模型同步时刻</label>
                <input
                  id="pool-sync-time"
                  v-model.trim="settingsDraft.model_sync_time"
                  class="input"
                  type="time"
                />
                <span class="help">每天到点自动同步模型目录；留空 = 不自动同步。</span>
              </div>
              <div class="field span-2">
                <span class="field-label">降档与思考</span>
                <div class="mini-checks">
                  <label class="mini-check">
                    <input v-model="settingsDraft.default_reasoning" type="checkbox" />
                    默认深度思考
                  </label>
                  <label class="mini-check">
                    <input v-model="settingsDraft.background_downgrade" type="checkbox" />
                    后台任务降级
                  </label>
                  <label class="mini-check">
                    <input v-model="settingsDraft.longctx_downgrade" type="checkbox" />
                    长上下文降档
                  </label>
                </div>
                <span class="help">降档会写入日志并回传 X-Aiworkgate-Downgraded。</span>
              </div>
            </div>
            <div class="row">
              <button
                type="button"
                class="btn primary"
                :disabled="savingSettings || !settingsDirty"
                @click="saveSettings"
              >
                {{ savingSettings ? '保存中…' : '保存参数' }}
              </button>
              <span v-if="settingsDirty" class="dirty-hint"><span class="dirty-dot"></span>有未保存的更改</span>
            </div>
          </div>
        </section>

        <section class="card">
          <div class="card-head">
            <h2>账号池</h2>
            <div class="row">
              <span class="badge plain muted">{{ selected.length }} / {{ detail.members.length }} 已入池</span>
              <button type="button" class="btn sm" @click="selectAllMembers">全选</button>
              <button type="button" class="btn sm" @click="clearMembers">全不选</button>
              <button
                type="button"
                class="btn sm primary"
                :disabled="savingMembers || !membersDirty"
                @click="saveMembers"
              >
                {{ savingMembers ? '保存中…' : '保存成员' }}
              </button>
            </div>
          </div>
          <div class="card-body stack">
            <p class="small muted plain-p">
              勾选的账号参与本池调度；清空 = 全部含凭据账号自动入池。保存后线程安全替换，进行中的请求不受影响。
            </p>
            <div v-if="detail.members.length === 0" class="empty">
              <strong>这个池还没有账号</strong>
              <span>先在账号页添加账号，或让插件完成登录后再回来配置池成员。</span>
              <RouterLink class="btn sm" to="/accounts">前往账号页</RouterLink>
            </div>
            <div v-else class="member-list">
              <label
                v-for="member in detail.members"
                :key="member.account_id"
                class="member-item"
                :class="{ 'is-off': !member.enabled }"
              >
                <input
                  type="checkbox"
                  :checked="selected.includes(member.account_id)"
                  @change="toggleMember(member.account_id)"
                />
                <span class="member-main">
                  <span class="member-name">
                    {{ member.display_name || member.account_id }}
                    <span v-if="!member.enabled" class="badge danger plain">已禁用</span>
                    <span class="badge plain muted">{{ member.status || '未知' }}</span>
                  </span>
                  <span class="member-meta mono">{{ member.account_id }}</span>
                </span>
                <span class="member-remain">
                  <span class="small muted">剩余</span>
                  <strong>{{ formatNumber(member.remaining) }}</strong>
                </span>
              </label>
            </div>
          </div>
        </section>

        <section class="card">
          <div class="card-head">
            <h2>模型目录</h2>
            <div class="row">
              <span class="small muted">人工覆盖优先于插件上报，同步不会覆盖人工值</span>
              <span class="badge plain muted">{{ enabledCount }} / {{ detail.catalog.length }} 已接入</span>
              <button type="button" class="btn sm" :disabled="syncing" @click="syncCatalog">
                {{ syncing ? '同步中…' : '同步官网模型' }}
              </button>
            </div>
          </div>
          <div v-if="detail.catalog.length === 0" class="empty">
            <strong>模型目录为空</strong>
            <span>点击「同步官网模型」从插件拉取模型清单；插件未上报的倍率/上下文/视觉/思考等字段显示「—」。</span>
            <button type="button" class="btn sm" :disabled="syncing" @click="syncCatalog">
              {{ syncing ? '同步中…' : '同步官网模型' }}
            </button>
          </div>
          <div v-else class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  <th>模型</th>
                  <th>展示名称</th>
                  <th>厂商</th>
                  <th>倍率</th>
                  <th>上下文</th>
                  <th>视觉</th>
                  <th>思考</th>
                  <th>免费</th>
                  <th>接入</th>
                  <th style="text-align: right">操作</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="row in catalogRows" :key="row.model.model_id">
                  <td class="title-cell mono">
                    {{ row.model.model_id }}
                    <span
                      v-if="row.override"
                      class="badge accent plain"
                      :title="`人工覆盖：${overriddenLabels(row.override).join('、') || '已清除'}`"
                    >
                      人工
                    </span>
                  </td>
                  <td class="small">{{ row.displayName || '—' }}</td>
                  <td class="small">{{ row.vendor || '—' }}</td>
                  <td class="num">{{ multiplierLabel(row.multiplier) }}</td>
                  <td class="num">{{ contextLabel(row.contextLength) }}</td>
                  <td>
                    <span class="badge plain" :class="row.vision ? 'ok' : 'muted'">
                      {{ row.vision ? '支持' : '—' }}
                    </span>
                  </td>
                  <td>
                    <span
                      class="badge plain"
                      :class="row.thinking.length ? 'ok' : 'muted'"
                      :title="
                        row.thinking.length
                          ? `思考档位：${row.thinking.join('、')}${row.override?.thinking ? '（人工）' : ''}`
                          : '插件未上报思考档位'
                      "
                    >
                      {{ thinkingLabel(row.thinking) }}
                    </span>
                  </td>
                  <td>
                    <span class="badge plain" :class="row.free ? 'accent' : 'muted'">
                      {{ row.free ? '免费' : '—' }}
                    </span>
                  </td>
                  <td>
                    <label class="mini-check" :title="row.model.enabled === false ? '未接入：网关不列出、也不转发该模型' : '已接入：网关会列出并允许调用'">
                      <input
                        type="checkbox"
                        :checked="row.model.enabled !== false"
                        :disabled="togglingModel === row.model.model_id"
                        @change="toggleModel(row.model, $event)"
                      />
                    </label>
                  </td>
                  <td>
                    <div class="btn-row" style="justify-content: flex-end">
                      <button
                        type="button"
                        class="btn sm"
                        :disabled="probingModel === row.model.model_id"
                        @click="probeCatalogModel(row.model)"
                      >
                        {{ probingModel === row.model.model_id ? '检测中…' : '检测' }}
                      </button>
                      <span
                        v-if="catalogProbe[row.model.model_id]"
                        class="probe-inline"
                        :class="probeTone(row.model.model_id)"
                        :title="probeTitle(row.model.model_id)"
                      >
                        {{ probeLabel(row.model.model_id) }}
                      </span>
                      <button type="button" class="btn sm" @click="openOverrideEditor(row.model)">编辑</button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>
      </template>
    </template>

    <!-- 编辑元数据：人工覆盖层（展示名/厂商/上下文/视觉/思考/倍率/免费） -->
    <Transition name="drawer">
    <div v-if="editingModel" class="drawer-backdrop" @click.self="closeOverrideEditor">
      <aside class="drawer" role="dialog" aria-modal="true" aria-labelledby="override-title">
        <header class="drawer-head">
          <div>
            <h2 id="override-title">编辑模型元数据</h2>
            <div class="small muted mono">{{ editingModel.model_id }}</div>
          </div>
          <button type="button" class="btn sm ghost" :disabled="overrideBusy" @click="closeOverrideEditor">
            关闭
          </button>
        </header>
        <div class="drawer-body">
          <p class="small muted plain-p">
            人工值优先于插件上报，同步官网模型不会覆盖；留空表示跟随插件上报。
          </p>
          <div class="form-grid">
            <div class="field">
              <label for="override-name">展示名称</label>
              <input
                id="override-name"
                v-model="overrideDraft.display_name"
                class="input"
                type="text"
                maxlength="128"
                placeholder="跟随插件上报"
              />
            </div>
            <div class="field">
              <label for="override-vendor">厂商</label>
              <input
                id="override-vendor"
                v-model="overrideDraft.vendor"
                class="input"
                type="text"
                maxlength="64"
                placeholder="跟随插件上报"
              />
            </div>
            <div class="field">
              <label for="override-context">上下文（tokens）</label>
              <input
                id="override-context"
                v-model="overrideDraft.context_length"
                class="input"
                type="number"
                min="1"
                step="1"
                placeholder="跟随插件上报"
              />
              <span class="help">须为正整数。</span>
            </div>
            <div class="field">
              <label for="override-multiplier">倍率</label>
              <input
                id="override-multiplier"
                v-model="overrideDraft.multiplier"
                class="input"
                type="number"
                min="0"
                step="0.01"
                placeholder="跟随插件上报"
              />
              <span class="help">须 ≥ 0；0 表示免费倍率。</span>
            </div>
            <div class="field">
              <label for="override-vision">视觉</label>
              <select id="override-vision" v-model="overrideDraft.vision" class="select">
                <option value="">跟随插件上报</option>
                <option value="true">支持</option>
                <option value="false">不支持</option>
              </select>
            </div>
            <div class="field">
              <label for="override-free">免费</label>
              <select id="override-free" v-model="overrideDraft.free" class="select">
                <option value="">跟随插件上报</option>
                <option value="true">免费</option>
                <option value="false">计费</option>
              </select>
            </div>
            <div class="field span-2">
              <label for="override-thinking">思考档位</label>
              <input
                id="override-thinking"
                v-model="overrideDraft.thinking"
                class="input"
                type="text"
                placeholder="如 low, medium, high"
              />
              <span class="help">用逗号分隔，最多 12 档；留空表示跟随插件上报。</span>
            </div>
          </div>
          <div class="row between">
            <button
              type="button"
              class="btn danger"
              :disabled="overrideBusy || !editingOverride"
              @click="clearOverride"
            >
              {{ overrideBusy ? '处理中…' : '清除覆盖' }}
            </button>
            <div class="btn-row">
              <button type="button" class="btn ghost" :disabled="overrideBusy" @click="closeOverrideEditor">
                取消
              </button>
              <button type="button" class="btn primary" :disabled="overrideBusy" @click="saveOverride">
                {{ overrideBusy ? '保存中…' : '保存' }}
              </button>
            </div>
          </div>
        </div>
      </aside>
    </div>
    </Transition>

    <div class="toasts" role="status" aria-live="polite">
      <div v-for="item in toasts" :key="item.id" class="toast" :class="item.tone">{{ item.text }}</div>
    </div>
  </div>
</template>

<style scoped>
/* Card intro copy that must not add outer margin next to the toolbar. */
.plain-p {
  margin: 0;
}

.intro-note {
  max-width: 760px;
  margin: 0;
}

.hint-row {
  gap: var(--space-3);
}

/* Inline result of one model probe, next to 检测 in the catalog row. */
.probe-inline {
  max-width: 132px;
  overflow: hidden;
  font-size: var(--text-xs);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.probe-inline.ok {
  color: var(--ok);
}

.probe-inline.danger {
  color: var(--danger);
}
</style>
