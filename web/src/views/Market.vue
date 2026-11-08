<script setup lang="ts">
// 市场（P2-3 改版，P2-6）：三源插件市场 —— 搜索 / 安装 / 已安装（含更新徽章与行内更新）。
// 后端契约（task-15 固定）：search / install / updates 三路由；已安装列表由
// /api/market/updates 派生（id=包名、from=已装版本、to=最新版、available=是否有更新）——
// 后端没有独立的 /installed 路由，也不存在「执行更新」端点（task-23 更新由宿主定时器
// 按 prompt/auto 策略处理），因此行内「更新」按钮做「重查 + 说明」，真执行留给宿主。
import { onMounted, ref } from 'vue'
import {
  api,
  errorMessage,
  normalizeMarketSearch,
  normalizeMarketInstalled,
  normalizeMarketUpdates,
  type MarketSearchResult,
  type MarketInstalledRow,
  type MarketSource,
  type MarketUpdateItem,
} from '../api'

const query = ref('')
const source = ref<MarketSource>('npm')
const localPath = ref('')
const sourceOptions: { value: MarketSource; label: string }[] = [
  { value: 'npm', label: 'npm' },
  { value: 'github', label: 'GitHub' },
  { value: 'local', label: '本地目录' },
]

const results = ref<MarketSearchResult[]>([])
/** magpie 官方社区包前缀（对齐 magpie plugin/updates.go:77 的 Official()）：启用/更新策略只认这批。 */
const OFFICIAL_PREFIX = '@magpie-community/'
function isOfficial(name: string): boolean {
  return (name || '').startsWith(OFFICIAL_PREFIX)
}

const searching = ref(false)
/** 已安装市场包：由 /api/market/updates 派生（一行一包，含 from→to）。 */
const installed = ref<MarketInstalledRow[]>([])
/** 更新可用性按 id 索引（来自 /api/market/updates）。 */
const updatesById = ref<Record<string, MarketUpdateItem>>({})
const checkingUpdates = ref(false)
const installingSpec = ref('')
const updatingId = ref('')
const error = ref('')
const message = ref('')
/** 接口 404/501/断连提示：后端已实现，404 只会出现在旧 exe 上 → 提示重启程序。 */
const backendOffline = ref('')

async function run<T>(fn: () => Promise<T>, offlineCheck?: (e: unknown) => boolean): Promise<T | undefined> {
  try {
    const value = await fn()
    // 成功调用后自动清除「接口未就绪」提示（旧 404 只会在重启前残留）
    backendOffline.value = ''
    return value
  } catch (e) {
    const status = (e as { status?: number }).status
    if (offlineCheck?.(e) || status === 404 || status === 501 || status === 0) {
      backendOffline.value = `市场接口需要重启程序后生效——若刚更新过程序请重启 AIWorkGate（HTTP ${status ?? 0}）。`
    } else {
      error.value = errorMessage(e)
    }
    return undefined
  }
}

async function search() {
  const q = query.value.trim()
  if (!q) return
  searching.value = true
  error.value = ''
  message.value = ''
  results.value = []
  const data = await run(() => api.market.search(q), () => true)
  results.value = normalizeMarketSearch(data)
  searching.value = false
  if (!results.value.length && !backendOffline.value) message.value = `没有找到与「${q}」匹配的包。`
}

async function install(pkg: MarketSearchResult) {
  if (installingSpec.value) return
  installingSpec.value = pkg.name
  error.value = ''
  message.value = ''
  const spec = source.value === 'local' ? localPath.value.trim() || pkg.name : pkg.name
  const data = await run(() =>
    api.market.install({
      spec,
      source: source.value,
      ...(source.value === 'local' && localPath.value.trim() ? { localPath: localPath.value.trim() } : {}),
    }),
  )
  if (data?.ok) {
    const { id, version } = data as { id?: string; version?: string }
    message.value = `已安装「${id ?? pkg.name}」${version ? `@${version}` : ''}。默认停用——请到「插件」页启用并启动（T3）。`
    installingSpec.value = ''
    await checkUpdates()
    return
  }
  if (data && (data as { ok?: boolean }).ok === undefined) {
    error.value = '安装返回异常：ok 字段缺失，请与后端核对 /api/market/install 契约。'
  }
  installingSpec.value = ''
}

async function checkUpdates() {
  checkingUpdates.value = true
  error.value = ''
  message.value = ''
  const [rawInstalled, rawUpdates] = await Promise.all([
    run(() => api.market.installed(), () => true),
    run(() => api.market.updates(), () => true),
  ])
  // 全量已装来自 /api/market/installed（覆盖 npm/GitHub/本地）；更新徽章仍由 updates 派生。
  const rows = normalizeMarketInstalled(rawInstalled)
  if (rows.length) {
    installed.value = rows
    const map: Record<string, MarketUpdateItem> = {}
    for (const u of normalizeMarketUpdates(rawUpdates)) map[u.id] = u
    updatesById.value = map
  } else {
    // 旧 exe 无 /installed → 退回 updates 派生（只含 market_installs）。
    const only = normalizeMarketUpdates(rawUpdates)
    installed.value = only.map((u) => ({ id: u.id, name: u.id, version: u.from, source: 'npm' }))
    const map: Record<string, MarketUpdateItem> = {}
    for (const u of only) map[u.id] = u
    updatesById.value = map
  }
  checkingUpdates.value = false
}

/** 该包是否有新版（available=true 且 to 与已装 from 不同）。 */
function hasUpdate(id: string): boolean {
  const u = updatesById.value[id]
  return u?.available === true && Boolean(u.to) && u.to !== u.from
}

/**
 * 行内「更新」（P2-6b）：POST /api/market/update {id} → {ok, from, to, rolledBack}。
 * 契约契约：健康检查失败时宿主自动回滚，rolledBack=true → 显示「更新失败已回滚到旧版本」；
 * 成功则行内徽章消失（重查 updates 后该包不再有 from→to 差异）并提示新版本。
 */
async function requestUpdate(id: string) {
  if (updatingId.value || checkingUpdates.value) return
  updatingId.value = id
  message.value = ''
  error.value = ''
  const data = await run(() => api.market.update(id))
  if (data?.ok === true) {
    if (data.rolledBack === true) {
      error.value = `「${id}」更新失败已回滚到旧版本（${data.from ?? '—'}）。请检查插件安装目录后重试。`
    } else {
      message.value = `「${id}」已更新 ${data.from ?? '—'} → ${data.to ?? '—'}。插件默认停用，请到「插件」页启用并启动。`
    }
    updatingId.value = ''
    await checkUpdates()
    return
  }
  if (data && (data as { ok?: boolean }).ok === undefined) {
    error.value = `「${id}」更新返回异常：ok 字段缺失，请与后端核对 /api/market/update 契约。`
  }
  updatingId.value = ''
}

onMounted(() => void checkUpdates())
</script>

<template>
  <div class="page">
    <div v-if="backendOffline" class="notice warn">{{ backendOffline }}</div>
    <div v-if="error" class="notice error">
      <span>{{ error }}</span>
      <button type="button" class="btn sm ghost" @click="search">重试搜索</button>
    </div>
    <div v-if="message" class="notice ok">{{ message }}</div>

    <!-- ① 浏览搜索：搜索框 + 三源切换 + 结果列表 + 行内安装 -->
    <section class="card">
      <div class="card-head">
        <h2>搜索插件</h2>
        <span class="sub">来源：npm registry · GitHub · 本地目录</span>
      </div>
      <div class="card-body">
        <div class="row">
          <div class="field">
            <span class="field-label">包名 / 关键词</span>
            <input
              v-model="query"
              class="input"
              type="text"
              placeholder="例如 opencode-trae-auth"
              @keyup.enter="search"
            />
          </div>
          <div class="field">
            <span class="field-label">来源</span>
            <div class="seg">
              <button
                v-for="opt in sourceOptions"
                :key="opt.value"
                type="button"
                class="seg-btn"
                :class="{ active: source === opt.value }"
                @click="source = opt.value"
              >
                {{ opt.label }}
              </button>
            </div>
          </div>
          <div class="field">
            <span class="field-label">&nbsp;</span>
            <button type="button" class="btn primary" :disabled="!query.trim() || searching" @click="search">
              {{ searching ? '搜索中…' : '搜索' }}
            </button>
          </div>
        </div>
        <div v-if="source === 'local'" class="row">
          <div class="field">
            <span class="field-label">本地目录（localPath）</span>
            <input v-model="localPath" class="input" type="text" placeholder="例如 E:\Alltools\Magpine\chajian" />
          </div>
        </div>
      </div>
    </section>

    <!-- 搜索结果 -->
    <section v-if="results.length" class="card">
      <div class="card-head">
        <h2>搜索结果</h2>
        <span class="sub">{{ results.length }} 个结果{{ results.length >= 100 ? '（单次最多返回 100 条，可加关键词缩小范围）' : '' }}</span>
      </div>
      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>包名</th>
              <th>最新版</th>
              <th>描述</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="pkg in results" :key="pkg.name">
              <td class="mono">
                {{ pkg.name }}
                <span v-if="isOfficial(pkg.name)" class="badge">官方</span>
              </td>
              <td><span class="badge">{{ pkg.version }}</span></td>
              <td class="truncate" :title="pkg.description || ''">{{ pkg.description || '—' }}</td>
              <td>
                <button
                  type="button"
                  class="btn sm primary"
                  :disabled="installingSpec === pkg.name"
                  @click="install(pkg)"
                >
                  {{ installingSpec === pkg.name ? '安装中…' : '安装' }}
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div class="card-body">
        <span class="muted">安装按上方「来源」选择执行；本地来源使用左侧 localPath。</span>
      </div>
    </section>

    <!-- ② 已安装市场包（含更新徽章 + 行内更新，替代独立「更新检查」卡片） -->
    <section class="card">
      <div class="card-head">
        <h2>已安装市场包</h2>
        <div class="row">
          <button type="button" class="btn sm" :disabled="checkingUpdates" @click="checkUpdates">
            {{ checkingUpdates ? '检查中…' : '检查更新' }}
          </button>
        </div>
      </div>
      <div v-if="checkingUpdates && installed.length === 0" class="empty">加载中…</div>
      <div v-else-if="installed.length === 0" class="empty">
        <strong>还没有已安装的市场包</strong>
        <span>在上方搜索并安装；安装后默认停用，请到「插件」页启用。每行会标注可用更新（from → to）。</span>
      </div>
      <div v-else class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>包名</th>
              <th>已装版本</th>
              <th>来源</th>
              <th>更新</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in installed" :key="item.id">
              <td class="mono">
                {{ item.id }}
                <span v-if="isOfficial(item.id)" class="badge">官方</span>
              </td>
              <td><span class="badge">{{ item.version || '—' }}</span></td>
              <td><span class="muted">{{ item.source || '—' }}</span></td>
              <td>
                <span v-if="hasUpdate(item.id)" class="badge warn">有更新 {{ updatesById[item.id]?.from }} → {{ updatesById[item.id]?.to }}</span>
                <span v-else class="muted">无更新</span>
              </td>
              <td>
                <button
                  v-if="hasUpdate(item.id)"
                  type="button"
                  class="btn sm"
                  :disabled="checkingUpdates || updatingId === item.id"
                  title="执行更新（健康检查失败会自动回滚到旧版本）"
                  @click="requestUpdate(item.id)"
                >
                  {{ updatingId === item.id ? '更新中…' : '更新' }}
                </button>
                <span v-else class="muted">—</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
  </div>
</template>