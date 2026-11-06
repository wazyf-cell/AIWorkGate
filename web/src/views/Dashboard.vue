<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api, errorMessage, type HostStatus } from '../api'
import { formatTimeOnly } from '../format'

const status = ref<HostStatus | null>(null)
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
  timer = window.setInterval(() => void load(true), 5000)
})

onBeforeUnmount(() => {
  if (timer !== undefined) window.clearInterval(timer)
})

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

.retry-btn {
  margin-left: var(--space-2);
}
</style>
