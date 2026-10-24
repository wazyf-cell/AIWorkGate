<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { api, errorMessage, type Proxy, type ProxyCheckResult } from '../api'
import { formatTimestamp } from '../format'

interface ProxyForm {
  id: string
  name: string
  type: 'http' | 'socks5'
  host: string
  port: number
  username: string
  password: string
  enabled: boolean
}

const proxies = ref<Proxy[]>([])
const loading = ref(false)
const error = ref('')
const message = ref('')
const saving = ref(false)
const showForm = ref(false)
const testing = reactive<Record<string, boolean>>({})
const checkResults = reactive<Record<string, ProxyCheckResult | null>>({})

const form = ref<ProxyForm>(emptyForm())

function emptyForm(): ProxyForm {
  return { id: '', name: '', type: 'http', host: '', port: 1080, username: '', password: '', enabled: true }
}

async function load() {
  loading.value = true
  try {
    proxies.value = (await api.proxies.list()) ?? []
    error.value = ''
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    loading.value = false
  }
}

onMounted(() => void load())

function startAdd() {
  form.value = emptyForm()
  showForm.value = true
  message.value = ''
  error.value = ''
}

function startEdit(proxy: Proxy) {
  form.value = {
    id: proxy.id,
    name: proxy.name,
    type: proxy.type === 'socks5' ? 'socks5' : 'http',
    host: proxy.host,
    port: proxy.port,
    username: proxy.username ?? '',
    password: '',
    enabled: proxy.enabled,
  }
  showForm.value = true
  message.value = ''
  error.value = ''
}

function cancel() {
  showForm.value = false
  form.value = emptyForm()
}

async function save() {
  const current = form.value
  const name = current.name.trim()
  const host = current.host.trim()
  const port = Number(current.port)
  if (!name) {
    error.value = '请填写代理名称。'
    return
  }
  if (!host) {
    error.value = '请填写代理主机地址。'
    return
  }
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    error.value = '端口必须是 1–65535 之间的整数。'
    return
  }
  saving.value = true
  error.value = ''
  message.value = ''
  try {
    await api.proxies.save({
      id: current.id || undefined,
      name,
      type: current.type,
      host,
      port,
      username: current.username.trim() || undefined,
      password: current.password || undefined,
      enabled: current.enabled,
    })
    message.value = current.id ? '代理已更新。' : '代理已添加。'
    showForm.value = false
    form.value = emptyForm()
    await load()
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    saving.value = false
  }
}

async function remove(proxy: Proxy) {
  if (!window.confirm(`确认删除代理「${proxy.name}」？使用它的账号将失去出口。`)) return
  error.value = ''
  message.value = ''
  try {
    await api.proxies.remove(proxy.id)
    message.value = '代理已删除。'
    await load()
  } catch (e) {
    error.value = errorMessage(e)
  }
}

async function check(proxy: Proxy) {
  testing[proxy.id] = true
  error.value = ''
  try {
    const result = await api.proxies.check(proxy.id)
    checkResults[proxy.id] = result ?? null
    message.value = result && result.error
      ? `检测失败：${result.error}`
      : `已检测「${proxy.name}」出口。`
    await load()
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    testing[proxy.id] = false
  }
}

function checkLine(proxy: Proxy): string {
  const result = checkResults[proxy.id]
  if (result) {
    if (result.error) return `失败：${result.error}`
    const parts = []
    if (result.exit_ip) parts.push(`出口 ${result.exit_ip}`)
    if (result.latency_ms) parts.push(`${result.latency_ms} ms`)
    return parts.length > 0 ? parts.join(' · ') : '检测完成'
  }
  if (proxy.last_error) return `失败：${proxy.last_error}`
  return '—'
}
</script>

<template>
  <div class="page">
    <div v-if="error" class="notice error">
      <span>操作失败（保存、删除或连通性检测未完成），请重试；下面是详细原因：{{ error }}</span>
      <button type="button" class="btn sm ghost" @click="load">重试</button>
    </div>
    <div v-if="message" class="notice ok">{{ message }}</div>

    <div class="row between">
      <p class="small muted proxy-intro">
        每个账号固定绑定一个代理，代理失败即请求失败，绝不回退直连；插件只拿到宿主转发器的本地端口，不会拿到代理账密。
        「系统代理（跟随 Windows）」由宿主读取 Windows 的代理设置自动维护，在 Windows 里改地址或开关即可，这里不能编辑。
      </p>
      <div class="row">
        <button type="button" class="btn" :disabled="loading" @click="load">
          {{ loading ? '刷新中…' : '刷新' }}
        </button>
        <button type="button" class="btn primary" @click="startAdd">添加代理</button>
      </div>
    </div>

    <section v-if="showForm" class="card">
      <div class="card-head">
        <h2>{{ form.id ? '编辑代理' : '添加代理' }}</h2>
        <span class="sub">账密由宿主加密保存，不下发插件</span>
      </div>
      <div class="card-body stack">
        <div class="form-grid">
          <div class="field">
            <label for="proxy-name">名称</label>
            <input id="proxy-name" v-model="form.name" class="input" type="text" placeholder="例如：香港-01" />
          </div>
          <div class="field">
            <label for="proxy-type">类型</label>
            <select id="proxy-type" v-model="form.type" class="select">
              <option value="http">HTTP CONNECT</option>
              <option value="socks5">SOCKS5</option>
            </select>
          </div>
          <div class="field">
            <label for="proxy-host">主机</label>
            <input id="proxy-host" v-model="form.host" class="input mono" type="text" placeholder="1.2.3.4" />
          </div>
          <div class="field">
            <label for="proxy-port">端口</label>
            <input id="proxy-port" v-model.number="form.port" class="input" type="number" min="1" max="65535" />
          </div>
          <div class="field">
            <label for="proxy-username">用户名（可选）</label>
            <input id="proxy-username" v-model="form.username" class="input" type="text" autocomplete="off" />
          </div>
          <div class="field">
            <label for="proxy-password">密码（可选）</label>
            <input
              id="proxy-password"
              v-model="form.password"
              class="input"
              type="password"
              autocomplete="new-password"
              :placeholder="form.id ? '留空保持不变' : ''"
            />
          </div>
          <div class="field">
            <span class="field-label">启用</span>
            <label class="switch">
              <input v-model="form.enabled" type="checkbox" />
              <span class="switch-track"></span>
              {{ form.enabled ? '已启用' : '已停用' }}
            </label>
            <span class="help">停用后，在严格模式下分配给它的账号会变为 blocked。</span>
          </div>
        </div>
        <div class="row">
          <button type="button" class="btn primary" :disabled="saving" @click="save">
            {{ saving ? '保存中…' : '保存' }}
          </button>
          <button type="button" class="btn" :disabled="saving" @click="cancel">取消</button>
        </div>
      </div>
    </section>

    <section class="card">
      <div class="card-head">
        <h2>代理列表</h2>
        <span class="badge plain muted">{{ proxies.length }} 个</span>
      </div>
      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>名称</th>
              <th>类型</th>
              <th>地址</th>
              <th>状态</th>
              <th>最近出口</th>
              <th>延迟</th>
              <th>最近检测</th>
              <th class="proxy-actions" style="text-align: right">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="proxy in proxies" :key="proxy.id">
              <td class="title-cell">
                {{ proxy.name }}
                <span v-if="proxy.managed" class="badge plain muted">跟随系统</span>
                <div v-if="proxy.managed" class="small muted">由 Windows 设置自动同步</div>
                <div v-else class="small muted mono">{{ proxy.id }}</div>
                <div v-if="proxy.managed_detail" class="proxy-note">{{ proxy.managed_detail }}</div>
              </td>
              <td class="small">{{ proxy.host ? proxy.type.toUpperCase() : '—' }}</td>
              <td class="mono small">{{ proxy.host ? `${proxy.host}:${proxy.port}` : '—' }}</td>
              <td>
                <span class="badge" :class="proxy.enabled ? 'ok' : 'muted'">
                  {{ proxy.enabled ? '启用' : '停用' }}
                </span>
              </td>
              <td class="small">
                <span v-if="proxy.last_exit_ip" class="mono">{{ proxy.last_exit_ip }}</span>
                <span v-else class="muted">—</span>
                <div v-if="checkLine(proxy) !== '—'" class="small" :style="{ color: (checkResults[proxy.id]?.error || proxy.last_error) ? 'var(--danger)' : 'var(--muted)' }">
                  {{ checkLine(proxy) }}
                </div>
              </td>
              <td class="num">{{ proxy.last_latency_ms ? `${proxy.last_latency_ms} ms` : '—' }}</td>
              <td class="small muted">{{ proxy.last_check_at ? formatTimestamp(proxy.last_check_at) : '尚未检测' }}</td>
              <td class="proxy-actions">
                <div class="btn-row" style="justify-content: flex-end">
                  <button
                    type="button"
                    class="btn sm"
                    :disabled="testing[proxy.id] || !proxy.host"
                    :title="proxy.host ? '' : '还没有可测试的地址'"
                    @click="check(proxy)"
                  >
                    {{ testing[proxy.id] ? '检测中…' : '测试' }}
                  </button>
                  <template v-if="!proxy.managed">
                    <button type="button" class="btn sm" @click="startEdit(proxy)">编辑</button>
                    <button type="button" class="btn sm danger" @click="remove(proxy)">删除</button>
                  </template>
                </div>
              </td>
            </tr>
            <tr v-if="!loading && proxies.length === 0">
              <td colspan="8">
                <div class="empty">
                  <strong>还没有配置代理</strong>
                  <span>添加代理后，可在账号页把账号固定绑定到某个出口。</span>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
  </div>
</template>
