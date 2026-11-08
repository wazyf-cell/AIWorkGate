<script setup lang="ts">
// task-58：路由监控面板（A 档，零后端改造）。
//
// 数据源（复用现有 api.usage.*，不碰 api.ts）：
//   sessions({page:1, page_size:20}) → 最近 20 条请求流水；
//   summary({})                      → 区间级 cache_hit_rate 摘要卡；
//   credits({})                      → 账号池 credit 卡（按 plugin 分组）。
// task-60：UsageSession 现带单条 cached_tokens——流水加「缓存」列
// （cached/prompt 百分比）+ 行点击面板内展开该请求详情（不跳页）。
// 与 RouteStage 的区别：那是逐请求实时观察流（trace），本组件是服务端聚合
// 统计的紧凑视图，页面可见时 20s 轮询一次。
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import {
  api,
  errorMessage,
  type UsageAccountCredit,
  type UsageSession,
  type UsageSummary,
} from '../api'
import { formatDate, formatLatency, formatTimeOnly, formatTokens } from '../format'

const REFRESH_MS = 20_000
const ROWS = 20

const loading = ref(true)
const error = ref('')
const sessions = ref<UsageSession[]>([])
const summary = ref<UsageSummary | null>(null)
const credits = ref<UsageAccountCredit[]>([])
const expandedReqId = ref('')

/** task-60：行点击下钻——同一行再点收回（面板内展开，不跳页）。 */
function toggleRow(row: UsageSession) {
  expandedReqId.value = expandedReqId.value === (row.req_id ?? '') ? '' : (row.req_id ?? '')
}

/** 单条缓存命中率：cached/prompt 百分比；prompt=0 或 cached=0 显示「—」。 */
function cachePercent(row: UsageSession): string {
  const cached = row.cached_tokens ?? 0
  const prompt = row.prompt_tokens ?? 0
  if (prompt <= 0 || cached <= 0) return '—'
  return `${((cached / prompt) * 100).toFixed(0)}%`
}

/* 时间/耗时/token/过期统一走 ../format 标准函数（task-58 契约第 8 条）：
 * formatTimeOnly（HH:mm:ss）/ formatLatency（ms→s 自适应）/
 * formatTokens（p / c）/ formatDate（expires_at 双形态兼容）。 */

function usedPercent(account: UsageAccountCredit): string {
  const { remaining, total } = account
  if (!total || total <= 0 || remaining === undefined) return '—'
  const used = Math.max(0, Math.min(100, ((total - remaining) / total) * 100))
  return `${used.toFixed(0)}%`
}

function runwayText(account: UsageAccountCredit): string {
  // 硬约束 4：余额充足历史时 runway_hours 缺省——判空显示「—」，别当必有。
  if (account.runway_hours === undefined) return '—'
  return `${account.runway_hours.toFixed(1)} h`
}

function statusBadge(row: UsageSession): { text: string; cls: string } {
  const status = row.status ?? 0
  if (status === 0) return { text: '不可达', cls: 'warn' }
  if (status >= 500) return { text: `HTTP ${status}`, cls: 'warn' }
  if (status >= 400) return { text: `HTTP ${status}`, cls: 'muted' }
  return { text: `HTTP ${status}`, cls: 'ok' }
}

/** 账号池按 plugin 分组（保序：credits 返回顺序即展示顺序）。 */
const creditGroups = computed<{ plugin: string; rows: UsageAccountCredit[] }[]>(() => {
  const groups = new Map<string, UsageAccountCredit[]>()
  for (const row of credits.value) {
    const key = row.plugin_id?.trim() || '其它'
    const list = groups.get(key)
    if (list) list.push(row)
    else groups.set(key, [row])
  }
  return [...groups.entries()].map(([plugin, rows]) => ({ plugin, rows }))
})

const cacheHitRate = computed<string>(() => {
  const rate = summary.value?.kpis?.cache_hit_rate
  if (rate === undefined || rate === null) return '—'
  return `${(rate * 100).toFixed(1)}%`
})

const latest = computed<UsageSession | null>(() => sessions.value[0] ?? null)

async function refresh() {
  try {
    const [sessionPage, summaryData, creditData] = await Promise.all([
      api.usage.sessions({ page: 1, page_size: ROWS }),
      api.usage.summary({}),
      api.usage.credits({}),
    ])
    sessions.value = sessionPage.items ?? []
    summary.value = summaryData ?? null
    credits.value = creditData.accounts ?? []
    error.value = ''
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    loading.value = false
  }
}

/* 轮询：页面可见时每 20s 刷新；隐藏暂停；卸载清理。 */
let timer = 0

function startPolling() {
  if (timer || document.hidden) return
  timer = window.setInterval(() => {
    if (!document.hidden) void refresh()
  }, REFRESH_MS)
}

function stopPolling() {
  if (timer) {
    clearInterval(timer)
    timer = 0
  }
}

function onVisibility() {
  if (document.hidden) stopPolling()
  else {
    void refresh()
    startPolling()
  }
}

onMounted(async () => {
  document.addEventListener('visibilitychange', onVisibility)
  await refresh()
  startPolling()
})

onBeforeUnmount(() => {
  document.removeEventListener('visibilitychange', onVisibility)
  stopPolling()
})
</script>

<template>
  <section class="card rs-monitor">
    <div class="card-head">
      <h2>
        路由监控
        <span class="badge plain muted">服务端聚合 · 每 20s</span>
      </h2>
      <span class="sub">与上方实时舞台不同：这里是 usage 统计的紧凑视图（最近路由摘要 / 请求流水 / 账号池），非逐请求观察流。</span>
    </div>

    <div v-if="error" class="card-body">
      <div class="notice error">
        <span>usage 统计读取失败：{{ error }}</span>
        <button type="button" class="btn sm ghost" @click="refresh">重试</button>
      </div>
    </div>

    <div v-else-if="loading" class="card-body">
      <span class="muted">加载 usage 统计中…</span>
    </div>

    <template v-else>
      <!-- ① 最近一次路由摘要 -->
      <div class="card-body stack">
        <div v-if="latest" class="rs-monitor-latest">
          <div class="rs-monitor-kv">
            <span class="muted">模型</span>
            <span class="mono">{{ latest.logical_model || latest.model || '—' }}</span>
          </div>
          <div class="rs-monitor-kv">
            <span class="muted">账号</span>
            <span class="mono">{{ latest.account_name || latest.account_id || '—' }}</span>
          </div>
          <div class="rs-monitor-kv">
            <span class="muted">耗时</span>
            <span>{{ formatLatency(latest.latency_ms) }}</span>
          </div>
          <div class="rs-monitor-kv">
            <span class="muted">首字</span>
            <span>{{ formatLatency(latest.ttfb_ms) }}</span>
          </div>
          <div class="rs-monitor-kv">
            <span class="muted">token</span>
            <span>{{ formatTokens(latest.prompt_tokens, latest.completion_tokens) }}</span>
          </div>
          <div class="rs-monitor-kv">
            <span class="muted">状态</span>
            <span class="badge" :class="statusBadge(latest).cls">{{ statusBadge(latest).text }}</span>
          </div>
          <div class="rs-monitor-kv">
            <span class="muted" title="区间级缓存命中率（summary.cache_hit_rate = cached/prompt）；单条命中率看流水「缓存」列">缓存命中（区间）</span>
            <span>{{ cacheHitRate }}</span>
          </div>
        </div>
        <div v-else class="empty">
          <strong>还没有请求记录</strong>
          <span>发一条真实请求后，这里会显示最近一次路由的摘要。</span>
        </div>
      </div>

      <!-- ② 请求流水 + ③ 账号池：左右两列。 -->
      <div class="rs-monitor-columns card-body">
        <div class="rs-monitor-flow">
          <div class="row between">
            <span class="field-label">请求流水（最近 {{ sessions.length }} 条）</span>
          </div>
          <div v-if="sessions.length === 0" class="empty">
            <span>暂无请求流水。</span>
          </div>
          <div v-else class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  <th>时间</th>
                  <th>路由组</th>
                  <th>账号</th>
                  <th>耗时</th>
                  <th>首字</th>
                  <th>token</th>
                  <th>缓存</th>
                  <th>状态</th>
                </tr>
              </thead>
              <tbody>
                <template v-for="row in sessions" :key="`${row.ts ?? ''}-${row.req_id ?? ''}`">
                  <tr
                    :class="{ 'rs-monitor-selected': expandedReqId !== '' && row.req_id === expandedReqId }"
                    @click="toggleRow(row)"
                  >
                    <td class="mono small">{{ formatTimeOnly(row.ts) }}</td>
                    <td class="mono small" :title="row.logical_model || ''">{{ row.logical_model || row.model || '—' }}</td>
                    <td class="small" :title="row.account_name || ''">{{ row.account_name || row.account_id || '—' }}</td>
                    <td class="small">{{ formatLatency(row.latency_ms) }}</td>
                    <td class="small">{{ formatLatency(row.ttfb_ms) }}</td>
                    <td class="small">{{ formatTokens(row.prompt_tokens, row.completion_tokens) }}</td>
                    <td class="small">{{ cachePercent(row) }}</td>
                    <td>
                      <span class="badge" :class="statusBadge(row).cls">{{ statusBadge(row).text }}</span>
                    </td>
                  </tr>
                  <tr v-if="expandedReqId !== '' && row.req_id === expandedReqId">
                    <td colspan="8">
                      <div class="rs-monitor-detail">
                        <span><span class="muted">req_id：</span><span class="mono">{{ row.req_id || '—' }}</span></span>
                        <span><span class="muted">时间：</span>{{ formatTimeOnly(row.ts) }}</span>
                        <span><span class="muted">模型：</span><span class="mono">{{ row.logical_model || row.model || '—' }}</span></span>
                        <span><span class="muted">账号：</span>{{ row.account_name || row.account_id || '—' }}</span>
                        <span><span class="muted">状态：</span>{{ statusBadge(row).text }}</span>
                        <span><span class="muted">耗时：</span>{{ formatLatency(row.latency_ms) }}</span>
                        <span><span class="muted">首字：</span>{{ formatLatency(row.ttfb_ms) }}</span>
                        <span>
                          <span class="muted">token：</span>{{ formatTokens(row.prompt_tokens, row.completion_tokens) }}
                        </span>
                        <span>
                          <span class="muted">缓存：</span>{{ cachePercent(row) }}
                          <span v-if="cachePercent(row) !== '—'" class="muted">
                            （{{ row.cached_tokens }} / {{ row.prompt_tokens }} tokens）
                          </span>
                        </span>
                      </div>
                    </td>
                  </tr>
                </template>
              </tbody>
            </table>
          </div>
        </div>

        <div class="rs-monitor-pool">
          <div class="row between">
            <span class="field-label">账号池</span>
          </div>
          <div v-if="creditGroups.length === 0" class="empty">
            <span>暂无账号额度数据。</span>
          </div>
          <div v-for="group in creditGroups" :key="group.plugin" class="stack">
            <span class="small muted mono">{{ group.plugin }}</span>
            <div v-for="row in group.rows" :key="`${row.plugin_id}-${row.account_id}`" class="rs-monitor-credit">
              <span class="rs-monitor-credit-name" :title="row.display_name || row.account_id || ''">
                {{ row.display_name || row.account_id || '—' }}
              </span>
              <span class="small muted">
                {{ row.remaining !== undefined ? row.remaining : '—' }} / {{ row.total !== undefined ? row.total : '—' }}
                · 已用 {{ usedPercent(row) }} · 可用 {{ runwayText(row) }} · 至 {{ formatDate(row.expires_at) }}
              </span>
            </div>
          </div>
        </div>
      </div>
    </template>
  </section>
</template>

<style scoped>
.rs-monitor-latest {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(9em, 1fr));
  gap: 8px 16px;
}

.rs-monitor-kv {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.rs-monitor-columns {
  display: grid;
  grid-template-columns: minmax(0, 3fr) minmax(0, 2fr);
  gap: 16px;
  align-items: start;
}

.rs-monitor-flow,
.rs-monitor-pool {
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-width: 0;
}

.rs-monitor-credit {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 8px 10px;
  background: var(--surface-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
}

.rs-monitor-credit-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.rs-monitor-selected {
  background: var(--accent-soft);
}

/* task-60：请求流水行可点击下钻 + 展开详情行 */
.rs-monitor-flow tbody tr {
  cursor: pointer;
}

.rs-monitor-detail {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 18px;
  padding: 6px 10px;
  background: var(--surface-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font-size: var(--text-sm, 13px);
}

@media (max-width: 900px) {
  .rs-monitor-columns {
    grid-template-columns: 1fr;
  }
}
</style>
