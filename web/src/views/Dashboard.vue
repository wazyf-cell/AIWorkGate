<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api, errorMessage, type HostStatus } from '../api'
import { formatLatency, formatTimeOnly, formatTimestamp } from '../format'

// The host status payload extended with the three-state A health fields.
type AState = 'ok' | 'unauthorized' | 'down'
interface StatusInfo extends HostStatus {
  a_state?: AState
  a_detail?: string
}

const status = ref<StatusInfo | null>(null)
const error = ref('')
const loading = ref(false)
const updatedAt = ref<Date | null>(null)
let timer: number | undefined

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    status.value = await api.status()
    error.value = ''
    updatedAt.value = new Date()
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void load()
  // The probe history is read once on mount; it never joins the 5s poll.
  void loadAProbeHistory()
  timer = window.setInterval(() => void load(true), 5000)
})

onBeforeUnmount(() => {
  if (timer !== undefined) window.clearInterval(timer)
})

const aState = computed<AState>(() => {
  const s = status.value
  if (!s) return 'down'
  if (s.a_state === 'ok' || s.a_state === 'unauthorized' || s.a_state === 'down') {
    return s.a_state
  }
  return s.a_running ? 'ok' : 'down'
})

const aTone = computed(() => {
  if (!status.value) return 'muted'
  if (aState.value === 'ok') return 'ok'
  return aState.value === 'unauthorized' ? 'warn' : 'danger'
})

const aLabel = computed(() => {
  if (!status.value) return '未知'
  if (aState.value === 'ok') return 'A 正常'
  if (aState.value === 'unauthorized') return 'A 需要 API Key'
  return 'A 未运行'
})

// Raw transport failures from the health probe (dial/connect/timeout) are not
// user-facing text. They are replaced by a short Chinese hint; the raw string
// survives only as the title of the small "详情" affordance.
const TRANSPORT_ERROR =
  /dial tcp|connectex|actively refused|connection refused|i\/o timeout|no such host|network is unreachable|connection reset|context deadline exceeded/i

const aDetail = computed(() => {
  const detail = status.value?.a_detail?.trim() ?? ''
  if (!detail) return ''
  // A down probe always carries host-side diagnostics; never show them raw.
  if (aState.value === 'down') return 'A 未运行或端口不可达'
  return TRANSPORT_ERROR.test(detail) ? 'A 未运行或端口不可达' : detail
})

const aRawDetail = computed(() => {
  const detail = status.value?.a_detail?.trim() ?? ''
  return detail && detail !== aDetail.value ? detail : ''
})

/* ------------------------------------------------------------------ */
/* Explicit A connectivity probe (POST /api/diagnostics/probe)         */
/* ------------------------------------------------------------------ */

/** One probe outcome; failures keep the host's redacted detail. */
interface AProbe {
  ok: boolean
  status: number
  latency_ms: number
  detail: string
  error_code?: string
  at: number
}

const aProbe = ref<AProbe | null>(null)
const probingA = ref(false)
const aProbedAt = ref(0)

const aProbeLabel = computed(() => {
  const probe = aProbe.value
  if (!probe) return ''
  if (probe.ok) return `连通 · ${formatLatency(probe.latency_ms)}`
  return probe.detail || '检测失败'
})

const aProbeTitle = computed(() => {
  const probe = aProbe.value
  if (!probe) return ''
  const parts = [probe.ok ? '连通正常' : probe.detail || '检测失败']
  if (probe.status) parts.push(`HTTP ${probe.status}`)
  if (probe.error_code) parts.push(probe.error_code)
  parts.push(formatTimestamp(probe.at))
  return parts.join(' · ')
})

const aProbedAtLabel = computed(() => (aProbedAt.value > 0 ? formatTimestamp(aProbedAt.value) : ''))

async function probeA() {
  probingA.value = true
  try {
    const result = await api.diagnostics.probe('a')
    aProbe.value = {
      ok: result.ok,
      status: result.status,
      latency_ms: result.latency_ms,
      detail: result.detail,
      error_code: result.error_code,
      at: result.at,
    }
    aProbedAt.value = result.at
  } catch (e) {
    const at = Math.floor(Date.now() / 1000)
    aProbe.value = { ok: false, status: 0, latency_ms: 0, detail: errorMessage(e), at }
    aProbedAt.value = at
  } finally {
    probingA.value = false
  }
}

/** Newest `a` probe from history, read once on mount. */
async function loadAProbeHistory() {
  try {
    const list = await api.diagnostics.probes({ limit: 20, kind: 'a' })
    const latest = list.find((probe) => probe.kind === 'a')
    if (latest) aProbedAt.value = latest.at
  } catch {
    // Optional: an older host without diagnostics simply shows no timestamp.
  }
}

const pluginLabel = computed(() => {
  if (!status.value) return '—'
  return `${status.value.plugins_running} / ${status.value.plugins_total}`
})

const updatedLabel = computed(() => (updatedAt.value ? formatTimeOnly(updatedAt.value) : '—'))

const egressFailed = computed(() => status.value?.egress_failed ?? 0)
</script>

<template>
  <div class="page">
    <div v-if="error" class="notice error">
      无法读取宿主状态：{{ error }}
      <button type="button" class="btn sm ghost retry-btn" @click="load()">重试</button>
    </div>

    <div v-if="egressFailed > 0" class="notice warn notice-row" role="status">
      <span>
        <strong>{{ egressFailed }} 个账号的代理出口最近失败</strong>：这些账号会先让位给其他账号，全部不可用时仍会尝试。
        请到账号页或代理页检查隧道。
      </span>
      <RouterLink to="/accounts" class="btn sm ghost">查看账号</RouterLink>
    </div>

    <div class="stat-grid">
      <section class="stat">
        <span class="stat-label">宿主版本</span>
        <span class="stat-value">{{ status?.version || '—' }}</span>
        <span class="stat-sub">AIWorkGate 单 exe 网关</span>
        <div v-if="!status" class="empty">
          <strong>暂无宿主版本</strong>
          <span>{{ error ? '宿主状态读取失败' : '宿主状态尚未读取到' }}</span>
          <button type="button" class="btn sm ghost" :disabled="loading" @click="load()">重新读取</button>
        </div>
      </section>

      <section class="stat">
        <span class="stat-label">监听端口</span>
        <span class="stat-value">{{ status?.port ?? '—' }}</span>
        <span class="stat-sub mono">127.0.0.1:{{ status?.port ?? '—' }}</span>
        <div v-if="!status" class="empty">
          <strong>暂无监听端口</strong>
          <span>{{ error ? '宿主状态读取失败' : '宿主状态尚未读取到' }}</span>
          <RouterLink to="/settings" class="btn sm ghost">检查端口设置</RouterLink>
        </div>
      </section>

      <section class="stat" :title="aDetail || undefined">
        <span class="stat-label">A 网关</span>
        <span class="stat-value small-value">
          <span class="badge" :class="aTone">{{ aLabel }}</span>
        </span>
        <span class="stat-sub">
          <span>{{ status?.a_configured ? 'Key 已配置' : '尚未填写 A Key' }}</span>
          <span v-if="status?.a_port">· 端口 {{ status.a_port }}</span>
        </span>
        <div class="probe-row">
          <button type="button" class="btn sm" :disabled="probingA" @click="probeA">
            {{ probingA ? '检测中…' : '检测' }}
          </button>
          <span
            v-if="aProbe"
            class="probe-inline"
            :class="aProbe.ok ? 'ok' : 'danger'"
            :title="aProbeTitle"
          >
            {{ aProbeLabel }}
          </span>
        </div>
        <span v-if="aProbedAtLabel" class="stat-sub">上次检测 {{ aProbedAtLabel }}</span>
        <div v-if="status && !status.a_configured" class="empty">
          <strong>尚未配置 A Key</strong>
          <span>A 网关需要 API Key 才能正常访问。</span>
          <RouterLink to="/settings" class="btn sm ghost">去设置填写</RouterLink>
        </div>
      </section>

      <section class="stat">
        <span class="stat-label">插件运行</span>
        <span class="stat-value">{{ pluginLabel }}</span>
        <span class="stat-sub">运行中 / 已安装</span>
        <div v-if="!status" class="empty">
          <strong>暂无插件数据</strong>
          <span>{{ error ? '宿主状态读取失败' : '宿主状态尚未读取到' }}</span>
          <button type="button" class="btn sm ghost" :disabled="loading" @click="load()">重新读取</button>
        </div>
        <div v-if="status && status.plugins_total === 0" class="empty">
          <strong>没有已安装的插件</strong>
          <span>宿主已运行，但插件列表为空。</span>
          <RouterLink to="/plugins" class="btn sm ghost">去插件管理安装</RouterLink>
        </div>
      </section>
    </div>

    <section class="card">
      <div class="card-head">
        <h2>宿主信息</h2>
        <span class="sub">每 5 秒自动刷新 · 最近 {{ updatedLabel }}</span>
      </div>
      <div class="card-body">
        <dl class="kv host-kv" style="grid-template-columns: 110px minmax(0, 1fr)">
          <dt>数据目录</dt>
          <dd class="mono">{{ status?.data_dir || '—' }}</dd>
          <dt>A 接入</dt>
          <dd>
            {{ aLabel }}<span v-if="status?.a_port">，端口 {{ status.a_port }}</span>
            <template v-if="status && aState !== 'ok' && aDetail">
              <span class="sub">
                （{{ aDetail }}<template v-if="aRawDetail">，<span
                  class="small muted"
                  :title="aRawDetail"
                  aria-label="A 健康详情（开发者）"
                >详情</span></template>）
              </span>
            </template>
          </dd>
          <dt>插件</dt>
          <dd>{{ status?.plugins_total ?? 0 }} 个已安装，{{ status?.plugins_running ?? 0 }} 个运行中</dd>
        </dl>
        <div v-if="loading" class="log-skeleton" aria-hidden="true">
          <span v-for="n in 6" :key="n" class="skeleton skeleton-line"></span>
        </div>
      </div>
    </section>

    <div class="quick-grid">
      <RouterLink to="/plugins" class="quick-link">
        <strong>插件管理</strong>
        <span>安装、启用、停止或卸载插件</span>
      </RouterLink>
      <RouterLink to="/accounts" class="quick-link">
        <strong>账号与额度</strong>
        <span>查看额度、设备身份与出口代理</span>
      </RouterLink>
      <RouterLink to="/checkin" class="quick-link">
        <strong>签到日历</strong>
        <span>按月查看签到结果并一键全签</span>
      </RouterLink>
      <RouterLink to="/logs" class="quick-link">
        <strong>日志</strong>
        <span>API 请求日志与宿主/插件运行日志</span>
      </RouterLink>
    </div>
  </div>
</template>

<style scoped>
/* Host info list: the label column width stays data-independent, only the
   type scale is tokenised here (was an inline 13px). */
.host-kv {
  font-size: var(--text-sm);
}

.probe-row {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

.probe-inline {
  min-width: 0;
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
.retry-btn {
  margin-left: var(--space-2);
}
</style>
