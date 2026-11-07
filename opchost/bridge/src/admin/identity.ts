// D-57 指纹重置透传：POST /accounts/{id}/identity/reset。
//
// 宿主侧契约（internal/plugins/client.go:197-207）：
//   POST /accounts/{accountID}/identity/reset，body {"mode":"request"|"os"}，
//   20s 超时，mode 由宿主小写/去空白后发来；宿主只代理调用，绝不接触指纹材料。
//
// 桥侧语义：
//   · 插件真带 auth.identity 钩子时，按 mode 透传（/meta 才会声明
//     identity_request / identity_os_switch，见 meta.ts bridgeCapabilities）；
//   · mode="os" 但插件未声明 OS 支持（capabilities 缺 identity_os_switch）
//     → 409 identity_os_unsupported，不伪造成功；
//   · 账号在任何已加载插件里都不存在 → 404（与 /accounts/{id}/checkin 一致）；
//   · 插件钩子抛错：带数字 status（400-599）时原样透传状态码，否则 500。
//
// 本端点自身绝不返回或记录任何指纹材料（对齐 admin/credentials.ts 的 D-57 红线）。

import type { IncomingMessage, ServerResponse } from 'node:http'
import type { AdminDeps, IdentityResetResult } from './types.ts'
import { isLoopback, readJSON, rejectRemote, sendJSON } from './util.ts'
import { locateAccount } from './checkin.ts'
import { withEgressFetch } from './egress_fetch.ts'

const IDENTITY_PATH = /^\/accounts\/([^/]+)\/identity\/reset$/

/**
 * POST /accounts/{id}/identity/reset —— 宿主契约端点。
 * 返回 true = 已处理；false = 交给其他路由。
 */
export async function handleIdentityReset(
  req: IncomingMessage,
  res: ServerResponse,
  deps: AdminDeps,
  pathname: string,
): Promise<boolean> {
  const m = IDENTITY_PATH.exec(pathname)
  if (!m || req.method !== 'POST') return false
  if (!isLoopback(req)) {
    rejectRemote(res)
    return true
  }
  const accountId = decodeURIComponent(m[1])
  const url = new URL(req.url ?? '/', 'http://127.0.0.1')
  const pluginHint = url.searchParams.get('plugin_id') ?? undefined
  const providerHint = url.searchParams.get('provider') ?? undefined

  const body = await readJSON(req).catch(() => ({}))
  const mode = String(body.mode ?? 'request').toLowerCase().trim()
  if (mode !== 'request' && mode !== 'os') {
    sendJSON(res, 400, {
      error: { message: `mode must be "request" or "os", got ${JSON.stringify(body.mode ?? '')}`, type: 'invalid_request' },
    })
    return true
  }

  const targets = await locateAccount(deps, accountId, pluginHint, providerHint)
  if (!targets.length) {
    sendJSON(res, 404, { error: { message: `no account ${accountId} in any loaded plugin`, type: 'not_found' } })
    return true
  }
  const [target] = targets
  const handle = deps.plugins.resolve(target.pluginId)
  if (!handle) {
    sendJSON(res, 404, { error: { message: `unknown plugin ${target.pluginId}`, type: 'not_found' } })
    return true
  }
  if (mode === 'os' && !handle.capabilities().includes('identity_os_switch')) {
    sendJSON(res, 409, {
      error: {
        message: `provider ${target.providerId} does not declare os-level identity support (permissions.os_identity)`,
        type: 'identity_os_unsupported',
        code: 'identity_os_unsupported',
      },
    })
    return true
  }
  if (!handle.capabilities().includes('identity_request')) {
    // 插件没有 auth.identity 钩子：/meta 也不会声明 identity_request（meta.ts），
    // 这里用 501 诚实回答「不实现」，绝不伪造成功。
    sendJSON(res, 501, {
      error: {
        message: `provider ${target.providerId} exposes no identity hook; identity reset is not implemented`,
        type: 'identity_unsupported',
        code: 'identity_unsupported',
      },
    })
    return true
  }

  let out: IdentityResetResult
  try {
    // task-35②：同 checkin——钩子的上游调用在 egress 守卫窗口内执行。
    out = await withEgressFetch(deps, target.pluginId, target.providerId, () =>
      handle.identity(target.providerId, accountId, mode),
    )
  } catch (e) {
    const status = (e as { status?: number }).status
    const passthrough = typeof status === 'number' && status >= 400 && status <= 599 ? status : 500
    sendJSON(res, passthrough, {
      error: { message: e instanceof Error ? e.message : String(e), type: 'identity_failed' },
    })
    return true
  }
  sendJSON(res, 200, { ok: out.ok, ...(out.message ? { message: out.message } : {}) })
  return true
}
