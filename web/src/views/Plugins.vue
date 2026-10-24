<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import {
  api,
  errorMessage,
  normalizeAccounts,
  type PluginAction,
  type PluginMeta,
  type PluginStatus,
  type StrictState,
} from '../api'
import { formatTimestamp } from '../format'

const plugins = ref<PluginStatus[]>([])
const accountCounts = reactive<Record<string, number | null>>({})
const loading = ref(false)
const error = ref('')
const message = ref('')
const busy = ref('')
const installFile = ref<File | null>(null)
const installEnabled = ref(false)
const installing = ref(false)

interface EditorState {
  plugin_id: string
  name: string
  enabled: boolean
  mode: string
  concurrency: number
  checkin_time: string
  strict: StrictState
  effective: { mode?: string; concurrency?: number; checkin_time?: string; strict?: boolean }
  global: { concurrency?: number; startup_mode?: string; strict_egress?: boolean; checkin_default_time?: string }
}

const editingId = ref('')
const settingsLoading = ref(false)
const savingSettings = ref(false)
const editor = reactive<EditorState>({
  plugin_id: '',
  name: '',
  enabled: true,
  mode: '',
  concurrency: 0,
  checkin_time: '',
  strict: 'default',
  effective: {},
  global: {},
})

async function load() {
  loading.value = true
  try {
    plugins.value = (await api.plugins.list()) ?? []
    error.value = ''
    await loadAccountCounts()
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    loading.value = false
  }
}

async function loadAccountCounts() {
  await Promise.all(
    plugins.value.map(async (plugin) => {
      const id = plugin.manifest?.id ?? ''
      try {
        const data = await api.accounts(id)
        accountCounts[id] = normalizeAccounts(data).length
      } catch {
        accountCounts[id] = null
      }
    }),
  )
}

function statusOf(plugin: PluginStatus): { label: string; tone: string } {
  if (plugin.failed) return { label: '异常', tone: 'danger' }
  if (!plugin.enabled) return { label: '已停用', tone: 'muted' }
  if (plugin.running && plugin.healthy) return { label: '运行中', tone: 'ok' }
  if (plugin.running) return { label: '运行中 · 未就绪', tone: 'warn' }
  return { label: '已停止', tone: 'warn' }
}

function modelLabel(plugin: PluginStatus): string {
  const models = plugin.manifest?.models ?? []
  if (models.length === 0) return '—'
  const names = models.slice(0, 3).map((item) => item.id)
  const suffix = models.length > 3 ? ` +${models.length - 3}` : ''
  return names.join(', ') + suffix
}

function accountLabel(plugin: PluginStatus): string {
  const id = plugin.manifest?.id ?? ''
  if (!(id in accountCounts)) return '—'
  const count = accountCounts[id]
  return count === null ? '—' : String(count)
}

/* ---------------------------------------------------------------- */
/* Plugin self-report (GET /meta)                                    */
/* ---------------------------------------------------------------- */

const metaId = ref('')
const metaLoading = ref(false)
const metaData = ref<PluginMeta | null>(null)
const metaError = ref('')

async function toggleMeta(plugin: PluginStatus) {
  const id = plugin.manifest?.id ?? ''
  if (!id) return
  if (metaId.value === id) {
    metaId.value = ''
    return
  }
  metaId.value = id
  metaData.value = null
  metaError.value = ''
  if (!plugin.running) {
    metaError.value = '插件未运行，自述需要进程在线才能读取。'
    return
  }
  metaLoading.value = true
  try {
    metaData.value = await api.plugins.meta(id)
  } catch (e) {
    metaError.value = errorMessage(e)
  } finally {
    metaLoading.value = false
  }
}

function uptimeLabel(seconds?: number): string {
  if (typeof seconds !== 'number' || !Number.isFinite(seconds) || seconds < 0) return '—'
  const total = Math.floor(seconds)
  const days = Math.floor(total / 86400)
  const hours = Math.floor((total % 86400) / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  if (days > 0) return `${days} 天 ${hours} 小时`
  if (hours > 0) return `${hours} 小时 ${minutes} 分`
  if (minutes > 0) return `${minutes} 分 ${total % 60} 秒`
  return `${total} 秒`
}

async function runAction(plugin: PluginStatus, action: PluginAction) {
  const id = plugin.manifest?.id ?? ''
  if (!id) return
  if (action === 'uninstall' && !window.confirm(`确认卸载插件「${plugin.manifest?.name || id}」？插件私有数据会保留。`)) {
    return
  }
  busy.value = `${id}/${action}`
  message.value = ''
  error.value = ''
  try {
    await api.plugins.action(id, action)
    message.value = `已对「${plugin.manifest?.name || id}」执行 ${{ enable: '启用', disable: '停用', start: '启动', stop: '停止', uninstall: '卸载' }[action]}。`
    await load()
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    busy.value = ''
  }
}

async function openEditor(plugin: PluginStatus) {
  const id = plugin.manifest?.id ?? ''
  if (!id) return
  editingId.value = id
  error.value = ''
  message.value = ''
  settingsLoading.value = true
  try {
    const s = await api.plugins.settings(id)
    editor.plugin_id = id
    editor.name = plugin.manifest?.name || id
    editor.enabled = Boolean(s.enabled)
    editor.mode = s.mode ?? ''
    editor.concurrency = Number(s.concurrency ?? 0)
    editor.checkin_time = s.checkin_time ?? ''
    editor.strict = (s.strict ?? 'default') as StrictState
    editor.effective = s.effective ?? {}
    editor.global = s.global ?? {}
  } catch (e) {
    error.value = errorMessage(e)
    editingId.value = ''
  } finally {
    settingsLoading.value = false
  }
}

function closeEditor() {
  editingId.value = ''
}

async function saveSettings() {
  const id = editor.plugin_id
  if (!id) return
  const concurrency = Number(editor.concurrency)
  if (!Number.isInteger(concurrency) || concurrency < 0 || concurrency > 32) {
    error.value = '并发数必须是 0（跟随全局）或 1–32 之间的整数。'
    return
  }
  const checkin = editor.checkin_time.trim()
  if (checkin && !/^([01][0-9]|2[0-3]):[0-5][0-9]$/.test(checkin)) {
    error.value = '签到时刻必须为 HH:MM（24 小时制），或留空表示跟随全局。'
    return
  }
  savingSettings.value = true
  error.value = ''
  message.value = ''
  try {
    await api.plugins.updateSettings(id, {
      enabled: editor.enabled,
      mode: editor.mode,
      concurrency,
      checkin_time: checkin,
      strict: editor.strict,
    })
    message.value = `已保存「${editor.name}」的插件设置。并发与严格出网即时生效；启动方式与启用开关在下次插件重启 / 宿主重启时完全生效。`
    closeEditor()
    await load()
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    savingSettings.value = false
  }
}

/** Records the file the user picked in the archive chooser. */
function onInstallFileChange(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0] ?? null
  installFile.value = file
  message.value = ''
  error.value = ''
  if (file && !file.name.toLowerCase().endsWith('.zip')) {
    error.value = '请选择 .zip 格式的插件包。'
    installFile.value = null
    input.value = ''
  }
}

async function install() {
  const file = installFile.value
  if (!file) return
  installing.value = true
  message.value = ''
  error.value = ''
  try {
    await api.plugins.install(file, installEnabled.value)
    message.value = installEnabled.value
      ? `已提交「${file.name}」，宿主正在解包并校验，安装成功后会自动启动。`
      : `已提交「${file.name}」，宿主正在解包并校验。安装后插件处于停用状态，请在上方表格中点击「启用」再「启动」。`
    installFile.value = null
    installEnabled.value = false
    const input = document.getElementById('install-file') as HTMLInputElement | null
    if (input) input.value = ''
    await load()
  } catch (e) {
    error.value = errorMessage(e)
    if ((e as { status?: number }).status === 501) {
      error.value = '宿主尚未实现插件上传安装接口（HTTP 501），当前只能手动放入 plugins 目录。'
    }
  } finally {
    installing.value = false
  }
}

onMounted(() => void load())
</script>

<template>
  <div class="page">
    <div v-if="error" class="notice error">
      <span>操作失败，请重试；下面是详细原因：{{ error }}</span>
      <button type="button" class="btn sm ghost" @click="load">重试</button>
    </div>
    <div v-if="message" class="notice ok">{{ message }}</div>

    <div class="notice warn">
      安全提示：插件以<strong>非沙箱</strong>方式运行，使用当前登录用户的权限访问本机文件、网络与凭据。
      请只安装受信任的插件——下方“权限”仅是插件自行声明，宿主并不强制隔离。
    </div>

    <section class="card">
      <div class="card-head">
        <h2>已安装插件</h2>
        <div class="row">
          <span class="badge plain muted">{{ plugins.length }} 个</span>
          <button type="button" class="btn sm" :disabled="loading" @click="load">
            {{ loading ? '刷新中…' : '刷新' }}
          </button>
        </div>
      </div>
      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>名称</th>
              <th>ID</th>
              <th>版本</th>
              <th>状态</th>
              <th>账号</th>
              <th>模型</th>
              <th style="text-align: right">操作</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="plugin in plugins" :key="plugin.manifest?.id || plugin.dir">
              <tr>
                <td class="title-cell">
                  <div class="plugin-title">
                    <span>{{ plugin.manifest?.name || plugin.manifest?.id || '未命名插件' }}</span>
                    <button
                      type="button"
                      class="meta-link"
                      :aria-expanded="metaId === (plugin.manifest?.id ?? '')"
                      title="读取插件自述（/meta）"
                      @click="toggleMeta(plugin)"
                    >
                      自述
                    </button>
                  </div>
                  <div v-if="plugin.last_error" class="small text-danger title-error">
                    {{ plugin.last_error }}
                  </div>
                </td>
                <td><code>{{ plugin.manifest?.id || '—' }}</code></td>
                <td class="num">{{ plugin.manifest?.version || '—' }}</td>
                <td>
                  <span class="badge" :class="statusOf(plugin).tone">{{ statusOf(plugin).label }}</span>
                  <div v-if="plugin.port" class="small muted mono port-line">
                    127.0.0.1:{{ plugin.port }}
                  </div>
                </td>
                <td class="num">{{ accountLabel(plugin) }}</td>
                <td class="small muted truncate" :title="modelLabel(plugin)">{{ modelLabel(plugin) }}</td>
                <td>
                  <div class="btn-row" style="justify-content: flex-end">
                    <button
                      type="button"
                      class="btn sm"
                      :disabled="busy !== '' || settingsLoading || savingSettings"
                      @click="openEditor(plugin)"
                    >
                      {{ editingId === (plugin.manifest?.id ?? '') ? '收起' : '编辑' }}
                    </button>
                    <button
                      v-if="!plugin.enabled"
                      type="button"
                      class="btn sm"
                      :disabled="busy !== ''"
                      @click="runAction(plugin, 'enable')"
                    >
                      启用
                    </button>
                    <button
                      v-else
                      type="button"
                      class="btn sm"
                      :disabled="busy !== ''"
                      @click="runAction(plugin, 'disable')"
                    >
                      停用
                    </button>
                    <button
                      type="button"
                      class="btn sm"
                      :disabled="!plugin.enabled || plugin.running || busy !== ''"
                      @click="runAction(plugin, 'start')"
                    >
                      启动
                    </button>
                    <button
                      type="button"
                      class="btn sm"
                      :disabled="!plugin.running || busy !== ''"
                      @click="runAction(plugin, 'stop')"
                    >
                      停止
                    </button>
                    <button
                      type="button"
                      class="btn sm danger"
                      :disabled="busy !== ''"
                      @click="runAction(plugin, 'uninstall')"
                    >
                      卸载
                    </button>
                  </div>
                </td>
              </tr>
              <tr v-if="metaId && metaId === (plugin.manifest?.id ?? '')">
                <td colspan="7" class="editor-cell">
                  <div class="editor meta-panel">
                    <div v-if="metaLoading" class="small muted">读取插件自述…</div>
                    <div v-else-if="metaError" class="notice warn">{{ metaError }}</div>
                    <template v-else-if="metaData">
                      <dl class="meta-grid">
                        <div>
                          <dt>自报 ID</dt>
                          <dd>{{ metaData.id || '—' }}</dd>
                        </div>
                        <div>
                          <dt>自报版本</dt>
                          <dd>{{ metaData.version || '—' }}</dd>
                        </div>
                        <div>
                          <dt>api_level</dt>
                          <dd>{{ metaData.api_level ?? '—' }}</dd>
                        </div>
                        <div>
                          <dt>账号数</dt>
                          <dd>{{ metaData.account_count ?? '—' }}</dd>
                        </div>
                        <div>
                          <dt>运行时长</dt>
                          <dd>{{ uptimeLabel(metaData.uptime_s) }}</dd>
                        </div>
                        <div>
                          <dt>启动时间</dt>
                          <dd>{{ metaData.started_at ? formatTimestamp(metaData.started_at) : '—' }}</dd>
                        </div>
                      </dl>
                      <div v-if="metaData.capabilities?.length" class="meta-caps">
                        <span class="small muted">自报能力</span>
                        <span v-for="cap in metaData.capabilities" :key="cap" class="badge plain">{{ cap }}</span>
                      </div>
                      <details class="meta-raw">
                        <summary class="small muted">原始 JSON</summary>
                        <pre>{{ JSON.stringify(metaData, null, 2) }}</pre>
                      </details>
                    </template>
                  </div>
                </td>
              </tr>
              <tr v-if="editingId === (plugin.manifest?.id ?? '')">
                <td colspan="7" class="editor-cell">
                  <div class="editor">
                    <div class="editor-grid">
                      <div class="field">
                        <span class="field-label">启用</span>
                        <label class="switch">
                          <input v-model="editor.enabled" type="checkbox" />
                          <span class="switch-track"></span>
                          {{ editor.enabled ? '已启用' : '已停用' }}
                        </label>
                        <span class="help">关闭后立即停用插件；与上方“启用/停用”按钮作用相同。</span>
                      </div>

                      <div class="field">
                        <label for="ed-mode">启动模式</label>
                        <select id="ed-mode" v-model="editor.mode" class="select">
                          <option value="">跟随全局</option>
                          <option value="always">随宿主启动 (always)</option>
                          <option value="on_demand">按需启动 (on_demand)</option>
                        </select>
                        <span class="help">当前生效：{{ editor.effective.mode || '—' }}；全局默认：{{ editor.global.startup_mode || '—' }}</span>
                      </div>

                      <div class="field">
                        <label for="ed-concurrency">并发 (1–32，0=跟随全局)</label>
                        <input
                          id="ed-concurrency"
                          v-model.number="editor.concurrency"
                          class="input"
                          type="number"
                          min="0"
                          max="32"
                        />
                        <span class="help">
                          0 表示跟随全局。当前生效：{{ editor.effective.concurrency ?? '—' }}；全局默认：{{ editor.global.concurrency ?? '—' }}。
                        </span>
                      </div>

                      <div class="field">
                        <label for="ed-checkin">签到时刻 (HH:MM，留空跟随全局)</label>
                        <input
                          id="ed-checkin"
                          v-model="editor.checkin_time"
                          class="input"
                          type="time"
                          step="60"
                        />
                        <span class="help">当前生效：{{ editor.effective.checkin_time || '—' }}；全局默认：{{ editor.global.checkin_default_time || '—' }}</span>
                      </div>

                      <div class="field">
                        <label for="ed-strict">严格出网</label>
                        <select id="ed-strict" v-model="editor.strict" class="select">
                          <option value="default">跟随全局</option>
                          <option value="on">严格（仅允许经代理出网）</option>
                          <option value="off">关闭（允许直连）</option>
                        </select>
                        <span class="help">
                          当前生效：{{ editor.effective.strict ? '严格' : '非严格' }}；全局默认：{{ editor.global.strict_egress ? '严格' : '非严格' }}。
                          保存后即时重写 egress 并重载运行中的插件。
                        </span>
                      </div>
                    </div>

                    <div class="permissions">
                      <div class="permissions-title">权限声明（只读，来自插件清单）</div>
                      <ul class="perm-list">
                        <li>
                          <span class="perm-key">出网域名 network_domains</span>
                          <span class="perm-val">
                            {{ plugin.manifest?.permissions?.network_domains?.length
                              ? plugin.manifest.permissions.network_domains.join(', ')
                              : '未声明' }}
                          </span>
                        </li>
                        <li>
                          <span class="perm-key">使用代理 use_proxy</span>
                          <span class="perm-val">{{ plugin.manifest?.permissions?.use_proxy ? '是' : '否' }}</span>
                        </li>
                        <li>
                          <span class="perm-key">改写系统身份 os_identity</span>
                          <span class="perm-val">{{ plugin.manifest?.permissions?.os_identity ? '是' : '否' }}</span>
                        </li>
                        <li v-if="plugin.manifest?.permissions?.writes_files !== undefined">
                          <span class="perm-key">写入文件 writes_files</span>
                          <span class="perm-val">{{ plugin.manifest?.permissions?.writes_files ? '是' : '否' }}</span>
                        </li>
                        <li v-if="plugin.manifest?.permissions?.require_admin">
                          <span class="perm-key">需要管理员 require_admin</span>
                          <span class="perm-val">是</span>
                        </li>
                      </ul>
                      <p class="perm-note">
                        以上为插件自报的权限，宿主不会据此隔离进程。插件以当前用户权限<strong>非沙箱</strong>运行，
                        可在本机读写文件、访问网络与凭据——请仅在信任该插件时启用它。
                      </p>
                    </div>

                    <div class="row editor-actions">
                      <button type="button" class="btn" :disabled="savingSettings" @click="closeEditor">取消</button>
                      <button type="button" class="btn primary" :disabled="savingSettings || settingsLoading" @click="saveSettings">
                        {{ savingSettings ? '保存中…' : '保存设置' }}
                      </button>
                    </div>
                  </div>
                </td>
              </tr>
            </template>
            <tr v-if="!loading && plugins.length === 0">
              <td colspan="7">
                <div class="empty">
                  <strong>还没有安装插件</strong>
                  <span>把插件目录放入 plugins\ 或在下方选择插件 zip 包上传安装。</span>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-if="loading && plugins.length === 0" class="log-skeleton" aria-hidden="true">
        <span v-for="n in 6" :key="n" class="skeleton skeleton-line"></span>
      </div>
    </section>

    <section class="card">
      <div class="card-head">
        <h2>安装插件</h2>
        <span class="sub">本地 zip 包，不做在线市场</span>
      </div>
      <div class="card-body stack install-body">
        <div class="toolbar">
          <div class="field grow">
            <label for="install-file">插件 zip 包</label>
            <input
              id="install-file"
              class="input"
              type="file"
              accept=".zip,application/zip"
              @change="onInstallFileChange"
            />
            <span class="help">
              选择本机的插件 zip 包上传。宿主只接受上传的文件，不再读取服务器端路径——
              路径安装可让任意调用方装入网关用户能读到的任何文件，UNC 路径还会外泄本机凭据。
            </span>
          </div>
          <label class="switch">
            <input v-model="installEnabled" type="checkbox" />
            <span class="switch-track"></span>
            安装后立即启用
          </label>
          <button type="button" class="btn primary" :disabled="installing || !installFile" @click="install">
            {{ installing ? '上传中…' : '上传并安装' }}
          </button>
        </div>
        <p class="small muted">
          出于安全考虑，新安装的插件默认<strong>不启用</strong>：插件以当前用户权限非沙箱运行，
          装好后需要在上方表格中手动点击「启用」（再「启动」）才会执行。
        </p>
      </div>
    </section>
  </div>
</template>

<style scoped>
.title-error {
  margin-top: var(--space-1);
}
.plugin-title {
  display: flex;
  align-items: center;
  gap: 8px;
}
.meta-link {
  padding: 0;
  border: none;
  background: none;
  color: var(--muted);
  font-size: var(--text-2xs);
  text-decoration: underline dotted;
  text-underline-offset: 2px;
  cursor: pointer;
}
.meta-link:hover,
.meta-link[aria-expanded='true'] {
  color: var(--accent-text);
}
.meta-link:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: 2px;
  border-radius: var(--radius-xs);
}
.editor-actions {
  justify-content: flex-end;
  gap: var(--space-2);
}
.install-body {
  gap: var(--space-3);
}
.editor-cell {
  background: var(--surface-2);
  padding: 0 !important;
}
.editor {
  padding: 16px;
  display: grid;
  gap: 16px;
}
.editor-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 14px 18px;
}
.meta-panel {
  gap: 12px;
}
.meta-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
  gap: 10px 18px;
  margin: 0;
}
.meta-grid div {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.meta-grid dt {
  color: var(--muted);
  font-size: var(--text-2xs);
}
.meta-grid dd {
  margin: 0;
  font-weight: 500;
}
.meta-caps {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
}
.meta-raw summary {
  cursor: pointer;
}
.meta-raw pre {
  max-height: 240px;
  margin: 8px 0 0;
  padding: 10px 12px;
  overflow: auto;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface);
  font-size: var(--text-xs);
}
.permissions {
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  padding: 12px 14px;
  background: var(--surface);
}
.permissions-title {
  font-weight: 600;
  margin-bottom: 8px;
}
.perm-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: 6px 18px;
}
.perm-list li {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  font-size: var(--text-sm);
}
.perm-key {
  color: var(--muted);
}
.perm-val {
  font-weight: 600;
  text-align: right;
  word-break: break-all;
}
.perm-note {
  margin: 10px 0 0;
  font-size: var(--text-xs);
  color: var(--muted);
}
.port-line {
  margin-top: var(--space-1);
}
</style>
