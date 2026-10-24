<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import {
  api,
  errorMessage,
  type CustomConnection,
  type CustomConnectionPatch,
  type CustomInputType,
  type CustomModel,
  type CustomModelInput,
  type CustomModelPatch,
  type CustomProtocol,
  type CustomReasoningTier,
  type ImportModelInput,
  type ImportSkip,
  type RemoteModel,
} from '../api'
import { formatLatency, formatNumber } from '../format'

/** Mirrors the host: 0 means "not configured"; this is the hard ceiling. */
const MAX_TOKENS = 10_000_000

const PROTOCOLS: { value: CustomProtocol; label: string; hint: string }[] = [
  {
    value: 'openai',
    label: 'OpenAI 兼容',
    hint: '调用 /chat/completions，Key 走 Authorization: Bearer；适用于 OpenAI、DeepSeek 与多数兼容网关。',
  },
  {
    value: 'anthropic',
    label: 'Anthropic',
    hint: '调用 /v1/messages，Key 走 x-api-key 并带 anthropic-version；思考档位换算为 thinking 预算。',
  },
  {
    value: 'gemini',
    label: 'Gemini',
    hint: '调用 Gemini 原生 generateContent，Key 走 x-goog-api-key；思考档位换算为 thinkingBudget。',
  },
]

const INPUT_TYPES: { value: CustomInputType; label: string }[] = [
  { value: 'text', label: '文本' },
  { value: 'image', label: '图片' },
]

const TIERS: CustomReasoningTier[] = ['off', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'auto']

/** Upstream spelling the gateway writes for each canonical tier. */
const TIER_WIRE: Record<CustomProtocol, Record<CustomReasoningTier, string>> = {
  openai: {
    off: '不发送',
    minimal: 'minimal',
    low: 'low',
    medium: 'medium',
    high: 'high',
    xhigh: '→ high',
    max: '→ high',
    auto: '不发送',
  },
  anthropic: {
    off: '不发送',
    minimal: '1K',
    low: '2K',
    medium: '4K',
    high: '8K',
    xhigh: '16K',
    max: '32K',
    auto: '不发送',
  },
  gemini: {
    off: 'budget 0',
    minimal: '512',
    low: '1024',
    medium: '4096',
    high: '8192',
    xhigh: '16384',
    max: '24576',
    auto: 'budget -1',
  },
}

/* ------------------------------------------------------------------ */
/* List state                                                          */
/* ------------------------------------------------------------------ */

const connections = ref<CustomConnection[]>([])
const models = ref<CustomModel[]>([])
const loading = ref(false)
const busy = ref(false)
const error = ref('')
const message = ref('')

interface TestState {
  ok: boolean
  text: string
  title: string
}

const expanded = ref<Record<string, boolean>>({})
const connTests = ref<Record<string, TestState>>({})
const modelTests = ref<Record<string, TestState>>({})
const connTesting = ref('')
const modelTesting = ref('')

const modelsByConnection = computed<Record<string, CustomModel[]>>(() => {
  const map: Record<string, CustomModel[]> = {}
  for (const model of models.value) {
    const key = model.connection_id || ''
    const list = map[key] ?? (map[key] = [])
    list.push(model)
  }
  for (const list of Object.values(map)) {
    list.sort((a, b) => a.name.localeCompare(b.name))
  }
  return map
})

function modelsOf(conn: CustomConnection): CustomModel[] {
  return modelsByConnection.value[conn.id] ?? []
}

function modelCount(conn: CustomConnection): number {
  return modelsOf(conn).length
}

function connectionOf(model: CustomModel): CustomConnection | null {
  return connections.value.find((item) => item.id === model.connection_id) ?? null
}

function protocolOf(model: CustomModel): CustomProtocol {
  return model.protocol ?? connectionOf(model)?.protocol ?? 'openai'
}

function protocolLabel(protocol: CustomProtocol): string {
  return PROTOCOLS.find((item) => item.value === protocol)?.label ?? protocol
}

function protocolHint(protocol: CustomProtocol): string {
  return PROTOCOLS.find((item) => item.value === protocol)?.hint ?? ''
}

function tierWire(tier: CustomReasoningTier, protocol: CustomProtocol): string {
  return TIER_WIRE[protocol]?.[tier] ?? ''
}

function inputTypesText(types: CustomInputType[] | undefined): string {
  if (!types?.length) return '文本、图片'
  return types.map((item) => (item === 'image' ? '图片' : '文本')).join('、')
}

function reasoningText(model: CustomModel): string {
  if (!model.reasoning_tiers?.length) return '未配置'
  const list = model.reasoning_tiers.join(' / ')
  return model.default_reasoning ? `${list} · 默认: ${model.default_reasoning}` : list
}

function toggleExpanded(id: string) {
  expanded.value = { ...expanded.value, [id]: !expanded.value[id] }
}

async function load(options: { quiet?: boolean } = {}) {
  if (!options.quiet) {
    loading.value = true
    error.value = ''
  }
  try {
    const [conns, rows] = await Promise.all([api.customConnections.list(), api.customModels.list()])
    connections.value = conns?.connections ?? []
    models.value = rows?.models ?? []
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    loading.value = false
  }
}

function refresh() {
  message.value = ''
  void load()
}

onMounted(() => {
  void load()
})

/* ------------------------------------------------------------------ */
/* Connection actions                                                  */
/* ------------------------------------------------------------------ */

async function toggleConnection(conn: CustomConnection) {
  busy.value = true
  error.value = ''
  message.value = ''
  try {
    await api.customConnections.update(conn.id, { enabled: !conn.enabled })
    await load({ quiet: true })
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    busy.value = false
  }
}

async function testConnection(conn: CustomConnection) {
  connTesting.value = conn.id
  error.value = ''
  message.value = ''
  try {
    const result = await api.customConnections.test(conn.id)
    if (result.ok) {
      connTests.value = {
        ...connTests.value,
        [conn.id]: {
          ok: true,
          text: `连通 · ${result.model_count} 个模型 · ${formatLatency(result.latency_ms)}`,
          title: `${conn.base_url} · ${formatLatency(result.latency_ms)}`,
        },
      }
    } else {
      const detail = result.error || '检测失败'
      connTests.value = {
        ...connTests.value,
        [conn.id]: { ok: false, text: detail, title: detail },
      }
    }
  } catch (e) {
    const detail = errorMessage(e)
    connTests.value = {
      ...connTests.value,
      [conn.id]: { ok: false, text: detail, title: detail },
    }
  } finally {
    connTesting.value = ''
  }
}

async function removeConnection(conn: CustomConnection) {
  const label = conn.label || conn.base_url
  const count = modelCount(conn)
  if (
    !window.confirm(
      `确认删除接入「${label}」？其下 ${count} 个模型与保存的 API Key 会一并删除，且无法恢复。`,
    )
  ) {
    return
  }
  busy.value = true
  error.value = ''
  message.value = ''
  try {
    await api.customConnections.remove(conn.id)
    message.value = '接入已删除。'
    await load({ quiet: true })
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    busy.value = false
  }
}

/* ------------------------------------------------------------------ */
/* Connection editor drawer                                            */
/* ------------------------------------------------------------------ */

const connEditorOpen = ref(false)
const editingConn = ref<CustomConnection | null>(null)
const connFormError = ref('')
const connSaving = ref(false)
const connDraft = reactive({
  label: '',
  protocol: 'openai' as CustomProtocol,
  base_url: '',
  api_key: '',
  enabled: true,
})

function openConnectionCreator() {
  editingConn.value = null
  connDraft.label = ''
  connDraft.protocol = 'openai'
  connDraft.base_url = ''
  connDraft.api_key = ''
  connDraft.enabled = true
  connFormError.value = ''
  connEditorOpen.value = true
}

function openConnectionEditor(conn: CustomConnection) {
  editingConn.value = conn
  connDraft.label = conn.label ?? ''
  connDraft.protocol = conn.protocol ?? 'openai'
  connDraft.base_url = conn.base_url
  connDraft.api_key = ''
  connDraft.enabled = conn.enabled
  connFormError.value = ''
  connEditorOpen.value = true
}

function closeConnectionEditor() {
  if (!connSaving.value) connEditorOpen.value = false
}

/** Mirrors the host rules for fast feedback; the host re-validates. */
function connectionFormError(): string {
  const raw = connDraft.base_url.trim()
  if (!raw) return 'API 地址不能为空。'
  let parsed: URL
  try {
    parsed = new URL(raw)
  } catch {
    return 'API 地址必须是完整的 http(s) 地址，例如 https://api.example.com/v1。'
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    return 'API 地址必须以 http:// 或 https:// 开头。'
  }
  if (parsed.search || parsed.hash) return 'API 地址不能包含查询参数或片段。'
  const loopback = ['localhost', '127.0.0.1', '[::1]'].includes(parsed.hostname)
  if (parsed.protocol === 'http:' && !loopback) {
    return '远程 API 地址必须使用 https（本机回环地址可用 http）。'
  }
  if (!editingConn.value && !connDraft.api_key.trim()) {
    return '创建接入时必须填写 API Key。'
  }
  return ''
}

async function saveConnection() {
  const localError = connectionFormError()
  if (localError) {
    connFormError.value = localError
    return
  }
  connSaving.value = true
  connFormError.value = ''
  try {
    const base = {
      label: connDraft.label.trim(),
      protocol: connDraft.protocol,
      base_url: connDraft.base_url.trim(),
      enabled: connDraft.enabled,
    }
    if (editingConn.value) {
      const patch: CustomConnectionPatch = { ...base }
      // Blank keeps the stored key; typing a new one rotates it.
      if (connDraft.api_key.trim()) patch.api_key = connDraft.api_key.trim()
      await api.customConnections.update(editingConn.value.id, patch)
      message.value = '接入已更新，其下所有模型立即使用新配置。'
    } else {
      const result = await api.customConnections.create({ ...base, api_key: connDraft.api_key.trim() })
      message.value = result.created
        ? '接入已创建，下一步可点「拉取模型」从上游选择模型。'
        : `该地址与 Key 已存在，已返回现有接入「${result.connection.label || result.connection.base_url}」。`
    }
    connEditorOpen.value = false
    await load({ quiet: true })
  } catch (e) {
    connFormError.value = errorMessage(e)
  } finally {
    connSaving.value = false
  }
}

/* ------------------------------------------------------------------ */
/* Model actions                                                       */
/* ------------------------------------------------------------------ */

async function toggleModel(model: CustomModel) {
  busy.value = true
  error.value = ''
  message.value = ''
  try {
    await api.customModels.update(model.id, { enabled: !model.enabled })
    await load({ quiet: true })
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    busy.value = false
  }
}

async function testModel(model: CustomModel) {
  modelTesting.value = model.id
  error.value = ''
  message.value = ''
  try {
    // The diagnostics probe records the result and keeps the failure detail.
    const result = await api.diagnostics.probe('custom_model', { custom_model_id: model.id })
    modelTests.value = {
      ...modelTests.value,
      [model.id]: result.ok
        ? {
            ok: true,
            text: `连通 · ${formatLatency(result.latency_ms)}`,
            title: `连通 · HTTP ${result.status}`,
          }
        : {
            ok: false,
            text: result.detail || result.error_code || '检测失败',
            title: `检测失败${result.status ? ` · HTTP ${result.status}` : ''}${result.detail ? ` · ${result.detail}` : ''}`,
          },
    }
  } catch (e) {
    const detail = errorMessage(e)
    modelTests.value = {
      ...modelTests.value,
      [model.id]: { ok: false, text: detail, title: detail },
    }
  } finally {
    modelTesting.value = ''
  }
}

async function removeModel(model: CustomModel) {
  if (!window.confirm(`确认删除模型「${model.name}」？该操作只删除这一条模型，接入与 Key 保留。`)) return
  busy.value = true
  error.value = ''
  message.value = ''
  try {
    await api.customModels.remove(model.id)
    message.value = `模型「${model.name}」已删除。`
    await load({ quiet: true })
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    busy.value = false
  }
}

/* ------------------------------------------------------------------ */
/* Model editor drawer                                                 */
/* ------------------------------------------------------------------ */

const modelEditorOpen = ref(false)
const editingModel = ref<CustomModel | null>(null)
const modelConnId = ref('')
const modelFormError = ref('')
const modelSaving = ref(false)
const modelDraft = reactive({
  name: '',
  upstream_model: '',
  enabled: true,
  context_window: 0 as number | '',
  max_output_tokens: 0 as number | '',
  input_types: ['text', 'image'] as CustomInputType[],
  reasoning_tiers: [] as CustomReasoningTier[],
  default_reasoning: '' as CustomReasoningTier | '',
})

const activeConnection = computed<CustomConnection | null>(
  () => connections.value.find((item) => item.id === modelConnId.value) ?? null,
)

const modelEditorProtocol = computed<CustomProtocol>(() => {
  if (editingModel.value) return protocolOf(editingModel.value)
  return activeConnection.value?.protocol ?? 'openai'
})

// A default tier must always be one of the checked tiers.
watch(
  () => modelDraft.reasoning_tiers.slice(),
  (tiers) => {
    const current = modelDraft.default_reasoning
    if (current && !tiers.includes(current)) modelDraft.default_reasoning = ''
  },
)

function openModelCreator(conn: CustomConnection) {
  editingModel.value = null
  modelConnId.value = conn.id
  modelDraft.name = ''
  modelDraft.upstream_model = ''
  modelDraft.enabled = true
  modelDraft.context_window = 0
  modelDraft.max_output_tokens = 0
  modelDraft.input_types = ['text', 'image']
  modelDraft.reasoning_tiers = []
  modelDraft.default_reasoning = ''
  modelFormError.value = ''
  modelEditorOpen.value = true
}

function openModelEditor(model: CustomModel) {
  editingModel.value = model
  modelConnId.value = model.connection_id
  modelDraft.name = model.name
  modelDraft.upstream_model = model.upstream_model
  modelDraft.enabled = model.enabled
  modelDraft.context_window = model.context_window || 0
  modelDraft.max_output_tokens = model.max_output_tokens || 0
  const types: CustomInputType[] =
    model.input_types && model.input_types.length > 0 ? [...model.input_types] : ['text', 'image']
  modelDraft.input_types = types
  modelDraft.reasoning_tiers = [...(model.reasoning_tiers ?? [])]
  modelDraft.default_reasoning = model.reasoning_tiers?.includes(
    model.default_reasoning as CustomReasoningTier,
  )
    ? (model.default_reasoning as CustomReasoningTier)
    : ''
  modelFormError.value = ''
  modelEditorOpen.value = true
}

function closeModelEditor() {
  if (!modelSaving.value) modelEditorOpen.value = false
}

function tokenError(label: string, raw: number | ''): string {
  if (raw === '' || raw === null || raw === undefined) return ''
  const value = Number(raw)
  if (!Number.isInteger(value) || value < 0 || value > MAX_TOKENS) {
    return `${label}必须是 0–10,000,000 之间的整数（0 或留空 = 未设置）。`
  }
  return ''
}

function toToken(raw: number | ''): number {
  return raw === '' ? 0 : Math.trunc(Number(raw))
}

function modelFormLocalError(): string {
  const name = modelDraft.name.trim()
  if (!name) return '模型名称不能为空。'
  if ([...name].length > 64) return '模型名称最多 64 个字符。'
  if (name.includes('/')) return '模型名称不能包含 “/”（该前缀保留给插件）。'
  const upstream = modelDraft.upstream_model.trim()
  if (!upstream) return '上游模型 ID 不能为空。'
  if ([...upstream].length > 128) return '上游模型 ID 最多 128 个字符。'
  const contextError = tokenError('上下文长度', modelDraft.context_window)
  if (contextError) return contextError
  const outputError = tokenError('最大输出 token', modelDraft.max_output_tokens)
  if (outputError) return outputError
  if (modelDraft.input_types.length === 0) return '至少保留一种输入类型（文本或图片）。'
  if (modelDraft.default_reasoning && !modelDraft.reasoning_tiers.includes(modelDraft.default_reasoning)) {
    return '默认推理档必须来自已勾选的档位。'
  }
  if (!modelConnId.value) return '缺少所属接入，请从接入卡片重新打开。'
  return ''
}

async function saveModel() {
  const localError = modelFormLocalError()
  if (localError) {
    modelFormError.value = localError
    return
  }
  const name = modelDraft.name.trim()
  const upstream = modelDraft.upstream_model.trim()
  const settings = {
    name,
    enabled: modelDraft.enabled,
    context_window: toToken(modelDraft.context_window),
    max_output_tokens: toToken(modelDraft.max_output_tokens),
    input_types: [...modelDraft.input_types],
    reasoning_tiers: [...modelDraft.reasoning_tiers],
    default_reasoning: modelDraft.default_reasoning,
  }
  modelSaving.value = true
  modelFormError.value = ''
  try {
    if (editingModel.value) {
      const patch: CustomModelPatch = { ...settings }
      const result = await api.customModels.update(editingModel.value.id, patch)
      if (result.model?.id) expanded.value = { ...expanded.value, [result.model.id]: true }
      message.value = `模型「${name}」已更新。`
    } else {
      const input: CustomModelInput = { connection_id: modelConnId.value, upstream_model: upstream, ...settings }
      const result = await api.customModels.create(input)
      if (result.model?.id) expanded.value = { ...expanded.value, [result.model.id]: true }
      message.value = `模型「${name}」已添加。`
    }
    modelEditorOpen.value = false
    await load({ quiet: true })
  } catch (e) {
    modelFormError.value = errorMessage(e)
  } finally {
    modelSaving.value = false
  }
}

/* ------------------------------------------------------------------ */
/* Pull-models drawer                                                  */
/* ------------------------------------------------------------------ */

const importOpen = ref(false)
const importConn = ref<CustomConnection | null>(null)
const remoteModels = ref<RemoteModel[]>([])
const remoteLoading = ref(false)
const remoteError = ref('')
const remoteQuery = ref('')
const selectedRemote = ref<Record<string, boolean>>({})
const importBusy = ref(false)
const importError = ref('')
const importCreated = ref(0)
const importSkips = ref<ImportSkip[]>([])

const filteredRemote = computed(() => {
  const query = remoteQuery.value.trim().toLowerCase()
  if (!query) return remoteModels.value
  return remoteModels.value.filter(
    (model) => model.id.toLowerCase().includes(query) || model.name.toLowerCase().includes(query),
  )
})

const existingUpstream = computed<Set<string>>(() => {
  const conn = importConn.value
  if (!conn) return new Set<string>()
  return new Set(modelsOf(conn).map((model) => model.upstream_model))
})

const selectedCount = computed(
  () => Object.values(selectedRemote.value).filter(Boolean).length,
)

/** Rows that can still be imported (already-added upstream ids stay disabled). */
const importableVisible = computed(() =>
  filteredRemote.value.filter((model) => !existingUpstream.value.has(model.id)),
)

const visibleSelectedCount = computed(
  () => importableVisible.value.filter((model) => selectedRemote.value[model.id]).length,
)

const allVisibleSelected = computed(
  () => importableVisible.value.length > 0 && visibleSelectedCount.value === importableVisible.value.length,
)

async function openPullModels(conn: CustomConnection) {
  importConn.value = conn
  importOpen.value = true
  remoteModels.value = []
  remoteQuery.value = ''
  selectedRemote.value = {}
  importCreated.value = 0
  importSkips.value = []
  importError.value = ''
  await loadRemoteModels()
}

async function loadRemoteModels() {
  const conn = importConn.value
  if (!conn) return
  remoteLoading.value = true
  remoteError.value = ''
  try {
    const result = await api.customConnections.remoteModels(conn.id)
    remoteModels.value = result?.models ?? []
  } catch (e) {
    remoteModels.value = []
    remoteError.value = errorMessage(e)
  } finally {
    remoteLoading.value = false
  }
}

function closePullModels() {
  if (!importBusy.value) importOpen.value = false
}

function toggleRemote(model: RemoteModel) {
  if (existingUpstream.value.has(model.id)) return
  selectedRemote.value = { ...selectedRemote.value, [model.id]: !selectedRemote.value[model.id] }
}

function toggleAllRemote() {
  const target = !allVisibleSelected.value
  const next = { ...selectedRemote.value }
  for (const model of filteredRemote.value) {
    if (existingUpstream.value.has(model.id)) continue
    next[model.id] = target
  }
  selectedRemote.value = next
}

async function importSelected() {
  const conn = importConn.value
  if (!conn) return
  const chosen = remoteModels.value.filter(
    (model) => selectedRemote.value[model.id] && !existingUpstream.value.has(model.id),
  )
  if (chosen.length === 0) {
    importError.value = '至少勾选一个尚未添加的模型。'
    return
  }
  importBusy.value = true
  importError.value = ''
  importCreated.value = 0
  importSkips.value = []
  try {
    const payload: ImportModelInput[] = chosen.map((model) => {
      const input: ImportModelInput = { upstream_model: model.id }
      if (model.context_window && model.context_window > 0) input.context_window = model.context_window
      if (model.max_output_tokens && model.max_output_tokens > 0) {
        input.max_output_tokens = model.max_output_tokens
      }
      return input
    })
    const result = await api.customConnections.importModels(conn.id, payload)
    importCreated.value = result?.created?.length ?? 0
    importSkips.value = result?.skipped ?? []
    selectedRemote.value = {}
    message.value =
      importCreated.value > 0
        ? `已导入 ${importCreated.value} 个模型${importSkips.value.length ? `，跳过 ${importSkips.value.length} 个。` : '。'}`
        : '没有导入新模型，请查看跳过原因。'
    await load({ quiet: true })
    if (importCreated.value > 0 && importSkips.value.length === 0) importOpen.value = false
  } catch (e) {
    importError.value = errorMessage(e)
  } finally {
    importBusy.value = false
  }
}
</script>

<template>
  <section class="card custom-panel">
    <div class="card-head">
      <h2>自定义模型</h2>
      <span class="sub">按接入分组：端点 + Key + 协议下挂多个模型；模型名称就是客户端请求的 model</span>
    </div>
    <div class="card-body stack">
      <div class="row between">
        <span class="help help-plain">
          命中的请求直达该接入的上游，不再经过调度与插件；Key 只保存在宿主，列表只显示是否已配置。
        </span>
        <div class="btn-row">
          <button type="button" class="btn ghost" :disabled="loading || busy" @click="refresh">
            刷新
          </button>
          <button type="button" class="btn primary" :disabled="busy" @click="openConnectionCreator">
            新建接入
          </button>
        </div>
      </div>

      <div v-if="error" class="notice error">{{ error }}</div>
      <div v-if="message" class="notice ok">{{ message }}</div>

      <div v-if="loading && connections.length === 0" class="loading">读取接入与模型…</div>
      <div v-else-if="connections.length === 0" class="empty">
        <strong>还没有自定义接入</strong>
        <span>
          先「新建接入」填写端点、协议与 API Key，再从上游拉取模型；一个接入下的模型共用同一把 Key。
        </span>
      </div>

      <div v-else class="conn-list">
        <article v-for="conn in connections" :key="conn.id" class="conn-card">
          <div class="conn-head">
            <div class="conn-ident">
              <div class="row conn-title-row">
                <strong>{{ conn.label || '未命名接入' }}</strong>
                <span class="badge plain accent">{{ protocolLabel(conn.protocol) }}</span>
                <span class="badge plain" :class="conn.key_set ? 'ok' : 'warn'">
                  {{ conn.key_set ? 'Key 已配置' : 'Key 未配置' }}
                </span>
                <span class="small muted">{{ modelCount(conn) }} 个模型</span>
                <span v-if="!conn.enabled" class="badge plain muted">已停用</span>
              </div>
              <div class="mono small muted conn-url">{{ conn.base_url }}</div>
            </div>
            <label class="switch" :title="conn.enabled ? '停用后该接入下所有模型都不再路由' : '启用该接入'">
              <input
                type="checkbox"
                :checked="conn.enabled"
                :disabled="busy"
                @change="toggleConnection(conn)"
              />
              <span class="switch-track"></span>
              {{ conn.enabled ? '启用' : '停用' }}
            </label>
          </div>

          <div class="row conn-actions">
            <button
              type="button"
              class="btn sm"
              :disabled="connTesting === conn.id"
              @click="testConnection(conn)"
            >
              {{ connTesting === conn.id ? '检测中…' : '检测' }}
            </button>
            <button type="button" class="btn sm" :disabled="busy" @click="openPullModels(conn)">
              拉取模型
            </button>
            <button type="button" class="btn sm" :disabled="busy" @click="openModelCreator(conn)">
              添加模型
            </button>
            <button type="button" class="btn sm" :disabled="busy" @click="openConnectionEditor(conn)">
              编辑
            </button>
            <button
              type="button"
              class="btn sm danger ghost"
              :disabled="busy"
              @click="removeConnection(conn)"
            >
              删除
            </button>
            <span
              v-if="connTests[conn.id]"
              class="probe-text small"
              :class="connTests[conn.id]?.ok ? 'ok' : 'danger'"
              :title="connTests[conn.id]?.title"
            >
              {{ connTests[conn.id]?.text }}
            </span>
          </div>

          <ul class="model-list">
            <li v-for="model in modelsOf(conn)" :key="model.id" class="model-row">
              <div class="model-main">
                <button
                  type="button"
                  class="model-toggle"
                  :aria-expanded="!!expanded[model.id]"
                  @click="toggleExpanded(model.id)"
                >
                  <span class="caret" :class="{ open: expanded[model.id] }">▸</span>
                  <span class="mono model-name">{{ model.name }}</span>
                </button>
                <span v-if="model.conflict" class="badge plain warn">与现有模型冲突</span>
                <span class="small muted model-upstream">
                  <span class="upstream-label">ID</span>
                  <span class="mono">{{ model.upstream_model }}</span>
                </span>
                <span class="spacer"></span>
                <label class="switch">
                  <input
                    type="checkbox"
                    :checked="model.enabled"
                    :disabled="busy"
                    @change="toggleModel(model)"
                  />
                  <span class="switch-track"></span>
                  {{ model.enabled ? '启用' : '停用' }}
                </label>
                <div class="btn-row">
                  <button
                    type="button"
                    class="btn sm"
                    :disabled="modelTesting === model.id"
                    @click="testModel(model)"
                  >
                    {{ modelTesting === model.id ? '检测中…' : '检测' }}
                  </button>
                  <button type="button" class="btn sm" @click="openModelEditor(model)">编辑</button>
                  <button
                    type="button"
                    class="btn sm danger ghost"
                    :disabled="busy"
                    @click="removeModel(model)"
                  >
                    删除
                  </button>
                </div>
              </div>
              <div
                v-if="modelTests[model.id]"
                class="probe-text small"
                :class="modelTests[model.id]?.ok ? 'ok' : 'danger'"
                :title="modelTests[model.id]?.title"
              >
                {{ modelTests[model.id]?.text }}
              </div>
              <dl v-if="expanded[model.id]" class="model-detail">
                <dt>上下文</dt>
                <dd>{{ model.context_window > 0 ? formatNumber(model.context_window) : '未设置' }}</dd>
                <dt>最大输出</dt>
                <dd>
                  {{ model.max_output_tokens > 0 ? formatNumber(model.max_output_tokens) : '未设置' }}
                </dd>
                <dt>输入类型</dt>
                <dd>{{ inputTypesText(model.input_types) }}</dd>
                <dt>推理档位</dt>
                <dd>{{ reasoningText(model) }}</dd>
              </dl>
            </li>
            <li v-if="modelsOf(conn).length === 0" class="model-empty small muted">
              还没有模型，点「拉取模型」从上游选择，或「添加模型」手动填写上游 ID。
            </li>
          </ul>
        </article>
      </div>

      <span class="help">
        仅启用且不冲突的模型会出现在 <code>/v1/models</code>；命中后按接入协议直达上游（OpenAI
        <code>/chat/completions</code>、Anthropic <code>/v1/messages</code>、Gemini
        <code>generateContent</code>）。停用接入会同时停用其下全部模型。
      </span>
    </div>

    <!-- 接入编辑：一个端点 + Key + 协议，其下模型共享 -->
    <div v-if="connEditorOpen" class="drawer-backdrop" @click.self="closeConnectionEditor">
      <aside class="drawer" role="dialog" aria-modal="true" aria-labelledby="conn-editor-title">
        <header class="drawer-head">
          <div>
            <h2 id="conn-editor-title">{{ editingConn ? '编辑接入' : '新建接入' }}</h2>
            <div v-if="editingConn" class="small muted mono">{{ editingConn.id }}</div>
          </div>
          <button type="button" class="btn sm ghost" :disabled="connSaving" @click="closeConnectionEditor">
            关闭
          </button>
        </header>
        <div class="drawer-body">
          <p class="small muted plain-p">
            一个接入 = 端点 + API Key + 协议；其下所有模型共用这把 Key 与地址。
          </p>
          <div class="field">
            <label for="conn-label">名称（可选）</label>
            <input
              id="conn-label"
              v-model="connDraft.label"
              class="input"
              type="text"
              maxlength="64"
              placeholder="例如 公司 OpenAI 网关"
              :disabled="connSaving"
            />
          </div>
          <div class="field">
            <label for="conn-protocol">协议</label>
            <select id="conn-protocol" v-model="connDraft.protocol" class="select" :disabled="connSaving">
              <option v-for="item in PROTOCOLS" :key="item.value" :value="item.value">
                {{ item.label }}
              </option>
            </select>
            <span class="help">{{ protocolHint(connDraft.protocol) }}</span>
            <span v-if="editingConn" class="help warn-text">
              修改协议会改变该接入下每个模型的调用方式与思考档位换算，请确认上游支持后再保存。
            </span>
          </div>
          <div class="field">
            <label for="conn-url">API 地址</label>
            <input
              id="conn-url"
              v-model="connDraft.base_url"
              class="input mono"
              type="text"
              spellcheck="false"
              placeholder="https://api.example.com/v1"
              :disabled="connSaving"
            />
            <span class="help">
              远程必须使用 https，本机回环地址可用 http；不能带查询参数。地址与 Key 完全相同的接入会被复用。
            </span>
          </div>
          <div class="field">
            <label for="conn-key">API Key</label>
            <input
              id="conn-key"
              v-model="connDraft.api_key"
              class="input"
              type="password"
              autocomplete="new-password"
              :placeholder="editingConn ? '留空 = 保持原 Key' : '必填，只写不回显'"
              :disabled="connSaving"
            />
            <span class="help">DPAPI 加密保存于宿主；修改后该接入下所有模型立即使用新 Key。</span>
          </div>
          <div class="field">
            <label class="switch">
              <input v-model="connDraft.enabled" type="checkbox" :disabled="connSaving" />
              <span class="switch-track"></span>
              启用该接入
            </label>
            <span class="help">停用后其下所有模型都不再路由，请求会回落到常规调度。</span>
          </div>
          <div v-if="connFormError" class="notice error">{{ connFormError }}</div>
          <div class="row">
            <button type="button" class="btn primary" :disabled="connSaving" @click="saveConnection">
              {{ connSaving ? '保存中…' : '保存' }}
            </button>
            <button type="button" class="btn ghost" :disabled="connSaving" @click="closeConnectionEditor">
              取消
            </button>
          </div>
        </div>
      </aside>
    </div>

    <!-- 模型编辑：接入与上游 ID 创建后固定，其余参数可改 -->
    <div v-if="modelEditorOpen" class="drawer-backdrop" @click.self="closeModelEditor">
      <aside class="drawer" role="dialog" aria-modal="true" aria-labelledby="model-editor-title">
        <header class="drawer-head">
          <div>
            <h2 id="model-editor-title">{{ editingModel ? '编辑模型' : '添加模型' }}</h2>
            <div v-if="editingModel" class="small muted mono">{{ editingModel.id }}</div>
          </div>
          <button type="button" class="btn sm ghost" :disabled="modelSaving" @click="closeModelEditor">
            关闭
          </button>
        </header>
        <div class="drawer-body">
          <p class="small muted plain-p">
            {{
              editingModel
                ? '模型的接入与上游 ID 固定；名称和调用参数可随时修改。'
                : '手动添加一个不在「拉取模型」列表里的上游模型。'
            }}
          </p>
          <div class="field">
            <label for="model-name">模型名称（客户端请求的 model）</label>
            <input
              id="model-name"
              v-model="modelDraft.name"
              class="input mono"
              type="text"
              maxlength="64"
              placeholder="例如 my-gpt"
              spellcheck="false"
              :disabled="modelSaving"
            />
            <span class="help">最多 64 个字符，不能包含 <code>/</code>（该前缀保留给插件）。</span>
          </div>
          <div class="field">
            <label for="model-upstream">上游模型 ID</label>
            <input
              v-if="!editingModel"
              id="model-upstream"
              v-model="modelDraft.upstream_model"
              class="input mono"
              type="text"
              maxlength="128"
              placeholder="例如 gpt-4o-mini"
              spellcheck="false"
              :disabled="modelSaving"
            />
            <input v-else id="model-upstream" class="input mono" type="text" :value="modelDraft.upstream_model" readonly disabled />
            <span class="help">
              {{
                editingModel
                  ? '创建后不可修改；需要更换请删除后重新添加。'
                  : '转发时写入上游请求的 model 字段，创建后不可修改。'
              }}
            </span>
            <span v-if="activeConnection" class="help mono">
              接入：{{ activeConnection.label || activeConnection.id }} · {{ activeConnection.base_url }}
            </span>
          </div>
          <div class="form-grid">
            <div class="field">
              <label for="model-context">上下文（tokens）</label>
              <input
                id="model-context"
                v-model.number="modelDraft.context_window"
                class="input"
                type="number"
                min="0"
                :max="MAX_TOKENS"
                step="1"
                placeholder="0 = 未设置"
                :disabled="modelSaving"
              />
              <span class="help">0 或留空 = 未设置；最多 10,000,000，超出估算的请求会被拒绝。</span>
            </div>
            <div class="field">
              <label for="model-max">最大输出（tokens）</label>
              <input
                id="model-max"
                v-model.number="modelDraft.max_output_tokens"
                class="input"
                type="number"
                min="0"
                :max="MAX_TOKENS"
                step="1"
                placeholder="0 = 未设置"
                :disabled="modelSaving"
              />
              <span class="help">0 或留空 = 未设置；会作为请求的 max_tokens 上限。</span>
            </div>
          </div>
          <div class="field">
            <span class="field-label">输入类型</span>
            <div class="check-grid">
              <label v-for="item in INPUT_TYPES" :key="item.value">
                <input
                  v-model="modelDraft.input_types"
                  type="checkbox"
                  :value="item.value"
                  :disabled="modelSaving"
                />
                {{ item.label }}
              </label>
            </div>
            <span class="help">至少选一种；只勾文本时，带图片的请求会返回 400。</span>
          </div>
          <div class="field">
            <span class="field-label">推理档位</span>
            <div class="tier-grid">
              <label v-for="tier in TIERS" :key="tier">
                <input
                  v-model="modelDraft.reasoning_tiers"
                  type="checkbox"
                  :value="tier"
                  :disabled="modelSaving"
                />
                <span class="mono">{{ tier }}</span>
                <span class="tier-wire">{{ tierWire(tier, modelEditorProtocol) }}</span>
              </label>
            </div>
            <span class="help">
              全部不勾 = 不限制档位；右侧是当前协议（{{ protocolLabel(modelEditorProtocol) }}）的上游取值。
            </span>
          </div>
          <div class="field">
            <label for="model-default">默认档位</label>
            <select
              id="model-default"
              v-model="modelDraft.default_reasoning"
              class="select"
              :disabled="modelSaving || modelDraft.reasoning_tiers.length === 0"
            >
              <option value="">不设默认（客户端未指定时不下发）</option>
              <option v-for="tier in modelDraft.reasoning_tiers" :key="tier" :value="tier">
                {{ tier }}
              </option>
            </select>
            <span class="help">只能选已勾选的档位；客户端未传档位时使用它。</span>
          </div>
          <div class="field">
            <label class="switch">
              <input v-model="modelDraft.enabled" type="checkbox" :disabled="modelSaving" />
              <span class="switch-track"></span>
              启用该模型
            </label>
            <span class="help">停用后该模型不再路由，接入与 Key 保持不变。</span>
          </div>
          <div v-if="modelFormError" class="notice error">{{ modelFormError }}</div>
          <div class="row">
            <button type="button" class="btn primary" :disabled="modelSaving" @click="saveModel">
              {{ modelSaving ? '保存中…' : '保存' }}
            </button>
            <button type="button" class="btn ghost" :disabled="modelSaving" @click="closeModelEditor">
              取消
            </button>
          </div>
        </div>
      </aside>
    </div>

    <!-- 拉取上游模型：勾选后批量导入，名称由宿主清理并去重 -->
    <div v-if="importOpen && importConn" class="drawer-backdrop" @click.self="closePullModels">
      <aside class="drawer" role="dialog" aria-modal="true" aria-labelledby="pull-models-title">
        <header class="drawer-head">
          <div>
            <h2 id="pull-models-title">拉取上游模型</h2>
            <div class="small muted">
              {{ importConn.label || '未命名接入' }} ·
              <span class="mono">{{ importConn.base_url }}</span>
            </div>
          </div>
          <button type="button" class="btn sm ghost" :disabled="importBusy" @click="closePullModels">
            关闭
          </button>
        </header>
        <div class="drawer-body">
          <div class="row between">
            <span class="help help-plain">
              勾选的模型会以当前接入的地址与 Key 创建，名称由宿主清理并去重。
            </span>
            <button
              type="button"
              class="btn sm ghost"
              :disabled="remoteLoading || importBusy"
              @click="loadRemoteModels"
            >
              {{ remoteLoading ? '拉取中…' : '重新拉取' }}
            </button>
          </div>

          <div v-if="remoteLoading" class="loading">正在读取上游模型列表…</div>
          <div v-else-if="remoteError" class="notice error">{{ remoteError }}</div>
          <div v-else class="stack import-stack">
            <div class="row import-tools">
              <div class="field grow" style="flex: 1">
                <label for="remote-search">搜索</label>
                <input
                  id="remote-search"
                  v-model="remoteQuery"
                  class="input"
                  type="text"
                  placeholder="按 ID 或显示名过滤"
                  :disabled="importBusy"
                />
              </div>
              <span class="small muted">{{ filteredRemote.length }} / {{ remoteModels.length }} 个</span>
            </div>

            <div class="import-list">
              <div class="import-head-row">
                <input
                  type="checkbox"
                  :checked="allVisibleSelected"
                  :indeterminate="!allVisibleSelected && visibleSelectedCount > 0"
                  :disabled="importBusy || importableVisible.length === 0"
                  aria-label="全选当前列表"
                  @change="toggleAllRemote"
                />
                <span>全选当前列表</span>
                <span class="spacer"></span>
                <span class="muted">已选 {{ selectedCount }}</span>
              </div>
              <div v-if="remoteModels.length === 0" class="import-empty small muted">
                上游没有返回任何模型。
              </div>
              <div v-else-if="filteredRemote.length === 0" class="import-empty small muted">
                没有匹配的模型。
              </div>
              <label v-for="model in filteredRemote" :key="model.id" class="import-row">
                <input
                  type="checkbox"
                  :checked="!!selectedRemote[model.id]"
                  :disabled="importBusy || existingUpstream.has(model.id)"
                  @change="toggleRemote(model)"
                />
                <span class="import-body">
                  <span class="row import-id-row">
                    <span class="import-id">{{ model.id }}</span>
                    <span v-if="existingUpstream.has(model.id)" class="badge plain muted">已添加</span>
                  </span>
                  <span v-if="model.name && model.name !== model.id" class="small muted">
                    {{ model.name }}
                  </span>
                  <span class="import-sub">
                    <span>
                      上下文：{{ model.context_window ? formatNumber(model.context_window) : '未提供' }}
                    </span>
                    <span>
                      最大输出：{{
                        model.max_output_tokens ? formatNumber(model.max_output_tokens) : '未提供'
                      }}
                    </span>
                    <span v-if="model.owned_by">归属：{{ model.owned_by }}</span>
                  </span>
                </span>
              </label>
            </div>

            <div v-if="importError" class="notice error">{{ importError }}</div>
            <div v-if="importCreated > 0 || importSkips.length > 0" class="import-result">
              <div class="small">
                已导入 <strong>{{ importCreated }}</strong> 个模型；跳过 {{ importSkips.length }} 个。
              </div>
              <ul v-if="importSkips.length" class="skip-list">
                <li v-for="item in importSkips" :key="item.upstream_model">
                  <span class="mono">{{ item.upstream_model }}</span> — {{ item.reason }}
                </li>
              </ul>
            </div>
            <div class="row">
              <button
                type="button"
                class="btn primary"
                :disabled="importBusy || selectedCount === 0"
                @click="importSelected"
              >
                {{ importBusy ? '导入中…' : `导入所选（${selectedCount}）` }}
              </button>
              <button type="button" class="btn ghost" :disabled="importBusy" @click="closePullModels">
                关闭
              </button>
            </div>
          </div>
        </div>
      </aside>
    </div>
  </section>
</template>

<style scoped>
/* Zero-margin resets for help/caption text that sits in a spaced container. */
.help-plain {
  margin: 0;
}

.plain-p {
  margin: 0;
}

.conn-title-row {
  gap: var(--space-2);
}

.import-stack {
  gap: var(--space-3);
}

.import-id-row {
  gap: var(--space-1);
}

.conn-list {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.conn-card {
  display: flex;
  flex-direction: column;
  gap: 9px;
  padding: 12px 14px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--surface);
}

.conn-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.conn-ident {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.conn-url {
  overflow-wrap: anywhere;
}

.conn-actions {
  align-items: center;
  gap: 6px;
}

.probe-text {
  max-width: 340px;
  overflow-wrap: anywhere;
}

.probe-text.ok {
  color: var(--ok);
}

.probe-text.danger {
  color: var(--danger);
}

.model-list {
  display: flex;
  flex-direction: column;
  margin: 0;
  padding: 0;
  border-top: 1px solid var(--border);
  list-style: none;
}

.model-row {
  padding: 8px 0;
  border-bottom: 1px solid var(--border);
}

.model-row:last-child {
  border-bottom: none;
}

.model-main {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.model-toggle {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 0;
  border: none;
  background: transparent;
  color: var(--text);
  font: inherit;
  text-align: left;
  cursor: pointer;
}

.model-toggle:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: 2px;
}

.caret {
  display: inline-block;
  color: var(--muted);
  font-size: var(--text-2xs);
  transition: transform 0.12s ease;
}

.caret.open {
  transform: rotate(90deg);
}

.model-name {
  font-weight: 600;
  overflow-wrap: anywhere;
}

.model-upstream {
  min-width: 0;
  max-width: 340px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.upstream-label {
  margin-right: 3px;
  color: var(--muted);
  font-size: var(--text-2xs);
  letter-spacing: 0.06em;
}

.model-detail {
  display: grid;
  grid-template-columns: 78px minmax(0, 1fr);
  gap: 4px 10px;
  margin: 8px 0 2px 22px;
  padding: 8px 10px;
  border-radius: 6px;
  background: var(--surface-2);
  font-size: var(--text-xs);
}

.model-detail dt {
  color: var(--muted);
}

.model-detail dd {
  margin: 0;
  color: var(--text-soft);
  overflow-wrap: anywhere;
}

.model-empty {
  padding: 10px 0 2px;
}

.warn-text {
  color: var(--warn);
}

.check-grid {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 18px;
}

.check-grid label,
.tier-grid label {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: var(--text-soft);
  font-size: var(--text-sm);
  cursor: pointer;
}

.tier-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 4px 14px;
}

.tier-wire {
  color: var(--muted);
  font-size: var(--text-2xs);
}

.import-tools {
  align-items: flex-end;
}

.import-list {
  display: flex;
  flex-direction: column;
  max-height: 46vh;
  overflow-y: auto;
  border: 1px solid var(--border);
  border-radius: 7px;
}

.import-head-row {
  position: sticky;
  top: 0;
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 7px 10px;
  border-bottom: 1px solid var(--border);
  background: var(--surface-2);
  font-size: var(--text-xs);
}

.import-row {
  display: flex;
  align-items: flex-start;
  gap: 9px;
  padding: 7px 10px;
  border-bottom: 1px solid var(--border);
  font-size: var(--text-xs);
  cursor: pointer;
}

.import-row:last-child {
  border-bottom: none;
}

.import-row input {
  margin-top: 3px;
}

.import-body {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.import-id {
  font-family: var(--mono);
  color: var(--text);
  overflow-wrap: anywhere;
}

.import-sub {
  display: flex;
  flex-wrap: wrap;
  gap: 2px 12px;
  color: var(--muted);
  font-size: var(--text-2xs);
}

.import-empty {
  padding: 14px 10px;
}

.import-result {
  display: flex;
  flex-direction: column;
  gap: 5px;
  padding: 9px 11px;
  border: 1px solid var(--border);
  border-radius: 7px;
  background: var(--surface-2);
}

.skip-list {
  display: flex;
  flex-direction: column;
  gap: 3px;
  margin: 0;
  padding-left: 18px;
  color: var(--warn);
  font-size: var(--text-xs);
  overflow-wrap: anywhere;
}
</style>
