<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { RouterLink } from 'vue-router'
import {
  api,
  errorMessage,
  normalizePoolSettings,
  normalizeSchedulerState,
  samePoolSettings,
  INTER_POOL_LABELS,
  POOL_STRATEGY_KEYS,
  POOL_STRATEGY_LABELS,
  type ModelBinding,
  type ModelBindingSource,
  type PluginStatus,
  type PoolSettings,
  type SchedulerCandidate,
  type SchedulerExplainRequest,
  type SchedulerExplainResponse,
  type SchedulerGlobalSettings,
  type SchedulerPoolSettings,
  type SchedulerPreset,
  type SchedulerState,
  type StickyBinding,
} from '../api'
import { formatTimestamp } from '../format'

interface ToastItem {
  id: number
  text: string
  tone: 'ok' | 'warn' | 'error'
}

interface PoolDraft extends PoolSettings {
  upstream_enabled: boolean
  pool_priority: number
}

interface SourceDraft extends ModelBindingSource {
  /** Free-text mode: the manifest has no model list or the model was typed by hand. */
  custom: boolean
}

interface BindingDraft {
  logical_model: string
  display_name: string
  items: SourceDraft[]
}

interface PluginOption {
  id: string
  name: string
  models: { id: string; name?: string }[]
}

interface PresetItem {
  key: SchedulerPreset
  label: string
  desc: string
  tag?: string
}

const PRESETS: PresetItem[] = [
  {
    key: 'balanced',
    label: '最佳组合',
    desc: '池间智能 + 积分先过期 + 并发 2 + 粘性 600s，对冲与降档关闭。',
    tag: '推荐',
  },
  {
    key: 'freshness',
    label: '积分保鲜',
    desc: '优先快到期账号，缩短池粘性，减少积分浪费。',
  },
  {
    key: 'amortized',
    label: '均匀摊销',
    desc: '剩余积分多优先，粘性更长，把消耗摊平到多个账号。',
  },
  {
    key: 'latency',
    label: '低延迟优先',
    desc: '按延迟加权随机，粘性更短，开启流式对冲。',
  },
  {
    key: 'fixed',
    label: '固定优先',
    desc: '池间与池内都严格按优先级顺序，失败才回退。',
  },
]

/* ------------------------------------------------------------------ */
/* State                                                               */
/* ------------------------------------------------------------------ */

const state = ref<SchedulerState | null>(null)
const plugins = ref<PluginStatus[]>([])
const loading = ref(false)
const loadError = ref('')

const globalDraft = ref<SchedulerGlobalSettings>({
  inter_pool: 'smart',
  defaults: normalizePoolSettings(null),
})
const globalSaving = ref(false)

const poolDrafts = reactive<Record<string, PoolDraft>>({})
const poolBusy = reactive<Record<string, boolean>>({})
const presetBusy = ref<SchedulerPreset | ''>('')

/* Model bindings */
const bindingOpen = ref(false)
const bindingEditing = ref('')
const bindingDraft = ref<BindingDraft>({ logical_model: '', display_name: '', items: [] })
const bindingError = ref('')
const bindingSaving = ref(false)
const bindingBusy = ref(false)
const pendingModelDelete = ref('')
let pendingDeleteTimer = 0

/* Sticky drawer */
const stickyOpen = ref(false)
const stickyItems = ref<StickyBinding[]>([])
const stickyLoading = ref(false)
const stickyError = ref('')
const stickyClearing = ref(false)
const stickyArmed = ref(false)
let stickyArmTimer = 0

/* Explain */
const explainModel = ref('')
const explainConv = ref('')
const explainBusy = ref(false)
const explainError = ref('')
const explainResult = ref<SchedulerExplainResponse | null>(null)

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
/* Loading                                                             */
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

function toDraft(pool: SchedulerPoolSettings): PoolDraft {
  return {
    ...normalizePoolSettings(pool),
    upstream_enabled: pool.upstream_enabled,
    pool_priority: pool.pool_priority,
  }
}

function poolPayload(pluginId: string, draft: PoolDraft): SchedulerPoolSettings {
  return {
    plugin_id: pluginId,
    upstream_enabled: draft.upstream_enabled,
    pool_priority: draft.pool_priority,
    ...settingsOf(draft),
  }
}

function poolDirty(pluginId: string): boolean {
  const pool = state.value?.pools.find((item) => item.plugin_id === pluginId)
  const draft = poolDrafts[pluginId]
  if (!pool || !draft) return false
  return (
    pool.upstream_enabled !== draft.upstream_enabled ||
    pool.pool_priority !== draft.pool_priority ||
    !samePoolSettings(pool, draft)
  )
}

const globalDirty = computed(() => {
  const current = state.value
  if (!current) return false
  return (
    current.global.inter_pool !== globalDraft.value.inter_pool ||
    !samePoolSettings(current.global.defaults, globalDraft.value.defaults)
  )
})

async function load() {
  loading.value = true
  try {
    const [sched, pluginList] = await Promise.all([
      api.scheduler.get(),
      api.plugins.list().catch(() => [] as PluginStatus[]),
    ])
    const next = normalizeSchedulerState(sched)
    const dirtyRows = new Set<string>()
    if (state.value) {
      for (const pool of state.value.pools) {
        if (poolDirty(pool.plugin_id)) dirtyRows.add(pool.plugin_id)
      }
    }
    const keepGlobal = globalDirty.value
    state.value = next
    if (!keepGlobal) {
      globalDraft.value = {
        inter_pool: next.global.inter_pool,
        defaults: settingsOf(next.global.defaults),
      }
    }
    for (const pool of next.pools) {
      if (!poolDrafts[pool.plugin_id] || !dirtyRows.has(pool.plugin_id)) {
        poolDrafts[pool.plugin_id] = toDraft(pool)
      }
    }
    for (const id of Object.keys(poolDrafts)) {
      if (!next.pools.some((pool) => pool.plugin_id === id)) delete poolDrafts[id]
    }
    plugins.value = pluginList ?? []
    loadError.value = ''
  } catch (e) {
    loadError.value = errorMessage(e)
  } finally {
    loading.value = false
  }
}

onMounted(() => void load())

onBeforeUnmount(() => {
  for (const timer of toastTimers) window.clearTimeout(timer)
  toastTimers.clear()
  window.clearTimeout(pendingDeleteTimer)
  window.clearTimeout(stickyArmTimer)
})

/* ------------------------------------------------------------------ */
/* Validation                                                          */
/* ------------------------------------------------------------------ */

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
  return ''
}

function validatePriority(value: number): string {
  const priority = Number(value)
  if (!Number.isInteger(priority) || priority < 0 || priority > 1000) {
    return '池优先级需为 0–1000 的整数，数值小者优先。'
  }
  return ''
}

/* ------------------------------------------------------------------ */
/* Global defaults                                                     */
/* ------------------------------------------------------------------ */

async function saveGlobal() {
  const problem = validateSettings(globalDraft.value.defaults)
  if (problem) {
    toast(problem, 'error')
    return
  }
  globalSaving.value = true
  try {
    await api.scheduler.saveGlobal({
      inter_pool: globalDraft.value.inter_pool,
      defaults: settingsOf(globalDraft.value.defaults),
    })
    await load()
    toast('全局默认已保存，立即生效。')
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    globalSaving.value = false
  }
}

/* ------------------------------------------------------------------ */
/* Presets                                                             */
/* ------------------------------------------------------------------ */

interface Snapshot {
  inter_pool: SchedulerGlobalSettings['inter_pool']
  defaults: PoolSettings
  pools: {
    plugin_id: string
    settings: PoolSettings
    priority: number
    upstream: boolean
  }[]
}

function snapshotState(): Snapshot | null {
  const current = state.value
  if (!current) return null
  return {
    inter_pool: current.global.inter_pool,
    defaults: settingsOf(current.global.defaults),
    pools: current.pools.map((pool) => ({
      plugin_id: pool.plugin_id,
      settings: settingsOf(pool),
      priority: pool.pool_priority,
      upstream: pool.upstream_enabled,
    })),
  }
}

function settingsChanges(prefix: string, before: PoolSettings, after: PoolSettings): string[] {
  const parts: string[] = []
  if (before.strategy !== after.strategy) {
    parts.push(`${prefix}池内策略 → ${POOL_STRATEGY_LABELS[after.strategy]}`)
  }
  if (before.account_concurrency !== after.account_concurrency) {
    parts.push(`${prefix}并发 → ${after.account_concurrency === 0 ? '不限' : after.account_concurrency}`)
  }
  if (before.pool_sticky_ttl !== after.pool_sticky_ttl) {
    parts.push(`${prefix}池粘性 → ${after.pool_sticky_ttl}s`)
  }
  if (before.conv_sticky_ttl !== after.conv_sticky_ttl) {
    parts.push(`${prefix}会话粘性 → ${after.conv_sticky_ttl}s`)
  }
  if (before.hedge_ms !== after.hedge_ms) {
    parts.push(`${prefix}对冲 → ${after.hedge_ms > 0 ? `${after.hedge_ms}ms` : '关闭'}`)
  }
  if (before.default_reasoning !== after.default_reasoning) {
    parts.push(`${prefix}深度思考 → ${after.default_reasoning ? '开' : '关'}`)
  }
  if (before.background_downgrade !== after.background_downgrade) {
    parts.push(`${prefix}后台降级 → ${after.background_downgrade ? '开' : '关'}`)
  }
  if (before.longctx_downgrade !== after.longctx_downgrade) {
    parts.push(`${prefix}长文降档 → ${after.longctx_downgrade ? '开' : '关'}`)
  }
  return parts
}

function diffChanges(before: Snapshot | null, after: SchedulerState | null): string[] {
  if (!before || !after) return []
  const parts: string[] = []
  if (before.inter_pool !== after.global.inter_pool) {
    parts.push(`池间策略 → ${INTER_POOL_LABELS[after.global.inter_pool]}`)
  }
  parts.push(...settingsChanges('默认', before.defaults, after.global.defaults))
  let changedPools = 0
  for (const pool of after.pools) {
    const previous = before.pools.find((item) => item.plugin_id === pool.plugin_id)
    if (
      !previous ||
      previous.priority !== pool.pool_priority ||
      previous.upstream !== pool.upstream_enabled ||
      !samePoolSettings(previous.settings, pool)
    ) {
      changedPools += 1
    }
  }
  if (changedPools > 0) parts.push(`已更新 ${changedPools} 个池`)
  return parts
}

function backendNote(result: Awaited<ReturnType<typeof api.scheduler.preset>>): string {
  const changes = result?.changes
  if (Array.isArray(changes) && changes.length > 0) return changes.join('；')
  if (typeof changes === 'string' && changes.trim()) return changes.trim()
  if (Array.isArray(result?.applied) && result.applied.length > 0) return result.applied.join('；')
  return result?.message ?? ''
}

async function applyPreset(preset: PresetItem) {
  if (presetBusy.value) return
  const before = snapshotState()
  presetBusy.value = preset.key
  try {
    const result = await api.scheduler.preset(preset.key)
    await load()
    const changes = diffChanges(before, state.value)
    const summary =
      changes.length > 0
        ? changes.join('；')
        : backendNote(result) || '配置已与预设一致，无需变更'
    toast(`${preset.label}已应用：${summary}`)
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    presetBusy.value = ''
  }
}

/* ------------------------------------------------------------------ */
/* Pools                                                               */
/* ------------------------------------------------------------------ */

function healthOf(pluginId: string): boolean {
  return state.value?.health.some((item) => item.plugin_id === pluginId && item.circuit_open) ?? false
}

function poolLabel(pluginId: string): string {
  const pool = state.value?.pools.find((item) => item.plugin_id === pluginId)
  return pool?.name || pluginId
}

async function savePool(pluginId: string) {
  const draft = poolDrafts[pluginId]
  if (!draft) return
  const problem = validateSettings(draft) || validatePriority(draft.pool_priority)
  if (problem) {
    toast(`${poolLabel(pluginId)}：${problem}`, 'error')
    return
  }
  poolBusy[pluginId] = true
  try {
    await api.scheduler.savePool(pluginId, poolPayload(pluginId, draft))
    await load()
    toast(`「${poolLabel(pluginId)}」的调度参数已保存。`)
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    poolBusy[pluginId] = false
  }
}

async function toggleUpstream(pool: SchedulerPoolSettings, event: Event) {
  const checked = (event.target as HTMLInputElement).checked
  const draft = poolDrafts[pool.plugin_id]
  if (!draft) return
  draft.upstream_enabled = checked
  poolBusy[pool.plugin_id] = true
  try {
    await api.scheduler.savePool(pool.plugin_id, poolPayload(pool.plugin_id, draft))
    await load()
    toast(
      checked
        ? `「${poolLabel(pool.plugin_id)}」上游已开启。`
        : `「${poolLabel(pool.plugin_id)}」上游已关闭，模型将从 /v1/models 隐藏。`,
      checked ? 'ok' : 'warn',
    )
  } catch (e) {
    toast(errorMessage(e), 'error')
    await load()
  } finally {
    poolBusy[pool.plugin_id] = false
  }
}

/* ------------------------------------------------------------------ */
/* Model bindings                                                      */
/* ------------------------------------------------------------------ */

const pluginOptions = computed<PluginOption[]>(() => {
  const map = new Map<string, PluginOption>()
  for (const plugin of plugins.value) {
    const id = plugin.manifest?.id ?? ''
    if (!id) continue
    map.set(id, {
      id,
      name: plugin.manifest?.name || id,
      models: (plugin.manifest?.models ?? []).map((model) => ({ id: model.id, name: model.name })),
    })
  }
  for (const pool of state.value?.pools ?? []) {
    if (!map.has(pool.plugin_id)) {
      map.set(pool.plugin_id, { id: pool.plugin_id, name: pool.name || pool.plugin_id, models: [] })
    }
  }
  return [...map.values()].sort((a, b) => a.name.localeCompare(b.name, 'zh-CN'))
})

function modelsFor(pluginId: string): { id: string; name?: string }[] {
  return pluginOptions.value.find((option) => option.id === pluginId)?.models ?? []
}

function sourceDraftOf(source: ModelBindingSource): SourceDraft {
  const known = modelsFor(source.plugin_id).some((model) => model.id === source.model)
  return { plugin_id: source.plugin_id, model: source.model, custom: !known }
}

function openCreate() {
  bindingEditing.value = ''
  bindingError.value = ''
  const pluginId = pluginOptions.value[0]?.id ?? ''
  bindingDraft.value = {
    logical_model: '',
    display_name: '',
    items: [{ plugin_id: pluginId, model: '', custom: modelsFor(pluginId).length === 0 }],
  }
  bindingOpen.value = true
}

function openEdit(model: ModelBinding) {
  bindingEditing.value = model.logical_model
  bindingError.value = ''
  bindingDraft.value = {
    logical_model: model.logical_model,
    display_name: model.display_name || model.logical_model,
    items: model.items.map(sourceDraftOf),
  }
  bindingOpen.value = true
}

function closeBinding() {
  bindingOpen.value = false
  bindingEditing.value = ''
  bindingError.value = ''
}

function addSource() {
  const pluginId = pluginOptions.value[0]?.id ?? ''
  bindingDraft.value.items.push({
    plugin_id: pluginId,
    model: '',
    custom: modelsFor(pluginId).length === 0,
  })
}

function removeSource(index: number) {
  bindingDraft.value.items.splice(index, 1)
}

function moveSource(index: number, delta: number) {
  const items = bindingDraft.value.items
  const target = index + delta
  if (target < 0 || target >= items.length) return
  const [moved] = items.splice(index, 1)
  items.splice(target, 0, moved)
}

function onPluginChange(item: SourceDraft, event: Event) {
  const pluginId = (event.target as HTMLSelectElement).value
  item.plugin_id = pluginId
  item.model = ''
  item.custom = modelsFor(pluginId).length === 0
}

function onModelSelect(item: SourceDraft, event: Event) {
  const value = (event.target as HTMLSelectElement).value
  if (!value) return
  if (value === '__custom__') {
    item.custom = true
    item.model = ''
    return
  }
  item.custom = false
  item.model = value
}

function validateBinding(): string {
  const draft = bindingDraft.value
  const logical = draft.logical_model.trim()
  if (!logical) return '请填写逻辑模型名称。'
  if (!/^[A-Za-z0-9][A-Za-z0-9._:/-]*$/.test(logical)) {
    return '逻辑模型名称只能包含字母、数字与 . _ : - /。'
  }
  if (!bindingEditing.value && state.value?.models.some((model) => model.logical_model === logical)) {
    return '该逻辑模型已存在，请直接编辑现有绑定。'
  }
  if (!draft.display_name.trim()) return '请填写展示名称。'
  if (draft.items.length === 0) return '至少添加一个来源。'
  const seen = new Set<string>()
  for (const item of draft.items) {
    if (!item.plugin_id) return '每个来源都需要选择插件。'
    const model = item.model.trim()
    if (!model) return '每个来源都需要填写模型。'
    const key = `${item.plugin_id}/${model}`
    if (seen.has(key)) return `来源重复：${key}`
    seen.add(key)
  }
  return ''
}

async function saveBinding() {
  const problem = validateBinding()
  bindingError.value = problem
  if (problem) return
  bindingSaving.value = true
  try {
    const logical = bindingDraft.value.logical_model.trim()
    await api.scheduler.saveModel(logical, {
      display_name: bindingDraft.value.display_name.trim(),
      items: bindingDraft.value.items.map((item) => ({
        plugin_id: item.plugin_id,
        model: item.model.trim(),
      })),
    })
    bindingOpen.value = false
    bindingEditing.value = ''
    await load()
    toast(`模型绑定「${logical}」已保存。`)
  } catch (e) {
    bindingError.value = errorMessage(e)
  } finally {
    bindingSaving.value = false
  }
}

async function persistModel(model: ModelBinding, items: ModelBindingSource[], message: string) {
  bindingBusy.value = true
  try {
    await api.scheduler.saveModel(model.logical_model, {
      display_name: model.display_name || model.logical_model,
      items,
    })
    await load()
    toast(message)
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    bindingBusy.value = false
  }
}

function moveBindingSource(model: ModelBinding, index: number, delta: number) {
  const items = model.items.map((item) => ({ ...item }))
  const target = index + delta
  if (target < 0 || target >= items.length) return
  const [moved] = items.splice(index, 1)
  items.splice(target, 0, moved)
  void persistModel(model, items, `已调整「${model.logical_model}」的来源顺序。`)
}

function removeBindingSource(model: ModelBinding, index: number) {
  if (model.items.length <= 1) {
    toast('至少保留一个来源；如需下线该逻辑模型请直接删除绑定。', 'warn')
    return
  }
  const items = model.items.filter((_, position) => position !== index).map((item) => ({ ...item }))
  void persistModel(model, items, `已移除「${model.logical_model}」的一个来源。`)
}

function requestDeleteModel(logical: string) {
  if (pendingModelDelete.value === logical) {
    pendingModelDelete.value = ''
    window.clearTimeout(pendingDeleteTimer)
    void deleteModel(logical)
    return
  }
  pendingModelDelete.value = logical
  window.clearTimeout(pendingDeleteTimer)
  pendingDeleteTimer = window.setTimeout(() => {
    pendingModelDelete.value = ''
  }, 5000)
}

async function deleteModel(logical: string) {
  try {
    await api.scheduler.removeModel(logical)
    await load()
    toast(`模型绑定「${logical}」已删除。`)
  } catch (e) {
    toast(errorMessage(e), 'error')
  }
}

/* ------------------------------------------------------------------ */
/* Sticky bindings                                                     */
/* ------------------------------------------------------------------ */

async function loadSticky() {
  stickyLoading.value = true
  stickyError.value = ''
  try {
    const data = await api.scheduler.sticky()
    stickyItems.value = Array.isArray(data?.items) ? data.items : []
  } catch (e) {
    stickyError.value = errorMessage(e)
  } finally {
    stickyLoading.value = false
  }
}

function openSticky() {
  stickyOpen.value = true
  stickyArmed.value = false
  void loadSticky()
}

function requestClearSticky() {
  if (!stickyArmed.value) {
    stickyArmed.value = true
    window.clearTimeout(stickyArmTimer)
    stickyArmTimer = window.setTimeout(() => {
      stickyArmed.value = false
    }, 5000)
    return
  }
  stickyArmed.value = false
  window.clearTimeout(stickyArmTimer)
  void clearSticky()
}

async function clearSticky() {
  stickyClearing.value = true
  stickyError.value = ''
  try {
    await api.scheduler.clearSticky()
    await loadSticky()
    await load()
    toast('粘性绑定已全部清理。')
  } catch (e) {
    stickyError.value = errorMessage(e)
  } finally {
    stickyClearing.value = false
  }
}

/* ------------------------------------------------------------------ */
/* Explain                                                             */
/* ------------------------------------------------------------------ */

const decision = computed(() => explainResult.value?.decision ?? null)
const candidates = computed<SchedulerCandidate[]>(() => explainResult.value?.candidates ?? [])

function isChosen(candidate: SchedulerCandidate): boolean {
  if (candidate.chosen) return true
  const current = decision.value
  if (!current) return false
  return (
    current.plugin_id === candidate.plugin_id && current.account_id === candidate.account_id
  )
}

function scoreText(score?: number): string {
  return typeof score === 'number' && Number.isFinite(score) ? score.toFixed(3) : '—'
}

async function runExplain() {
  const model = explainModel.value.trim()
  if (!model) {
    explainError.value = '请填写要解释的模型，逻辑模型名或 plugin/model 均可。'
    explainResult.value = null
    return
  }
  explainBusy.value = true
  explainError.value = ''
  explainResult.value = null
  try {
    const payload: SchedulerExplainRequest = { model }
    const conversation = explainConv.value.trim()
    if (conversation) payload.conversation_id = conversation
    explainResult.value = (await api.scheduler.explain(payload)) ?? null
  } catch (e) {
    explainError.value = errorMessage(e)
  } finally {
    explainBusy.value = false
  }
}
</script>

<template>
  <div class="page">
    <div v-if="loadError" class="notice error">
      <div class="row between">
        <span>{{ loadError }}</span>
        <button type="button" class="btn sm" :disabled="loading" @click="load">重试</button>
      </div>
    </div>

    <div class="notice">
      保存即生效：每次保存都会写入池设置并递增策略版本号，网关每请求读取原子快照，无需重启。
    </div>

    <div v-if="loading && !state" class="loading">正在加载调度配置…</div>

    <template v-if="state">
      <section class="card">
        <div class="card-head">
          <h2>全局默认</h2>
          <div class="row">
            <span class="badge ok plain">即时生效</span>
            <span class="badge plain muted">当前版本 v{{ state.version }}</span>
          </div>
        </div>
        <div class="card-body stack">
          <div class="form-grid">
            <div class="field">
              <label for="global-inter-pool">池间策略</label>
              <select id="global-inter-pool" v-model="globalDraft.inter_pool" class="select">
                <option v-for="(label, value) in INTER_POOL_LABELS" :key="value" :value="value">
                  {{ label }}
                </option>
              </select>
              <span class="help">智能调度按积分到期、倍率、剩余积分与池优先级排序；固定优先严格按池顺序。</span>
            </div>
            <div class="field">
              <label for="global-strategy">默认池内策略</label>
              <select id="global-strategy" v-model="globalDraft.defaults.strategy" class="select">
                <option v-for="key in POOL_STRATEGY_KEYS" :key="key" :value="key">
                  {{ POOL_STRATEGY_LABELS[key] }}
                </option>
              </select>
              <span class="help">作为新建池的默认值，单个池可在下方覆盖。</span>
            </div>
            <div class="field">
              <label for="global-concurrency">全局并发</label>
              <input
                id="global-concurrency"
                v-model.number="globalDraft.defaults.account_concurrency"
                class="input"
                type="number"
                min="0"
                max="32"
                step="1"
              />
              <span class="help">每个账号的在途上限，0 = 不限。</span>
            </div>
            <div class="field">
              <label for="global-pool-ttl">池粘性（秒）</label>
              <input
                id="global-pool-ttl"
                v-model.number="globalDraft.defaults.pool_sticky_ttl"
                class="input"
                type="number"
                min="0"
                max="86400"
                step="1"
              />
              <span class="help">同一模型 + 客户端固定到同一账号，复用上游缓存。</span>
            </div>
            <div class="field">
              <label for="global-conv-ttl">会话粘性（秒）</label>
              <input
                id="global-conv-ttl"
                v-model.number="globalDraft.defaults.conv_sticky_ttl"
                class="input"
                type="number"
                min="0"
                max="86400"
                step="1"
              />
              <span class="help">优先于池粘性，按 conversation_id 绑定账号。</span>
            </div>
            <div class="field">
              <label for="global-hedge">对冲（毫秒）</label>
              <input
                id="global-hedge"
                v-model.number="globalDraft.defaults.hedge_ms"
                class="input"
                type="number"
                min="0"
                max="8000"
                step="100"
              />
              <span class="help">0 = 关闭；开启需 1000–8000 ms，首字节超时后向下一候选对冲。</span>
            </div>
            <div class="field span-2">
              <span class="field-label">默认降档与思考</span>
              <div class="mini-checks">
                <label class="mini-check">
                  <input v-model="globalDraft.defaults.default_reasoning" type="checkbox" />
                  默认深度思考
                </label>
                <label class="mini-check">
                  <input v-model="globalDraft.defaults.background_downgrade" type="checkbox" />
                  后台任务降级
                </label>
                <label class="mini-check">
                  <input v-model="globalDraft.defaults.longctx_downgrade" type="checkbox" />
                  长上下文降档
                </label>
              </div>
              <span class="help">降档会在日志与 X-Aiworkgate-Downgraded 响应头标记，客户端可感知。</span>
            </div>
          </div>
          <div class="row">
            <button
              type="button"
              class="btn primary"
              :disabled="globalSaving || !globalDirty"
              @click="saveGlobal"
            >
              {{ globalSaving ? '保存中…' : '保存全局默认' }}
            </button>
            <span v-if="globalDirty" class="dirty-hint"><span class="dirty-dot"></span>有未保存的更改</span>
          </div>
        </div>
      </section>

      <section class="card">
        <div class="card-head">
          <h2>预设</h2>
          <span class="sub">一键写入所有池，应用后自动刷新</span>
        </div>
        <div class="card-body">
          <div class="preset-grid">
            <button
              v-for="preset in PRESETS"
              :key="preset.key"
              type="button"
              class="preset-card"
              :disabled="presetBusy !== ''"
              @click="applyPreset(preset)"
            >
              <span class="preset-head">
                <strong>{{ presetBusy === preset.key ? `${preset.label}应用中…` : preset.label }}</strong>
                <span v-if="preset.tag" class="preset-tag">{{ preset.tag }}</span>
              </span>
              <span class="preset-desc">{{ preset.desc }}</span>
            </button>
          </div>
        </div>
      </section>

      <section class="card">
        <div class="card-head">
          <h2>池与账号</h2>
          <span class="badge plain muted">{{ state.pools.length }} 个池</span>
        </div>
        <div v-if="state.pools.length === 0" class="empty">
          <strong>还没有可调度的池</strong>
          <span>安装并启用插件后，宿主会为每个插件创建资源池。</span>
          <RouterLink class="btn sm" to="/plugins">前往插件页</RouterLink>
        </div>
        <div v-else class="table-wrap">
          <table class="table sched-table">
            <thead>
              <tr>
                <th>池</th>
                <th>上游</th>
                <th>池内策略</th>
                <th>优先级</th>
                <th>并发</th>
                <th>池粘性(s)</th>
                <th>会话粘性(s)</th>
                <th>对冲(ms)</th>
                <th>思考 / 降档</th>
                <th style="text-align: right">操作</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="pool in state.pools"
                :key="pool.plugin_id"
                :class="{ 'row-dirty': poolDirty(pool.plugin_id) }"
              >
                <td class="title-cell col-pool">
                  <div class="row pool-title-row">
                    <span>{{ poolLabel(pool.plugin_id) }}</span>
                    <span v-if="healthOf(pool.plugin_id)" class="badge warn">熔断中</span>
                  </div>
                  <div class="small muted mono">{{ pool.plugin_id }}</div>
                </td>
                <td>
                  <label class="switch">
                    <input
                      type="checkbox"
                      :checked="poolDrafts[pool.plugin_id]?.upstream_enabled"
                      :disabled="poolBusy[pool.plugin_id]"
                      @change="toggleUpstream(pool, $event)"
                    />
                    <span class="switch-track"></span>
                    <span class="small">
                      {{ poolDrafts[pool.plugin_id]?.upstream_enabled ? '开' : '关' }}
                    </span>
                  </label>
                </td>
                <td>
                  <select
                    v-model="poolDrafts[pool.plugin_id].strategy"
                    class="select"
                    :aria-label="`${pool.plugin_id} 池内策略`"
                  >
                    <option v-for="key in POOL_STRATEGY_KEYS" :key="key" :value="key">
                      {{ POOL_STRATEGY_LABELS[key] }}
                    </option>
                  </select>
                </td>
                <td>
                  <input
                    v-model.number="poolDrafts[pool.plugin_id].pool_priority"
                    class="input num-input"
                    type="number"
                    min="0"
                    max="1000"
                    step="1"
                    :aria-label="`${pool.plugin_id} 池优先级`"
                  />
                </td>
                <td>
                  <input
                    v-model.number="poolDrafts[pool.plugin_id].account_concurrency"
                    class="input num-input"
                    type="number"
                    min="0"
                    max="32"
                    step="1"
                    :aria-label="`${pool.plugin_id} 账号并发`"
                  />
                </td>
                <td>
                  <input
                    v-model.number="poolDrafts[pool.plugin_id].pool_sticky_ttl"
                    class="input num-input"
                    type="number"
                    min="0"
                    max="86400"
                    step="1"
                    :aria-label="`${pool.plugin_id} 池粘性`"
                  />
                </td>
                <td>
                  <input
                    v-model.number="poolDrafts[pool.plugin_id].conv_sticky_ttl"
                    class="input num-input"
                    type="number"
                    min="0"
                    max="86400"
                    step="1"
                    :aria-label="`${pool.plugin_id} 会话粘性`"
                  />
                </td>
                <td>
                  <input
                    v-model.number="poolDrafts[pool.plugin_id].hedge_ms"
                    class="input num-input"
                    type="number"
                    min="0"
                    max="8000"
                    step="100"
                    :aria-label="`${pool.plugin_id} 对冲毫秒`"
                  />
                </td>
                <td>
                  <div class="mini-checks">
                    <label class="mini-check">
                      <input v-model="poolDrafts[pool.plugin_id].default_reasoning" type="checkbox" />
                      思考
                    </label>
                    <label class="mini-check">
                      <input v-model="poolDrafts[pool.plugin_id].background_downgrade" type="checkbox" />
                      后台
                    </label>
                    <label class="mini-check">
                      <input v-model="poolDrafts[pool.plugin_id].longctx_downgrade" type="checkbox" />
                      长文
                    </label>
                  </div>
                </td>
                <td>
                  <div class="btn-row" style="justify-content: flex-end">
                    <button
                      type="button"
                      class="btn sm"
                      :class="{ primary: poolDirty(pool.plugin_id) }"
                      :disabled="poolBusy[pool.plugin_id] || !poolDirty(pool.plugin_id)"
                      @click="savePool(pool.plugin_id)"
                    >
                      {{ poolBusy[pool.plugin_id] ? '保存中…' : '保存' }}
                    </button>
                    <RouterLink class="btn sm ghost" :to="`/pools/${pool.plugin_id}`">编辑</RouterLink>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <section class="card">
        <div class="card-head">
          <h2>模型绑定</h2>
          <div class="row">
            <span class="badge plain muted">{{ state.models.length }} 个逻辑模型</span>
            <button type="button" class="btn sm" @click="openCreate">新建绑定</button>
          </div>
        </div>
        <div class="card-body stack">
          <p class="small muted plain-p">
            逻辑模型按来源顺序走池间调度；未绑定的 <code>plugin/model</code> 直达单池，仅做池内选号。
          </p>

          <div v-if="bindingOpen" class="binding-editor">
            <div class="row between">
              <strong class="small">{{ bindingEditing ? `编辑绑定 ${bindingEditing}` : '新建绑定' }}</strong>
              <button type="button" class="btn sm ghost" @click="closeBinding">取消</button>
            </div>
            <div class="form-grid">
              <div class="field">
                <label for="binding-logical">逻辑模型名</label>
                <input
                  id="binding-logical"
                  v-model="bindingDraft.logical_model"
                  class="input mono"
                  type="text"
                  placeholder="deepseek-v4-pro"
                  :readonly="Boolean(bindingEditing)"
                />
                <span class="help">保存后不可改名；如需改名请删除后重新创建。</span>
              </div>
              <div class="field">
                <label for="binding-display">展示名称</label>
                <input
                  id="binding-display"
                  v-model="bindingDraft.display_name"
                  class="input"
                  type="text"
                  placeholder="DeepSeek V4 Pro"
                />
              </div>
            </div>

            <div class="field">
              <span class="field-label">来源（自上而下为候选顺序）</span>
              <div v-for="(item, index) in bindingDraft.items" :key="index" class="binding-row">
                <select
                  class="select"
                  :value="item.plugin_id"
                  :aria-label="`来源 ${index + 1} 插件`"
                  @change="onPluginChange(item, $event)"
                >
                  <option value="" disabled>选择插件</option>
                  <option v-for="option in pluginOptions" :key="option.id" :value="option.id">
                    {{ option.name }}（{{ option.id }}）
                  </option>
                </select>
                <input
                  v-if="item.custom"
                  v-model="item.model"
                  class="input mono"
                  type="text"
                  placeholder="插件模型 id，或 插件id/模型id"
                  :aria-label="`来源 ${index + 1} 模型`"
                />
                <select
                  v-else
                  class="select"
                  :value="item.model"
                  :aria-label="`来源 ${index + 1} 模型`"
                  @change="onModelSelect(item, $event)"
                >
                  <option value="" disabled>选择模型</option>
                  <option v-for="model in modelsFor(item.plugin_id)" :key="model.id" :value="model.id">
                    {{ model.name ? `${model.id} · ${model.name}` : model.id }}
                  </option>
                  <option value="__custom__">自定义…</option>
                </select>
                <div class="btn-row">
                  <button
                    type="button"
                    class="btn sm ghost"
                    :disabled="index === 0"
                    aria-label="上移来源"
                    @click="moveSource(index, -1)"
                  >
                    ↑
                  </button>
                  <button
                    type="button"
                    class="btn sm ghost"
                    :disabled="index === bindingDraft.items.length - 1"
                    aria-label="下移来源"
                    @click="moveSource(index, 1)"
                  >
                    ↓
                  </button>
                  <button type="button" class="btn sm danger" @click="removeSource(index)">移除</button>
                </div>
              </div>
              <div class="row">
                <button type="button" class="btn sm" @click="addSource">添加来源</button>
                <span class="small muted">插件清单没有模型时可手填模型 id。</span>
              </div>
            </div>

            <div v-if="bindingError" class="notice error">
              <div class="row between">
                <span>模型绑定保存失败：{{ bindingError }}</span>
                <button type="button" class="btn sm" :disabled="bindingSaving" @click="saveBinding">重试</button>
              </div>
            </div>
            <div class="row">
              <button type="button" class="btn primary" :disabled="bindingSaving" @click="saveBinding">
                {{ bindingSaving ? '保存中…' : '保存绑定' }}
              </button>
            </div>
          </div>

          <div v-if="state.models.length === 0 && !bindingOpen" class="empty">
            <strong>还没有模型绑定</strong>
            <span>新建绑定后，客户端可直接用逻辑模型名请求，由宿主在多个池之间调度。</span>
            <button type="button" class="btn sm" @click="openCreate">新建绑定</button>
          </div>
          <div v-else class="binding-list">
            <div v-for="model in state.models" :key="model.logical_model" class="binding">
              <div class="binding-name">
                <span>{{ model.display_name || model.logical_model }}</span>
                <div class="small muted mono">{{ model.logical_model }}</div>
              </div>
              <div class="chip-row">
                <span
                  v-for="(source, index) in model.items"
                  :key="`${source.plugin_id}/${source.model}`"
                  class="chip"
                >
                  <span class="mono">{{ source.plugin_id }}/{{ source.model }}</span>
                  <button
                    type="button"
                    :disabled="bindingBusy || index === 0"
                    aria-label="上移来源"
                    @click="moveBindingSource(model, index, -1)"
                  >
                    ↑
                  </button>
                  <button
                    type="button"
                    :disabled="bindingBusy || index === model.items.length - 1"
                    aria-label="下移来源"
                    @click="moveBindingSource(model, index, 1)"
                  >
                    ↓
                  </button>
                  <button
                    type="button"
                    :disabled="bindingBusy"
                    aria-label="移除来源"
                    @click="removeBindingSource(model, index)"
                  >
                    ×
                  </button>
                </span>
                <span v-if="model.items.length === 0" class="small muted">无来源</span>
              </div>
              <div class="spacer"></div>
              <div class="btn-row">
                <button type="button" class="btn sm" @click="openEdit(model)">编辑</button>
                <button
                  type="button"
                  class="btn sm danger"
                  :disabled="bindingBusy"
                  @click="requestDeleteModel(model.logical_model)"
                >
                  {{ pendingModelDelete === model.logical_model ? '确认删除' : '删除' }}
                </button>
              </div>
            </div>
          </div>
        </div>
      </section>

      <section class="card">
        <div class="card-head">
          <h2>粘性绑定</h2>
          <div class="row">
            <span class="badge plain muted">{{ state.sticky_count }} 条</span>
            <button type="button" class="btn sm" @click="openSticky">查看列表</button>
          </div>
        </div>
        <div class="card-body">
          <p class="small muted plain-p">
            粘性把同一模型或会话固定到同一账号以复用上游 KV cache；账号被禁用、冷却或额度耗尽时自动失效。
          </p>
        </div>
      </section>

      <section class="card">
        <div class="card-head">
          <h2>解释一次调度</h2>
          <span class="sub">按当前策略离线演算，不占用账号</span>
        </div>
        <div class="card-body stack">
          <form class="explain-form" @submit.prevent="runExplain">
            <div class="field grow">
              <label for="explain-model">模型</label>
              <input
                id="explain-model"
                v-model="explainModel"
                class="input mono"
                type="text"
                placeholder="逻辑模型名，或 plugin/model"
              />
            </div>
            <div class="field grow">
              <label for="explain-conv">会话 ID（可选）</label>
              <input
                id="explain-conv"
                v-model="explainConv"
                class="input mono"
                type="text"
                placeholder="conversation_id"
              />
            </div>
            <button type="submit" class="btn primary" :disabled="explainBusy">
              {{ explainBusy ? '演算中…' : '解释调度' }}
            </button>
          </form>

          <div v-if="explainError" class="notice error">
            <div class="row between">
              <span>调度解释失败：{{ explainError }}</span>
              <button type="button" class="btn sm" :disabled="explainBusy" @click="runExplain">重试</button>
            </div>
          </div>

          <div v-if="explainResult" class="stack">
            <div v-if="decision" class="decision-card">
              <div class="decision-head">
                <strong>{{ decision.plugin_id || '—' }} / {{ decision.account_id || '—' }}</strong>
                <span class="badge" :class="decision.sticky ? 'accent' : 'muted plain'">
                  {{ decision.sticky ? '粘性命中' : '新选择' }}
                </span>
                <span class="small muted mono">{{ decision.logical_model || decision.model || '—' }}</span>
              </div>
              <ul v-if="decision.reasons && decision.reasons.length" class="reason-list">
                <li v-for="(reason, index) in decision.reasons" :key="index">{{ reason }}</li>
              </ul>
              <span v-else class="small muted">宿主未返回决策原因。</span>
            </div>
            <div v-else class="empty">
              <strong>本次没有可用候选</strong>
              <span>候选池都被过滤：上游关闭、无健康账号、熔断中或并发已满。</span>
              <button type="button" class="btn sm ghost" :disabled="explainBusy" @click="runExplain">
                {{ explainBusy ? '演算中…' : '重新演算' }}
              </button>
            </div>

            <div v-if="candidates.length" class="table-wrap">
              <table class="table">
                <thead>
                  <tr>
                    <th>插件</th>
                    <th>账号</th>
                    <th>得分</th>
                    <th>结果</th>
                  </tr>
                </thead>
                <tbody>
                  <tr
                    v-for="(candidate, index) in candidates"
                    :key="`${candidate.plugin_id}-${candidate.account_id}-${index}`"
                    :class="{ 'chosen-row': isChosen(candidate) }"
                  >
                    <td class="title-cell">{{ candidate.plugin_id || '—' }}</td>
                    <td class="small mono">{{ candidate.account_id || '—' }}</td>
                    <td class="num">{{ scoreText(candidate.score) }}</td>
                    <td class="small">
                      <span v-if="isChosen(candidate)" class="badge ok">已选中</span>
                      <span v-else class="muted">{{ candidate.rejected_reason || '未选中' }}</span>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
        </div>
      </section>
    </template>

    <Transition name="drawer">
    <div v-if="stickyOpen" class="drawer-backdrop" @click.self="stickyOpen = false">
      <aside class="drawer" role="dialog" aria-modal="true" aria-labelledby="sticky-title">
        <header class="drawer-head">
          <div>
            <h2 id="sticky-title">粘性绑定</h2>
            <div class="small muted">共 {{ stickyItems.length }} 条 · 到期后自动失效</div>
          </div>
          <div class="btn-row">
            <button
              type="button"
              class="btn sm danger"
              :disabled="stickyClearing || stickyItems.length === 0"
              @click="requestClearSticky"
            >
              {{ stickyClearing ? '清理中…' : stickyArmed ? '确认清理全部' : '清理全部' }}
            </button>
            <button type="button" class="btn sm ghost" @click="stickyOpen = false">关闭</button>
          </div>
        </header>
        <div class="drawer-body">
          <div v-if="stickyError" class="notice error">
            <div class="row between">
              <span>粘性绑定读取或清理失败：{{ stickyError }}</span>
              <button type="button" class="btn sm" :disabled="stickyLoading" @click="loadSticky">重试</button>
            </div>
          </div>
          <div v-if="stickyLoading" class="log-skeleton" aria-hidden="true">
            <span v-for="n in 5" :key="n" class="skeleton skeleton-line"></span>
          </div>
          <div v-else-if="stickyItems.length === 0" class="empty">
            <strong>当前没有粘性绑定</strong>
            <span>打开池粘性或会话粘性后，命中记录会出现在这里。</span>
          </div>
          <div v-else class="sticky-list">
            <div v-for="(item, index) in stickyItems" :key="`${item.scope}-${item.key}-${index}`" class="sticky-item">
              <div class="row between">
                <span class="badge plain" :class="item.scope === 'conv' ? 'accent' : 'muted'">
                  {{ item.scope === 'conv' ? '会话粘性' : item.scope === 'pool' ? '池粘性' : item.scope }}
                </span>
                <span class="small muted">{{ formatTimestamp(item.expires_at) }} 到期</span>
              </div>
              <div class="small mono truncate">{{ item.key }}</div>
              <div class="small muted mono">{{ item.plugin_id }} / {{ item.account_id }}</div>
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
.plain-p {
  margin: 0;
}

.pool-title-row {
  gap: var(--space-1);
}
</style>
