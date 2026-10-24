<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { RouterLink, RouterView, useRoute, useRouter } from 'vue-router'
import { api } from './api'
import AppIcon from './components/AppIcon.vue'

const THEME_KEY = 'aiworkgate.theme'
const MOTION_KEY = 'aiworkgate.motion'
const motionQuery = window.matchMedia('(prefers-reduced-motion: reduce)')

/**
 * Session-scoped guard so a genuinely broken build cannot reload forever:
 * the first stale-chunk failure sets the flag and reloads once, any failure
 * seen while the flag is set only offers a manual reload instead.
 */
const RELOAD_GUARD_KEY = 'aiworkgate.chunk-reload-guard'
const RELOAD_GUARD_TTL_MS = 20000
const CHUNK_ERROR_MARKERS = [
  'Failed to fetch dynamically imported module',
  'Importing a module script failed',
]

const route = useRoute()
const router = useRouter()
const theme = ref<'light' | 'dark'>(
  document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light',
)
const port = ref<number | null>(null)
const version = ref('')

const nav = [
  { to: '/', label: '概览', icon: 'overview' },
  { to: '/plugins', label: '插件', icon: 'plugins' },
  { to: '/accounts', label: '账号', icon: 'accounts' },
  { to: '/checkin', label: '签到', icon: 'checkin' },
  { to: '/usage', label: '用量', icon: 'usage' },
  { to: '/scheduler', label: '调度', icon: 'scheduler' },
  { to: '/pools', label: '资源', icon: 'pools' },
  { to: '/rules', label: '规则', icon: 'rules' },
  { to: '/proxies', label: '代理', icon: 'proxies' },
  { to: '/logs', label: '日志', icon: 'logs' },
  { to: '/settings', label: '设置', icon: 'settings' },
]

/** True once the document has scrolled far enough for the topbar to need a shadow. */
const scrolled = ref(false)

function syncScrolled() {
  scrolled.value = window.scrollY > 4
}

// The /rules view keeps the ruleset brand ("指纹清洗") in the subtitle so the
// page heading stays consistent with every other page ("规则").
const title = computed(() => {
  if (route.name === 'rules') return '规则'
  return (route.meta.title as string | undefined) ?? '概览'
})
const subtitle = computed(() => {
  const meta = (route.meta.subtitle as string | undefined) ?? ''
  if (route.name === 'rules') return meta ? `指纹清洗 · ${meta}` : '指纹清洗'
  return meta
})

function toggleTheme() {
  theme.value = theme.value === 'dark' ? 'light' : 'dark'
  document.documentElement.dataset.theme = theme.value
  try {
    localStorage.setItem(THEME_KEY, theme.value)
  } catch {
    /* storage can be unavailable, the in-memory theme still applies */
  }
}

/* Motion preference. Most Windows installs report `prefers-reduced-motion:
 * reduce` when "animation effects" is off, which silently removes every
 * transition. "自动" keeps following the OS; "开/关" is an explicit override so
 * the panel's motion is reachable without changing a system setting. */
type MotionPref = 'auto' | 'full' | 'reduced'

function readMotion(): MotionPref {
  try {
    const stored = localStorage.getItem(MOTION_KEY)
    if (stored === 'full' || stored === 'reduced') return stored
  } catch {
    /* storage can be unavailable */
  }
  return 'auto'
}

const motionPref = ref<MotionPref>(readMotion())
const motionSystemReduced = ref(motionQuery.matches)

const motionEffective = computed(
  () => motionPref.value === 'reduced' || (motionPref.value === 'auto' && motionSystemReduced.value),
)
const motionLabel = computed(() => {
  if (motionPref.value === 'full') return '开'
  if (motionPref.value === 'reduced') return '关'
  return motionSystemReduced.value ? '跟随系统·关' : '跟随系统·开'
})
const motionTitle = computed(() =>
  motionEffective.value
    ? '界面动效当前关闭（系统设置了“减少动画”或你选择了关）。点击切换。'
    : '界面动效当前开启。点击切换：自动 → 开 → 关。',
)

function applyMotion() {
  document.documentElement.dataset.motion = motionEffective.value ? 'reduced' : 'full'
}

function cycleMotion() {
  motionPref.value =
    motionPref.value === 'auto' ? 'full' : motionPref.value === 'full' ? 'reduced' : 'auto'
  try {
    localStorage.setItem(MOTION_KEY, motionPref.value)
  } catch {
    /* storage can be unavailable, the in-memory choice still applies */
  }
  applyMotion()
}

function onMotionMediaChange() {
  motionSystemReduced.value = motionQuery.matches
  if (motionPref.value === 'auto') applyMotion()
}

applyMotion()

/* ------------------------------------------------------------------ */
/* Stale chunk recovery                                                */
/* ------------------------------------------------------------------ */

type ReloadState = '' | 'reloading' | 'blocked'

const reloadState = ref<ReloadState>('')

let reloadTimer: number | undefined
let guardResetTimer: number | undefined

function errorText(error: unknown): string {
  if (error instanceof Error) return error.message
  if (typeof error === 'string') return error
  return ''
}

function isChunkErrorMessage(message: string): boolean {
  return CHUNK_ERROR_MARKERS.some((marker) => message.includes(marker))
}

function readGuard(): boolean {
  try {
    return sessionStorage.getItem(RELOAD_GUARD_KEY) === '1'
  } catch {
    return true
  }
}

function writeGuard(): boolean {
  try {
    sessionStorage.setItem(RELOAD_GUARD_KEY, '1')
    return true
  } catch {
    return false
  }
}

function clearGuard() {
  try {
    sessionStorage.removeItem(RELOAD_GUARD_KEY)
  } catch {
    /* nothing to clear when storage is unavailable */
  }
}

/** Clears the guard once the page has been stable long enough to prove the build loads. */
function scheduleGuardReset() {
  window.clearTimeout(guardResetTimer)
  guardResetTimer = window.setTimeout(() => {
    if (reloadState.value === '') clearGuard()
  }, RELOAD_GUARD_TTL_MS)
}

function recoverFromStaleChunk() {
  if (reloadState.value === 'reloading') return
  window.clearTimeout(guardResetTimer)
  if (readGuard()) {
    reloadState.value = 'blocked'
    return
  }
  if (!writeGuard()) {
    // Without storage the guard cannot survive a reload, so never auto-reload.
    reloadState.value = 'blocked'
    return
  }
  reloadState.value = 'reloading'
  window.clearTimeout(reloadTimer)
  reloadTimer = window.setTimeout(() => {
    window.location.reload()
  }, 800)
}

function manualReload() {
  clearGuard()
  window.location.reload()
}

function onPreloadError(event: Event) {
  event.preventDefault()
  recoverFromStaleChunk()
}

window.addEventListener('vite:preloadError', onPreloadError)
const removeRouterErrorHook = router.onError((error: unknown) => {
  if (isChunkErrorMessage(errorText(error))) recoverFromStaleChunk()
})

// Each page starts at the top; moving focus to the content region keeps
// screen-reader and keyboard users oriented after the swap.
const removeAfterEach = router.afterEach((to, from) => {
  if (to.path === from.path) return
  window.scrollTo({ top: 0, behavior: 'auto' })
  window.requestAnimationFrame(() => {
    document.querySelector<HTMLElement>('.content')?.focus({ preventScroll: true })
  })
})

scheduleGuardReset()

onUnmounted(() => {
  window.removeEventListener('vite:preloadError', onPreloadError)
  window.removeEventListener('scroll', syncScrolled)
  motionQuery.removeEventListener('change', onMotionMediaChange)
  removeRouterErrorHook()
  removeAfterEach()
  window.clearTimeout(reloadTimer)
  window.clearTimeout(guardResetTimer)
})

onMounted(async () => {
  window.addEventListener('scroll', syncScrolled, { passive: true })
  motionQuery.addEventListener('change', onMotionMediaChange)
  syncScrolled()
  try {
    const status = await api.status()
    port.value = status.port
    version.value = status.version
  } catch {
    /* the dashboard surfaces connection errors in detail */
  }
})
</script>

<template>
  <div class="shell">
    <aside class="sidebar">
      <div class="brand">
        <span class="brand-mark">AG</span>
        <div class="brand-text">
          <strong>AIWorkGate</strong>
          <small>本地网关</small>
        </div>
      </div>
      <nav class="nav">
        <RouterLink
          v-for="item in nav"
          :key="item.to"
          :to="item.to"
          class="nav-item"
          :class="{
            'router-link-active': item.to === '/pools' && route.path.startsWith('/pools/'),
          }"
        >
          <AppIcon :name="item.icon" class="nav-icon" />
          <span class="nav-label">{{ item.label }}</span>
        </RouterLink>
      </nav>
      <div class="sidebar-foot">
        <span>本机管理界面</span>
        <span class="mono">127.0.0.1:{{ port ?? '—' }}</span>
        <span class="mono">{{ version ? `宿主 v${version}` : '版本未知' }}</span>
      </div>
    </aside>

    <div class="main">
      <header class="topbar" :class="{ 'is-scrolled': scrolled }">
        <div :key="route.path" class="topbar-title">
          <h1>{{ title }}</h1>
          <span>{{ subtitle }}</span>
        </div>
        <div class="topbar-actions">
          <span class="badge plain accent">本机免密</span>
          <button
            type="button"
            class="btn sm ghost motion-toggle"
            :class="{ 'is-off': motionEffective }"
            :title="motionTitle"
            @click="cycleMotion"
          >
            <AppIcon name="zap" :size="15" />
            动效 {{ motionLabel }}
          </button>
          <button type="button" class="btn sm ghost theme-toggle" @click="toggleTheme">
            <AppIcon :name="theme === 'dark' ? 'sun' : 'moon'" :size="15" />
            {{ theme === 'dark' ? '浅色模式' : '深色模式' }}
          </button>
        </div>
      </header>
      <main class="content" tabindex="-1">
        <RouterView v-slot="{ Component, route: current }">
          <Transition name="page" mode="out-in">
            <component :is="Component" :key="current.path" />
          </Transition>
        </RouterView>
      </main>
    </div>

    <div
      v-if="reloadState"
      class="toasts reload-notice"
      role="status"
      aria-live="polite"
    >
      <div class="toast warn reload-toast" :class="{ error: reloadState === 'blocked' }">
        <span v-if="reloadState === 'reloading'">页面资源已更新，正在重新加载…</span>
        <template v-else>
          <span>页面加载失败，请重启 AIWorkGate 后重试</span>
          <button type="button" class="btn sm ghost" @click="manualReload">重新加载</button>
        </template>
      </div>
    </div>
  </div>
</template>

<style scoped>
.reload-notice {
  z-index: 90;
}

.reload-toast {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}

.reload-toast .btn {
  flex: none;
}
</style>
