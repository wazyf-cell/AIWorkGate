<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { api, errorMessage, type PluginStatus, type RulePack, type ToolAliasStatus } from '../api'

interface RuleRow {
  id: number
  from: string
  to: string
}

interface StripRow {
  id: number
  pattern: string
}

// —— 问题6A（task-43）：视图切换 + 工具别名指纹清洗 ——
type RulesView = 'packs' | 'alias'
const view = ref<RulesView>('packs')
const aliasLoading = ref(false)
const aliasSaving = ref(false)
const aliasStatus = ref<ToolAliasStatus | null>(null)
const aliasEnabled = ref(false)
const showDefaults = ref(true)
const aliasRules = ref<Array<{ id: number; from: string; to: string }>>([])
let aliasRowSeq = 0

function newAliasRow(from = '', to = '') {
  aliasRowSeq += 1
  return { id: aliasRowSeq, from, to }
}

async function loadAlias() {
  aliasLoading.value = true
  error.value = ''
  try {
    const data = await api.toolAlias.status()
    aliasStatus.value = data ?? null
    aliasEnabled.value = data?.enabled ?? false
    aliasRules.value = (data?.rules ?? []).map((rule) => newAliasRow(rule.from, rule.to))
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    aliasLoading.value = false
  }
}

function switchView(next: RulesView) {
  view.value = next
  message.value = ''
  if (next === 'alias' && aliasStatus.value === null) void loadAlias()
}

async function saveAlias() {
  aliasSaving.value = true
  error.value = ''
  message.value = ''
  warning.value = ''
  try {
    await api.toolAlias.save({
      enabled: aliasEnabled.value,
      rules: aliasRules.value
        .map((row) => ({ from: row.from.trim(), to: row.to.trim() }))
        .filter((row) => row.from !== ''),
    })
    await loadAlias()
    message.value = '工具别名已保存，网关即时生效（无需重启）。'
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    aliasSaving.value = false
  }
}

// —— task-64：内置默认表直接可编辑 ——
// 后端合并语义（tool_alias 合并：自定义按 from 覆盖内置别名）已经支持覆盖，
// 所以「编辑内置某条」= 生成一条同 from 的自定义覆盖规则，零后端改动。
// 内置表始终全量生效（升级程序补新条目不丢），用户的覆盖行按 from 生效且
// 升级不丢；to 填空保存 = 停用该条改写（原名透传，与自定义区语义一致）。

/** 某内置工具名的自定义覆盖行（无 = 未覆盖，内置别名生效）。 */
function overrideOf(from: string) {
  return aliasRules.value.find((row) => row.from.trim() === from)
}

/** 内置行的展示值：有覆盖行显示自定义值（空串=已停用），否则内置别名。 */
function builtinDisplayTo(rule: { from: string; to: string }): string {
  const override = overrideOf(rule.from)
  if (!override) return rule.to
  return override.to.trim() === '' ? '（已停用）' : override.to
}

/** 点内置行「编辑」：生成一条同 from 的覆盖行（to 预填内置别名）；已有覆盖行
 * 则不重复生成（直接编辑那份）。 */
function editBuiltin(rule: { from: string; to: string }) {
  if (!overrideOf(rule.from)) aliasRules.value.push(newAliasRow(rule.from, rule.to))
}

/** 内置行「恢复默认」= 删除同 from 的自定义覆盖行（保存后内置别名重新生效）。 */
function restoreBuiltin(rule: { from: string; to: string }) {
  const index = aliasRules.value.findIndex((row) => row.from.trim() === rule.from)
  if (index >= 0) aliasRules.value.splice(index, 1)
}

const expandAllConfirm = ref(false)
let expandAllTimer: number | undefined

function clearExpandAllConfirm() {
  if (expandAllTimer !== undefined) {
    window.clearTimeout(expandAllTimer)
    expandAllTimer = undefined
  }
  expandAllConfirm.value = false
}

/** 「全部展开」将新增的覆盖条数（未覆盖的内置行数）。 */
function expandAllAddedCount(): number {
  return (aliasStatus.value?.default_rules ?? []).filter((rule) => !overrideOf(rule.from)).length
}

/** 一键「全部展开为可编辑副本」：内置全部条目加为自定义覆盖行（已覆盖的行
 * 保留用户当前编辑值，不重置）。两段式确认：一次新增 N 条自定义条目。 */
function expandAllBuiltin() {
  const defaults = aliasStatus.value?.default_rules ?? []
  if (defaults.length === 0) return
  if (!expandAllConfirm.value) {
    expandAllConfirm.value = true
    if (expandAllTimer !== undefined) window.clearTimeout(expandAllTimer)
    expandAllTimer = window.setTimeout(() => {
      expandAllConfirm.value = false
      expandAllTimer = undefined
    }, 4000)
    return
  }
  clearExpandAllConfirm()
  let added = 0
  for (const rule of defaults) {
    if (!overrideOf(rule.from)) {
      aliasRules.value.push(newAliasRow(rule.from, rule.to))
      added += 1
    }
  }
  message.value = `已展开内置默认表为可编辑副本：新增 ${added} 条覆盖规则，改完点「保存并生效」。`
}

onUnmounted(clearExpandAllConfirm)

let rowSeq = 0

function newRuleRow(from = '', to = ''): RuleRow {
  rowSeq += 1
  return { id: rowSeq, from, to }
}

function newStripRow(pattern = ''): StripRow {
  rowSeq += 1
  return { id: rowSeq, pattern }
}

const packs = ref<RulePack[]>([])
const builtin = ref<string[]>([])
const plugins = ref<PluginStatus[]>([])
const loading = ref(false)
const saving = ref(false)
const reloading = ref(false)
const error = ref('')
const message = ref('')
const warning = ref('')
/** Name of the pack being edited; empty while creating a new pack. */
const editing = ref('')
const confirmState = ref<'' | 'delete' | 'reset'>('')

const form = reactive({
  name: '',
  description: '',
  rules: [] as RuleRow[],
  strip: [] as StripRow[],
})

let confirmTimer: number | undefined

function clearConfirm() {
  if (confirmTimer !== undefined) {
    window.clearTimeout(confirmTimer)
    confirmTimer = undefined
  }
  confirmState.value = ''
}

function armConfirm(kind: 'delete' | 'reset') {
  if (confirmTimer !== undefined) window.clearTimeout(confirmTimer)
  confirmState.value = kind
  confirmTimer = window.setTimeout(() => {
    confirmState.value = ''
    confirmTimer = undefined
  }, 4000)
}

onUnmounted(clearConfirm)

const usages = computed(() => {
  const map = new Map<string, string[]>()
  for (const plugin of plugins.value) {
    const label = plugin.manifest?.name || plugin.manifest?.id || ''
    if (!label) continue
    for (const name of plugin.manifest?.rules?.packs ?? []) {
      if (!name) continue
      const list = map.get(name) ?? []
      if (!list.includes(label)) list.push(label)
      map.set(name, list)
    }
  }
  return map
})

function isBuiltin(name: string): boolean {
  return builtin.value.includes(name)
}

function usedBy(name: string): string {
  const list = usages.value.get(name) ?? []
  return list.length > 0 ? `被 ${list.join('、')} 使用` : ''
}

function ruleCount(pack: RulePack): number {
  return pack.rules?.length ?? 0
}

function stripCount(pack: RulePack): number {
  return pack.strip?.length ?? 0
}

function selectPack(pack: RulePack) {
  editing.value = pack.name
  form.name = pack.name
  form.description = pack.description ?? ''
  form.rules = (pack.rules ?? []).map((rule) => newRuleRow(rule.from, rule.to))
  form.strip = (pack.strip ?? []).map((strip) => newStripRow(strip.pattern))
  clearConfirm()
}

function choose(pack: RulePack) {
  error.value = ''
  message.value = ''
  warning.value = ''
  selectPack(pack)
}

function newPack() {
  editing.value = ''
  form.name = ''
  form.description = ''
  form.rules = [newRuleRow()]
  form.strip = []
  clearConfirm()
}

function startNew() {
  error.value = ''
  message.value = ''
  warning.value = ''
  newPack()
}

async function loadPacks() {
  loading.value = true
  error.value = ''
  try {
    const data = await api.rules.list()
    packs.value = data?.packs ?? []
    builtin.value = data?.builtin ?? []
    // The edited pack may have been removed elsewhere; fall back to a fresh form.
    if (editing.value && !packs.value.some((pack) => pack.name === editing.value)) newPack()
  } catch (e) {
    error.value = errorMessage(e)
    packs.value = []
    builtin.value = []
  } finally {
    loading.value = false
  }
}

async function loadPlugins() {
  try {
    plugins.value = (await api.plugins.list()) ?? []
  } catch {
    plugins.value = []
  }
}

onMounted(async () => {
  await Promise.all([loadPacks(), loadPlugins()])
  if (packs.value.length > 0) selectPack(packs.value[0])
  else newPack()
})

async function savePack() {
  const name = form.name.trim()
  if (!name) {
    error.value = '请填写规则包名称。'
    return
  }
  const payload: RulePack = {
    name,
    description: form.description.trim() || undefined,
    rules: form.rules
      .map((row) => ({ from: row.from.trim(), to: row.to }))
      .filter((row) => row.from !== ''),
    strip: form.strip
      .map((row) => ({ pattern: row.pattern.trim() }))
      .filter((row) => row.pattern !== ''),
  }
  saving.value = true
  error.value = ''
  message.value = ''
  warning.value = ''
  try {
    await api.rules.save(payload)
    await loadPacks()
    selectPack(payload)
    message.value = `规则包「${name}」已保存。`
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    saving.value = false
  }
}

async function doDelete() {
  const name = editing.value
  if (!name) return
  clearConfirm()
  saving.value = true
  error.value = ''
  message.value = ''
  warning.value = ''
  try {
    await api.rules.remove(name)
    await loadPacks()
    if (packs.value.length > 0) selectPack(packs.value[0])
    else newPack()
    message.value = `规则包「${name}」已删除。`
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    saving.value = false
  }
}

async function doReset() {
  const name = editing.value
  if (!name) return
  clearConfirm()
  saving.value = true
  error.value = ''
  message.value = ''
  warning.value = ''
  try {
    await api.rules.reset(name)
    await loadPacks()
    const restored = packs.value.find((pack) => pack.name === name)
    if (restored) selectPack(restored)
    message.value = `规则包「${name}」已恢复默认。`
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    saving.value = false
  }
}

async function reloadPlugins() {
  reloading.value = true
  error.value = ''
  message.value = ''
  warning.value = ''
  try {
    const result = await api.rules.reload()
    const reloaded = result?.reloaded ?? []
    const failed = result?.failed ?? []
    message.value = `已重新加载 ${reloaded.length} 个插件的规则。`
    if (failed.length > 0) warning.value = `以下插件重新加载失败：${failed.join('、')}`
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    reloading.value = false
  }
}
</script>

<template>
  <div class="page">
    <div v-if="error" class="notice error">
      <span>操作失败，请重试；下面是详细原因：{{ error }}</span>
      <button type="button" class="btn sm ghost" @click="loadPacks">重试</button>
    </div>
    <div v-if="warning" class="notice warn">{{ warning }}</div>
    <div v-if="message" class="notice ok">{{ message }}</div>

    <section class="card">
      <div class="card-body tight">
        <div class="toolbar">
          <div class="row">
            <button type="button" class="btn" :class="{ primary: view === 'packs' }" @click="switchView('packs')">
              规则包
            </button>
            <button type="button" class="btn" :class="{ primary: view === 'alias' }" @click="switchView('alias')">
              工具别名
            </button>
          </div>
          <div class="field grow">
            <span class="field-label">
              {{ view === 'packs' ? '共享规则包' : '工具别名（指纹清洗）' }}
            </span>
            <span class="small muted">
              {{
                view === 'packs'
                  ? '宿主保存并下发到插件数据目录；修改后需要重新加载插件才会生效。'
                  : '网关进程内逐请求改写客户端独家工具名（上游只见语义保持的别名），响应侧自动换回原名；保存即生效。'
              }}
            </span>
          </div>
          <template v-if="view === 'packs'">
            <button type="button" class="btn primary" @click="startNew">新建规则包</button>
            <button type="button" class="btn" :disabled="reloading" @click="reloadPlugins">
              {{ reloading ? '重新加载中…' : '重新加载插件规则' }}
            </button>
          </template>
          <button type="button" class="btn" :disabled="loading" @click="loadPacks">
            {{ loading ? '刷新中…' : '刷新' }}
          </button>
        </div>
      </div>
    </section>

    <!-- 问题6A：工具别名 -->
    <section v-if="view === 'alias'" class="card">
      <div class="card-head">
        <h2>工具别名</h2>
        <span v-if="aliasStatus" class="badge plain muted">
          内置默认 {{ aliasStatus.defaults ?? 0 }} 条 · 自定义 {{ aliasRules.length }} 条
        </span>
      </div>
      <div class="card-body stack">
        <div v-if="aliasLoading && !aliasStatus" class="log-skeleton" aria-hidden="true">
          <span v-for="n in 4" :key="n" class="skeleton skeleton-line"></span>
        </div>
        <template v-else>
          <div class="field">
            <label class="check-row">
              <input v-model="aliasEnabled" type="checkbox" />
              <span>启用工具别名改写</span>
            </label>
            <span class="help">
              内置默认表（{{ aliasStatus?.defaults ?? 0 }} 条）全量生效；在下方内置表里点「编辑」即生成一条
              同名覆盖规则（升级程序补新条目不丢你的改动），别名填空即停用该工具的改写。
              改写要求请求/响应双向一致，因此不支持多候选。
            </span>
          </div>

          <div v-if="aliasStatus?.warnings?.length" class="notice warn">
            {{ aliasStatus.warnings.join('；') }}
          </div>

          <div class="field">
            <span class="field-label">自定义规则（{{ aliasRules.length }} 条）</span>
            <div v-if="aliasRules.length === 0" class="small muted">
              还没有自定义规则；内置默认表仍生效，添加规则可覆盖或新增。
            </div>
            <div v-for="(row, index) in aliasRules" :key="row.id" class="rule-row">
              <input v-model="row.from" class="input mono" type="text" placeholder="真实工具名（逐字）" spellcheck="false" />
              <span class="rule-arrow small muted">→</span>
              <input v-model="row.to" class="input mono" type="text" placeholder="别名（留空 = 停用）" spellcheck="false" />
              <button type="button" class="btn sm ghost" @click="aliasRules.splice(index, 1)">删除</button>
            </div>
            <div>
              <button type="button" class="btn sm" @click="aliasRules.push(newAliasRow())">添加一条</button>
            </div>
          </div>

          <div class="row between">
            <div class="btn-row">
              <button type="button" class="btn primary" :disabled="aliasSaving" @click="saveAlias">
                {{ aliasSaving ? '保存中…' : '保存并生效' }}
              </button>
              <button type="button" class="btn" :disabled="aliasLoading" @click="loadAlias">刷新</button>
            </div>
          </div>

          <div class="field">
            <div class="row between">
              <button type="button" class="btn sm ghost" @click="showDefaults = !showDefaults">
                {{ showDefaults ? '收起内置默认表' : `展开内置默认表（${aliasStatus?.defaults ?? 0} 条，可编辑）` }}
              </button>
              <template v-if="showDefaults && (aliasStatus?.default_rules?.length ?? 0) > 0">
                <button
                  v-if="!expandAllConfirm"
                  type="button"
                  class="btn sm"
                  @click="expandAllBuiltin"
                >
                  全部展开为可编辑副本
                </button>
                <button v-else type="button" class="btn sm danger" @click="expandAllBuiltin">
                  确认：新增 {{ expandAllAddedCount() }} 条覆盖规则
                </button>
              </template>
            </div>
            <div v-if="showDefaults" class="table-wrap">
              <table class="table">
                <thead>
                  <tr><th>真实工具名</th><th>生效别名</th><th style="text-align: right">操作</th></tr>
                </thead>
                <tbody>
                  <tr v-for="rule in aliasStatus?.default_rules ?? []" :key="rule.from">
                    <td class="mono small">
                      {{ rule.from }}
                      <span v-if="overrideOf(rule.from)" class="badge plain accent" title="该条已被同名自定义覆盖规则改写；升级程序不会丢这条改动">已覆盖</span>
                    </td>
                    <td class="mono small" :class="{ muted: overrideOf(rule.from)?.to.trim() === '' }">
                      {{ builtinDisplayTo(rule) }}
                    </td>
                    <td>
                      <div class="btn-row" style="justify-content: flex-end">
                        <button type="button" class="btn sm" @click="editBuiltin(rule)">
                          {{ overrideOf(rule.from) ? '编辑覆盖' : '编辑' }}
                        </button>
                        <button
                          v-if="overrideOf(rule.from)"
                          type="button"
                          class="btn sm ghost"
                          title="删除该覆盖规则，保存后恢复内置别名"
                          @click="restoreBuiltin(rule)"
                        >
                          恢复默认
                        </button>
                      </div>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
        </template>
      </div>
    </section>

    <div v-else class="rules-layout">
      <section class="card">
        <div class="card-head">
          <h2>规则包</h2>
          <span class="badge plain muted">{{ packs.length }} 个</span>
        </div>

        <div v-if="loading && packs.length === 0" class="log-skeleton" aria-hidden="true">
          <span v-for="n in 6" :key="n" class="skeleton skeleton-line"></span>
        </div>

        <div v-else-if="packs.length === 0" class="empty">
          <strong>还没有规则包</strong>
          <span>还没有规则包，点击“新建规则包”创建。</span>
        </div>

        <div v-else class="rule-list">
          <button
            v-for="pack in packs"
            :key="pack.name"
            type="button"
            class="rule-item"
            :class="{ active: editing === pack.name }"
            @click="choose(pack)"
          >
            <span class="rule-item-head">
              <strong>{{ pack.name }}</strong>
              <span v-if="isBuiltin(pack.name)" class="badge plain accent">内置</span>
            </span>
            <span v-if="pack.description" class="small muted">{{ pack.description }}</span>
            <span class="small muted">{{ ruleCount(pack) }} 条替换 · {{ stripCount(pack) }} 条正则</span>
            <span v-if="usedBy(pack.name)" class="small muted">{{ usedBy(pack.name) }}</span>
          </button>
        </div>
      </section>

      <section class="card">
        <div class="card-head">
          <h2>{{ editing ? `编辑：${editing}` : '新建规则包' }}</h2>
          <span v-if="editing && isBuiltin(editing)" class="badge plain accent">内置</span>
          <span v-else-if="editing" class="badge plain muted">自定义</span>
          <span v-else class="sub">尚未保存</span>
        </div>

        <div class="card-body stack">
          <div class="form-grid">
            <div class="field">
              <label for="rule-name">名称</label>
              <input
                id="rule-name"
                v-model="form.name"
                class="input mono"
                type="text"
                placeholder="例如：wb-12.4"
                spellcheck="false"
                :disabled="editing !== ''"
              />
              <span class="help">
                {{ editing ? '名称创建后不可修改；如需改名请新建规则包。' : '仅允许字母、数字、下划线与短横线等安全字符。' }}
              </span>
            </div>
            <div class="field">
              <label for="rule-desc">描述</label>
              <input
                id="rule-desc"
                v-model="form.description"
                class="input"
                type="text"
                placeholder="可选，说明适用的客户端版本或用途"
              />
            </div>
          </div>

          <div class="field">
            <span class="field-label">字面替换（{{ form.rules.length }} 条）</span>
            <div v-if="form.rules.length === 0" class="small muted">还没有替换规则，可点击下方按钮添加。</div>
            <div v-for="(row, index) in form.rules" :key="row.id" class="rule-row">
              <input v-model="row.from" class="input mono" type="text" placeholder="原文" spellcheck="false" />
              <span class="rule-arrow small muted">→</span>
              <input v-model="row.to" class="input mono" type="text" placeholder="替换为" spellcheck="false" />
              <button type="button" class="btn sm ghost" @click="form.rules.splice(index, 1)">删除</button>
            </div>
            <div>
              <button type="button" class="btn sm" @click="form.rules.push(newRuleRow())">添加一条</button>
            </div>
          </div>

          <div class="field">
            <span class="field-label">正则清洗（{{ form.strip.length }} 条）</span>
            <div v-if="form.strip.length === 0" class="small muted">还没有正则规则，可点击下方按钮添加。</div>
            <div v-for="(row, index) in form.strip" :key="row.id" class="rule-strip-row">
              <input
                v-model="row.pattern"
                class="input mono"
                type="text"
                placeholder="例如：[0-9a-f]{8}-[0-9a-f]{4}"
                spellcheck="false"
              />
              <button type="button" class="btn sm ghost" @click="form.strip.splice(index, 1)">删除</button>
            </div>
            <div>
              <button type="button" class="btn sm" @click="form.strip.push(newStripRow())">添加一条</button>
            </div>
            <span class="help">正则匹配到的内容会被移除，用于清理请求中的设备指纹与遥测字段。</span>
          </div>

          <div class="row between">
            <div class="btn-row">
              <button
                type="button"
                class="btn primary"
                :disabled="saving || !form.name.trim()"
                @click="savePack"
              >
                {{ saving ? '保存中…' : '保存' }}
              </button>
              <button type="button" class="btn ghost" :disabled="saving" @click="startNew">清空</button>
            </div>

            <div v-if="editing" class="btn-row">
              <template v-if="isBuiltin(editing)">
                <button
                  v-if="confirmState !== 'reset'"
                  type="button"
                  class="btn sm"
                  :disabled="saving"
                  @click="armConfirm('reset')"
                >
                  恢复默认
                </button>
                <button v-else type="button" class="btn sm danger" :disabled="saving" @click="doReset">
                  确认恢复默认
                </button>
              </template>
              <template v-else>
                <button
                  v-if="confirmState !== 'delete'"
                  type="button"
                  class="btn sm danger"
                  :disabled="saving"
                  @click="armConfirm('delete')"
                >
                  删除
                </button>
                <button v-else type="button" class="btn sm danger" :disabled="saving" @click="doDelete">
                  确认删除
                </button>
              </template>
            </div>
          </div>
          <span v-if="confirmState && editing" class="help">再次点击按钮确认；4 秒内未操作将自动取消。</span>
        </div>
      </section>
    </div>
  </div>
</template>
