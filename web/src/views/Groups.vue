<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  api,
  errorMessage,
  type RoutingAutoGroup,
  type RoutingGroup,
  type RoutingGroupView,
  type RoutingGroupsResponse,
  type RoutingRule,
  type RoutingTemplate,
  type RoutingWhen,
} from '../api'
import RouteStage from '../components/RouteStage.vue'
import '../route-stage.css'

interface RuleDraft {
  name: string
  matchText: string
  minChars: string
  maxChars: string
  minTokens: string
  maxTokens: string
  image: boolean
  thinking: boolean
  agent: string
  startHour: string
  endHour: string
}

interface RouteDraft {
  label: string
  target: string
}

interface GroupDraft {
  uid: number
  id: string
  display: string
  matchText: string
  disabled: boolean
  weightsText: string
  rules: RuleDraft[]
  intentEnabled: boolean
  intentUpstream: string
  intentProbeChars: string
  intentDefault: string
  routes: RouteDraft[]
}

/* uid 给 TransitionGroup 提供稳定 key：拖拽重排时 move 过渡按内容跟随，
 * 而不是按下标抖动。 */
let uidSeq = 0

const loading = ref(false)
const saving = ref(false)
const error = ref('')
const message = ref('')
const drafts = ref<GroupDraft[]>([])
const savedGroups = ref<RoutingGroupView[]>([])
const upstreams = ref<RoutingGroupsResponse['decision_upstreams']>([])
const templates = ref<RoutingTemplate[]>([])
const autoGroups = ref<RoutingAutoGroup[]>([])
/** 实时舞台的独立提示位：501 未就绪等（与保存/操作反馈的 message 分开，避免误导读）。 */
const stageNotice = ref('')

function onStageUnavailable(message: string) {
  stageNotice.value = message
}
const editing = ref(-1)
const baseline = ref('')

function newRuleDraft(): RuleDraft {
  return {
    name: '',
    matchText: '',
    minChars: '',
    maxChars: '',
    minTokens: '',
    maxTokens: '',
    image: false,
    thinking: false,
    agent: '',
    startHour: '',
    endHour: '',
  }
}

function newDraft(): GroupDraft {
  uidSeq += 1
  return {
    uid: uidSeq,
    id: '',
    display: '',
    matchText: '',
    disabled: false,
    weightsText: '',
    rules: [],
    intentEnabled: false,
    intentUpstream: '',
    intentProbeChars: '2000',
    intentDefault: '',
    routes: [],
  }
}

function numberText(value?: number): string {
  return typeof value === 'number' ? String(value) : ''
}

function ruleToDraft(rule: RoutingRule): RuleDraft {
  return {
    name: rule.name ?? '',
    matchText: (rule.match ?? []).join('\n'),
    minChars: numberText(rule.when?.min_chars),
    maxChars: numberText(rule.when?.max_chars),
    minTokens: numberText(rule.when?.min_tokens),
    maxTokens: numberText(rule.when?.max_tokens),
    image: Boolean(rule.when?.image),
    thinking: Boolean(rule.when?.thinking),
    agent: rule.when?.agent ?? '',
    startHour: numberText(rule.when?.start_hour),
    endHour: numberText(rule.when?.end_hour),
  }
}

function toDraft(group: RoutingGroupView): GroupDraft {
  const intent = group.intent
  uidSeq += 1
  return {
    uid: uidSeq,
    id: group.id,
    display: group.display ?? '',
    matchText: (group.match ?? []).join('\n'),
    disabled: Boolean(group.disabled),
    weightsText: Object.entries(group.weights ?? {})
      .map(([member, weight]) => `${member}=${weight}`)
      .join('\n'),
    rules: (group.rules ?? []).map(ruleToDraft),
    intentEnabled: Boolean(intent?.enabled),
    intentUpstream: intent?.upstream ?? '',
    intentProbeChars: numberText(intent?.probe_chars) || '2000',
    intentDefault: intent?.default ?? '',
    routes: Object.entries(intent?.route ?? {}).map(([label, target]) => ({ label, target })),
  }
}

function lines(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line.length > 0)
}

function optionalNumber(text: string, field: string): number | undefined {
  const trimmed = text.trim()
  if (!trimmed) return undefined
  const value = Number(trimmed)
  if (!Number.isFinite(value) || !Number.isInteger(value)) {
    throw new Error(`${field}必须是整数`)
  }
  return value
}

function parseWeights(text: string): Record<string, number> | undefined {
  const out: Record<string, number> = {}
  for (const line of lines(text)) {
    const at = line.lastIndexOf('=')
    if (at <= 0) throw new Error(`权重行「${line}」应写成 成员=权重`)
    const member = line.slice(0, at).trim()
    const weight = Number(line.slice(at + 1).trim())
    if (!member) throw new Error(`权重行「${line}」缺少成员`)
    if (!Number.isInteger(weight) || weight < 1 || weight > 1000) {
      throw new Error(`权重 ${line} 必须是 1~1000 的整数`)
    }
    out[member] = weight
  }
  return Object.keys(out).length > 0 ? out : undefined
}

function ruleWhen(rule: RuleDraft, index: number): RoutingWhen {
  const when: RoutingWhen = {}
  const minChars = optionalNumber(rule.minChars, `规则 ${index + 1} 的最小字符数`)
  const maxChars = optionalNumber(rule.maxChars, `规则 ${index + 1} 的最大字符数`)
  const minTokens = optionalNumber(rule.minTokens, `规则 ${index + 1} 的最小 Token`)
  const maxTokens = optionalNumber(rule.maxTokens, `规则 ${index + 1} 的最大 Token`)
  const startHour = optionalNumber(rule.startHour, `规则 ${index + 1} 的起始小时`)
  const endHour = optionalNumber(rule.endHour, `规则 ${index + 1} 的结束小时`)
  if (minChars !== undefined) when.min_chars = minChars
  if (maxChars !== undefined) when.max_chars = maxChars
  if (minTokens !== undefined) when.min_tokens = minTokens
  if (maxTokens !== undefined) when.max_tokens = maxTokens
  if (startHour !== undefined) when.start_hour = startHour
  if (endHour !== undefined) when.end_hour = endHour
  if (rule.image) when.image = true
  if (rule.thinking) when.thinking = true
  if (rule.agent.trim()) when.agent = rule.agent.trim()
  return when
}

function buildGroups(): RoutingGroup[] {
  const ids = new Set<string>()
  return drafts.value.map((draft) => {
    const id = draft.id.trim()
    if (!id) throw new Error('每个路由组都需要一个 ID')
    if (/[/\s]/.test(id)) throw new Error(`路由组 ID「${id}」不能包含斜杠或空白`)
    if (ids.has(id)) throw new Error(`路由组 ID「${id}」重复`)
    ids.add(id)
    const match = lines(draft.matchText)
    if (match.length === 0) throw new Error(`路由组「${id}」至少需要一条匹配模式`)
    const group: RoutingGroup = {
      id,
      match,
      disabled: draft.disabled,
    }
    if (draft.display.trim()) group.display = draft.display.trim()
    const weights = parseWeights(draft.weightsText)
    if (weights) group.weights = weights
    if (draft.rules.length > 0) {
      group.rules = draft.rules.map((rule, index): RoutingRule => {
        const ruleMatch = lines(rule.matchText)
        if (ruleMatch.length === 0) throw new Error(`路由组「${id}」的规则 ${index + 1} 需要匹配模式`)
        return { name: rule.name.trim(), match: ruleMatch, when: ruleWhen(rule, index) }
      })
    }
    if (draft.intentEnabled) {
      const routes = draft.routes
        .map((route) => ({ label: route.label.trim(), target: route.target.trim() }))
        .filter((route) => route.label || route.target)
      if (routes.length === 0) throw new Error(`路由组「${id}」的意图规则至少需要一条标签路由`)
      if (routes.some((route) => !route.label || !route.target)) {
        throw new Error(`路由组「${id}」的意图标签和目标都要填写`)
      }
      const route: Record<string, string> = {}
      for (const item of routes) {
        if (!ids.has(item.target) && item.target !== id && !drafts.value.some((d) => d.id.trim() === item.target)) {
          throw new Error(`意图目标「${item.target}」不是已定义的路由组`)
        }
        route[item.label] = item.target
      }
      group.intent = {
        enabled: true,
        labels: routes.map((route) => route.label),
        route,
      }
      if (draft.intentUpstream) group.intent.upstream = draft.intentUpstream
      if (draft.intentDefault) group.intent.default = draft.intentDefault
      const probe = optionalNumber(draft.intentProbeChars, '探针字符数')
      if (probe !== undefined) group.intent.probe_chars = probe
    }
    return group
  })
}

const dirty = computed(() => {
  try {
    return JSON.stringify(buildGroups()) !== baseline.value
  } catch {
    return true
  }
})

function apply(response: RoutingGroupsResponse) {
  savedGroups.value = response.groups ?? []
  upstreams.value = response.decision_upstreams ?? []
  templates.value = response.templates ?? []
  autoGroups.value = response.auto_groups ?? []
  drafts.value = savedGroups.value.map(toDraft)
  try {
    baseline.value = JSON.stringify(buildGroups())
  } catch {
    baseline.value = ''
  }
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    apply(await api.routingGroups.get())
    editing.value = drafts.value.length > 0 ? 0 : -1
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  error.value = ''
  message.value = ''
  try {
    const groups = buildGroups()
    apply(await api.routingGroups.save(groups))
    editing.value = -1
    message.value = '已保存，下一次调度快照生效。'
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    saving.value = false
  }
}

function startNew() {
  drafts.value.push(newDraft())
  editing.value = drafts.value.length - 1
}

function removeDraft(index: number) {
  drafts.value.splice(index, 1)
  editing.value = Math.min(index, drafts.value.length - 1)
}

function addRule() {
  const draft = drafts.value[editing.value]
  if (draft) draft.rules.push(newRuleDraft())
}

function addRoute() {
  const draft = drafts.value[editing.value]
  if (draft) draft.routes.push({ label: '', target: '' })
}

/** 唯一化组 id：模板/自动组名被占用时追加 -2、-3…，避免覆盖已有组。 */
function uniqueDraftID(base: string): string {
  let id = base
  for (let n = 2; drafts.value.some((d) => d.id === id); n++) {
    id = `${base}-${n}`
  }
  return id
}

/** task-44：把一个模板预填成草稿并打开编辑器；不自动保存。 */
function applyTemplate(template: RoutingTemplate) {
  if (template.why) return
  const draft = newDraft()
  draft.id = uniqueDraftID(template.kind)
  draft.display = template.name
  draft.matchText = (template.match ?? []).join('\n')
  draft.rules = (template.rules ?? []).map(ruleToDraft)
  drafts.value.push(draft)
  editing.value = drafts.value.length - 1
  message.value = `已按模板「${template.name}」预填草稿，检查成员与规则后点「保存全部」生效。`
}

/** task-44：把一个自动组转成可编辑草稿（成员写成精确 id 模式）。 */
function saveAutoGroup(group: RoutingAutoGroup) {
  if (group.edited) return
  const draft = newDraft()
  draft.id = uniqueDraftID(group.id)
  draft.display = group.display || group.id
  draft.matchText = group.members.join('\n')
  drafts.value.push(draft)
  editing.value = drafts.value.length - 1
  message.value = '已把自动组转成可编辑草稿：可改名、加权重或配规则；保存同名组后该自动组由你的配置接管。'
}

/* ── task-47：左侧列表拖拽排序（曲线 = magpie routing.css:560-563）── */

const dragIndex = ref(-1)

function onDragStart(index: number, event: DragEvent) {
  dragIndex.value = index
  if (event.dataTransfer) {
    event.dataTransfer.effectAllowed = 'move'
    event.dataTransfer.setData('text/plain', String(index))
  }
}

/* dragover 即重排（sortable 惯例）：splices 草稿并随动 editing 下标，
 * TransitionGroup 的 .rs-list-move 给其余行补 0.22s 位移动画。 */
function onDragOver(index: number, event: DragEvent) {
  const from = dragIndex.value
  if (from < 0 || from === index) return
  event.preventDefault()
  reorder(from, index)
  dragIndex.value = index
}

function reorder(from: number, to: number) {
  if (from === to) return
  const moved = drafts.value.splice(from, 1)[0]
  if (!moved) return
  drafts.value.splice(to, 0, moved)
  // 编辑焦点跟随被拖的行，其余下标按搬移方向平移。
  if (editing.value === from) editing.value = to
  else if (from < editing.value && editing.value <= to) editing.value -= 1
  else if (to <= editing.value && editing.value < from) editing.value += 1
}

function onDragEnd() {
  dragIndex.value = -1
}

function membersOf(id: string): string[] {
  return savedGroups.value.find((group) => group.id === id)?.members ?? []
}

function membersLabel(id: string): string {
  const members = membersOf(id)
  if (members.length === 0) return '未匹配任何模型'
  const shown = members.slice(0, 3).join('、')
  return members.length > 3 ? `${shown} 等 ${members.length} 个` : shown
}

/** task-44：自动组的成员摘要（成员列表直接给定，不走已保存组查找）。 */
function membersLabelOf(members: string[]): string {
  if (members.length === 0) return '未匹配任何模型'
  const shown = members.slice(0, 3).join('、')
  return members.length > 3 ? `${shown} 等 ${members.length} 个` : shown
}

function lineCount(text: string): number {
  return lines(text).length
}

onMounted(load)
</script>

<template>
  <div class="page">
    <div v-if="error" class="notice error">
      <div class="row between">
        <span>{{ error }}</span>
        <button type="button" class="btn sm ghost" :disabled="loading" @click="load">重新读取</button>
      </div>
    </div>
    <div v-if="message" class="notice ok rs-hit">{{ message }}</div>

    <section class="card">
      <div class="card-body tight">
        <div class="toolbar">
          <div class="field grow">
            <span class="field-label">路由组</span>
            <span class="small muted">
              用模式把多个模型聚合成一个可路由的名字：组规则按请求特征（长度/Token/图片/思考/Agent/时段）换成员，意图规则让判据模型选组。保存写入 config.json，下一次调度快照生效。
            </span>
          </div>
          <button type="button" class="btn" :disabled="loading" @click="load">重新读取</button>
          <button type="button" class="btn" @click="startNew">新建路由组</button>
          <button type="button" class="btn primary" :disabled="saving || !dirty" @click="save">
            {{ saving ? '保存中…' : '保存全部' }}
          </button>
        </div>
      </div>
    </section>

    <!-- 实时观察流（A 阶段舞台，task-53 组件）：展示网关真实转发流；
         数据源 = 本页已加载的 groups/autoGroups，组件不自己拉。 -->
    <div v-if="stageNotice" class="notice warn">{{ stageNotice }}</div>
    <RouteStage
      :groups="savedGroups"
      :auto-groups="autoGroups"
      @unavailable="onStageUnavailable"
    />

    <section class="card">
      <div class="card-body tight">
        <div class="row between">
          <span class="field-label">一键模板</span>
          <span class="small muted">按当前目录的费率与上下文自动挑模型，预填一个组草稿——不直接保存，检查后点「保存全部」。三张模板本身是内置默认值、不可编辑；但点「使用模板」生成的组草稿完全可编辑：改名、成员、权重、规则都可以改，保存后就是你自己的组。</span>
        </div>
        <div class="stack">
          <div v-for="t in templates" :key="t.kind" class="row between rs-in">
            <div class="stack">
              <strong>{{ t.name }}</strong>
              <span class="small muted" :class="{ 'text-danger': t.why }">{{ t.why || t.description }}</span>
            </div>
            <button
              type="button"
              class="btn sm"
              :disabled="Boolean(t.why) || loading"
              :title="t.why || t.description"
              @click="applyTemplate(t)"
            >
              {{ t.why ? '不可用' : '使用模板' }}
            </button>
          </div>
        </div>
      </div>
    </section>

    <div class="rules-layout">
      <section class="card">
        <div class="card-head">
          <h2>已有路由组</h2>
          <span class="badge plain muted">{{ drafts.length }} 个</span>
        </div>
        <div class="card-body tight">
          <span class="small muted">这是什么：每一行是一个会写进 config.json 的组草稿。拖动可调列表顺序（只影响展示与保存顺序，不影响路由）；点一行在右侧编辑，改动点「保存全部」前不生效。</span>
        </div>
        <div v-if="loading && drafts.length === 0" class="log-skeleton" aria-hidden="true">
          <span v-for="n in 4" :key="n" class="skeleton skeleton-line"></span>
        </div>
        <div v-else-if="drafts.length === 0" class="empty">
          <strong>还没有路由组</strong>
          <span>新建一个，用 <code>*</code> 或 <code>plugin/model</code> 把成员圈进来。</span>
        </div>
        <TransitionGroup v-else tag="div" name="rs-list" class="rule-list">
          <button
            v-for="(draft, index) in drafts"
            :key="draft.uid"
            type="button"
            class="rule-item"
            :class="{ active: editing === index, 'rs-dragging': dragIndex === index }"
            draggable="true"
            @click="editing = index"
            @dragstart="onDragStart(index, $event)"
            @dragover="onDragOver(index, $event)"
            @dragend="onDragEnd"
            @drop.prevent="onDragEnd"
          >
            <span class="rule-item-head">
              <strong>{{ draft.id || '未命名' }}</strong>
              <span class="badge plain" :class="draft.disabled ? 'muted' : 'ok'">
                {{ draft.disabled ? '停用' : '启用' }}
              </span>
            </span>
            <span v-if="draft.display" class="small muted">{{ draft.display }}</span>
            <span class="small muted">
              {{ lineCount(draft.matchText) }} 条模式 · {{ draft.rules.length }} 条规则<template v-if="draft.intentEnabled"> · 意图规则</template>
            </span>
            <span class="small muted mono">{{ membersLabel(draft.id) }}</span>
          </button>
        </TransitionGroup>
      </section>

      <section class="card">
        <div class="card-head">
          <h2>{{ editing >= 0 ? '编辑路由组' : '路由组详情' }}</h2>
          <span v-if="editing >= 0 && dirty" class="badge plain muted">未保存的改动不生效</span>
        </div>
        <div class="card-body tight">
          <span class="small muted">这是什么：从上到下依次是 身份（ID/展示名）→ 成员匹配（用模式圈模型）→ 权重（成员分摊比例）→ 组规则（按请求特征临时换成员，首条命中）→ 意图规则（判据模型替你选组）。只填前两层组就能用，其余按需。</span>
        </div>
        <Transition name="rs-in" mode="out-in">
          <div v-if="editing < 0" class="empty" key="empty">
            <strong>选择一个路由组</strong>
            <span>左侧列表点一条开始编辑，或点右上角「新建路由组」。</span>
          </div>
          <div v-else class="card-body stack" :key="drafts[editing]?.uid">
          <div class="form-grid">
            <div class="field">
              <label :for="`group-id-${editing}`">ID（路由名）</label>
              <input :id="`group-id-${editing}`" v-model="drafts[editing].id" class="input mono" type="text" placeholder="例如 smart" spellcheck="false" />
              <span class="help">客户端用这个 ID 请求；不能包含斜杠或空白。</span>
            </div>
            <div class="field">
              <label :for="`group-display-${editing}`">展示名</label>
              <input :id="`group-display-${editing}`" v-model="drafts[editing].display" class="input" type="text" placeholder="可选，仅管理页显示" />
            </div>
          </div>

          <div class="field">
            <label :for="`group-match-${editing}`">成员匹配（每行一条）</label>
            <textarea :id="`group-match-${editing}`" v-model="drafts[editing].matchText" class="input mono group-textarea" spellcheck="false" placeholder="trae/*&#10;re:^officeace/"></textarea>
            <span class="help">支持精确 ID、<code>*</code>/<code>?</code> 通配与 <code>re:</code> 正则，按当前模型目录实时求解。</span>
          </div>

          <div class="field">
            <label :for="`group-weights-${editing}`">成员权重（每行 成员=权重，可留空）</label>
            <textarea :id="`group-weights-${editing}`" v-model="drafts[editing].weightsText" class="input mono group-textarea-sm" spellcheck="false" placeholder="trae/gpt-4o=3&#10;officeace/gpt-4o=1"></textarea>
            <span class="help">权重 1~1000；成员写成 <code>plugin/model</code>，按平滑轮询分配。</span>
          </div>

          <label class="switch">
            <input v-model="drafts[editing].disabled" type="checkbox" />
            <span class="switch-track"></span>
            停用（保留配置，不参与路由）
          </label>

          <div class="stack">
            <div class="row between">
              <span class="field-label">组规则（按顺序首条命中）</span>
              <button type="button" class="btn sm" @click="addRule">添加规则</button>
            </div>
            <div v-if="drafts[editing].rules.length === 0" class="small muted">还没有规则，请求按「成员匹配」的成员顺序轮转。</div>
            <div v-for="(rule, ruleIndex) in drafts[editing].rules" :key="ruleIndex" class="group-rule stack">
              <div class="row between">
                <strong>规则 {{ ruleIndex + 1 }}</strong>
                <button type="button" class="btn sm ghost" @click="drafts[editing].rules.splice(ruleIndex, 1)">删除</button>
              </div>
              <div class="form-grid">
                <div class="field">
                  <label>名称</label>
                  <input v-model="rule.name" class="input" type="text" placeholder="例如 large-context" />
                </div>
                <div class="field">
                  <label>命中时成员（每行一条）</label>
                  <textarea v-model="rule.matchText" class="input mono group-textarea-sm" spellcheck="false" placeholder="vendor/pro"></textarea>
                </div>
              </div>
              <div class="form-grid">
                <div class="field"><label>最小字符数</label><input v-model="rule.minChars" class="input" type="number" min="0" /></div>
                <div class="field"><label>最大字符数</label><input v-model="rule.maxChars" class="input" type="number" min="0" /></div>
                <div class="field"><label>最小 Token</label><input v-model="rule.minTokens" class="input" type="number" min="0" /></div>
                <div class="field"><label>最大 Token</label><input v-model="rule.maxTokens" class="input" type="number" min="0" /></div>
                <div class="field"><label>起始小时</label><input v-model="rule.startHour" class="input" type="number" min="0" max="24" /></div>
                <div class="field"><label>结束小时</label><input v-model="rule.endHour" class="input" type="number" min="0" max="24" /></div>
              </div>
              <div class="row group-when-row">
                <label class="switch">
                  <input v-model="rule.image" type="checkbox" />
                  <span class="switch-track"></span>
                  带图
                </label>
                <label class="switch">
                  <input v-model="rule.thinking" type="checkbox" />
                  <span class="switch-track"></span>
                  思考
                </label>
                <div class="field grow"><label>Agent 包含</label><input v-model="rule.agent" class="input" type="text" placeholder="例如 claude" /></div>
              </div>
            </div>
          </div>

          <div class="stack">
            <div class="row between">
              <span class="field-label">意图规则</span>
              <label class="switch">
                <input v-model="drafts[editing].intentEnabled" type="checkbox" />
                <span class="switch-track"></span>
                启用
              </label>
            </div>
            <div class="rs-fold" :class="{ open: drafts[editing].intentEnabled }">
              <div class="rs-fold-inner" :inert="!drafts[editing].intentEnabled">
                <div class="stack">
                  <div class="form-grid">
                <div class="field">
                  <label>判据上游</label>
                  <select v-model="drafts[editing].intentUpstream" class="select">
                    <option value="">首个决策上游</option>
                    <option v-for="upstream in upstreams" :key="upstream.id" :value="upstream.id">
                      {{ upstream.id }}（{{ upstream.protocol }} / {{ upstream.model_id }}）
                    </option>
                  </select>
                </div>
                <div class="field">
                  <label>兜底组</label>
                  <select v-model="drafts[editing].intentDefault" class="select">
                    <option value="">保持本组</option>
                    <option v-for="draft in drafts" :key="draft.id" :value="draft.id.trim()">{{ draft.id || '未命名' }}</option>
                  </select>
                </div>
                <div class="field">
                  <label>探针字符数</label>
                  <input v-model="drafts[editing].intentProbeChars" class="input" type="number" min="100" max="20000" />
                </div>
              </div>
              <div class="stack">
                <div class="row between">
                  <span class="field-label">标签路由（标签 → 路由组）</span>
                  <button type="button" class="btn sm" @click="addRoute">添加标签</button>
                </div>
                <div v-if="drafts[editing].routes.length === 0" class="small muted">还没有标签路由。</div>
                <div v-for="(route, routeIndex) in drafts[editing].routes" :key="routeIndex" class="group-route-row">
                  <input v-model="route.label" class="input mono" type="text" placeholder="标签，如 hard" />
                  <select v-model="route.target" class="select">
                    <option value="">选择目标组</option>
                    <option v-for="draft in drafts" :key="draft.id" :value="draft.id.trim()">{{ draft.id || '未命名' }}</option>
                  </select>
                  <button type="button" class="btn sm ghost" @click="drafts[editing].routes.splice(routeIndex, 1)">删除</button>
                </div>
              </div>
                  <span class="help">
                    判据请求最多等 3 秒；超时、上游报错或答案无法解析时走兜底组；同一段用户输入的判定结果缓存 10 分钟。
                  </span>
                </div>
              </div>
            </div>
          </div>

          <div v-if="dirty" class="notice warn">
            <span>改动尚未保存；保存后写入 config.json，并在下一次调度快照生效。</span>
            <button type="button" class="btn sm" :disabled="saving" @click="save">立即保存</button>
          </div>

          <div class="row between">
            <button type="button" class="btn sm danger" @click="removeDraft(editing)">删除该组</button>
            <span class="small muted">成员预览：{{ membersLabel(drafts[editing]?.id ?? '') }}</span>
          </div>
          </div>
        </Transition>
      </section>
    </div>

    <section v-if="autoGroups.length > 0" class="card">
      <div class="card-head">
        <h2><span class="rs-dot" aria-hidden="true" title="随每次调度快照实时重新求解"></span> 自动分组</h2>
        <span class="badge plain muted">{{ autoGroups.length }} 个</span>
      </div>
      <div class="card-body stack">
        <span class="small muted rs-in">
          这是什么：同一模型被两个及以上插件同时服务时，调度会在每次快照自动生成 auto- 开头的组并按插件顺序轮转——无需手工维护。要把某一个改成自己的（改名/权重/规则），点「存为可编辑组」：保存同名组后你的配置接管该名字，之后新插件服务同模型时会自动尾随加入你的成员之后。
        </span>
        <div v-for="g in autoGroups" :key="g.id" class="row between rs-found">
          <span class="row">
            <strong class="mono">{{ g.id }}</strong>
            <span class="small muted mono">{{ membersLabelOf(g.members) }}</span>
            <span v-if="g.edited" class="badge plain muted">已有同名自定义组接管</span>
          </span>
          <button type="button" class="btn sm" :disabled="g.edited || loading" @click="saveAutoGroup(g)">
            {{ g.edited ? '已接管' : '存为可编辑组' }}
          </button>
        </div>
      </div>
    </section>
  </div>
</template>
