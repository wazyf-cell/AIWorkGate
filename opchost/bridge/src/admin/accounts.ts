// GET /accounts —— 宿主调度枚举账号的契约端点（之前桥缺这个，独立验证者点出）。
//
// 契约（internal/model/model.go:176-194，宿主 internal/plugins/client.go:97-103 调用）：
//   响应体 {"accounts":[Account]}
//   Account{ id, label, enabled, status, added_at,
//            quota{remaining,total,unlimited,reset_at,expires_at,plan,rate_limited,today_checked_in,checked_in_at},
//            credential{present,expires_at,refreshable}, identity{...}, last_error,
//            可选 priority/weight/cooldown_until/model_cooldowns }
//   status ∈ ok|expired|invalid|rate_limited|unknown
//
// 数据来源：
//   · provider 注册表（wiring.registryFromLoad）→ 每个 provider 一个账号条目
//   · 凭据库 → credential{present,expires_at,refreshable}；无凭据 status=expired（D-19：未登录即 expired）
//   · 签到日账 → quota.today_checked_in / checked_in_at（真实数据，不伪造）
//
// D-57：identity 是**指纹**，管理面与宿主都不该看到明文，故这里**整体省略** identity
// 字段（Go 侧是 *Identity omitempty，省略即合法）；指纹能力留在插件侧。
//
// PATCH /accounts/{id}：{enabled:bool} 落 <data-dir>/accounts.json 账本（重启仍生效）。
// DELETE /accounts/{id}：写墓碑，GET 不再列出（幂等，2xx）。

import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import type { IncomingMessage, ServerResponse } from 'node:http'
import type { AdminDeps, AdminCredential, AdminCredentialStore } from './types.ts'
import type { CheckinLedger } from './ledger.ts'
import {
  defaultQuotaPath,
  loadQuotaWindows,
  quotaRecordForAccount,
  quotaWindowsView,
  type QuotaRecord,
  type QuotaWindow,
} from './quotas.ts'
import { http400, isLoopback, readJSON, rejectRemote, sendJSON } from './util.ts'

/** Account.quota 的字段子集；未提供的省略（Go 侧全部 omitempty）。 */
export interface AccountQuota {
  remaining?: number
  total?: number
  unlimited?: boolean
  reset_at?: number
  expires_at?: number
  plan?: string
  rate_limited?: boolean
  today_checked_in?: boolean
  checked_in_at?: number
  /** P9-1：插件产出的多 window 额度，原样透传（宿主不做字段映射）。 */
  windows?: QuotaWindow[]
  display?: string
  read_at?: string
}

/** Account.credential：只有存在性与生命周期，永不含 token。 */
export interface AccountCredential {
  present: boolean
  expires_at?: number
  refreshable?: boolean
}

/** 契约 Account（与 model.go:176-194 逐字段对齐）。 */
export interface AccountPayload {
  id: string
  label: string
  enabled: boolean
  status: 'ok' | 'expired' | 'invalid' | 'rate_limited' | 'unknown'
  added_at?: number
  quota: AccountQuota
  credential: AccountCredential
  last_error?: string
  priority?: number
  weight?: number
  cooldown_until?: number
  model_cooldowns?: Record<string, number>
  // 注意：没有 identity —— D-57 指纹不出现
}

interface LedgerRow {
  enabled: boolean
  added_at: number
  updated_at: number
  /** 墓碑：DELETE 后置 true，GET 不再列出 */
  deleted?: boolean
}

export type AccountLedger = ReturnType<typeof createAccountLedger>

/** 账号账本：enabled / added_at / deleted 落盘（不含任何凭据）。 */
export function createAccountLedger(dataDir: string, now: () => number = Date.now) {
  mkdirSync(dataDir, { recursive: true })
  const file = join(dataDir, 'accounts.json')
  const keyOf = (pluginId: string, accountId: string) => `${pluginId}\u0000${accountId}`

  function load(): Record<string, LedgerRow> {
    if (!existsSync(file)) return {}
    try {
      return JSON.parse(readFileSync(file, 'utf8')) as Record<string, LedgerRow>
    } catch {
      return {}
    }
  }
  function save(rows: Record<string, LedgerRow>): void {
    writeFileSync(file, JSON.stringify(rows, null, 2), { mode: 0o600 })
  }

  return {
    path(): string {
      return file
    },
    get(pluginId: string, accountId: string): LedgerRow | undefined {
      return load()[keyOf(pluginId, accountId)]
    },
    /** 首次出现时登记 added_at；enabled 缺省 true（契约默认启用）。 */
    ensure(pluginId: string, accountId: string): LedgerRow {
      const rows = load()
      const k = keyOf(pluginId, accountId)
      const prev = rows[k]
      if (prev) return prev
      const row: LedgerRow = { enabled: true, added_at: Math.floor(now() / 1000), updated_at: Math.floor(now() / 1000) }
      rows[k] = row
      save(rows)
      return row
    },
    setEnabled(pluginId: string, accountId: string, enabled: boolean): LedgerRow {
      const rows = load()
      const k = keyOf(pluginId, accountId)
      const row: LedgerRow = { ...(rows[k] ?? { enabled: true, added_at: Math.floor(now() / 1000), updated_at: 0 }), enabled, updated_at: Math.floor(now() / 1000) }
      rows[k] = row
      save(rows)
      return row
    },
    remove(pluginId: string, accountId: string): boolean {
      const rows = load()
      const k = keyOf(pluginId, accountId)
      const existed = Boolean(rows[k] && !rows[k].deleted)
      rows[k] = { ...(rows[k] ?? { enabled: true, added_at: Math.floor(now() / 1000), updated_at: 0 }), deleted: true, updated_at: Math.floor(now() / 1000) }
      save(rows)
      return existed
    },
  }
}

/** 账号条目（pluginId + accountId）从哪来：provider 注册表 ∪ 凭据库。 */
export interface AccountRef {
  pluginId: string
  accountId: string
  label: string
  credential?: AdminCredential
}

/** 枚举一个插件下的账号：每个 provider 一个 + 凭据库里额外的 account 级账号。 */
export function accountRefsFor(deps: AdminDeps, store: AdminCredentialStore, pluginId: string): AccountRef[] {
  const handle = deps.plugins.resolve(pluginId)
  if (!handle) return []
  const out: AccountRef[] = []
  const seen = new Set<string>()
  for (const providerId of handle.providerIds) {
    seen.add(providerId)
    out.push({
      pluginId,
      accountId: providerId,
      label: providerId,
      credential: store.get(pluginId, providerId),
    })
  }
  // 登录流程产生的 account 级凭据（accountId 可能是 uid/名称，与 provider id 不同）
  for (const rec of store.list(pluginId)) {
    if (seen.has(rec.accountId)) continue
    seen.add(rec.accountId)
    out.push({ pluginId, accountId: rec.accountId, label: rec.accountId, credential: rec })
  }
  return out
}

/** 由凭据与签到日账推导 status（D-19：未登录即 expired）。 */
export function statusOf(ref: AccountRef, nowSec: number): AccountPayload['status'] {
  const cred = ref.credential
  if (!cred) return 'expired'
  if (typeof cred.expiresAt === 'number' && cred.expiresAt > 0 && cred.expiresAt <= nowSec) return 'expired'
  return 'ok'
}

/** 组装一条契约 Account。 */
export function buildAccount(
  ref: AccountRef,
  row: LedgerRow,
  checkin: { result: 'ok' | 'already'; at: number } | undefined,
  nowSec: number,
  quotas?: Map<string, QuotaRecord>,
): AccountPayload {
  const credential: AccountCredential = {
    present: Boolean(ref.credential),
    ...(ref.credential && typeof ref.credential.expiresAt === 'number' ? { expires_at: ref.credential.expiresAt } : {}),
    ...(ref.credential && typeof ref.credential.refreshable === 'boolean' ? { refreshable: ref.credential.refreshable } : {}),
  }
  const windows = quotaWindowsView(quotas ? quotaRecordForAccount(quotas, ref.pluginId, ref.accountId) : undefined)
  const quota: AccountQuota = {
    // 额度是 P9 的多 window 模型：windows 原样透传，legacy 标量由首条 window 派生；
    // 没有窗口数据时只带签到标记，不伪造 remaining/total。
    ...(windows ?? {}),
    ...(checkin ? { today_checked_in: true, checked_in_at: checkin.at } : {}),
  }
  return {
    id: ref.accountId,
    label: ref.label,
    enabled: row.enabled !== false,
    status: statusOf(ref, nowSec),
    ...(row.added_at ? { added_at: row.added_at } : {}),
    quota,
    credential,
  }
}

/** 全量列出（按 plugin → account 顺序，deleted 的不列）。 */
export function listAccounts(
  deps: AdminDeps,
  store: AdminCredentialStore,
  ledger: AccountLedger,
  checkins: CheckinLedger,
  nowSec: number,
): AccountPayload[] {
  const quotas = loadQuotaWindows(deps.quotaPath ?? defaultQuotaPath())
  const out: AccountPayload[] = []
  const seen = new Set<string>()
  for (const handle of deps.plugins.list()) {
    for (const ref of accountRefsFor(deps, store, handle.id)) {
      // 宿主按 id 认账号：跨插件同名 id 只留第一条，避免调度看到重复账号
      if (seen.has(ref.accountId)) continue
      const row = ledger.ensure(ref.pluginId, ref.accountId)
      if (row.deleted) continue
      seen.add(ref.accountId)
      const today = checkins.get(ref.pluginId, ref.accountId)
      const checkin = today && (today.result === 'ok' || today.result === 'already') ? { result: today.result, at: today.at } : undefined
      out.push(buildAccount(ref, row, checkin, nowSec, quotas))
    }
  }
  return out
}

/** 按 id 找账号（可带 plugin_id 消歧）。 */
function findAccount(
  deps: AdminDeps,
  store: AdminCredentialStore,
  ledger: AccountLedger,
  accountId: string,
  pluginHint?: string,
): { ref: AccountRef; row: LedgerRow } | undefined {
  const handles = pluginHint ? [deps.plugins.resolve(pluginHint)].filter(Boolean) : deps.plugins.list()
  for (const h of handles) {
    if (!h) continue
    for (const ref of accountRefsFor(deps, store, h.id)) {
      if (ref.accountId !== accountId) continue
      const row = ledger.ensure(ref.pluginId, ref.accountId)
      if (row.deleted) return undefined
      return { ref, row }
    }
  }
  return undefined
}

/** GET /accounts · PATCH /accounts/{id} · DELETE /accounts/{id} */
export async function handleAccounts(
  req: IncomingMessage,
  res: ServerResponse,
  deps: AdminDeps,
  store: AdminCredentialStore,
  ledger: AccountLedger,
  checkins: CheckinLedger,
  pathname: string,
): Promise<boolean> {
  if (!isLoopback(req)) {
    rejectRemote(res)
    return true
  }
  const nowSec = Math.floor((deps.now ?? Date.now)() / 1000)
  const url = new URL(req.url ?? '/', 'http://127.0.0.1')

  if (pathname === '/accounts' && req.method === 'GET') {
    const accounts = listAccounts(deps, store, ledger, checkins, nowSec)
    sendJSON(res, 200, { accounts })
    return true
  }

  const m = /^\/accounts\/([^/]+)$/.exec(pathname)
  if (!m) return false
  const accountId = decodeURIComponent(m[1])
  const pluginHint = url.searchParams.get('plugin_id') ?? undefined

  if (req.method === 'PATCH') {
    const body = await readJSON(req)
    const keys = Object.keys(body)
    if (keys.length === 0) throw http400('PATCH body is empty; expected {"enabled":true|false}')
    if ('enabled' in body && typeof body.enabled !== 'boolean') throw http400('enabled must be a boolean')
    const found = findAccount(deps, store, ledger, accountId, pluginHint)
    if (!found) {
      sendJSON(res, 404, { error: { message: `no account ${accountId}`, type: 'not_found' } })
      return true
    }
    const row = typeof body.enabled === 'boolean' ? ledger.setEnabled(found.ref.pluginId, found.ref.accountId, body.enabled) : found.row
    const today = checkins.get(found.ref.pluginId, found.ref.accountId)
    const checkin = today && (today.result === 'ok' || today.result === 'already') ? { result: today.result, at: today.at } : undefined
    sendJSON(res, 200, { ok: true, account: buildAccount(found.ref, row, checkin, nowSec, loadQuotaWindows(deps.quotaPath ?? defaultQuotaPath())) })
    return true
  }

  if (req.method === 'DELETE') {
    const found = findAccount(deps, store, ledger, accountId, pluginHint)
    if (!found) {
      // 幂等：未知账号也回 2xx，但如实说没删到
      sendJSON(res, 200, { ok: true, deleted: false })
      return true
    }
    const existed = ledger.remove(found.ref.pluginId, found.ref.accountId)
    // 契约里账号与其凭据同生共死（model.go:196 DeleteAccount removes both）
    store.delete(found.ref.pluginId, found.ref.accountId)
    sendJSON(res, 200, { ok: true, deleted: existed })
    return true
  }

  return false
}
