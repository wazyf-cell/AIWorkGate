<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { api, errorMessage, type PluginStatus, type RulePack } from '../api'

interface RuleRow {
  id: number
  from: string
  to: string
}

interface StripRow {
  id: number
  pattern: string
}

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
          <div class="field grow">
            <span class="field-label">共享规则包</span>
            <span class="small muted">
              宿主保存并下发到插件数据目录；修改后需要重新加载插件才会生效。
            </span>
          </div>
          <button type="button" class="btn primary" @click="startNew">新建规则包</button>
          <button type="button" class="btn" :disabled="reloading" @click="reloadPlugins">
            {{ reloading ? '重新加载中…' : '重新加载插件规则' }}
          </button>
          <button type="button" class="btn" :disabled="loading" @click="loadPacks">
            {{ loading ? '刷新中…' : '刷新' }}
          </button>
        </div>
      </div>
    </section>

    <div class="rules-layout">
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
