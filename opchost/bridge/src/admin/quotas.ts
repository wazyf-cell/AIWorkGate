// P9-1：多 window 额度透传（F-14 / D-21）。
//
// magpie 侧把每个账号的额度写在 ~/.config/magpie/quotas.json：
//   "<provider>/<account>": {
//     at, quota: { provider, plan, readAt,
//                  windows: [{ name, amount, limit, used, unit,
//                              resetsAt, runsOutAt, runsOut, display, ... }] } }
// 「到期」有两种语义：resetsAt（周期重置）与 runsOutAt（券/批次何时烧完）。
// 宿主不做字段强映射，所以这里把 windows[] **原样**带给桥的 /accounts 响应；
// 旧调度字段（remaining/total/reset_at/expires_at/plan）只是从首条 window
// 派生的兼容值，供还没升级的消费方使用。

import { existsSync, readFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'

/** 一条额度 window；保留未知字段（不同平台字段不同，宿主原样透传）。 */
export interface QuotaWindow {
  name?: string
  amount?: number
  limit?: number
  used?: number
  unit?: string
  resetsAt?: string
  runsOutAt?: string
  runsOut?: number
  display?: string
  [key: string]: unknown
}

/** quotas.json 里一条账号的 quota 对象。 */
export interface QuotaRecord {
  provider?: string
  plan?: string
  readAt?: string
  windows?: QuotaWindow[]
  [key: string]: unknown
}

/** 桥 /accounts 的 quota 里透传的字段子集。 */
export interface QuotaWindowsView {
  windows: QuotaWindow[]
  plan?: string
  remaining?: number
  total?: number
  reset_at?: number
  expires_at?: number
  read_at?: string
}

/** 缺省 quotas.json 位置（与 magpie 其它配置同目录）。 */
export function defaultQuotaPath(): string {
  return join(homedir(), '.config', 'magpie', 'quotas.json')
}

/**
 * 读取 quotas.json，只保留带 windows[] 的记录。
 * 坏文件/坏记录不致命：返回已解析的部分，读不到就空表（额度不是必需数据）。
 */
export function loadQuotaWindows(path: string = defaultQuotaPath()): Map<string, QuotaRecord> {
  const out = new Map<string, QuotaRecord>()
  if (!existsSync(path)) return out
  try {
    const raw = JSON.parse(readFileSync(path, 'utf8')) as unknown
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return out
    for (const [key, value] of Object.entries(raw as Record<string, unknown>)) {
      if (!value || typeof value !== 'object') continue
      const quota = (value as { quota?: unknown }).quota
      if (!quota || typeof quota !== 'object' || Array.isArray(quota)) continue
      const windows = (quota as { windows?: unknown }).windows
      if (!Array.isArray(windows) || windows.length === 0) continue
      out.set(key, quota as QuotaRecord)
    }
  } catch {
    return new Map()
  }
  return out
}

/**
 * 找某账号的 quota 记录。先按 "<plugin>/<account>" 精确命中；
 * 再按 key 的 account 尾段扫描（quotas.json 的前缀可能是 provider id，
 * 不一定等于桥的 plugin id），provider 字段匹配者优先。
 */
export function quotaRecordForAccount(
  index: Map<string, QuotaRecord>,
  pluginId: string,
  accountId: string,
): QuotaRecord | undefined {
  const direct = index.get(`${pluginId}/${accountId}`)
  if (direct) return direct
  let fallback: QuotaRecord | undefined
  for (const [key, rec] of index) {
    const cut = key.indexOf('/')
    if (cut < 0 || key.slice(cut + 1) !== accountId) continue
    const prefix = key.slice(0, cut)
    if (rec.provider === pluginId || prefix === pluginId) return rec
    if (!fallback) fallback = rec
  }
  return fallback
}

/** 把一条 quota 记录转成桥 /accounts 的 quota 字段（windows 原样保留）。 */
export function quotaWindowsView(rec: QuotaRecord | undefined): QuotaWindowsView | undefined {
  if (!rec || !Array.isArray(rec.windows) || rec.windows.length === 0) return undefined
  const windows = rec.windows
  // 首条带 limit 的 window 是「总积分」类汇总（插件侧已保证总积分置顶）；
  // 赠送包与总积分有重叠，不能相加。
  const head = windows.find((w) => Number.isFinite(Number(w.limit)) && Number(w.limit) > 0) ?? windows[0]
  const limit = Number(head.limit)
  const amount = Number(head.amount)
  const used = Number(head.used)
  const remaining = Number.isFinite(amount)
    ? amount
    : Number.isFinite(limit) && Number.isFinite(used)
      ? limit - used
      : undefined
  const total = Number.isFinite(limit) ? limit : undefined

  let resetAt: number | undefined
  let expiresAt: number | undefined
  for (const w of windows) {
    const r = epochSeconds(w.resetsAt)
    const e = epochSeconds(w.runsOutAt)
    if (r !== undefined && (resetAt === undefined || r < resetAt)) resetAt = r
    if (e !== undefined && (expiresAt === undefined || e < expiresAt)) expiresAt = e
  }

  return {
    windows,
    ...(typeof rec.plan === 'string' && rec.plan ? { plan: rec.plan } : {}),
    ...(remaining !== undefined ? { remaining } : {}),
    ...(total !== undefined ? { total } : {}),
    ...(resetAt !== undefined ? { reset_at: resetAt } : {}),
    ...(expiresAt !== undefined ? { expires_at: expiresAt } : {}),
    ...(typeof rec.readAt === 'string' && rec.readAt ? { read_at: rec.readAt } : {}),
  }
}

/** RFC3339 → epoch 秒；缺字段/坏字符串返回 undefined（不猜）。 */
function epochSeconds(raw: unknown): number | undefined {
  if (typeof raw !== 'string' || !raw.trim()) return undefined
  const ms = Date.parse(raw)
  return Number.isFinite(ms) ? Math.floor(ms / 1000) : undefined
}
