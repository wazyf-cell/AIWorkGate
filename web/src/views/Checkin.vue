<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import {
  api,
  errorMessage,
  normalizeCheckinAll,
  normalizeCheckins,
  type CheckinAllItem,
  type CheckinRecord,
  type PluginStatus,
} from '../api'
import { formatNumber, monthBounds, monthLabel, pad, weekdayLabel } from '../format'

function supportsCheckin(plugin: PluginStatus | undefined): boolean {
  return Boolean(plugin?.manifest?.capabilities?.includes('checkin'))
}

const ALL_PLUGINS = '__all__'
/**
 * Every installed plugin. The dropdown and the calendar use `plugins` instead:
 * a plugin whose manifest does not declare `checkin` never reports a calendar,
 * so offering it produced a permanently empty grid with no explanation.
 */
const allPlugins = ref<PluginStatus[]>([])
const plugins = computed(() => allPlugins.value.filter((plugin) => supportsCheckin(plugin)))
const pluginId = ref('')

/**
 * A selection can name an installed plugin that does not declare `checkin`
 * (an old link, or a plugin that dropped the capability). Its calendar is
 * empty by design, so the view says so in one line instead of showing a blank
 * month that reads like "no check-ins yet".
 */
const selectedPlugin = computed(() =>
  allPlugins.value.find((plugin) => plugin.manifest?.id === pluginId.value),
)
const selectedUnsupported = computed(
  () => Boolean(selectedPlugin.value) && !supportsCheckin(selectedPlugin.value),
)
const month = ref(new Date(new Date().getFullYear(), new Date().getMonth(), 1))
const records = ref<CheckinRecord[]>([])
const accountRefs = ref<{ id: string; label: string; pluginId: string }[]>([])
const loading = ref(false)
const error = ref('')
const message = ref('')
const checkinRunning = ref(false)
const allResults = ref<CheckinAllItem[]>([])
const allError = ref('')

const bounds = computed(() => monthBounds(month.value))
const days = computed(() => Array.from({ length: bounds.value.days }, (_, index) => index + 1))

function rowKey(pluginId: string, accountId: string): string {
  return `${pluginId}\u0000${accountId}`
}

function pluginName(id: string): string {
  const plugin = allPlugins.value.find((item) => item.manifest?.id === id)
  return plugin?.manifest?.name || id
}

const recordMap = computed(() => {
  const map = new Map<string, CheckinRecord>()
  for (const record of records.value) {
    const date = (record.date || '').slice(0, 10)
    if (!date) continue
    map.set(`${rowKey(record.plugin_id || '', record.account_id)}|${date}`, record)
  }
  return map
})

const rows = computed(() => {
  const seen = new Map<string, { key: string; id: string; label: string; plugin: string; pluginName: string }>()
  for (const refItem of accountRefs.value) {
    const key = rowKey(refItem.pluginId, refItem.id)
    seen.set(key, { key, id: refItem.id, label: refItem.label, plugin: refItem.pluginId, pluginName: pluginName(refItem.pluginId) })
  }
  for (const record of records.value) {
    const plugin = record.plugin_id || ''
    const key = rowKey(plugin, record.account_id)
    if (!seen.has(key)) {
      seen.set(key, {
        key,
        id: record.account_id,
        label: record.account_label || record.account_id,
        plugin,
        pluginName: pluginName(plugin),
      })
    }
  }
  return Array.from(seen.values())
})

const summary = computed(() => {
  let ok = 0
  let already = 0
  let fail = 0
  for (const record of records.value) {
    if (record.result === 'ok') ok += 1
    else if (record.result === 'already') already += 1
    else if (record.result === 'fail') fail += 1
  }
  return { ok, already, fail }
})

/**
 * Counting has not started until a month has actually come back, so the three
 * counters must not render as 0 — a zero reads as "no failures" when it really
 * means "no data yet". The arithmetic above stays untouched; only the display is
 * gated on having fetched something.
 */
const recordsLoaded = ref(false)

function statText(value: number): string {
  return recordsLoaded.value && !loading.value ? String(value) : '—'
}

/**
 * Row count for the loading skeleton. `records` is not cleared while a month is
 * being fetched, so the previous render is the best available guess at how many
 * account rows the new month will have; 5 is the floor for the first paint.
 * Columns always come from `days`, which is known without any request.
 */
const skeletonRows = computed(() => Math.max(rows.value.length, 5))

async function loadPlugins() {
  try {
    allPlugins.value = (await api.plugins.list()) ?? []
    const capable = plugins.value
    if (!pluginId.value && capable.length > 0) {
      pluginId.value = capable.length > 1 ? ALL_PLUGINS : capable[0].manifest?.id ?? ''
    }
  } catch (e) {
    error.value = errorMessage(e)
  }
}

async function loadCheckins() {
  // No checkin endpoint to read: clear the grid and let the template explain.
  if (selectedUnsupported.value) {
    loading.value = false
    records.value = []
    accountRefs.value = []
    recordsLoaded.value = false
    return
  }
  const targets =
    pluginId.value === ALL_PLUGINS
      ? plugins.value.map((plugin) => plugin.manifest?.id ?? '').filter(Boolean)
      : [pluginId.value]
  if (targets.length === 0) {
    records.value = []
    accountRefs.value = []
    recordsLoaded.value = false
    return
  }
  loading.value = true
  error.value = ''
  try {
    const responses = await Promise.allSettled(
      targets.map((id) => api.checkins(id, bounds.value.from, bounds.value.to)),
    )
    const mergedRecords: CheckinRecord[] = []
    const mergedAccounts: { id: string; label: string; pluginId: string }[] = []
    const failures: string[] = []
    responses.forEach((result, index) => {
      const owner = targets[index]
      if (result.status === 'rejected') {
        failures.push(`${pluginName(owner)}：${errorMessage(result.reason)}`)
        return
      }
      const normalized = normalizeCheckins(result.value)
      for (const record of normalized.records) {
        mergedRecords.push({ ...record, plugin_id: record.plugin_id || owner })
      }
      for (const account of normalized.accounts) {
        mergedAccounts.push({ ...account, pluginId: owner })
      }
    })
    records.value = mergedRecords
    accountRefs.value = mergedAccounts
    recordsLoaded.value = true
    if (failures.length > 0) {
      error.value = `部分插件签到读取失败：${failures.join('；')}`
    }
  } catch (e) {
    error.value = errorMessage(e)
    records.value = []
    accountRefs.value = []
    recordsLoaded.value = false
  } finally {
    loading.value = false
  }
}

watch([pluginId, month], () => void loadCheckins())

onMounted(async () => {
  await loadPlugins()
  if (pluginId.value) await loadCheckins()
})

function cellRecord(key: string, day: number): CheckinRecord | undefined {
  const date = `${month.value.getFullYear()}-${pad(month.value.getMonth() + 1)}-${pad(day)}`
  return recordMap.value.get(`${key}|${date}`)
}

function markClass(result?: string): string {
  switch (result) {
    case 'ok':
      return 'ok'
    case 'already':
      return 'already'
    case 'fail':
      return 'fail'
    case undefined:
    case '':
      return ''
    default:
      return 'pending'
  }
}

function markText(result?: string): string {
  switch (result) {
    case 'ok':
      return '成'
    case 'already':
      return '已'
    case 'fail':
      return '失'
    case undefined:
    case '':
      return ''
    default:
      return '?'
  }
}

function shiftMonth(delta: number) {
  month.value = new Date(month.value.getFullYear(), month.value.getMonth() + delta, 1)
}

function thisMonth() {
  const now = new Date()
  month.value = new Date(now.getFullYear(), now.getMonth(), 1)
}

async function checkinAll() {
  checkinRunning.value = true
  allError.value = ''
  message.value = ''
  allResults.value = []
  try {
    allResults.value = normalizeCheckinAll(await api.checkinAll())
    message.value = '一键全签已完成。'
    await loadCheckins()
  } catch (e) {
    allError.value = errorMessage(e)
  } finally {
    checkinRunning.value = false
  }
}

function resultTone(result?: string): string {
  if (result === 'ok') return 'ok'
  if (result === 'already') return 'accent'
  if (result === 'fail' || result === 'error') return 'danger'
  return 'muted'
}

function creditDelta(item: CheckinAllItem): string {
  if (item.credits_before === undefined || item.credits_after === undefined) return ''
  const delta = item.credits_after - item.credits_before
  return `${formatNumber(item.credits_before)} → ${formatNumber(item.credits_after)}（${delta >= 0 ? '+' : ''}${formatNumber(delta)}）`
}
</script>

<template>
  <div class="page">
    <div v-if="error" class="notice error">
      <span>加载失败，请重试；下面是详细原因：{{ error }}</span>
      <button type="button" class="btn sm ghost" @click="loadCheckins">重试</button>
    </div>
    <div v-if="message" class="notice ok">{{ message }}</div>

    <section class="card">
      <div class="card-body tight">
        <div class="toolbar">
          <div class="field grow">
            <label for="checkin-plugin">插件</label>
            <select id="checkin-plugin" v-model="pluginId" class="select">
              <option v-if="plugins.length === 0" value="">暂无可用插件</option>
              <option v-else-if="plugins.length > 1" :value="ALL_PLUGINS">全部插件</option>
              <option v-if="selectedUnsupported" :value="pluginId">
                {{ selectedPlugin?.manifest?.name || pluginId }}（不支持签到）
              </option>
              <option v-for="plugin in plugins" :key="plugin.manifest?.id" :value="plugin.manifest?.id">
                {{ plugin.manifest?.name || plugin.manifest?.id }}
              </option>
            </select>
          </div>
          <div class="field">
            <span class="field-label">月份</span>
            <div class="btn-row">
              <button type="button" class="btn sm" @click="shiftMonth(-1)">上月</button>
              <span class="mono" style="min-width: 116px; text-align: center">{{ monthLabel(month) }}</span>
              <button type="button" class="btn sm" @click="shiftMonth(1)">下月</button>
              <button type="button" class="btn sm ghost" @click="thisMonth">本月</button>
            </div>
          </div>
          <div class="spacer"></div>
          <button type="button" class="btn primary" :disabled="checkinRunning" @click="checkinAll">
            {{ checkinRunning ? '签到中…' : '一键全签' }}
          </button>
        </div>
      </div>
    </section>

    <section class="card">
      <div class="card-head">
        <h2>{{ monthLabel(month) }} · 签到日历</h2>
        <div class="row small muted">
          <span>成功 {{ statText(summary.ok) }}</span>
          <span>已签 {{ statText(summary.already) }}</span>
          <span>失败 {{ statText(summary.fail) }}</span>
        </div>
      </div>

      <div class="card-body calendar-body">
        <div class="legend legend-boards">
          <span><span class="mark ok" aria-hidden="true"></span>成功</span>
          <span><span class="mark already" aria-hidden="true"></span>已签过</span>
          <span><span class="mark fail" aria-hidden="true"></span>失败</span>
          <span><span class="mark pending" aria-hidden="true"></span>其他状态</span>
          <span class="spacer"></span>
          <span>仅显示插件账号；A 的 trae / workbuddy / qoder 请查看 A 自己的日历。</span>
        </div>

        <div v-if="loading" class="calendar-wrap" aria-hidden="true">
          <table class="calendar is-skeleton">
            <thead>
              <tr>
                <th class="account-col">
                  <span class="skeleton sk-bar"></span>
                </th>
                <th v-for="day in days" :key="`skeleton-head-${day}`">
                  <span class="skeleton sk-day"></span>
                  <span class="wd"><span class="skeleton sk-day"></span></span>
                </th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="row in skeletonRows" :key="`skeleton-row-${row}`">
                <td class="account-col">
                  <div><span class="skeleton sk-bar"></span></div>
                  <div class="small mono"><span class="skeleton sk-bar is-sub"></span></div>
                </td>
                <td v-for="day in days" :key="`skeleton-cell-${row}-${day}`" class="cell">
                  <span class="skeleton mark"></span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <div v-else-if="allPlugins.length === 0" class="empty">
          <strong>请先安装插件</strong>
          <span>安装并启用插件后即可查看签到日历。</span>
        </div>

        <div v-else-if="plugins.length === 0" class="empty">
          <strong>没有支持签到的插件</strong>
          <span>已安装的插件都没有声明 checkin 能力。</span>
        </div>

        <div v-else-if="selectedUnsupported" class="empty">
          <span>该插件不支持签到</span>
        </div>

        <div v-else-if="rows.length === 0" class="empty">
          <strong>本月没有签到数据</strong>
          <span>该插件尚未上报账号，或本月还没有签到记录。可点击「一键全签」。</span>
        </div>

        <div v-else class="calendar-wrap">
          <table class="calendar">
            <thead>
              <tr>
                <th class="account-col">账号</th>
                <th v-for="day in days" :key="day">
                  {{ day }}
                  <span class="wd">{{ weekdayLabel(month, day) }}</span>
                </th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="row in rows" :key="row.key">
                <td class="account-col">
                  <div>{{ row.label }}</div>
                  <div class="small muted mono">
                    <template v-if="pluginId === ALL_PLUGINS">{{ row.pluginName }} · </template>{{ row.id }}
                  </div>
                </td>
                <td v-for="day in days" :key="day" class="cell">
                  <span
                    class="mark"
                    :class="markClass(cellRecord(row.key, day)?.result)"
                    :title="
                      cellRecord(row.key, day)
                        ? `${row.label} ${cellRecord(row.key, day)?.date} · ${cellRecord(row.key, day)?.result}${cellRecord(row.key, day)?.message ? ' · ' + cellRecord(row.key, day)?.message : ''}`
                        : `${row.label} 无记录`
                    "
                  >
                    {{ markText(cellRecord(row.key, day)?.result) }}
                  </span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </section>

    <section v-if="allError || allResults.length > 0" class="card">
      <div class="card-head">
        <h2>一键全签结果</h2>
        <span class="sub">按顺序执行所有启用插件账号与 A 的三家</span>
      </div>
      <div class="card-body">
        <div v-if="allError" class="notice error">
          <span>一键全签失败，请重试；下面是详细原因：{{ allError }}</span>
          <button type="button" class="btn sm ghost" :disabled="checkinRunning" @click="checkinAll">重试</button>
        </div>
        <div v-else class="result-list">
          <div v-for="(item, index) in allResults" :key="`${item.plugin_id}-${item.account_id}-${index}`" class="result-row">
            <span class="badge" :class="resultTone(item.result)">{{ item.result || '未知' }}</span>
            <strong>{{ item.account_label || item.account_id || '未知账号' }}</strong>
            <span class="small muted">{{ item.plugin_name || item.plugin_id || '' }}</span>
            <span v-if="creditDelta(item)" class="small muted mono">{{ creditDelta(item) }}</span>
            <span v-if="item.message" class="small muted">{{ item.message }}</span>
            <span v-if="item.error" class="small text-danger">{{ item.error }}</span>
            <span v-if="item.credits_after !== undefined" class="small muted">
              余额 {{ formatNumber(item.credits_after) }}
            </span>
          </div>
        </div>
      </div>
    </section>

    <p class="small muted">
      签到记录按账号 × 日期保存；失败会重试两次（间隔 5s / 20s），already 不重试。成功判定为 ok 或 already，积分差值仅供参考。
    </p>
  </div>
</template>

<style scoped>
.result-row > strong {
  font-size: var(--text-sm);
}
.calendar-body {
  padding-bottom: var(--space-3);
}

.legend-boards {
  margin-bottom: var(--space-3);
}
</style>
