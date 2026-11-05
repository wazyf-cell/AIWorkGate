<script setup lang="ts">
// 市场（P2-3）：三源插件市场 —— 搜索 / 安装 / 已装列表 / 更新检查。
// 契约与后端 task-15 对齐；后端未就绪时给出明确提示而不是空白页。
// T3 语义：市场安装默认停用，安装成功后提示去「插件」页启用并启动。
import { onMounted, ref } from 'vue'
import {
  api,
  errorMessage,
  normalizeMarketInstalled,
  normalizeMarketSearch,
  normalizeMarketUpdates,
  type MarketInstalledRow,
  type MarketSearchResult,
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
const searching = ref(false)
const installed = ref<MarketInstalledRow[]>([])
const loadingInstalled = ref(false)
const updates = ref<MarketUpdateItem[]>([])
const checkingUpdates = ref(false)
const installingSpec = ref('')
const error = ref('')
const message = ref('')
/** 后端连不上/未实现时整车可用的提示开关 */
const backendOffline = ref('')

async function run<T>(fn: () => Promise<T>, offlineCheck?: (e: unknown) => boolean): Promise<T | undefined> {
  try {
    return await fn()
  } catch (e) {
    const status = (e as { status?: number }).status
    if (offlineCheck?.(e) || status === 404 || status === 501 || status === 0) {
      backendOffline.value = `市场接口尚未就绪（HTTP ${status ?? 0}）：请确认宿主已实现 /api/market/*（后端 task-15），联调前此处只读当前进程的调用结果。`
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
  backendOffline.value = ''
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
  backendOffline.value = ''
  const spec =
    source.value === 'local'
      ? (localPath.value.trim() || pkg.name)
      : pkg.name
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
    await Promise.all([loadInstalled(), checkUpdates()])
    return
  }
  if (data && (data as { ok?: boolean }).ok === undefined) {
    error.value = '安装返回异常：ok 字段缺失，请与后端核对 /api/market/install 契约。'
  }
  installingSpec.value = ''
}

async function loadInstalled() {
  loadingInstalled.value = true
  const data = await run(() => api.market.installed(), () => true)
  installed.value = normalizeMarketInstalled(data)
  loadingInstalled.value = false
}

async function checkUpdates() {
  checkingUpdates.value = true
  error.value = ''
  message.value = ''
  backendOffline.value = ''
  const data = await run(() => api.market.updates(), () => true)
  updates.value = normalizeMarketUpdates(data)
  checkingUpdates.value = false
}

function policyText(item: MarketUpdateItem): string {
  if (item.available === false) return '无可用更新'
  if (item.available === true) return '可更新'
  return '未知'
}

onMounted(() => {
  void loadInstalled()
  void checkUpdates()
})
</script>

<template>
  <div class="page">
    <div v-if="backendOffline" class="notice warn">{{ backendOffline }}</div>
    <div v-if="error" class="notice error">
      <span>{{ error }}</span>
      <button type="button" class="btn sm ghost" @click="search">重试搜索</button>
    </div>
    <div v-if="message" class="notice ok">{{ message }}</div>

    <!-- 搜索 -->
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
        </div>
        <div v-if="source === 'local'" class="row">
          <div class="field">
            <span class="field-label">本地目录（localPath）</span>
            <input v-model="localPath" class="input" type="text" placeholder="例如 E:\Alltools\Magpine\chajian" />
          </div>
        </div>
        <div class="row">
          <button type="button" class="btn primary" :disabled="!query.trim() || searching" @click="search">
            {{ searching ? '搜索中…' : '搜索' }}
          </button>
        </div>
      </div>
    </section>

    <!-- 搜索结果 -->
    <section v-if="results.length" class="card">
      <div class="card-head">
        <h2>搜索结果</h2>
        <span class="sub">{{ results.length }} 个结果</span>
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
              <td class="mono">{{ pkg.name }}</td>
              <td><span class="badge">{{ pkg.version }}</span></td>
              <td class="truncate">{{ pkg.description || '—' }}</td>
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

    <!-- 已装市场包 -->
    <section class="card">
      <div class="card-head">
        <h2>已安装市场包</h2>
        <div class="row">
          <button type="button" class="btn sm" :disabled="checkingUpdates" @click="checkUpdates">
            {{ checkingUpdates ? '检查中…' : '检查更新' }}
          </button>
        </div>
      </div>
      <div v-if="loadingInstalled" class="empty">加载中…</div>
      <div v-else-if="installed.length === 0" class="empty">
        <strong>还没有已安装的市场包</strong>
        <span>在上方搜索并安装；安装后默认停用，请到「插件」页启用。</span>
      </div>
      <div v-else class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>包名</th>
              <th>来源</th>
              <th>已装版本</th>
              <th>状态</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="pkg in installed" :key="pkg.id">
              <td class="mono">{{ pkg.name }}</td>
              <td>{{ pkg.source || '—' }}</td>
              <td><span class="badge">{{ pkg.version || '—' }}</span></td>
              <td>
                <span v-if="pkg.enabled === true" class="badge ok">已启用</span>
                <span v-else-if="pkg.enabled === false" class="badge muted">已停用</span>
                <span v-else class="muted">—</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <!-- 更新检查 -->
    <section class="card">
      <div class="card-head">
        <h2>更新检查</h2>
        <span class="sub">from → to · 策略：available 由后端 /api/market/updates 给出</span>
      </div>
      <div v-if="updates.length === 0" class="empty">
        <strong v-if="checkingUpdates">检查中…</strong>
        <strong v-else>没有发现可更新的市场包</strong>
        <span v-if="!checkingUpdates">点击上方「检查更新」重新核对。</span>
      </div>
      <div v-else class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>包名</th>
              <th>已装</th>
              <th>可更新到</th>
              <th>策略</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in updates" :key="item.id">
              <td class="mono">{{ item.id }}</td>
              <td><span class="badge">{{ item.from || '—' }}</span></td>
              <td><span class="badge">{{ item.to || '—' }}</span></td>
              <td>
                <span v-if="item.available === true" class="badge warn">{{ policyText(item) }}</span>
                <span v-else class="muted">{{ policyText(item) }}</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
  </div>
</template>