// POST /admin/signin/start —— 本机登录会话。
//
// 两种插件风格都已覆盖（实测真包）：
//   plugin 模式（默认）：插件自己的 authorize() 已经在进程内起了 127.0.0.1 监听
//     （trae/cline：createServer().listen(0,"127.0.0.1")），或直接轮询上游
//     （workbuddy）。桥只把 url 交给宿主打开，并等待插件给出的 callback() 承诺。
//   bridge 模式：插件要求宿主提供 redirect_uri 时，桥起一个只绑 127.0.0.1 的
//     临时回调服务器，把浏览器拿到的参数交回插件 callback。
//
// 非本机来源请求一律拒绝；超时与取消都会回收监听与定时器。

import { createServer } from 'node:http'
import type { IncomingMessage, Server, ServerResponse } from 'node:http'
import { randomUUID } from 'node:crypto'
import type { AdminDeps, PluginHandle, PluginSigninResult } from './types.ts'
import { dayOf, http400, http404, isLoopback, readJSON, rejectRemote, sendJSON, sendText } from './util.ts'

export type SigninState = 'pending' | 'success' | 'failed' | 'cancelled' | 'timeout'

interface Session {
  id: string
  pluginId: string
  providerId: string
  mode: 'plugin' | 'bridge'
  url: string
  instructions?: string
  method?: string
  redirectUri?: string
  state: SigninState
  createdAt: number
  deadline: number
  error?: string
  /** redacted account result; holds no token */
  accountId?: string
  expiresAt?: number
  loopback?: Server
  timer?: NodeJS.Timeout
}

const ADDR: '127.0.0.1' = '127.0.0.1'

export function createSigninManager(deps: AdminDeps) {
  const sessions = new Map<string, Session>()
  const timeoutMs = deps.signinTimeoutMs ?? 5 * 60 * 1000
  const now = deps.now ?? Date.now

  function view(s: Session) {
    return {
      session_id: s.id,
      plugin_id: s.pluginId,
      provider_id: s.providerId,
      state: s.state,
      url: s.url,
      instructions: s.instructions ?? '',
      method: s.method ?? 'auto',
      mode: s.mode,
      ...(s.redirectUri ? { redirect_uri: s.redirectUri } : {}),
      created_at: s.createdAt,
      expires_at: s.deadline,
      ...(s.accountId ? { account_id: s.accountId } : {}),
      ...(s.error ? { error: s.error } : {}),
      // D-57：指纹与 token 都不出现在管理面
    }
  }

  function closeSession(s: Session, state: SigninState, error?: string) {
    if (s.state !== 'pending') return
    s.state = state
    s.error = error
    if (s.timer) clearTimeout(s.timer)
    s.timer = undefined
    if (s.loopback) {
      const srv = s.loopback
      s.loopback = undefined
      // 关掉监听；迟到的浏览器请求会拿到连接被拒
      srv.close(() => {})
      // 防御：node 里若尚有 keep-alive 连接，强制断开
      ;(srv as Server & { closeAllConnections?: () => void }).closeAllConnections?.()
    }
  }

  /** 完成一次登录：写凭据（脱敏回显），关闭会话。 */
  async function settle(s: Session, result: PluginSigninResult, params?: Record<string, string>) {
    if (s.state !== 'pending') return
    if (result?.type === 'success') {
      const accountId = String(result.accountId ?? result.uid ?? `${s.providerId}-default`)
      deps.credentials.put({
        pluginId: s.pluginId,
        accountId,
        secret: { ...result },
        expiresAt: typeof result.expires === 'number' && result.expires > 0 ? Math.floor(result.expires / 1000) : undefined,
        refreshable: Boolean(result.refresh),
      })
      s.accountId = accountId
      s.expiresAt = typeof result.expires === 'number' ? Math.floor(result.expires / 1000) : undefined
      closeSession(s, 'success')
      deps.logger?.info('signin ok', { plugin: s.pluginId, provider: s.providerId, account: accountId })
    } else {
      closeSession(s, 'failed', result?.error ?? `sign-in failed${params ? ` (${Object.keys(params).join(',')})` : ''}`)
      deps.logger?.info('signin failed', { plugin: s.pluginId, provider: s.providerId })
    }
  }

  /** 起一个只绑 127.0.0.1 的临时回调服务器（bridge 模式）。 */
  async function startLoopback(s: Session, handle: PluginHandle, providerId: string) {
    const srv = createServer((req: IncomingMessage, res: ServerResponse) => {
      // 即使反代/端口转发也无法绕过：只认回环来源
      if (!isLoopback(req)) {
        res.writeHead(403, { 'content-type': 'text/plain; charset=utf-8' })
        res.end('loopback only')
        return
      }
      const url = new URL(req.url ?? '/', `http://${ADDR}`)
      if (url.pathname !== `/admin/signin/callback/${s.id}`) {
        res.writeHead(404).end()
        return
      }
      const params: Record<string, string> = {}
      for (const [k, v] of url.searchParams) params[k] = v
      const failed = params.error || params.error_description
      if (failed) {
        void settle(s, { type: 'failed', error: String(failed) }, params)
        sendText(res, 200, 'Sign-in did not finish. You can close this tab.')
        return
      }
      // 有插件 callback 就把参数交回去；没有就当作成功的凭据参数
      void (async () => {
        try {
          const result = await handle.authorize(providerId, { redirectUri: s.redirectUri })
          const cb = (result as { callback?: (p?: Record<string, string>) => Promise<PluginSigninResult> }).callback
          const final = cb ? await cb(params) : ({ type: 'success', ...params } as PluginSigninResult)
          await settle(s, final, params)
        } catch (e) {
          await settle(s, { type: 'failed', error: e instanceof Error ? e.message : String(e) }, params)
        }
      })()
      sendText(res, 200, 'Sign-in received. You can close this tab.')
    })
    await new Promise<void>((resolve, reject) => {
      srv.once('error', reject)
      srv.listen(0, ADDR, () => resolve())
    })
    const addr = srv.address()
    if (!addr || typeof addr === 'string') throw new Error('loopback listener has no port')
    s.loopback = srv
    return `http://${ADDR}:${addr.port}/admin/signin/callback/${s.id}`
  }

  async function start(body: Record<string, unknown>) {
    const pluginId = String(body.plugin_id ?? '').trim()
    if (!pluginId) throw http400('plugin_id is required')
    const handle = deps.plugins.resolve(pluginId)
    if (!handle) throw http404(`unknown plugin ${pluginId}`)
    const providerId = String(body.provider ?? body.provider_id ?? handle.providerIds[0] ?? '').trim()
    if (!providerId || !handle.providerIds.includes(providerId)) {
      throw http400(`provider ${providerId || '<empty>'} is not served by plugin ${pluginId}`)
    }
    const mode = body.mode === 'bridge' ? 'bridge' : 'plugin'

    const id = randomUUID()
    const s: Session = {
      id,
      pluginId,
      providerId,
      mode,
      url: '',
      state: 'pending',
      createdAt: now(),
      deadline: now() + timeoutMs,
    }
    sessions.set(id, s)

    try {
      let start: Awaited<ReturnType<PluginHandle['authorize']>>
      if (mode === 'bridge') {
        const redirectUri = await startLoopback(s, handle, providerId)
        s.redirectUri = redirectUri
        start = await handle.authorize(providerId, { redirectUri })
      } else {
        start = await handle.authorize(providerId)
      }
      if (!start?.url || typeof start.url !== 'string') throw new Error('plugin returned no sign-in URL')
      s.url = start.url
      s.instructions = start.instructions
      s.method = start.method
      s.timer = setTimeout(() => closeSession(s, 'timeout', 'the sign-in timed out'), timeoutMs)
      s.timer.unref?.()
      // 插件的 callback 承诺：bridge 模式下由回环回调驱动，不在这里 await
      if (mode === 'plugin' && typeof start.callback === 'function') {
        void Promise.resolve()
          .then(() => (start.callback as () => Promise<PluginSigninResult>)())
          .then((r) => settle(s, r))
          .catch((e: unknown) => settle(s, { type: 'failed', error: e instanceof Error ? e.message : String(e) }))
      } else if (mode === 'plugin') {
        // 插件只给 URL 不给 callback：留待宿主轮询或取消
        deps.logger?.info('signin started without callback', { plugin: pluginId, provider: providerId })
      }
      deps.logger?.info('signin started', { plugin: pluginId, provider: providerId, mode, day: dayOf(now()) })
      return view(s)
    } catch (e) {
      closeSession(s, 'failed', e instanceof Error ? e.message : String(e))
      throw e
    }
  }

  function get(id: string) {
    const s = sessions.get(id)
    if (!s) return undefined
    // 超时判定（定时器是最初防线，这里兜底）
    if (s.state === 'pending' && now() > s.deadline) closeSession(s, 'timeout', 'the sign-in timed out')
    return view(s)
  }

  function cancel(id: string) {
    const s = sessions.get(id)
    if (!s) return undefined
    closeSession(s, 'cancelled', 'cancelled by the host')
    return view(s)
  }

  function closeAll() {
    for (const s of sessions.values()) if (s.state === 'pending') closeSession(s, 'cancelled', 'bridge shutting down')
  }

  /** 会话数量（诊断用，不含任何凭据信息）。 */
  function size() {
    return sessions.size
  }

  return { start, get, cancel, closeAll, size }
}

/** 路由：/admin/signin/start、/admin/signin/{sid}、/admin/signin/{sid}/cancel */
export async function handleSignin(
  req: IncomingMessage,
  res: ServerResponse,
  mgr: ReturnType<typeof createSigninManager>,
  pathname: string,
): Promise<boolean> {
  if (!isLoopback(req)) {
    rejectRemote(res)
    return true
  }
  if (pathname === '/admin/signin/start' && req.method === 'POST') {
    try {
      sendJSON(res, 200, await mgr.start(await readJSON(req)))
    } catch (e) {
      const status = (e as { status?: number }).status ?? 500
      sendJSON(res, status, { error: { message: e instanceof Error ? e.message : String(e), type: 'signin_error' } })
    }
    return true
  }
  const m = /^\/admin\/signin\/([0-9a-f-]{8,64})(\/cancel)?$/.exec(pathname)
  if (m) {
    const sid = m[1]
    if (m[2] && req.method === 'POST') {
      const v = mgr.cancel(sid)
      if (!v) {
        sendJSON(res, 404, { error: { message: 'no such sign-in session', type: 'not_found' } })
        return true
      }
      sendJSON(res, 200, v)
      return true
    }
    if (!m[2] && req.method === 'GET') {
      const v = mgr.get(sid)
      if (!v) {
        sendJSON(res, 404, { error: { message: 'no such sign-in session', type: 'not_found' } })
        return true
      }
      sendJSON(res, 200, v)
      return true
    }
  }
  return false
}
