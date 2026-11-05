// 管理面路由表。/admin/** 全部只对本机回环来源开放（D-06/G-本机语义）。
//
// 路由：
//   POST /admin/signin/start          起本机登录会话，返回登录 URL
//   GET  /admin/signin/{sid}          查会话状态（不含 token）
//   POST /admin/signin/{sid}/cancel   取消会话（回收回环监听与定时器）
//   GET  /admin/credentials           脱敏列出凭据（键名/存在/过期）
//   POST /admin/credentials           注入凭据（磁盘加密）
//   POST /admin/credentials/export    脱敏导出（宿主永远看不到明文）
//   DELETE /admin/credentials         删除
//   GET  /admin/manifest              从 SITES 导出 network_domains + 共享组
//   GET  /admin/quotagroups           D-20 共享组定义
//   GET  /admin/checkin/switches      D-19④ 签到开关（继承 magpie）
//   GET  /accounts                    宿主契约端点：账号列表（P1-6）
//   PATCH /accounts/{id}              {enabled:bool}，落 data-dir 账本
//   DELETE /accounts/{id}             写墓碑，GET 不再列出
//   POST /accounts/{id}/checkin       宿主契约端点（D-19）
//   POST /checkin                     桥侧聚合端点（D-19）

import type { IncomingMessage, ServerResponse } from 'node:http'
import type { AdminDeps, QuotaGroup } from './types.ts'
import { createFileCredentialStore } from './credentials.ts'
import { createSigninManager, handleSignin } from './signin.ts'
import { handleCredentials } from './credentials.ts'
import { deriveManifest, handleManifest } from './manifest.ts'
import { buildIndex, loadQuotaGroups } from './quotagroups.ts'
import { createCheckinLedger } from './ledger.ts'
import { handleAccountCheckin, handleCheckinAll } from './checkin.ts'
import { createAccountLedger, handleAccounts } from './accounts.ts'
import { loadCheckinSwitches } from './switches.ts'
import { isLoopback, rejectRemote, sendJSON } from './util.ts'

export interface AdminRouter {
  /** true = 本路由已处理该请求；false = 交给桥的其他端点 */
  handle(req: IncomingMessage, res: ServerResponse): Promise<boolean>
  /** 关闭回环监听与挂起的会话 */
  close(): void
  /** 测试/诊断用 */
  debug(): {
    signinSessions: number
    quotaGroups: QuotaGroup[]
    quotaGroupSource: string
    checkinSwitchSource: string
  }
}

export function createAdminRouter(deps: AdminDeps): AdminRouter {
  const store = deps.credentials ?? createFileCredentialStore(deps.dataDir)
  const signin = createSigninManager({ ...deps, credentials: store })
  const ledger = createCheckinLedger(deps.dataDir, deps.now)
  const accounts = createAccountLedger(deps.dataDir, deps.now)
  const quota = deps.quotaGroups
    ? { groups: deps.quotaGroups, source: 'config', warning: undefined as string | undefined }
    : loadQuotaGroups(deps.dataDir)
  const index = buildIndex(quota.groups, quota.source, quota.warning)
  const switches = loadCheckinSwitches({
    dataDir: deps.dataDir,
    ...(deps.checkinSettingsPath ? { settingsPath: deps.checkinSettingsPath } : {}),
    ...(deps.checkinOverridePath ? { overridePath: deps.checkinOverridePath } : {}),
  })

  async function handle(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    const url = new URL(req.url ?? '/', 'http://127.0.0.1')
    const pathname = url.pathname
    const isAdmin = pathname.startsWith('/admin/')
    const isCheckin = pathname === '/checkin' || /^\/accounts\/[^/]+\/checkin$/.test(pathname)
    const isAccounts = pathname === '/accounts' || /^\/accounts\/[^/]+$/.test(pathname)
    if (!isAdmin && !isCheckin && !isAccounts) return false

    // 统一入口守卫：非回环来源直接 403，绝不进业务分支
    if (!isLoopback(req)) {
      rejectRemote(res)
      return true
    }

    try {
      if (await handleSignin(req, res, signin, pathname)) return true
      if (await handleCredentials(req, res, store, pathname)) return true
      if (
        await handleManifest(req, res, pathname, (id) => deps.plugins.resolve(id), (h) =>
          deriveManifest(h, quota.groups, index.prefixToRecord),
        )
      ) {
        return true
      }
      if (pathname === '/admin/quotagroups' && req.method === 'GET') {
        sendJSON(res, 200, {
          ok: true,
          source: quota.source,
          ...(quota.warning ? { warning: quota.warning } : {}),
          count: quota.groups.length,
          groups: quota.groups,
        })
        return true
      }
      if (pathname === '/admin/checkin/switches' && req.method === 'GET') {
        sendJSON(res, 200, {
          ok: true,
          // D-19④：magpie 的 pluginCheckins 原样继承，桥侧覆盖优先
          source: switches.source,
          switches: switches.table(),
        })
        return true
      }
      if (await handleAccounts(req, res, deps, store, accounts, ledger, pathname)) return true
      if (await handleAccountCheckin(req, res, deps, ledger, switches, pathname)) return true
      if (await handleCheckinAll(req, res, deps, ledger, switches, pathname)) return true

      if (isAdmin) {
        sendJSON(res, 404, { error: { message: `no such admin endpoint: ${req.method} ${pathname}`, type: 'not_found' } })
        return true
      }
      return false
    } catch (e) {
      const status = (e as { status?: number }).status ?? 500
      sendJSON(res, status, {
        error: { message: e instanceof Error ? e.message : String(e), type: status >= 500 ? 'internal_error' : 'invalid_request' },
      })
      return true
    }
  }

  return {
    handle,
    close() {
      signin.closeAll()
    },
    debug() {
      return {
        signinSessions: signin.size(),
        quotaGroups: quota.groups,
        quotaGroupSource: quota.source,
        checkinSwitchSource: switches.source.magpie ?? switches.source.override ?? 'builtin',
      }
    },
  }
}
