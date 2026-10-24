<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import {
  api,
  errorMessage,
  normalizeAccounts,
  normalizeIdentityDetails,
  normalizeLoginState,
  type Account,
  type AccountEgressBinding,
  type AccountGroup,
  type AccountSavePatch,
  type BulkAction,
  type GroupSave,
  type IdentityDetails,
  type LoginMethod,
  type LoginStart,
  type LoginStateEntry,
  type LoginStatus,
  type PluginStatus,
  type Proxy,
} from '../api'
import { formatDate, formatNumber, formatTimestamp, percent, toDate } from '../format'

/* ------------------------------------------------------------------ */
/* Core state                                                          */
/* ------------------------------------------------------------------ */

const plugins = ref<PluginStatus[]>([])
const proxies = ref<Proxy[]>([])
const groups = ref<AccountGroup[]>([])
const accounts = ref<Account[]>([])
const loading = ref(false)
const error = ref('')
const pluginErrors = ref<Record<string, string>>({})

/** '' = all plugins / all groups. */
const pluginFilter = ref('')
const groupFilter = ref('')
const search = ref('')
const saving = ref(false)
const groupsCollapsed = ref(false)

const timers = new Map<string, number>()

/** Re-renders cooldown countdowns without reloading the account list. */
const nowMs = ref(Date.now())
let cooldownTicker = 0

function schedule(key: string, fn: () => void, ms: number) {
  const existing = timers.get(key)
  if (existing !== undefined) window.clearTimeout(existing)
  timers.set(
    key,
    window.setTimeout(() => {
      timers.delete(key)
      fn()
    }, ms),
  )
}

/* ------------------------------------------------------------------ */
/* Toasts                                                              */
/* ------------------------------------------------------------------ */

interface ToastItem {
  id: number
  text: string
  tone: 'ok' | 'error' | 'warn'
}

let toastSeq = 0
const toasts = ref<ToastItem[]>([])

function toast(text: string, tone: ToastItem['tone'] = 'ok') {
  const id = ++toastSeq
  toasts.value.push({ id, text, tone })
  schedule(`toast:${id}`, () => {
    toasts.value = toasts.value.filter((item) => item.id !== id)
  }, 4200)
}

/* ------------------------------------------------------------------ */
/* Rows                                                                */
/* ------------------------------------------------------------------ */

interface Row extends Account {
  plugin_id: string
  key: string
}

const rows = computed<Row[]>(() =>
  accounts.value.map((account) => {
    const plugin = account.plugin_id ?? ''
    return { ...account, plugin_id: plugin, key: `${plugin}:${account.id}` }
  }),
)

const pluginRuntime = computed<Record<string, PluginStatus>>(() => {
  const map: Record<string, PluginStatus> = {}
  for (const plugin of plugins.value) {
    const id = plugin.manifest?.id
    if (id) map[id] = plugin
  }
  return map
})

function pluginRunning(id: string): boolean {
  return Boolean(pluginRuntime.value[id]?.running)
}

function pluginCapabilities(id: string): string[] {
  return pluginRuntime.value[id]?.manifest?.capabilities ?? []
}

function canRefreshPlugin(id: string): boolean {
  return pluginCapabilities(id).includes('refresh')
}

// Identity details/reset and egress (bind IP) are host features that a plugin must
// implement itself. Nine of the twelve bundled plugins do not: their registration
// flow has no device-identity or multi-egress concept, so the buttons used to lead
// to a 502 or a silent no-op. Gate them on the declared capability instead of
// offering an action that cannot work.
function canReadIdentity(id: string): boolean {
  const caps = pluginCapabilities(id)
  // A plugin that answers /accounts/{id}/identity either advertises the capability
  // or declares an OS identity; anything else cannot serve device identity.
  return caps.includes('identity_os_switch') || caps.includes('identity_request') || caps.includes('identity')
}

function canBindEgress(id: string): boolean {
  return pluginCapabilities(id).includes('egress')
}

const offlinePlugins = computed(() => {
  const ids = new Set(accounts.value.map((account) => account.plugin_id ?? ''))
  return plugins.value
    .filter((plugin) => {
      const id = plugin.manifest?.id ?? ''
      return id && ids.has(id) && !plugin.running
    })
    .map((plugin) => plugin.manifest?.id ?? '')
})

const pluginErrorList = computed(() =>
  Object.entries(pluginErrors.value).map(([id, message]) => ({ id, message })),
)

const scopedRows = computed(() => {
  const query = search.value.trim().toLowerCase()
  return rows.value.filter((row) => {
    if (pluginFilter.value && row.plugin_id !== pluginFilter.value) return false
    if (!query) return true
    return searchText(row).includes(query)
  })
})

const filteredRows = computed(() =>
  scopedRows.value.filter((row) => {
    if (!groupFilter.value) return true
    return rowGroup(row) === groupFilter.value
  }),
)

function searchText(row: Row): string {
  return [displayName(row), row.id, row.label, row.alias ?? '', row.plugin_id]
    .join(' ')
    .toLowerCase()
}

function resetFilters() {
  pluginFilter.value = ''
  groupFilter.value = ''
  search.value = ''
}

/* ------------------------------------------------------------------ */
/* Draft overlay                                                       */
/* ------------------------------------------------------------------ */

interface Draft {
  alias: string
  group_id: string
  priority: number
  weight: number
  egress: string
}

const draft = reactive<Record<string, Draft>>({})

function snapshot(row: Row): Draft {
  return {
    alias: row.alias ?? '',
    group_id: row.group_id ?? '',
    priority: row.priority ?? 100,
    weight: row.weight ?? 1,
    egress: egressKey(row),
  }
}

function ensureDraft(row: Row): Draft {
  const existing = draft[row.key]
  if (existing) return existing
  const created = snapshot(row)
  draft[row.key] = created
  return created
}

function normalizeDraft(row: Row) {
  const current = draft[row.key]
  if (!current) return
  const base = snapshot(row)
  const same =
    current.alias === base.alias &&
    current.group_id === base.group_id &&
    current.priority === base.priority &&
    current.weight === base.weight &&
    current.egress === base.egress
  if (same) delete draft[row.key]
}

function rowAlias(row: Row): string {
  return draft[row.key]?.alias ?? row.alias ?? ''
}

function displayName(row: Row): string {
  return rowAlias(row) || row.label || row.display_name || row.id
}

function rowGroup(row: Row): string {
  const value = draft[row.key]?.group_id ?? (row.group_id ?? '')
  return deletedGroups[value] ? '' : value
}

function rowPriority(row: Row): number {
  return draft[row.key]?.priority ?? row.priority ?? 100
}

function rowWeight(row: Row): number {
  return draft[row.key]?.weight ?? row.weight ?? 1
}

function rowEgressKey(row: Row): string {
  return draft[row.key]?.egress ?? egressKey(row)
}

function isDirty(row: Row): boolean {
  const current = draft[row.key]
  if (current) {
    const base = snapshot(row)
    if (
      current.alias !== base.alias ||
      current.group_id !== base.group_id ||
      current.priority !== base.priority ||
      current.weight !== base.weight ||
      current.egress !== base.egress
    ) {
      return true
    }
  }
  return Boolean(deletedGroups[row.group_id ?? ''])
}

function setAlias(row: Row, value: string) {
  ensureDraft(row).alias = value.trim()
  normalizeDraft(row)
}

function setGroup(row: Row, value: string) {
  ensureDraft(row).group_id = value
  normalizeDraft(row)
}

function onGroupChange(row: Row, event: Event) {
  setGroup(row, (event.target as HTMLSelectElement).value)
}

function setPriority(row: Row, value: number) {
  if (!Number.isFinite(value)) return
  const next = Math.max(0, Math.round(value))
  if (next === rowPriority(row)) return
  ensureDraft(row).priority = next
  normalizeDraft(row)
}

function setWeight(row: Row, value: number) {
  if (!Number.isFinite(value)) return
  const next = Math.max(0, Math.round(value * 100) / 100)
  if (next === rowWeight(row)) return
  ensureDraft(row).weight = next
  normalizeDraft(row)
}

function onNumberChange(row: Row, event: Event, field: 'priority' | 'weight') {
  const input = event.target as HTMLInputElement
  if (input.value.trim() === '') {
    input.value = String(field === 'priority' ? rowPriority(row) : rowWeight(row))
    return
  }
  const value = Number(input.value)
  if (field === 'priority') setPriority(row, value)
  else setWeight(row, value)
}

/* ------------------------------------------------------------------ */
/* Group drafts                                                        */
/* ------------------------------------------------------------------ */

interface PendingGroup {
  key: string
  name: string
}

const newGroups = ref<PendingGroup[]>([])
const newGroupName = ref('')
let pendingSeq = 0

const groupNameEdits = reactive<Record<string, string>>({})
const deletedGroups = reactive<Record<string, boolean>>({})
const groupRenaming = ref('')
const groupRenameValue = ref('')
const groupDeleteConfirm = reactive<Record<string, boolean>>({})

const sidebarGroups = computed(() => groups.value.filter((group) => !deletedGroups[group.id]))

const persistedGroups = computed(() => sidebarGroups.value)

const groupCount = computed<Record<string, number>>(() => {
  const counts: Record<string, number> = {}
  for (const row of scopedRows.value) {
    const key = rowGroup(row)
    counts[key] = (counts[key] ?? 0) + 1
  }
  return counts
})

function countFor(key: string): number {
  return groupCount.value[key] ?? 0
}

/** Deletion warning counts every account, not just the current filter. */
function totalGroupCount(key: string): number {
  return rows.value.filter((row) => rowGroup(row) === key).length
}

function groupName(group: AccountGroup): string {
  return groupNameEdits[group.id] ?? group.name
}

function groupRename(group: AccountGroup): void {
  groupRenaming.value = group.id
  groupRenameValue.value = groupName(group)
  void nextTick(() => {
    const input = document.getElementById('group-rename-input') as HTMLInputElement | null
    input?.focus()
    input?.select()
  })
}

function cancelGroupRename() {
  groupRenaming.value = ''
  groupRenameValue.value = ''
}

function commitGroupRename(group: AccountGroup) {
  const next = groupRenameValue.value.trim()
  if (!next) {
    toast('分组名称不能为空', 'error')
    return
  }
  const duplicate = sidebarGroups.value.some(
    (item) =>
      item.id !== group.id &&
      groupName(item).toLowerCase() === next.toLowerCase(),
  )
  if (duplicate || newGroups.value.some((item) => item.name.toLowerCase() === next.toLowerCase())) {
    toast('分组名称已存在', 'error')
    return
  }
  if (next === group.name) delete groupNameEdits[group.id]
  else groupNameEdits[group.id] = next
  cancelGroupRename()
}

function askGroupDelete(group: AccountGroup) {
  groupDeleteConfirm[group.id] = true
  schedule(`group-delete:${group.id}`, () => {
    groupDeleteConfirm[group.id] = false
  }, 6000)
}

function confirmGroupDelete(group: AccountGroup) {
  deletedGroups[group.id] = true
  groupDeleteConfirm[group.id] = false
  delete groupNameEdits[group.id]
  if (groupFilter.value === group.id) groupFilter.value = ''
}

function addNewGroup() {
  const name = newGroupName.value.trim()
  if (!name) return
  const exists =
    sidebarGroups.value.some((item) => groupName(item).toLowerCase() === name.toLowerCase()) ||
    newGroups.value.some((item) => item.name.toLowerCase() === name.toLowerCase())
  if (exists) {
    toast('分组名称已存在', 'error')
    return
  }
  pendingSeq += 1
  newGroups.value.push({ key: `tmp-${pendingSeq}`, name })
  newGroupName.value = ''
}

function removeNewGroup(key: string) {
  newGroups.value = newGroups.value.filter((item) => item.key !== key)
  if (groupFilter.value === key) groupFilter.value = ''
}

const dirtyCount = computed(() => {
  let count = 0
  for (const row of rows.value) if (isDirty(row)) count += 1
  count += newGroups.value.length
  count += Object.keys(deletedGroups).length
  for (const [id, name] of Object.entries(groupNameEdits)) {
    const group = groups.value.find((item) => item.id === id)
    if (group && group.name !== name && !deletedGroups[id]) count += 1
  }
  return count
})

function discardDrafts() {
  for (const key of Object.keys(draft)) delete draft[key]
  for (const key of Object.keys(groupNameEdits)) delete groupNameEdits[key]
  for (const key of Object.keys(deletedGroups)) delete deletedGroups[key]
  for (const key of Object.keys(groupDeleteConfirm)) delete groupDeleteConfirm[key]
  newGroups.value = []
  cancelGroupRename()
  toast('已放弃未保存的更改')
}

/* ------------------------------------------------------------------ */
/* Loading                                                             */
/* ------------------------------------------------------------------ */

async function loadPlugins() {
  try {
    plugins.value = (await api.plugins.list()) ?? []
  } catch (e) {
    error.value = errorMessage(e)
  }
}

async function loadProxies() {
  try {
    proxies.value = (await api.proxies.list()) ?? []
  } catch {
    proxies.value = []
  }
}

const groupsLoading = ref(false)
const groupsError = ref('')

async function loadGroups(silent = false) {
  if (!silent) groupsLoading.value = true
  groupsError.value = ''
  try {
    const response = await api.groups.list()
    groups.value = response?.groups ?? []
  } catch (e) {
    groupsError.value = errorMessage(e)
  } finally {
    if (!silent) groupsLoading.value = false
  }
}

async function loadAccounts(silent = false) {
  if (!silent) loading.value = true
  pluginErrors.value = {}
  const ids = plugins.value
    .map((plugin) => plugin.manifest?.id ?? '')
    .filter((id) => id.length > 0)
  if (ids.length === 0) {
    accounts.value = []
    loading.value = false
    return
  }
  const results: Account[][] = []
  await Promise.all(
    ids.map(async (id) => {
      try {
        const data = await api.accounts(id)
        results.push(
          normalizeAccounts(data).map((account) =>
            account.plugin_id ? account : { ...account, plugin_id: id },
          ),
        )
      } catch (e) {
        pluginErrors.value[id] = errorMessage(e)
        results.push([])
      }
    }),
  )
  accounts.value = results.flat()
  pruneDrafts()
  pruneSelection()
  if (!silent) loading.value = false
}

function pruneDrafts() {
  const keys = new Set(rows.value.map((row) => row.key))
  for (const key of Object.keys(draft)) {
    if (!keys.has(key)) delete draft[key]
  }
}

async function refreshAll() {
  await Promise.all([loadAccounts(), loadGroups()])
}

/* ------------------------------------------------------------------ */
/* Selection and bulk actions                                          */
/* ------------------------------------------------------------------ */

const selected = reactive<Record<string, boolean>>({})
const bulkBusy = ref(false)
const bulkDeleteConfirm = ref(false)
const bulkReportOpen = ref(false)

interface BulkReport {
  action: string
  ok: number
  fail: number
  items: { name: string; ok: boolean; message: string }[]
}

const bulkReport = ref<BulkReport | null>(null)

const selectedRows = computed(() => rows.value.filter((row) => selected[row.key]))
const selectedCount = computed(() => selectedRows.value.length)
const allSelected = computed(
  () =>
    filteredRows.value.length > 0 &&
    filteredRows.value.every((row) => selected[row.key]),
)
const someSelected = computed(
  () =>
    !allSelected.value && filteredRows.value.some((row) => selected[row.key]),
)

function toggleAll(event: Event) {
  const checked = (event.target as HTMLInputElement).checked
  for (const row of filteredRows.value) {
    if (checked) selected[row.key] = true
    else delete selected[row.key]
  }
}

function clearSelection() {
  for (const key of Object.keys(selected)) delete selected[key]
}

function pruneSelection() {
  const keys = new Set(rows.value.map((row) => row.key))
  for (const key of Object.keys(selected)) {
    if (!keys.has(key)) delete selected[key]
  }
}

const BULK_LABELS: Record<BulkAction, string> = {
  checkin: '签到',
  refresh: '续期',
  enable: '启用',
  disable: '禁用',
  delete: '删除',
  group: '加入分组',
  egress: '绑定 IP',
}

async function runBulk(action: BulkAction, params?: Record<string, string | number | boolean>) {
  const targets = [...selectedRows.value]
  if (targets.length === 0 || bulkBusy.value) return
  bulkBusy.value = true
  bulkDeleteConfirm.value = false
  try {
    const response = await api.accounts.bulk({
      action,
      items: targets.map((row) => ({ plugin_id: row.plugin_id, account_id: row.id })),
      params,
    })
    const results = response?.results ?? []
    const detail = results.map((item) => {
      const row = targets.find(
        (candidate) => candidate.plugin_id === item.plugin_id && candidate.id === item.account_id,
      )
      return {
        name: row ? displayName(row) : item.account_id,
        ok: item.ok !== false,
        message: item.message ?? '',
      }
    })
    const fail = detail.filter((item) => !item.ok).length
    const ok = detail.length > 0 ? detail.length - fail : targets.length
    bulkReport.value = { action: BULK_LABELS[action], ok, fail, items: detail }
    bulkReportOpen.value = fail > 0
    if (action === 'group' || action === 'egress') {
      for (const row of targets) delete draft[row.key]
    }
    if (action === 'delete') {
      for (const row of targets) {
        delete selected[row.key]
        delete draft[row.key]
      }
    }
    await loadAccounts(true)
    toast(
      `${BULK_LABELS[action]}完成：${ok} 成功${fail > 0 ? `，${fail} 失败` : ''}`,
      fail > 0 ? 'warn' : 'ok',
    )
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    bulkBusy.value = false
  }
}

function askBulkDelete() {
  bulkDeleteConfirm.value = true
  schedule('bulk-delete', () => (bulkDeleteConfirm.value = false), 6000)
}

function onBulkGroup(event: Event) {
  const select = event.target as HTMLSelectElement
  const value = select.value
  select.value = ''
  if (value) void runBulk('group', { group_id: value })
}

function onBulkEgress(event: Event) {
  const select = event.target as HTMLSelectElement
  const value = select.value
  select.value = ''
  if (!value) return
  const binding = egressBinding(value)
  const params: Record<string, string | number | boolean> = { mode: binding.mode }
  if (binding.proxy_id) params.proxy_id = binding.proxy_id
  void runBulk('egress', params)
}

const bulkEgressOptions = computed(() => {
  const options = [
    { value: 'default', label: '跟随默认' },
    { value: 'direct', label: '直连' },
  ]
  for (const proxy of proxies.value) {
    if (proxy.enabled) options.push({ value: `proxy:${proxy.id}`, label: proxy.name || proxy.id })
  }
  return options
})

/* ------------------------------------------------------------------ */
/* Row menu                                                            */
/* ------------------------------------------------------------------ */

interface MenuState {
  key: string
  top: number
  left: number
}

const menu = ref<MenuState | null>(null)
const menuEl = ref<HTMLElement | null>(null)

const menuRow = computed(() => {
  const state = menu.value
  if (!state) return null
  return rows.value.find((row) => row.key === state.key) ?? null
})

function toggleMenu(row: Row, event: MouseEvent) {
  if (menu.value?.key === row.key) {
    menu.value = null
    return
  }
  const target = event.currentTarget as HTMLElement
  const rect = target.getBoundingClientRect()
  const width = 224
  const left = Math.max(8, Math.min(rect.right - width, window.innerWidth - width - 8))
  const top = Math.max(8, Math.min(rect.bottom + 4, window.innerHeight - 400))
  menu.value = { key: row.key, top, left }
}

function closeMenu() {
  menu.value = null
}

function withMenuRow(fn: (row: Row) => void) {
  const row = menuRow.value
  menu.value = null
  if (row) fn(row)
}

function menuLocked(): boolean {
  const row = menuRow.value
  return !row || rowLocked(row) || Boolean(busy[row.key])
}

function menuEnabled(): boolean {
  const row = menuRow.value
  return row ? isEnabled(row) : false
}

// The identity panels only make sense for a plugin that answers identity calls.
// Disabling the entry is honest; letting the click through produced a 502.
function menuCannotReadIdentity(): boolean {
  const row = menuRow.value
  if (!row) return true
  return !canReadIdentity(row.plugin_id ?? '')
}

// Binding an IP is the UI name for the egress assignment, which only plugins that
// declare the egress capability can honour. Without it the save is a silent no-op.
function menuCannotBindEgress(): boolean {
  const row = menuRow.value
  if (!row) return true
  return !canBindEgress(row.plugin_id ?? '')
}

function menuCanRefresh(): boolean {
  const row = menuRow.value
  if (!row) return false
  return canRefreshPlugin(row.plugin_id) || row.credential?.refreshable === true
}

watch(menu, async (state) => {
  if (!state) return
  await nextTick()
  menuEl.value?.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus()
})

function onDocumentPointerDown(event: MouseEvent) {
  const target = event.target as HTMLElement | null
  if (target && target.closest('[data-row-menu]')) return
  menu.value = null
}

function onWindowKeydown(event: KeyboardEvent) {
  if (event.key !== 'Escape') return
  if (menu.value) {
    menu.value = null
    return
  }
  if (loginStateOpen.value) {
    loginStateOpen.value = false
    return
  }
  if (editingKey.value) {
    editingKey.value = ''
  }
}

/* ------------------------------------------------------------------ */
/* Egress helpers                                                      */
/* ------------------------------------------------------------------ */

function egressKey(account: Account): string {
  const egress = account.egress
  if (!egress) return 'default'
  if (egress.mode === 'direct') return 'direct'
  if (egress.mode === 'proxy' && egress.proxy_id) return `proxy:${egress.proxy_id}`
  return 'default'
}

function egressBinding(value: string): AccountEgressBinding {
  if (value === 'direct') return { mode: 'direct' }
  if (value.startsWith('proxy:')) return { mode: 'proxy', proxy_id: value.slice('proxy:'.length) }
  return { mode: 'default' }
}

function proxyById(id: string): Proxy | undefined {
  return proxies.value.find((proxy) => proxy.id === id)
}

function egressOptions(account: Account): { value: string; label: string }[] {
  const options = [
    { value: 'default', label: '跟随默认' },
    { value: 'direct', label: '直连' },
  ]
  for (const proxy of proxies.value) {
    if (!proxy.enabled) continue
    options.push({
      value: `proxy:${proxy.id}`,
      label: `${proxy.name}（${proxy.host}:${proxy.port}）`,
    })
  }
  const assignedId = account.egress?.mode === 'proxy' ? account.egress.proxy_id : undefined
  if (assignedId && !proxies.value.some((proxy) => proxy.id === assignedId && proxy.enabled)) {
    const known = proxyById(assignedId)
    options.push({
      value: `proxy:${assignedId}`,
      label: known ? `${known.name}（${known.host}:${known.port}，已停用）` : `未知代理（${assignedId}）`,
    })
  }
  return options
}

function egressLabel(row: Row): string {
  const key = rowEgressKey(row)
  if (key === 'default') return '跟随默认'
  if (key === 'direct') return '直连'
  const proxy = proxyById(key.slice('proxy:'.length))
  return proxy ? proxy.name : '未知代理'
}

function egressExitIp(row: Row): string {
  const key = rowEgressKey(row)
  if (!key.startsWith('proxy:')) return ''
  // A failed tunnel means the stored exit IP is from the last success; showing
  // it as current would be a lie, so the failure badge takes its place.
  if (row.egress_health?.status === 'failed') return ''
  return proxyById(key.slice('proxy:'.length))?.last_exit_ip ?? ''
}

function elapsedText(seconds: number): string {
  if (!seconds || seconds < 0) return '刚刚'
  if (seconds < 60) return `${Math.max(1, Math.floor(seconds))} 秒前`
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时前`
  return `${Math.floor(seconds / 86400)} 天前`
}

function egressHealthText(row: Row): string {
  const health = row.egress_health
  if (!health) return ''
  const age = health.at ? elapsedText(health.age_seconds) : ''
  switch (health.status) {
    case 'failed':
      return `出口失败${age ? ` · ${age}` : ''}`
    case 'ok':
      return age ? `出口正常 · ${age}` : '出口正常'
    default:
      return '出口待验证'
  }
}

function egressHealthTitle(row: Row): string {
  const health = row.egress_health
  if (!health) return ''
  const parts = [health.proxy_label || health.proxy_id || '代理出口']
  if (health.status === 'failed') parts.push('最近一次隧道失败', health.error || '')
  if (health.status === 'unknown') parts.push('近 10 分钟内没有隧道记录，也未做过检测')
  if (health.at) parts.push(`证据时间 ${formatTimestamp(health.at)}`)
  return parts.filter(Boolean).join(' · ')
}

function egressInfo(row: Row): string {
  const key = rowEgressKey(row)
  if (key === 'default') return '未单独指定出口，使用插件自身的默认出网方式。'
  if (key === 'direct') return '未分配代理，请求从本机直接出网。'
  const proxy = proxyById(key.slice('proxy:'.length))
  if (!proxy) return '代理记录不存在或已删除，请重新选择出口。'
  return [
    `${proxy.type.toUpperCase()} ${proxy.host}:${proxy.port}`,
    proxy.last_exit_ip ? `出口 ${proxy.last_exit_ip}` : '出口未知',
    proxy.last_latency_ms ? `${proxy.last_latency_ms} ms` : '',
    proxy.last_check_at ? `检测于 ${formatTimestamp(proxy.last_check_at)}` : '尚未检测',
    proxy.enabled ? '' : '代理已停用',
    proxy.last_error ? `错误：${proxy.last_error}` : '',
  ]
    .filter(Boolean)
    .join(' · ')
}

function onEgressChange(row: Row, event: Event) {
  const value = (event.target as HTMLSelectElement).value
  ensureDraft(row).egress = value
  normalizeDraft(row)
}

function egressDirty(row: Row): boolean {
  return rowEgressKey(row) !== egressKey(row)
}

/* ------------------------------------------------------------------ */
/* Row state helpers                                                   */
/* ------------------------------------------------------------------ */

function isEnabled(row: Row): boolean {
  return row.enabled_effective ?? row.enabled
}

function rowLocked(row: Row): boolean {
  return Boolean(row.stale) || !pluginRunning(row.plugin_id)
}

function quotaPercent(row: Account): number {
  return percent(row.quota?.remaining, row.quota?.total)
}

// A plugin that reports no quota (the honest "unknown" state) must not render as
// 0% — that reads as "credits exhausted".
function hasQuota(row: Account): boolean {
  const total = Number(row.quota?.total)
  return Number.isFinite(total) && total > 0
}

function quotaTone(row: Account): string {
  const value = quotaPercent(row)
  if (value < 10) return 'low'
  if (value < 30) return 'mid'
  return ''
}

/* ---- Health: one merged verdict from the host ---- */

// state may be a plugin status (expired/invalid/rate_limited/unknown), a host
// status_kind (session_dead/banned/...) or "normal". The host resolves which
// one wins; the page renders exactly one badge.
const STATE_LABELS: Record<string, string> = {
  normal: '正常',
  expired: '已过期',
  invalid: '失效',
  rate_limited: '限流中',
  session_dead: '登录失效',
  banned: '已封禁',
  plan_limit: '套餐限制',
  device_signed: '设备已签',
  quota: '额度不足',
  unknown: '未知',
}

const PERMANENT_COOLDOWN = 9_999_999_999

function stateText(row: Row): string {
  return STATE_LABELS[row.state || ''] || '未知'
}

function stateTone(row: Row): string {
  switch (row.state) {
    case 'normal':
      return 'ok'
    case 'invalid':
    case 'banned':
    case 'session_dead':
      return 'danger'
    case 'expired':
    case 'rate_limited':
    case 'plan_limit':
    case 'device_signed':
    case 'quota':
      return 'warn'
    default:
      return 'muted'
  }
}

function stateTitle(row: Row): string {
  const detail = row.state_detail || ''
  if (row.state_source === 'host') {
    const left = cooldownLeft(row)
    const base = detail || `${stateText(row)}，暂不参与签到与调度`
    return left ? `${base}（剩余 ${left}）` : base
  }
  if (row.state_source === 'plugin' && detail) {
    return `${detail}（由插件上报）`
  }
  return detail || '账号状态正常'
}

function cooldownLeft(row: Row): string {
  const until = Number(row.cooldown_until || 0)
  if (until >= PERMANENT_COOLDOWN) return '需重新登录'
  const ms = until * 1000 - nowMs.value
  if (ms <= 0) return ''
  const total = Math.ceil(ms / 1000)
  if (total >= 3600) return `${Math.floor(total / 3600)} 小时 ${Math.ceil((total % 3600) / 60)} 分`
  if (total >= 60) return `${Math.ceil(total / 60)} 分钟`
  return `${total} 秒`
}

const DAY_MS = 24 * 60 * 60 * 1000

function expiryClass(row: Row): string {
  const date = toDate(row.quota?.expires_at)
  if (!date) return ''
  const diff = date.getTime() - Date.now()
  if (diff < 0) return 'past'
  if (diff < 7 * DAY_MS) return 'soon'
  return ''
}

function expiryText(row: Row): string {
  const date = toDate(row.quota?.expires_at)
  if (!date) return '到期未知'
  const formatted = formatDate(date)
  return date.getTime() < Date.now() ? `已过期（${formatted}）` : `到期 ${formatted}`
}

function checkedInText(row: Row): string {
  if (!row.quota?.today_checked_in) return '否'
  return row.quota.checked_in_at ? `是（${formatTimestamp(row.quota.checked_in_at)}）` : '是'
}

/* ------------------------------------------------------------------ */
/* Busy + immediate actions                                            */
/* ------------------------------------------------------------------ */

const busy = reactive<Record<string, boolean>>({})

async function toggleAccount(row: Row) {
  if (rowLocked(row) || busy[row.key]) return
  const enabled = !isEnabled(row)
  busy[row.key] = true
  try {
    await api.accounts.patchAccount(row.plugin_id, row.id, { enabled })
    await loadAccounts(true)
    toast(enabled ? '账号已启用' : '账号已停用')
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    busy[row.key] = false
  }
}

async function checkinRow(row: Row) {
  if (rowLocked(row) || busy[row.key]) return
  busy[row.key] = true
  try {
    const response = await api.accounts.checkin(row.plugin_id, row.id)
    const result = response?.result
    const message =
      result?.message || (result?.result === 'already' ? '今日已签到' : '签到完成')
    toast(`${displayName(row)}：${message}`, result?.result === 'fail' ? 'error' : 'ok')
    await loadAccounts(true)
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    busy[row.key] = false
  }
}

async function refreshRow(row: Row) {
  if (rowLocked(row) || busy[row.key]) return
  busy[row.key] = true
  try {
    const response = await api.accounts.refreshAccount(row.plugin_id, row.id)
    const result = response?.result
    if (result?.result === 'fail') {
      toast(result.message || '续期失败，请重新登录', 'error')
      return
    }
    toast(result?.message || (result?.result === 'already' ? '凭据仍然有效' : '凭据已续期'))
    await loadAccounts(true)
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    busy[row.key] = false
  }
}

const deleteConfirm = reactive<Record<string, boolean>>({})

function askDelete(row: Row) {
  deleteConfirm[row.key] = true
  schedule(`delete:${row.key}`, () => (deleteConfirm[row.key] = false), 6000)
}

async function confirmDelete(row: Row) {
  if (busy[row.key]) return
  deleteConfirm[row.key] = false
  busy[row.key] = true
  try {
    await api.accounts.deleteAccount(row.plugin_id, row.id)
    delete selected[row.key]
    delete draft[row.key]
    await loadAccounts(true)
    toast('账号已删除')
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    busy[row.key] = false
  }
}

/* ------------------------------------------------------------------ */
/* Alias editing                                                       */
/* ------------------------------------------------------------------ */

const editingKey = ref('')
const aliasEditValue = ref('')

async function startAlias(row: Row) {
  editingKey.value = row.key
  aliasEditValue.value = rowAlias(row)
  await nextTick()
  const input = document.getElementById('account-alias-input') as HTMLInputElement | null
  input?.focus()
  input?.select()
}

function commitAlias(row: Row) {
  if (editingKey.value !== row.key) return
  setAlias(row, aliasEditValue.value)
  editingKey.value = ''
}

function cancelAlias() {
  editingKey.value = ''
}

/* ------------------------------------------------------------------ */
/* Row panels + identity                                               */
/* ------------------------------------------------------------------ */

type Panel = 'quota' | 'identity' | 'egress'

const panels = reactive<Record<string, Panel>>({})
const identityData = reactive<Record<string, IdentityDetails>>({})
const identityErrors = reactive<Record<string, string>>({})
const identityLoading = reactive<Record<string, boolean>>({})
const identityRevealed = reactive<Record<string, boolean>>({})
const identityConfirm = reactive<Record<string, '' | 'request' | 'os'>>({})

function panelOf(row: Row): Panel | '' {
  return panels[row.key] ?? ''
}

function openPanel(row: Row, panel: Panel) {
  if (panels[row.key] === panel) {
    delete panels[row.key]
    return
  }
  panels[row.key] = panel
  if (panel === 'identity') void loadIdentity(row, identityRevealed[row.key] !== false)
}

function closePanel(row: Row) {
  delete panels[row.key]
}

function openIdentityReset(row: Row) {
  panels[row.key] = 'identity'
  identityConfirm[row.key] = 'request'
  if (!identityData[row.key]) void loadIdentity(row, identityRevealed[row.key] !== false)
}

async function loadIdentity(row: Row, reveal: boolean) {
  identityLoading[row.key] = true
  identityErrors[row.key] = ''
  try {
    const data = await api.accounts.identityDetails(row.plugin_id, row.id, reveal)
    identityData[row.key] = normalizeIdentityDetails(data)
    identityRevealed[row.key] = reveal
  } catch (e) {
    identityErrors[row.key] = errorMessage(e)
  } finally {
    identityLoading[row.key] = false
  }
}

function toggleIdentityReveal(row: Row) {
  void loadIdentity(row, identityRevealed[row.key] === false)
}

function identityRows(row: Row): { label: string; value: string }[] {
  const identity = identityData[row.key]
  return [
    { label: '机器码', value: identity?.machine_id ?? '' },
    { label: '设备号', value: identity?.device_id ?? '' },
    { label: '会话', value: identity?.session_id ?? '' },
    { label: '身份来源', value: identity?.source ?? '' },
    {
      label: '更新时间',
      value: identity?.updated_at ? formatTimestamp(identity.updated_at) : '',
    },
  ]
}

function canOsIdentity(row: Row): boolean {
  const manifest = pluginRuntime.value[row.plugin_id]?.manifest
  if (!manifest) return false
  return (
    (manifest.capabilities ?? []).includes('identity_os_switch') &&
    Boolean(manifest.permissions?.os_identity)
  )
}

function identityConfirmOpen(row: Row): boolean {
  return Boolean(identityConfirm[row.key])
}

async function resetIdentity(row: Row, mode: 'request' | 'os') {
  if (rowLocked(row) || busy[row.key]) return
  identityConfirm[row.key] = ''
  busy[row.key] = true
  try {
    await api.accounts.resetIdentity(row.plugin_id, row.id, mode, mode === 'os')
    await loadAccounts(true)
    delete identityData[row.key]
    if (panels[row.key] === 'identity') await loadIdentity(row, identityRevealed[row.key] !== false)
    toast('设备身份已重置')
  } catch (e) {
    toast(errorMessage(e), 'error')
  } finally {
    busy[row.key] = false
  }
}

async function copyText(text: string) {
  if (!text) return
  try {
    await navigator.clipboard.writeText(text)
    toast('已复制')
  } catch {
    toast('复制失败，请手动选择文本', 'error')
  }
}

/* ------------------------------------------------------------------ */
/* Login state drawer                                                  */
/* ------------------------------------------------------------------ */

const loginStateOpen = ref(false)
const loginState = ref<LoginStateEntry[]>([])
const loginStateLoading = ref(false)
const loginStateError = ref('')

async function loadLoginState() {
  loginStateLoading.value = true
  loginStateError.value = ''
  try {
    loginState.value = normalizeLoginState(await api.accounts.loginState())
  } catch (e) {
    loginStateError.value = errorMessage(e)
    loginState.value = []
  } finally {
    loginStateLoading.value = false
  }
}

function toggleLoginState() {
  loginStateOpen.value = !loginStateOpen.value
  if (loginStateOpen.value) void loadLoginState()
}

function yesNo(value: boolean | undefined): string {
  if (value === undefined) return '—'
  return value ? '是' : '否'
}

const METHOD_LABELS: Record<string, string> = {
  oauth: '浏览器登录',
  ide_plugin: 'IDE 插件登录',
  api_key: 'API Key',
  local_state: '读取本机登录态',
  cookie: 'Cookie',
}

function entryMethods(entry: LoginStateEntry): string {
  const methods = entry.methods ?? []
  if (methods.length === 0) return '—'
  return methods.map((method) => METHOD_LABELS[method] ?? method).join('、')
}

function entrySource(entry: LoginStateEntry): string {
  return entry.source === 'manifest' ? 'manifest' : '推导'
}

/* ------------------------------------------------------------------ */
/* Login panel                                                         */
/* ------------------------------------------------------------------ */

const LOGIN_METHODS: { capability: string; method: LoginMethod; label: string }[] = [
  { capability: 'login_oauth', method: 'oauth', label: '浏览器登录' },
  { capability: 'login_ide_plugin', method: 'ide_plugin', label: 'IDE 插件登录' },
  // Three different paste shapes share this method: a callback URL (zcode), an
  // authorization code (codearts) and an access token (cline/minimax). The
  // plugin-provided fields carry the exact wording; the button stays generic.
  { capability: 'login_paste_code', method: 'paste_code', label: '粘贴授权码/Token' },
  { capability: 'login_api_key', method: 'api_key', label: 'API Key' },
  { capability: 'login_local_state', method: 'local_state', label: '读取本机登录态' },
  { capability: 'login_cookie', method: 'cookie', label: 'Cookie 登录' },
]

const LOGIN_POLL_MS = 2000
const LOGIN_TIMEOUT_MS = 15 * 60 * 1000

const loginOpen = ref(false)
const loginPlugin = ref('')
const loginMethod = ref<LoginMethod | ''>('')
const loginSession = ref<LoginStart | null>(null)
const loginStatus = ref('')
const loginDetected = ref<string[]>([])
const loginValues = reactive<Record<string, string>>({})
const loginBusy = ref(false)
const loginError = ref('')
const oauthFallback = ref('')

let pollTimer: number | undefined
let pollDeadline = 0

const currentPlugin = computed(() =>
  plugins.value.find((item) => item.manifest?.id === loginPlugin.value),
)

const loginMethods = computed(() => {
  const capabilities = currentPlugin.value?.manifest?.capabilities ?? []
  return LOGIN_METHODS.filter((item) => capabilities.includes(item.capability))
})

const loginPluginRunning = computed(() => Boolean(currentPlugin.value?.running))

/** The copyable login URL is only offered while the session can still complete. */
const loginUrl = computed(() => {
  const session = loginSession.value
  if (!session?.url) return ''
  return isLoginOpenStatus(loginStatus.value) ? session.url : ''
})

function selectLoginUrl(event: Event) {
  const input = event.currentTarget as HTMLInputElement | null
  input?.select()
}

function legacyCopyText(text: string): boolean {
  try {
    const area = document.createElement('textarea')
    area.value = text
    area.setAttribute('readonly', '')
    area.style.position = 'fixed'
    area.style.top = '-9999px'
    area.style.opacity = '0'
    document.body.appendChild(area)
    area.select()
    const copied = document.execCommand('copy')
    document.body.removeChild(area)
    return copied
  } catch {
    return false
  }
}

async function copyLoginUrl() {
  const url = loginUrl.value
  if (!url) return
  let copied = false
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(url)
      copied = true
    }
  } catch {
    copied = false
  }
  if (!copied) copied = legacyCopyText(url)
  if (copied) {
    toast('登录链接已复制')
  } else {
    toast('复制失败，请手动选择链接复制', 'warn')
  }
}

function isLoginOpenStatus(status: string): boolean {
  return status === 'pending' || status === 'need_input'
}

function stopPolling() {
  if (pollTimer !== undefined) {
    window.clearTimeout(pollTimer)
    pollTimer = undefined
  }
}

function clearLoginValues() {
  for (const key of Object.keys(loginValues)) delete loginValues[key]
}

function resetLoginSession() {
  stopPolling()
  loginSession.value = null
  loginStatus.value = ''
  loginDetected.value = []
  loginBusy.value = false
  loginError.value = ''
  oauthFallback.value = ''
  clearLoginValues()
  const available = loginMethods.value
  loginMethod.value = available.length > 0 ? available[0].method : ''
}

function closeLoginPanel() {
  resetLoginSession()
  loginOpen.value = false
}

function openLogin(target?: string) {
  const plugin =
    target || pluginFilter.value || plugins.value[0]?.manifest?.id || ''
  if (!plugin) {
    toast('请先安装并启用插件', 'warn')
    return
  }
  closeLoginPanel()
  loginPlugin.value = plugin
  loginOpen.value = true
  resetLoginSession()
}

watch(loginPlugin, (_next, previous) => {
  const session = loginSession.value
  if (session && previous && isLoginOpenStatus(loginStatus.value)) {
    void api.accounts.loginCancel(previous, session.session_id).catch(() => undefined)
  }
  resetLoginSession()
})

function goLogin(plugin: string) {
  loginStateOpen.value = false
  pluginFilter.value = plugin
  openLogin(plugin)
}

async function startLogin() {
  const plugin = loginPlugin.value
  if (!plugin || loginBusy.value || loginMethods.value.length === 0) return
  stopPolling()
  loginBusy.value = true
  loginError.value = ''
  loginSession.value = null
  loginStatus.value = ''
  loginDetected.value = []
  clearLoginValues()
  try {
    const start = await api.accounts.loginStart(plugin, loginMethod.value)
    if (!loginOpen.value || loginPlugin.value !== plugin) return
    applyLoginStart(plugin, start)
  } catch (e) {
    loginError.value = errorMessage(e)
  } finally {
    loginBusy.value = false
  }
}

function applyLoginStart(plugin: string, start: LoginStart) {
  loginSession.value = start
  loginStatus.value = start.status || 'pending'
  oauthFallback.value = ''
  clearLoginValues()
  for (const field of start.fields ?? []) loginValues[field.name] = ''
  if (start.status === 'success') {
    void onLoginSuccess()
    return
  }
  if (start.status === 'failed') {
    loginError.value = start.error || '登录失败，请重试。'
    return
  }
  // A pending session may still carry a reason (expired local login, upstream
  // notice). Showing it beats an indefinite spinner with no explanation; the
  // poll below still runs in case the flow completes on its own.
  if (start.error) loginError.value = start.error
  pollDeadline = Date.now() + LOGIN_TIMEOUT_MS
  scheduleLoginPoll(plugin, start.session_id)
}

function applyLoginStatus(status: LoginStatus) {
  loginStatus.value = status.status || 'pending'
  if (status.detected && status.detected.length > 0) {
    loginDetected.value = status.detected.map((item) => item.label).filter(Boolean)
  }
  if (status.status === 'success') {
    void onLoginSuccess()
    return
  }
  if (status.status === 'failed') {
    loginError.value = status.error || '登录失败，请重试。'
  }
}

function scheduleLoginPoll(plugin: string, sessionId: string) {
  stopPolling()
  pollTimer = window.setTimeout(() => void pollLogin(plugin, sessionId), LOGIN_POLL_MS)
}

async function pollLogin(plugin: string, sessionId: string) {
  pollTimer = undefined
  if (!loginOpen.value || loginPlugin.value !== plugin || loginSession.value?.session_id !== sessionId) {
    return
  }
  if (Date.now() > pollDeadline) {
    loginStatus.value = 'failed'
    loginError.value = '登录等待超时，请重新发起登录。'
    return
  }
  try {
    const status = await api.accounts.loginPoll(plugin, sessionId)
    if (!loginOpen.value || loginSession.value?.session_id !== sessionId) return
    applyLoginStatus(status)
  } catch (e) {
    loginStatus.value = 'failed'
    loginError.value = errorMessage(e)
    return
  }
  if (isLoginOpenStatus(loginStatus.value)) scheduleLoginPoll(plugin, sessionId)
}

async function submitLogin() {
  const session = loginSession.value
  const plugin = loginPlugin.value
  if (!session || !plugin || loginBusy.value) return
  const fields = session.fields ?? []
  const missing = fields.filter((field) => field.required && !loginValues[field.name]?.trim())
  if (missing.length > 0) {
    loginError.value = `请填写${missing.map((field) => field.label || field.name).join('、')}`
    return
  }
  const values: Record<string, string> = {}
  for (const field of fields) values[field.name] = loginValues[field.name] ?? ''
  loginBusy.value = true
  loginError.value = ''
  try {
    const status = await api.accounts.loginSubmit(plugin, session.session_id, values)
    if (!loginOpen.value || loginSession.value?.session_id !== session.session_id) return
    applyLoginStatus(status)
    if (status.status === 'need_input') {
      loginError.value = status.error || '输入未通过校验，请检查后重试。'
    }
    if (isLoginOpenStatus(loginStatus.value)) {
      pollDeadline = Date.now() + LOGIN_TIMEOUT_MS
      scheduleLoginPoll(plugin, session.session_id)
    }
  } catch (e) {
    loginError.value = errorMessage(e)
  } finally {
    loginBusy.value = false
  }
}

async function submitOAuthFallback() {
  const session = loginSession.value
  const plugin = loginPlugin.value
  const raw = oauthFallback.value.trim()
  if (!session || !plugin || loginBusy.value) return
  if (!raw) {
    loginError.value = '请粘贴浏览器地址栏的完整回调 URL'
    return
  }
  loginBusy.value = true
  loginError.value = ''
  try {
    const status = await api.accounts.loginSubmit(plugin, session.session_id, { code: raw })
    if (!loginOpen.value || loginSession.value?.session_id !== session.session_id) return
    applyLoginStatus(status)
  } catch (e) {
    loginError.value = errorMessage(e)
  } finally {
    loginBusy.value = false
  }
}

async function onLoginSuccess() {
  stopPolling()
  loginStatus.value = 'success'
  loginSession.value = null
  loginDetected.value = []
  clearLoginValues()
  loginOpen.value = false
  loginError.value = ''
  toast('账号已添加')
  await loadAccounts()
}

async function cancelLogin() {
  const session = loginSession.value
  const plugin = loginPlugin.value
  stopPolling()
  loginBusy.value = true
  if (session && plugin) {
    try {
      await api.accounts.loginCancel(plugin, session.session_id)
    } catch {
      // Cancelling is best effort; the panel closes either way.
    }
  }
  loginBusy.value = false
  closeLoginPanel()
}

/* ------------------------------------------------------------------ */
/* Global save                                                         */
/* ------------------------------------------------------------------ */

function remapDraftGroups(map: Record<string, string>) {
  for (const row of rows.value) {
    const current = draft[row.key]
    if (!current || !current.group_id) continue
    const mapped = map[current.group_id]
    if (!mapped) continue
    current.group_id = mapped
    if (groupFilter.value === Object.keys(map).find((key) => map[key] === mapped)) {
      groupFilter.value = mapped
    }
    normalizeDraft(row)
  }
}

async function saveAll() {
  if (saving.value || dirtyCount.value === 0) return
  saving.value = true
  error.value = ''
  try {
    // New groups have to exist before accounts can point at them.
    const created: PendingGroup[] = []
    for (const pending of [...newGroups.value]) {
      await api.groups.create({ name: pending.name })
      created.push(pending)
      newGroups.value = newGroups.value.filter((item) => item.key !== pending.key)
    }
    if (created.length > 0) {
      await loadGroups(true)
      const map: Record<string, string> = {}
      for (const pending of created) {
        const found = groups.value.find(
          (group) => group.name.trim() === pending.name.trim(),
        )
        if (found) map[pending.key] = found.id
      }
      if (Object.keys(map).length !== created.length) {
        throw new Error('分组已创建，但未能读取到新分组 ID，请刷新后重试。')
      }
      remapDraftGroups(map)
    }

    const patches: AccountSavePatch[] = []
    for (const row of rows.value) {
      if (!isDirty(row)) continue
      const current = draft[row.key]
      const patch: AccountSavePatch = {
        plugin_id: row.plugin_id,
        account_id: row.id,
        group_id: rowGroup(row) || null,
      }
      if (current) {
        patch.alias = current.alias
        if (current.priority !== (row.priority ?? 100)) patch.priority = current.priority
        if (current.weight !== (row.weight ?? 1)) patch.weight = current.weight
        if (current.egress !== egressKey(row)) patch.egress = egressBinding(current.egress)
      }
      patches.push(patch)
    }

    const groupPayload: GroupSave[] = groups.value
      .filter((group) => !deletedGroups[group.id])
      .map((group) => ({
        id: group.id,
        name: groupName(group),
        color: group.color,
        sort: group.sort,
      }))

    await api.accounts.save({
      accounts: patches,
      groups: groupPayload,
      deleted_groups: Object.keys(deletedGroups),
      memberships: [],
    })

    for (const key of Object.keys(draft)) delete draft[key]
    for (const key of Object.keys(groupNameEdits)) delete groupNameEdits[key]
    for (const key of Object.keys(deletedGroups)) delete deletedGroups[key]
    newGroups.value = []
    await Promise.all([loadAccounts(), loadGroups(true)])
    toast(`配置已保存（${patches.length} 个账号）`)
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    saving.value = false
  }
}

/* ------------------------------------------------------------------ */
/* Lifecycle                                                           */
/* ------------------------------------------------------------------ */

onMounted(async () => {
  cooldownTicker = window.setInterval(() => {
    nowMs.value = Date.now()
  }, 30_000)
  window.addEventListener('pointerdown', onDocumentPointerDown)
  window.addEventListener('keydown', onWindowKeydown)
  window.addEventListener('scroll', closeMenu)
  window.addEventListener('resize', closeMenu)
  await Promise.all([loadPlugins(), loadProxies()])
  await Promise.all([loadGroups(), loadAccounts()])
})

onUnmounted(() => {
  window.clearInterval(cooldownTicker)
  window.removeEventListener('pointerdown', onDocumentPointerDown)
  window.removeEventListener('keydown', onWindowKeydown)
  window.removeEventListener('scroll', closeMenu)
  window.removeEventListener('resize', closeMenu)
  for (const timer of timers.values()) window.clearTimeout(timer)
  timers.clear()
  stopPolling()
  const session = loginSession.value
  if (session && loginPlugin.value && isLoginOpenStatus(loginStatus.value)) {
    void api.accounts.loginCancel(loginPlugin.value, session.session_id).catch(() => undefined)
  }
})
</script>

<template>
  <div class="page accounts-page">
    <div v-if="error" class="notice error">
      <span>账号页读取或保存失败，可重试；下面是宿主返回的原始信息：{{ error }}</span>
      <button type="button" class="btn sm ghost" @click="refreshAll">重试</button>
    </div>
    <div v-if="pluginErrorList.length > 0" class="notice warn small">
      账号读取失败：{{ pluginErrorList.map((item) => `${item.id}（${item.message}）`).join('；') }}
    </div>
    <div v-if="offlinePlugins.length > 0" class="notice warn small">
      {{ offlinePlugins.join('、') }} 未运行，相关账号显示为离线快照，即时操作已禁用。
    </div>

    <section class="card">
      <div class="card-body tight">
        <div class="toolbar accounts-toolbar">
          <div class="field">
            <label for="account-plugin">插件</label>
            <select id="account-plugin" v-model="pluginFilter" class="select">
              <option value="">全部插件</option>
              <option v-for="plugin in plugins" :key="plugin.manifest?.id" :value="plugin.manifest?.id">
                {{ plugin.manifest?.name || plugin.manifest?.id }}
              </option>
            </select>
          </div>
          <div class="field grow">
            <label for="account-search">搜索</label>
            <input
              id="account-search"
              v-model="search"
              class="input"
              type="search"
              placeholder="别名 / 账号 ID / 插件"
              autocomplete="off"
              spellcheck="false"
            />
          </div>
          <div class="spacer"></div>
          <div class="group-pane group-bar" role="group" aria-label="账号分组" :class="{ 'is-collapsed': groupsCollapsed }">
            <div class="group-head">
              <button
                type="button"
                class="group-toggle"
                :aria-expanded="!groupsCollapsed"
                aria-controls="account-group-list"
                @click="groupsCollapsed = !groupsCollapsed"
              >
                <span class="field-label">分组</span>
                <span class="small muted">{{ groupsCollapsed ? '展开全部' : '收起全部' }}</span>
              </button>
            </div>
            <div v-show="!groupsCollapsed" id="account-group-list" class="group-body">
              <div v-if="groupsError" class="group-error">
                <span class="small text-danger">{{ groupsError }}</span>
                <button type="button" class="btn sm ghost" @click="loadGroups()">重试</button>
              </div>
              <div v-else-if="groupsLoading && groups.length === 0" class="small muted">读取分组…</div>
              <div class="group-list" role="group" aria-label="账号分组">
                <button
                  type="button"
                  class="group-item"
                  :class="{ active: groupFilter === '' }"
                  @click="groupFilter = ''"
                >
                  <span class="group-name">全部</span>
                  <span class="group-count">{{ scopedRows.length }}</span>
                </button>

                <template v-for="group in sidebarGroups" :key="group.id">
                  <div class="group-row" :class="{ active: groupFilter === group.id }">
                    <button
                      type="button"
                      class="group-item"
                      :class="{ active: groupFilter === group.id }"
                      :aria-pressed="groupFilter === group.id"
                      @click="groupFilter = group.id"
                    >
                      <span
                        class="group-dot"
                        :style="{ background: group.color || 'var(--accent)' }"
                        aria-hidden="true"
                      ></span>
                      <span class="group-name">{{ groupName(group) }}</span>
                      <span class="group-count">{{ countFor(group.id) }}</span>
                    </button>
                    <span class="group-tools">
                      <button
                        type="button"
                        class="btn sm ghost"
                        :aria-label="`重命名分组 ${groupName(group)}`"
                        @click.stop="groupRename(group)"
                      >
                        改名
                      </button>
                      <button
                        type="button"
                        class="btn sm ghost group-delete"
                        :aria-label="`删除分组 ${groupName(group)}`"
                        @click.stop="askGroupDelete(group)"
                      >
                        删除
                      </button>
                    </span>
                  </div>
                  <div v-if="groupRenaming === group.id" class="group-inline">
                    <input
                      id="group-rename-input"
                      v-model="groupRenameValue"
                      class="input"
                      :aria-label="`分组名称 ${group.name}`"
                      @keydown.enter.prevent="commitGroupRename(group)"
                      @keydown.esc.prevent="cancelGroupRename"
                    />
                    <div class="btn-row">
                      <button type="button" class="btn sm" @click="commitGroupRename(group)">保存</button>
                      <button type="button" class="btn sm ghost" @click="cancelGroupRename">取消</button>
                    </div>
                  </div>
                  <div v-if="groupDeleteConfirm[group.id]" class="group-inline warn">
                    <span class="small">{{ totalGroupCount(group.id) }} 个账号将变为未分组</span>
                    <div class="btn-row">
                      <button type="button" class="btn sm danger" @click="confirmGroupDelete(group)">确认删除</button>
                      <button type="button" class="btn sm ghost" @click="groupDeleteConfirm[group.id] = false">取消</button>
                    </div>
                  </div>
                </template>

                <div v-for="pending in newGroups" :key="pending.key" class="group-row">
                  <button
                    type="button"
                    class="group-item"
                    :class="{ active: groupFilter === pending.key }"
                    :aria-pressed="groupFilter === pending.key"
                    @click="groupFilter = pending.key"
                  >
                    <span class="group-dot" aria-hidden="true"></span>
                    <span class="group-name">{{ pending.name }}</span>
                    <span class="badge muted plain">待创建</span>
                  </button>
                  <span class="group-tools">
                    <button
                      type="button"
                      class="btn sm ghost"
                      :aria-label="`移除待创建分组 ${pending.name}`"
                      @click="removeNewGroup(pending.key)"
                    >
                      移除
                    </button>
                  </span>
                </div>
              </div>

              <form class="group-create" @submit.prevent="addNewGroup">
                <input
                  v-model="newGroupName"
                  id="new-group-name"
                  name="new-group-name"
                  class="input"
                  placeholder="新分组名称"
                  aria-label="新分组名称"
                />
                <button type="submit" class="btn sm" :disabled="!newGroupName.trim()">新建分组</button>
              </form>
              <p v-if="!groupsLoading && !groupsError && sidebarGroups.length === 0 && newGroups.length === 0" class="small muted">
                还没有分组，账号默认未分组。
              </p>
            </div>
          </div>

          <div class="btn-row">
            <button type="button" class="btn ghost" :disabled="loading" @click="refreshAll">
              {{ loading ? '刷新中…' : '刷新' }}
            </button>
            <button
              type="button"
              class="btn"
              aria-haspopup="dialog"
              :aria-expanded="loginStateOpen"
              @click="toggleLoginState"
            >
              登录态
            </button>
            <button
              type="button"
              class="btn"
              :disabled="plugins.length === 0"
              :title="plugins.length === 0 ? '当前没有已安装的插件，请先在插件管理页安装并启用' : '选择插件并发起登录'"
              @click="openLogin()"
            >
              添加账号 / 登录
            </button>
            <button
              type="button"
              class="btn primary"
              :disabled="dirtyCount === 0 || saving"
              :title="dirtyCount === 0 ? '没有未保存的更改' : '提交别名、分组、优先级/权重与 IP 绑定'"
              @click="saveAll"
            >
              {{ saving ? '保存中…' : '保存配置' }}
              <span v-if="dirtyCount > 0" class="dirty-count">{{ dirtyCount }}</span>
            </button>
          </div>
        </div>

        <div v-if="dirtyCount > 0" class="dirty-hint">
          <span class="badge warn plain">未保存的更改 {{ dirtyCount }} 项</span>
          <span class="small muted">别名、分组、优先级/权重与 IP 绑定进入草稿；签到/续期/启停/删除为即时动作。</span>
          <div class="spacer"></div>
          <button type="button" class="btn sm primary" :disabled="saving" @click="saveAll">
            {{ saving ? '保存中…' : '保存配置' }}
          </button>
          <button type="button" class="btn sm ghost" :disabled="saving" @click="discardDrafts">放弃</button>
        </div>
      </div>
    </section>

    <section v-if="loginOpen" class="login-panel">
      <div class="login-row">
        <div class="field login-plugin-field">
          <label for="login-plugin">添加账号</label>
          <select id="login-plugin" v-model="loginPlugin" name="login-plugin" class="select">
            <option v-for="plugin in plugins" :key="plugin.manifest?.id" :value="plugin.manifest?.id">
              {{ plugin.manifest?.name || plugin.manifest?.id }}
            </option>
          </select>
        </div>
        <span
          v-if="currentPlugin"
          class="badge plain"
          :class="loginPluginRunning ? 'ok' : 'muted'"
        >
          {{ loginPluginRunning ? '插件运行中' : '插件未运行' }}
        </span>
        <div v-if="loginMethods.length > 0" class="login-methods">
          <button
            v-for="item in loginMethods"
            :key="item.method"
            type="button"
            class="login-method"
            :class="{ active: loginMethod === item.method }"
            :disabled="loginBusy"
            @click="loginMethod = item.method"
          >
            {{ item.label }}
          </button>
        </div>
        <button
          v-if="loginMethods.length > 0"
          type="button"
          class="btn primary"
          :disabled="!loginPluginRunning || loginBusy"
          @click="startLogin"
        >
          {{ loginBusy ? '请求中…' : loginSession ? '重新开始' : '开始登录' }}
        </button>
        <span v-if="loginMethods.length > 0 && !loginPluginRunning" class="small muted">
          插件未运行，无法发起登录。
        </span>
        <div class="spacer"></div>
        <button type="button" class="btn sm ghost" :disabled="loginBusy" @click="cancelLogin">取消</button>
      </div>

      <div v-if="loginMethods.length === 0" class="notice warn small">
        该插件没有声明登录方式，请在插件文档中配置账号
      </div>

      <div v-if="loginSession" class="login-session">
        <p v-if="loginSession.instructions" class="small">{{ loginSession.instructions }}</p>
        <div v-if="loginSession.url" class="row">
          <a class="btn sm" :href="loginSession.url" target="_blank" rel="noopener">打开登录页</a>
          <span class="small muted">已打开浏览器？完成后会自动检测</span>
        </div>
        <div v-if="loginUrl" class="login-link">
          <label class="login-link-label" for="login-url">登录链接</label>
          <div class="login-link-row">
            <input
              id="login-url"
              class="input mono login-link-input"
              :value="loginUrl"
              type="text"
              readonly
              autocomplete="off"
              spellcheck="false"
              aria-label="登录链接"
              @click="selectLoginUrl"
              @focus="selectLoginUrl"
            />
            <button type="button" class="btn sm" @click="copyLoginUrl">复制链接</button>
          </div>
          <span class="help">在浏览器打开该链接完成登录；链接仅本次有效</span>
        </div>
        <p v-if="loginSession.code" class="small">
          验证码：<code>{{ loginSession.code }}</code>
        </p>

        <div v-if="(loginSession.fields?.length ?? 0) > 0" class="login-form">
          <div v-for="field in loginSession.fields" :key="field.name" class="field">
            <label :for="`login-field-${field.name}`">
              {{ field.label || field.name }}
              <span v-if="field.required" class="muted">*</span>
            </label>
            <input
              :id="`login-field-${field.name}`"
              v-model="loginValues[field.name]"
              class="input mono"
              :type="field.type === 'password' ? 'password' : 'text'"
              :placeholder="field.placeholder || ''"
              :disabled="loginBusy"
              autocomplete="off"
              spellcheck="false"
              @keyup.enter="submitLogin"
            />
            <span v-if="field.help" class="help">{{ field.help }}</span>
          </div>
          <div class="btn-row">
            <button type="button" class="btn primary" :disabled="loginBusy" @click="submitLogin">
              {{ loginBusy ? '提交中…' : '提交' }}
            </button>
          </div>
        </div>

        <div
          v-else-if="loginSession.method === 'oauth' && loginStatus === 'pending'"
          class="login-form"
        >
          <div class="field">
            <label for="login-oauth-fallback">回调页打不开？粘贴地址栏 URL</label>
            <input
              id="login-oauth-fallback"
              v-model="oauthFallback"
              class="input mono"
              type="text"
              placeholder="http://127.0.0.1:端口/oauth/callback?code=..."
              :disabled="loginBusy"
              autocomplete="off"
              spellcheck="false"
              @keyup.enter="submitOAuthFallback"
            />
            <span class="help">
              浏览器提示 127.0.0.1 拒绝连接时，地址栏里的 URL 仍然带着 code，直接粘贴即可完成登录。
            </span>
          </div>
          <div class="btn-row">
            <button type="button" class="btn primary" :disabled="loginBusy" @click="submitOAuthFallback">
              {{ loginBusy ? '提交中…' : '用回调地址完成登录' }}
            </button>
          </div>
        </div>

        <div class="row between">
          <span class="small muted">
            <template v-if="loginStatus === 'pending'">等待完成登录…</template>
            <template v-else-if="loginStatus === 'need_input'">等待输入…</template>
            <template v-else-if="loginStatus === 'failed'">登录失败，可重试或取消。</template>
          </span>
          <span v-if="loginDetected.length > 0" class="small muted">
            检测到：{{ loginDetected.join('、') }}
          </span>
        </div>
      </div>

      <div v-if="loginError" class="notice error small">{{ loginError }}</div>
    </section>

      <section class="accounts-main">
        <div class="card">
          <div v-if="loading && accounts.length === 0" class="log-skeleton accounts-loading" aria-hidden="true">
            <span v-for="n in 6" :key="n" class="skeleton skeleton-line"></span>
          </div>
          <div v-else-if="filteredRows.length === 0" class="empty">
            <strong>{{ accounts.length > 0 ? '当前筛选下没有账号' : '没有可显示的账号' }}</strong>
            <span v-if="plugins.length === 0">请先安装并启用插件。</span>
            <span v-else-if="search || groupFilter || pluginFilter">调整筛选条件后再试。</span>
            <span v-else>插件尚未上报账号，或插件当前未运行。</span>
            <button
              v-if="search || groupFilter || pluginFilter"
              type="button"
              class="btn sm"
              @click="resetFilters"
            >
              清除筛选
            </button>
          </div>

          <template v-else>
            <div class="table-wrap accounts-table-wrap">
              <table class="table accounts-table">
                <thead>
                  <tr>
                    <th class="col-check">
                      <input
                        type="checkbox"
                        id="accounts-select-all"
                        name="accounts-select-all"
                        :checked="allSelected"
                        :indeterminate="someSelected"
                        :disabled="filteredRows.length === 0"
                        aria-label="全选当前筛选的账号"
                        @change="toggleAll"
                      />
                    </th>
                    <th class="col-name">别名</th>
                    <th>插件</th>
                    <th>分组</th>
                    <th>状态</th>
                    <th class="col-quota">额度</th>
                    <th>到期 / 重置</th>
                    <th>IP</th>
                    <th class="col-actions"><span class="sr-only">操作</span></th>
                  </tr>
                </thead>
                <tbody>
                  <template v-for="row in filteredRows" :key="row.key">
                    <tr
                      class="account-row"
                      :class="{
                        'is-stale': rowLocked(row),
                        'is-dirty': isDirty(row),
                        'is-disabled': !isEnabled(row),
                      }"
                    >
                      <td class="col-check">
                        <input
                          v-model="selected[row.key]"
                          type="checkbox"
                          :name="`select-${row.key}`"
                          :aria-label="`选择 ${displayName(row)}`"
                        />
                      </td>

                      <td class="col-name">
                        <div v-if="editingKey === row.key" class="alias-editor">
                          <input
                            id="account-alias-input"
                            v-model="aliasEditValue"
                            class="input input-compact"
                            :placeholder="row.label || row.id"
                            :aria-label="`编辑别名：${row.label || row.id}`"
                            @keydown.enter.prevent="commitAlias(row)"
                            @keydown.esc.prevent="cancelAlias"
                            @blur="commitAlias(row)"
                          />
                        </div>
                        <template v-else>
                          <div class="name-line">
                            <button
                              type="button"
                              class="alias-btn"
                              :title="`点击重命名；当前：${displayName(row)}`"
                              @click="startAlias(row)"
                            >
                              {{ displayName(row) }}
                              <span class="alias-pencil" aria-hidden="true">✎</span>
                            </button>
                            <span
                              v-if="isDirty(row)"
                              class="dirty-dot"
                              title="有未保存的更改"
                              aria-label="有未保存的更改"
                            ></span>
                          </div>
                          <div class="small muted mono account-id">{{ row.id }}</div>
                        </template>
                      </td>

                      <td>
                        <span class="badge plain mono plugin-badge">{{ row.plugin_id || '—' }}</span>
                        <div v-if="rowLocked(row)" class="small muted">
                          {{ row.stale ? '离线快照' : '插件未运行' }}
                        </div>
                      </td>

                      <td>
                        <select
                          class="select select-compact"
                          :name="`group-${row.key}`"
                          :value="rowGroup(row)"
                          :aria-label="`分组：${displayName(row)}`"
                          @change="onGroupChange(row, $event)"
                        >
                          <option value="">未分组</option>
                          <option v-for="group in sidebarGroups" :key="group.id" :value="group.id">
                            {{ groupName(group) }}
                          </option>
                          <option v-for="pending in newGroups" :key="pending.key" :value="pending.key">
                            {{ pending.name }}（待创建）
                          </option>
                        </select>
                      </td>

                      <td class="num">
                        <div class="row badge-cluster">
                          <span class="badge" :class="stateTone(row)" :title="stateTitle(row)">
                            {{ stateText(row) }}
                          </span>
                          <span
                            v-if="row.state_source === 'host' && cooldownLeft(row)"
                            class="badge muted plain"
                          >{{ cooldownLeft(row) }}</span>
                          <span v-if="row.stale" class="badge muted plain" title="插件离线，显示上次快照">快照</span>
                          <span v-if="!isEnabled(row)" class="badge muted plain">已禁用</span>
                        </div>
                      </td>

                      <td class="col-quota num">
                        <div class="quota-cell">
                          <template v-if="hasQuota(row)">
                            <div class="quota-bar">
                              <div
                                class="quota-fill"
                                :class="quotaTone(row)"
                                :style="{ width: `${quotaPercent(row)}%` }"
                              ></div>
                            </div>
                            <div class="row between small quota-meta">
                              <strong>{{ quotaPercent(row) }}%</strong>
                              <span class="muted">
                                {{ formatNumber(row.quota?.remaining) }} / {{ formatNumber(row.quota?.total) }}
                              </span>
                            </div>
                          </template>
                          <span v-else class="small muted" title="该渠道未上报额度">未上报</span>
                        </div>
                      </td>

                      <td class="num">
                        <div class="expiry" :class="expiryClass(row)">
                          <span>{{ expiryText(row) }}</span>
                          <span class="small muted">重置 {{ formatDate(row.quota?.reset_at) }}</span>
                        </div>
                      </td>

                      <td>
                        <div class="egress-cell">
                          <span>{{ egressLabel(row) }}</span>
                          <span
                            v-if="row.egress_health && row.egress_health.status !== 'ok'"
                            class="small"
                            :class="row.egress_health.status === 'failed' ? 'text-danger' : 'muted'"
                            :title="egressHealthTitle(row)"
                          >{{ egressHealthText(row) }}</span>
                          <span v-else-if="egressExitIp(row)" class="small muted mono">{{ egressExitIp(row) }}</span>
                          <span
                            v-if="egressDirty(row)"
                            class="dirty-dot"
                            title="出口已修改，保存后生效"
                            aria-label="出口未保存"
                          ></span>
                        </div>
                      </td>

                      <td class="col-actions">
                        <template v-if="deleteConfirm[row.key]">
                          <div class="inline-confirm">
                            <span class="small text-danger text-danger">删除后需重新登录</span>
                            <div class="btn-row">
                              <button
                                type="button"
                                class="btn sm danger"
                                :disabled="busy[row.key]"
                                @click="confirmDelete(row)"
                              >
                                确认删除
                              </button>
                              <button
                                type="button"
                                class="btn sm ghost"
                                @click="deleteConfirm[row.key] = false"
                              >
                                取消
                              </button>
                            </div>
                          </div>
                        </template>
                        <span v-else class="row-menu-wrap" data-row-menu>
                          <button
                            type="button"
                            class="btn sm ghost menu-trigger"
                            aria-haspopup="menu"
                            :aria-expanded="menu?.key === row.key"
                            :aria-label="`更多操作：${displayName(row)}`"
                            @click.stop="toggleMenu(row, $event)"
                          >
                            ⋯
                          </button>
                        </span>
                        <span v-if="busy[row.key]" class="small muted">处理中…</span>
                      </td>
                    </tr>

                    <tr v-if="panelOf(row)" class="detail-row">
                      <td :colspan="9">
                        <div class="detail-panel">
                          <div class="detail-head">
                            <strong>{{ displayName(row) }}</strong>
                            <span class="small muted mono">{{ row.plugin_id }} / {{ row.id }}</span>
                            <div class="spacer"></div>
                            <button
                              type="button"
                              class="btn sm ghost"
                              :aria-label="`收起 ${displayName(row)} 详情`"
                              @click="closePanel(row)"
                            >
                              收起
                            </button>
                          </div>

                          <div v-if="panelOf(row) === 'quota'" class="detail-grid">
                            <div class="detail-block">
                              <h4>额度</h4>
                              <dl class="kv">
                                <dt>套餐</dt>
                                <dd>{{ row.quota?.plan || '—' }}</dd>
                                <dt>剩余</dt>
                                <dd>{{ hasQuota(row) ? `${formatNumber(row.quota?.remaining)}（${quotaPercent(row)}%）` : '未上报' }}</dd>
                                <dt>总量</dt>
                                <dd>{{ hasQuota(row) ? formatNumber(row.quota?.total) : '未上报' }}</dd>
                                <dt>重置时间</dt>
                                <dd>{{ row.quota?.reset_at ? formatTimestamp(row.quota.reset_at) : '—' }}</dd>
                                <dt>到期时间</dt>
                                <dd>{{ row.quota?.expires_at ? formatTimestamp(row.quota.expires_at) : '未知' }}</dd>
                                <dt>今日签到</dt>
                                <dd>{{ checkedInText(row) }}</dd>
                                <dt>限流</dt>
                                <dd>{{ row.quota?.rate_limited ? '是' : '否' }}</dd>
                              </dl>
                            </div>

                            <div class="detail-block">
                              <h4>凭据</h4>
                              <dl class="kv">
                                <dt>是否存在</dt>
                                <dd>{{ row.credential ? (row.credential.present ? '是' : '否') : '未知' }}</dd>
                                <dt>到期</dt>
                                <dd>{{ row.credential?.expires_at ? formatTimestamp(row.credential.expires_at) : '—' }}</dd>
                                <dt>可续期</dt>
                                <dd>{{ yesNo(row.credential?.refreshable) }}</dd>
                                <dt>启停覆盖</dt>
                                <dd>
                                  {{
                                    row.enabled_override === null || row.enabled_override === undefined
                                      ? '跟随插件'
                                      : row.enabled_override
                                        ? '强制启用'
                                        : '强制禁用'
                                  }}
                                </dd>
                              </dl>
                            </div>

                            <div class="detail-block">
                              <h4>调度参数 <span class="badge muted plain">草稿</span></h4>
                              <div class="row">
                                <label class="field field-compact" :for="`priority-${row.key}`">
                                  优先级
                                  <input
                                    :id="`priority-${row.key}`"
                                    :name="`priority-${row.key}`"
                                    class="input input-compact"
                                    type="number"
                                    min="0"
                                    step="1"
                                    :value="rowPriority(row)"
                                    @change="onNumberChange(row, $event, 'priority')"
                                  />
                                </label>
                                <label class="field field-compact" :for="`weight-${row.key}`">
                                  权重
                                  <input
                                    :id="`weight-${row.key}`"
                                    :name="`weight-${row.key}`"
                                    class="input input-compact"
                                    type="number"
                                    min="0"
                                    step="0.1"
                                    :value="rowWeight(row)"
                                    @change="onNumberChange(row, $event, 'weight')"
                                  />
                                </label>
                              </div>
                              <p class="small muted">优先级、权重随「保存配置」提交。</p>
                              <p v-if="row.last_error" class="small text-danger text-danger">
                                {{ row.last_error }}
                              </p>
                            </div>
                          </div>

                          <div v-else-if="panelOf(row) === 'egress'" class="detail-panel-inner">
                            <div class="row egress-row">
                              <select
                                class="select egress-select"
                                :value="rowEgressKey(row)"
                                :aria-label="`出口：${displayName(row)}`"
                                @change="onEgressChange(row, $event)"
                              >
                                <option v-for="option in egressOptions(row)" :key="option.value" :value="option.value">
                                  {{ option.label }}
                                </option>
                              </select>
                              <button
                                type="button"
                                class="btn sm"
                                :disabled="!egressDirty(row) || saving"
                                @click="saveAll"
                              >
                                保存配置
                              </button>
                            </div>
                            <p class="small muted">{{ egressInfo(row) }}</p>
                            <p class="small muted">出口绑定进入草稿，点击工具栏「保存配置」统一提交。</p>
                            <p v-if="row.egress?.mode === 'blocked'" class="small text-danger text-danger">
                              严格模式下该账号没有可用代理，签到与模型请求都会被拒绝。
                            </p>
                          </div>

                          <div v-else-if="panelOf(row) === 'identity'" class="detail-panel-inner">
                            <div class="row between">
                              <h4>设备身份</h4>
                              <div class="btn-row">
                                <button type="button" class="btn sm" :disabled="identityLoading[row.key]" @click="toggleIdentityReveal(row)">
                                  {{ identityRevealed[row.key] === false ? '显示' : '隐藏' }}
                                </button>
                                <button type="button" class="btn sm ghost" :disabled="identityLoading[row.key]" @click="loadIdentity(row, identityRevealed[row.key] !== false)">
                                  刷新
                                </button>
                              </div>
                            </div>

                            <div v-if="identityErrors[row.key]" class="notice error small">
                              {{ identityErrors[row.key] }}
                            </div>
                            <div v-else-if="identityLoading[row.key]" class="loading">读取设备身份…</div>
                            <dl v-else class="kv identity-kv">
                              <template v-for="item in identityRows(row)" :key="item.label">
                                <dt>{{ item.label }}</dt>
                                <dd>
                                  <code v-if="item.value">{{ item.value }}</code>
                                  <span v-else class="muted">—</span>
                                  <button
                                    v-if="item.value"
                                    type="button"
                                    class="btn sm ghost copy-btn"
                                    :aria-label="`复制 ${item.label}`"
                                    @click="copyText(item.value)"
                                  >
                                    复制
                                  </button>
                                </dd>
                              </template>
                            </dl>

                            <div class="identity-reset">
                              <template v-if="!identityConfirmOpen(row)">
                                <div class="btn-row">
                                  <button
                                    type="button"
                                    class="btn sm danger"
                                    :disabled="rowLocked(row) || busy[row.key]"
                                    @click="identityConfirm[row.key] = 'request'"
                                  >
                                    重置设备身份
                                  </button>
                                  <button
                                    v-if="canOsIdentity(row)"
                                    type="button"
                                    class="btn sm danger"
                                    :disabled="rowLocked(row) || busy[row.key]"
                                    @click="identityConfirm[row.key] = 'os'"
                                  >
                                    重置整机身份（OS 级）
                                  </button>
                                </div>
                              </template>
                              <template v-else>
                                <div class="notice warn small">
                                  {{
                                    identityConfirm[row.key] === 'os'
                                      ? '会修改本机注册表/客户端文件，必须先关闭官方客户端，同一时间只能执行一个 OS 级操作。'
                                      : '重置后该账号会得到新的设备身份，可能影响现有会话，确认继续？'
                                  }}
                                </div>
                                <div class="btn-row">
                                  <button
                                    type="button"
                                    class="btn sm danger"
                                    :disabled="busy[row.key]"
                                    @click="resetIdentity(row, identityConfirm[row.key] === 'os' ? 'os' : 'request')"
                                  >
                                    {{ busy[row.key] ? '执行中…' : '确认重置' }}
                                  </button>
                                  <button
                                    type="button"
                                    class="btn sm ghost"
                                    :disabled="busy[row.key]"
                                    @click="identityConfirm[row.key] = ''"
                                  >
                                    取消
                                  </button>
                                </div>
                              </template>
                              <span v-if="rowLocked(row)" class="small muted">插件未运行，无法重置设备身份。</span>
                            </div>
                          </div>
                        </div>
                      </td>
                    </tr>
                  </template>
                </tbody>
              </table>
            </div>

            <div v-if="selectedCount > 0" class="batch-bar">
              <div class="batch-row">
                <span class="small">选中 <strong>{{ selectedCount }}</strong> 项</span>
                <div class="btn-row">
                  <button type="button" class="btn sm" :disabled="bulkBusy" @click="runBulk('checkin')">签到</button>
                  <button type="button" class="btn sm" :disabled="bulkBusy" @click="runBulk('refresh')">续期</button>
                  <button type="button" class="btn sm" :disabled="bulkBusy" @click="runBulk('enable')">启用</button>
                  <button type="button" class="btn sm" :disabled="bulkBusy" @click="runBulk('disable')">禁用</button>
                  <select
                    id="bulk-group-select"
                    name="bulk-group-select"
                    class="select select-compact"
                    :value="''"
                    :disabled="bulkBusy"
                    aria-label="加入分组"
                    title="批量加入分组立即生效"
                    @change="onBulkGroup"
                  >
                    <option value="">加入分组…</option>
                    <option v-for="group in persistedGroups" :key="group.id" :value="group.id">
                      {{ groupName(group) }}
                    </option>
                  </select>
                  <select
                    id="bulk-egress-select"
                    name="bulk-egress-select"
                    class="select select-compact"
                    :value="''"
                    :disabled="bulkBusy"
                    aria-label="绑定 IP"
                    title="批量绑定 IP 立即生效"
                    @change="onBulkEgress"
                  >
                    <option value="">绑定 IP…</option>
                    <option v-for="option in bulkEgressOptions" :key="option.value" :value="option.value">
                      {{ option.label }}
                    </option>
                  </select>
                  <template v-if="!bulkDeleteConfirm">
                    <button type="button" class="btn sm danger" :disabled="bulkBusy" @click="askBulkDelete">删除</button>
                  </template>
                  <template v-else>
                    <span class="small text-danger text-danger">删除后需重新登录，确认？</span>
                    <button type="button" class="btn sm danger" :disabled="bulkBusy" @click="runBulk('delete')">
                      确认删除
                    </button>
                    <button type="button" class="btn sm ghost" @click="bulkDeleteConfirm = false">取消</button>
                  </template>
                  <button type="button" class="btn sm ghost" :disabled="bulkBusy" @click="clearSelection">清除选择</button>
                </div>
              </div>

              <div v-if="bulkReport" class="bulk-report">
                <div class="row between">
                  <span class="small">
                    {{ bulkReport.action }}：{{ bulkReport.ok }} 成功 / {{ bulkReport.fail }} 失败
                  </span>
                  <button
                    v-if="bulkReport.items.length > 0"
                    type="button"
                    class="btn sm ghost"
                    :aria-expanded="bulkReportOpen"
                    @click="bulkReportOpen = !bulkReportOpen"
                  >
                    {{ bulkReportOpen ? '收起详情' : '展开详情' }}
                  </button>
                </div>
                <ul v-if="bulkReportOpen" class="bulk-lines">
                  <li
                    v-for="(item, index) in bulkReport.items"
                    :key="index"
                    :class="item.ok ? 'ok' : 'fail'"
                  >
                    {{ item.name }}：{{ item.ok ? '成功' : '失败' }}{{ item.message ? ` · ${item.message}` : '' }}
                  </li>
                </ul>
              </div>
            </div>
          </template>
        </div>
      </section>

    <Transition name="drawer">
    <div v-if="loginStateOpen" class="drawer-backdrop" @click.self="loginStateOpen = false">
      <aside class="drawer" role="dialog" aria-modal="true" aria-labelledby="login-state-title">
        <header class="drawer-head">
          <div>
            <h2 id="login-state-title">插件登录态</h2>
            <p class="small muted">策略来自 manifest，缺失时由宿主推导；检测只读，不触发续期。</p>
          </div>
          <div class="btn-row">
            <button type="button" class="btn sm" :disabled="loginStateLoading" @click="loadLoginState">
              {{ loginStateLoading ? '检测中…' : '重新检测' }}
            </button>
            <button type="button" class="btn sm ghost" aria-label="关闭登录态面板" @click="loginStateOpen = false">
              关闭
            </button>
          </div>
        </header>
        <div class="drawer-body">
          <div v-if="loginStateError" class="notice error small">{{ loginStateError }}</div>
          <div v-else-if="loginStateLoading && loginState.length === 0" class="loading">检测中…</div>
          <div v-else-if="loginState.length === 0" class="empty">
            <strong>暂无登录态信息</strong>
            <span>插件未安装或尚未检测。</span>
            <button type="button" class="btn sm ghost" :disabled="loginStateLoading" @click="loadLoginState">
              立即重新检测
            </button>
            <button type="button" class="btn sm ghost" @click="openLogin()">添加账号 / 登录</button>
          </div>
          <div v-else class="login-state-list">
            <article v-for="entry in loginState" :key="entry.plugin_id" class="login-state-card">
              <header class="row between">
                <div>
                  <strong>{{ entry.name || entry.plugin_id }}</strong>
                  <div class="small muted mono">{{ entry.plugin_id }} · API L{{ entry.api_level ?? '—' }}</div>
                </div>
                <div class="btn-row">
                  <span class="badge plain" :class="entry.source === 'manifest' ? 'accent' : 'muted'">
                    {{ entrySource(entry) }}
                  </span>
                  <button type="button" class="btn sm" @click="goLogin(entry.plugin_id)">去登录</button>
                </div>
              </header>
              <dl class="kv">
                <dt>登录方式</dt>
                <dd>{{ entryMethods(entry) }}</dd>
                <dt>读取本机登录态</dt>
                <dd>{{ yesNo(entry.reads_local_state) }}</dd>
                <dt>依赖官方客户端</dt>
                <dd>{{ yesNo(entry.requires_app) }}</dd>
                <dt>自动续期</dt>
                <dd>{{ yesNo(entry.auto_refresh) }}</dd>
                <dt>最近检测</dt>
                <dd>{{ entry.checked_at ? formatTimestamp(entry.checked_at) : '—' }}</dd>
              </dl>
              <p v-if="entry.notes" class="small muted">{{ entry.notes }}</p>
            </article>
          </div>
        </div>
      </aside>
    </div>
    </Transition>

    <div v-if="menuRow" class="row-menu" role="menu" data-row-menu ref="menuEl" :style="{ top: `${menu?.top ?? 0}px`, left: `${menu?.left ?? 0}px` }">
      <button type="button" role="menuitem" @click="withMenuRow((row) => openPanel(row, 'quota'))">
        查看额度详情
      </button>
      <button type="button" role="menuitem" @click="withMenuRow(startAlias)">
        重命名 <span class="menu-tag">改名</span>
      </button>
      <button
        type="button"
        role="menuitem"
        :disabled="menuCannotReadIdentity()"
        :title="menuCannotReadIdentity() ? '该插件未提供设备身份能力' : ''"
        @click="withMenuRow((row) => openPanel(row, 'identity'))"
      >
        设备身份详情
      </button>
      <button
        type="button"
        role="menuitem"
        :disabled="menuCannotBindEgress()"
        :title="menuCannotBindEgress() ? '该插件未声明出口（egress）能力' : ''"
        @click="withMenuRow((row) => openPanel(row, 'egress'))"
      >
        绑定 IP <span class="menu-tag">草稿</span>
      </button>
      <div class="menu-sep"></div>
      <button type="button" role="menuitem" :disabled="menuLocked()" @click="withMenuRow(checkinRow)">
        签到 <span class="menu-tag">即时</span>
      </button>
      <button
        v-if="menuCanRefresh()"
        type="button"
        role="menuitem"
        :disabled="menuLocked()"
        @click="withMenuRow(refreshRow)"
      >
        凭证续期 <span class="menu-tag">即时</span>
      </button>
      <button type="button" role="menuitem" :disabled="menuLocked()" @click="withMenuRow(toggleAccount)">
        {{ menuEnabled() ? '禁用' : '启用' }} <span class="menu-tag">即时</span>
      </button>
      <button
        type="button"
        role="menuitem"
        :disabled="menuLocked() || menuCannotReadIdentity()"
        :title="menuCannotReadIdentity() ? '该插件未提供设备身份能力' : ''"
        @click="withMenuRow(openIdentityReset)"
      >
        重置设备身份 <span class="menu-tag">即时</span>
      </button>
      <div class="menu-sep"></div>
      <button
        type="button"
        role="menuitem"
        class="menu-danger"
        :disabled="menuLocked()"
        @click="withMenuRow(askDelete)"
      >
        删除 <span class="menu-tag">即时</span>
      </button>
    </div>

    <div class="toasts" role="status" aria-live="polite">
      <div v-for="item in toasts" :key="item.id" class="toast" :class="item.tone">
        {{ item.text }}
      </div>
    </div>
  </div>
</template>

<style scoped>
.badge-cluster {
  gap: var(--space-1);
}

.egress-row {
  gap: var(--space-2);
}

.login-link {
  display: flex;
  flex-direction: column;
  gap: 6px;
  max-width: 560px;
}

.login-link-label {
  font-size: var(--text-xs);
  font-weight: 500;
  color: var(--text-soft);
}

.login-link .help {
  font-size: var(--text-xs);
  color: var(--muted);
}

.login-link-row {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.login-link-input {
  flex: 1;
  min-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  cursor: text;
}

.login-link-row .btn {
  flex: none;
}
</style>
