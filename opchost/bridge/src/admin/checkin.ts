// D-19 签到委派：把插件内置的签到函数适配成 aiworkgate 契约的结构化结果。
//
// 宿主侧契约（internal/plugins/client.go:106-113）：
//   POST /accounts/{id}/checkin，body {}，响应直接解码为 internal/model/model.go 的
//   CheckinResult：result / credits_before / credits_after / message / at /
//   kind / reset_at / dedup_scope / device_key。
//   result ∈ {"ok","already","fail","skipped"}（model.go:36-42）。
//
// 插件侧真包返回（workbuddy checkinOf / trae 等同类）：
//   { outcome: "claimed"|"done"|"inactive"|"ineligible"|"failed", credit?, streak?, message? }
//
// 「重复签到返回 already 不重复记账」是桥的责任：桥按 (plugin, account, 当天) 记一份
// 日账（checkin-ledger.json），已记过 ok/already 的账号再签到一律回 already，
// credits_after 不再累加。

import type { IncomingMessage, ServerResponse } from 'node:http'
import type { AdminDeps, CheckinLedgerRow, PluginCheckinResult } from './types.ts'
import type { CheckinSwitchIndex } from './switches.ts'
import type { CheckinLedger } from './ledger.ts'
import { dayOf, isLoopback, readJSON, rejectRemote, sendJSON } from './util.ts'

/** aiworkgate 契约的结果对象（字段名逐字对齐 model.CheckinResult）。 */
export interface CheckinResultPayload {
  result: 'ok' | 'already' | 'fail' | 'skipped'
  credits_before?: number
  credits_after?: number
  message?: string
  at?: number
  kind?: string
  reset_at?: number
  dedup_scope?: string
  device_key?: string
}

const KIND = {
  rateLimited: 'rate_limited',
  sessionDead: 'session_dead',
  planLimit: 'plan_limit',
  deviceSigned: 'device_signed',
  quota: 'quota',
  banned: 'banned',
} as const

/** 插件 outcome → (result, kind)。映射原因写在注释里，方便对照。 */
export function mapOutcome(outcome: string, message = ''): { result: CheckinResultPayload['result']; kind?: string } {
  switch (outcome) {
    case 'claimed':
      return { result: 'ok' }
    case 'done':
      // 上游说今天已经签过：等价 already，不重复记账
      return { result: 'already' }
    case 'inactive':
      // 「活动未开启」= 该计划没有签到额度，不是桥失败
      return { result: 'fail', kind: KIND.planLimit }
    case 'ineligible': {
      // 真包两种来源：code 1002（不符合资格）与 401 要求「用 WorkBuddy 自己的 App」。
      // 后者是设备/客户端约束，归 device_signed；其余归 plan_limit。
      const deviceish = /own app|desktop|客户端|app/i.test(message)
      return { result: 'fail', kind: deviceish ? KIND.deviceSigned : KIND.planLimit }
    }
    case 'failed':
      return { result: 'fail', kind: classifyMessage(message) }
    default:
      return { result: 'fail', kind: classifyMessage(message) }
  }
}

/** 对失败消息做归类；归不出的不瞎猜（宿主会自行按状态码推导）。 */
function classifyMessage(message: string): string | undefined {
  const m = message.toLowerCase()
  if (/\b429\b|rate.?limit|too many requests|频率限制/.test(m)) return KIND.rateLimited
  if (/\b401\b|unauthor|not signed in|登录|token.*expire|会话/.test(m)) return KIND.sessionDead
  if (/quota|insufficient|额度|积分不足|14018/.test(m)) return KIND.quota
  if (/banned|封禁|停用/.test(m)) return KIND.banned
  return undefined
}

// 日账（createCheckinLedger）在 ./ledger.ts：按 (plugin, account, 当天) 只记一次成功。
export type { CheckinLedger } from './ledger.ts'

/**
 * 把插件 outcome + 日账合成契约结果。
 * credits_before/after 的语义 = 该账号当天累计已到账积分的「签前/签后」，因此
 * 重复签到不会出现第二次增长（凭据面与账面对得上）。
 */
export function normalizeCheckin(
  pluginId: string,
  accountId: string,
  out: PluginCheckinResult,
  ledger: CheckinLedger,
): CheckinResultPayload {
  const mapped = mapOutcome(String(out?.outcome ?? ''), out?.message ?? '')
  const today = ledger.today()
  const prior = ledger.get(pluginId, accountId, today)
  const credit = Number.isFinite(Number(out?.credit)) ? Number(out.credit) : 0
  const at = Math.floor(Date.now() / 1000)

  // 已经记过账：无论如何都回 already，不重复累计
  if (prior && (prior.result === 'ok' || prior.result === 'already')) {
    return {
      result: 'already',
      credits_before: prior.credit_total,
      credits_after: prior.credit_total,
      ...(out?.message ? { message: String(out.message) } : {}),
      at,
      ...(mapped.kind && mapped.result === 'fail' ? { kind: mapped.kind } : {}),
    }
  }

  if (mapped.result === 'ok') {
    const before = prior?.credit_total ?? 0
    const row = ledger.record(pluginId, accountId, credit, today)
    return {
      result: 'ok',
      credits_before: before,
      credits_after: row.credit_total,
      ...(out?.message ? { message: String(out.message) } : {}),
      at,
    }
  }

  if (mapped.result === 'already') {
    // 插件说今天已经签过：补记一条（credit 只在已知时计入），之后走上面 already 分支
    const before = prior?.credit_total ?? 0
    const row = ledger.record(pluginId, accountId, credit, today)
    return {
      result: 'already',
      credits_before: before,
      credits_after: row.credit_total,
      ...(out?.message ? { message: String(out.message) } : {}),
      at,
    }
  }

  // fail：不写日账（不污染「今天已签」的判断）
  return {
    result: 'fail',
    ...(mapped.kind ? { kind: mapped.kind } : {}),
    ...(out?.message ? { message: String(out.message) } : {}),
    at,
  }
}

/** 结算一次签到：开关 → 调插件 → 归一化 → 返回契约结果。 */
export async function runCheckin(
  deps: AdminDeps,
  ledger: CheckinLedger,
  switches: CheckinSwitchIndex,
  pluginId: string,
  providerId: string,
  accountId: string,
): Promise<CheckinResultPayload> {
  const handle = deps.plugins.resolve(pluginId)
  if (!handle) {
    return { result: 'fail', message: `unknown plugin ${pluginId}`, at: Math.floor(Date.now() / 1000) }
  }
  let out: PluginCheckinResult
  try {
    out = await handle.checkin(providerId, accountId)
  } catch (e) {
    // 插件抛错 = 上游没给答案（离线/网络）。契约里留给 fail + 归类，不冒充成功。
    const msg = e instanceof Error ? e.message : String(e)
    const kind = classifyMessage(msg)
    return {
      result: 'fail',
      ...(kind ? { kind } : {}),
      message: msg,
      at: Math.floor(Date.now() / 1000),
    }
  }
  return normalizeCheckin(pluginId, accountId, out ?? { outcome: 'failed' }, ledger)
}

/** 在一簇插件里找账号所属的 (plugin, provider)。 */
async function locateAccount(
  deps: AdminDeps,
  accountId: string,
  pluginIdHint?: string,
  providerHint?: string,
): Promise<Array<{ pluginId: string; providerId: string }>> {
  const handles = pluginIdHint ? [deps.plugins.resolve(pluginIdHint)].filter(Boolean) as ReturnType<typeof deps.plugins.resolve>[] : deps.plugins.list()
  const found: Array<{ pluginId: string; providerId: string }> = []
  for (const h of handles) {
    if (!h) continue
    const providers = providerHint ? h.providerIds.filter((p) => p === providerHint) : h.providerIds
    for (const p of providers) {
      if (!h.accounts) {
        // 插件不列账号时，退化为「按 id 直接调」——签到契约只要求 id 可寻址
        if (!providerHint || h.providerIds.includes(providerHint)) found.push({ pluginId: h.id, providerId: p })
        break
      }
      try {
        const list = await h.accounts(p)
        if (list.some((a) => a.id === accountId)) found.push({ pluginId: h.id, providerId: p })
      } catch {
        // 列账号失败不影响单账号签到：忽略该 provider
      }
    }
  }
  return found
}

/** POST /accounts/{id}/checkin —— 宿主契约端点，响应体即 CheckinResult。 */
export async function handleAccountCheckin(
  req: IncomingMessage,
  res: ServerResponse,
  deps: AdminDeps,
  ledger: CheckinLedger,
  switches: CheckinSwitchIndex,
  pathname: string,
): Promise<boolean> {
  if (!isLoopback(req)) {
    rejectRemote(res)
    return true
  }
  const m = /^\/accounts\/([^/]+)\/checkin$/.exec(pathname)
  if (!m || req.method !== 'POST') return false
  const accountId = decodeURIComponent(m[1])
  const url = new URL(req.url ?? '/', 'http://127.0.0.1')
  const pluginHint = url.searchParams.get('plugin_id') ?? undefined
  const providerHint = url.searchParams.get('provider') ?? undefined
  // 宿主可能发 {} 或空体；读掉避免连接悬挂
  await readJSON(req).catch(() => ({}))

  const targets = await locateAccount(deps, accountId, pluginHint, providerHint)
  if (!targets.length) {
    sendJSON(res, 404, { error: { message: `no account ${accountId} in any loaded plugin`, type: 'not_found' } })
    return true
  }
  const [target] = targets

  // D-19④：magpie 里关掉的签到不再自动执行。这里用 409（配置态）而不是伪造一条 fail，
  // 免得 checkin_results 里每天多一行假失败；聚合端点 /checkin 则是列进 skipped。
  if (!switches.enabled(target.pluginId, target.providerId)) {
    sendJSON(res, 409, {
      error: {
        message: `check-in is off for ${target.pluginId}/${target.providerId} (magpie settings.pluginCheckins); enable it in the panel before checking in`,
        type: 'checkin_disabled',
        code: 'checkin_disabled',
      },
    })
    return true
  }

  sendJSON(res, 200, await runCheckin(deps, ledger, switches, target.pluginId, target.providerId, accountId))
  return true
}

/** POST /checkin —— 桥侧聚合端点（D-19 两个契约端点之一）。 */
export async function handleCheckinAll(
  req: IncomingMessage,
  res: ServerResponse,
  deps: AdminDeps,
  ledger: CheckinLedger,
  switches: CheckinSwitchIndex,
  pathname: string,
): Promise<boolean> {
  if (!isLoopback(req)) {
    rejectRemote(res)
    return true
  }
  if (pathname !== '/checkin' || req.method !== 'POST') return false
  const body = await readJSON(req).catch(() => ({}))
  const accountId = String(body.account_id ?? '').trim()
  const pluginId = String(body.plugin_id ?? '').trim() || undefined
  const providerId = String(body.provider ?? '').trim() || undefined

  const results: Array<{ plugin_id: string; provider_id: string; account_id: string; checkin: CheckinResultPayload }> = []
  const skipped: Array<{ plugin_id: string; provider_id: string; account_id?: string; reason: string }> = []
  const handles = pluginId ? [deps.plugins.resolve(pluginId)].filter(Boolean) : deps.plugins.list()
  for (const h of handles) {
    if (!h) continue
    const providers = providerId ? h.providerIds.filter((p) => p === providerId) : h.providerIds
    for (const p of providers) {
      // D-19④：关闭的渠道在聚合里跳过，不落账、不伪造失败
      if (!switches.enabled(h.id, p)) {
        skipped.push({ plugin_id: h.id, provider_id: p, ...(accountId ? { account_id: accountId } : {}), reason: 'pluginCheckins off' })
        continue
      }
      let ids: string[]
      if (accountId) ids = [accountId]
      else if (h.accounts) {
        try {
          ids = (await h.accounts(p)).map((a) => a.id)
        } catch {
          ids = []
        }
      } else ids = []
      for (const id of ids) {
        results.push({ plugin_id: h.id, provider_id: p, account_id: id, checkin: await runCheckin(deps, ledger, switches, h.id, p, id) })
      }
    }
  }
  sendJSON(res, 200, {
    ok: true,
    // 日期只出现在桥侧聚合信封里；宿主契约的 CheckinResult 没有 day 字段
    date: ledger.today(),
    count: results.length,
    results,
    skipped,
  })
  return true
}
