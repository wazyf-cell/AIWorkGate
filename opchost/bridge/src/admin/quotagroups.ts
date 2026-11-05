// D-20 配额共享组：多个 model 前缀 → 同一条额度记录。
//
// 依据：计划 §1 台账 D-20 —— AIwork 分类下 codebuddy 与 workbuddy 积分通用，
// 二者不得作为两个独立渠道接入（否则同一额度被算两次、调度重复投放）。
// 具体到 magpie 的 workbuddy 包：一个插件导出两个前缀
//   workbuddy/    （China build, copilot.tencent.com）
//   workbuddy-ai/ （international build, www.workbuddy.ai）
// 两前缀共用一份 credits，quota_snapshots 只应有一条。
//
// 组定义来源优先级：deps.quotaGroups > <data-dir>/quota-groups.json > 内置默认。

import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import type { QuotaGroup } from './types.ts'

/** 内置默认（按 magpie 真包的 SITES 表实测得出，可被配置覆盖）。 */
export const DEFAULT_QUOTA_GROUPS: QuotaGroup[] = [
  {
    id: 'workbuddy-credits',
    prefixes: ['workbuddy', 'workbuddy-ai'],
    account_ns: 'workbuddy',
    label: 'WorkBuddy 积分（共享）',
    reason: 'D-20：CodeBuddy/WorkBuddy 同一份积分，两个 model 前缀共用一条额度记录',
  },
]

/** 归一化前缀：去空白、去尾部斜杠。 */
export function normalizePrefix(prefix: string): string {
  return String(prefix).trim().replace(/\/+$/, '')
}

/** 读配置覆盖（可选文件；坏文件不致命，回落默认并保留原因）。 */
export function loadQuotaGroups(dataDir: string): { groups: QuotaGroup[]; source: string; warning?: string } {
  const file = join(dataDir, 'quota-groups.json')
  if (!existsSync(file)) return { groups: DEFAULT_QUOTA_GROUPS, source: 'builtin' }
  try {
    const raw = JSON.parse(readFileSync(file, 'utf8')) as unknown
    const list = Array.isArray(raw) ? raw : (raw as { groups?: unknown }).groups
    if (!Array.isArray(list)) throw new Error('expected an array or {groups:[]}')
    const groups = list.map((g, i) => {
      const it = g as Partial<QuotaGroup>
      if (!it || typeof it.id !== 'string' || !Array.isArray(it.prefixes) || !it.prefixes.length) {
        throw new Error(`group #${i} needs id and non-empty prefixes[]`)
      }
      return {
        id: it.id,
        prefixes: it.prefixes.map(normalizePrefix),
        ...(it.account_ns ? { account_ns: it.account_ns } : {}),
        ...(it.label ? { label: it.label } : {}),
        ...(it.reason ? { reason: it.reason } : {}),
      } satisfies QuotaGroup
    })
    return { groups, source: file }
  } catch (e) {
    return {
      groups: DEFAULT_QUOTA_GROUPS,
      source: 'builtin',
      warning: `quota-groups.json ignored: ${e instanceof Error ? e.message : String(e)}`,
    }
  }
}

/** 校验一个组定义（建组/合并前的护栏）。 */
export function validateGroups(groups: QuotaGroup[]): string[] {
  const errors: string[] = []
  const seen = new Map<string, string>()
  for (const g of groups) {
    if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(g.id)) errors.push(`group id "${g.id}" is not a safe slug`)
    if (!g.prefixes.length) errors.push(`group "${g.id}" has no prefixes`)
    for (const p of g.prefixes.map(normalizePrefix)) {
      if (!p) errors.push(`group "${g.id}" has an empty prefix`)
      const owner = seen.get(p)
      if (owner && owner !== g.id) errors.push(`prefix "${p}" is claimed by both "${owner}" and "${g.id}"`)
      seen.set(p, g.id)
    }
  }
  return errors
}

export interface QuotaGroupIndex {
  groups: QuotaGroup[]
  source: string
  warning?: string
  /** prefix → group id */
  byPrefix: Map<string, string>
  /** prefix → the shared quota record key (plugin-agnostic) */
  prefixToRecord: Map<string, string>
}

/** 建索引：前缀 → 组。多个前缀命中同一组即共用一条额度记录。 */
export function buildIndex(groups: QuotaGroup[], source: string, warning?: string): QuotaGroupIndex {
  const byPrefix = new Map<string, string>()
  const prefixToRecord = new Map<string, string>()
  for (const g of groups) {
    const ns = normalizePrefix(g.account_ns ?? g.id)
    for (const p of g.prefixes.map(normalizePrefix)) {
      byPrefix.set(p, g.id)
      prefixToRecord.set(p, `group:${ns}`)
    }
  }
  return { groups, source, warning, byPrefix, prefixToRecord }
}

/** 某个 model 前缀落到哪条额度记录（未分组时就是它自己）。 */
export function quotaRecordFor(index: QuotaGroupIndex, prefix: string): string {
  return index.prefixToRecord.get(normalizePrefix(prefix)) ?? normalizePrefix(prefix)
}

/**
 * 把一份「按前缀记账」的额度列表合并成「按额度记录记账」的列表。
 * 共享组内的多条记录会合成一条（取最大 remaining、最大 total、最保守的 expires_at），
 * 并带上 group / prefixes 供面板合并展示。
 */
export interface QuotaLike {
  plugin_id?: string
  account_id?: string
  prefix: string
  remaining: number
  total: number
  expires_at?: number
  plan?: string
  display?: string
}

export function mergeQuotaRecords(index: QuotaGroupIndex, rows: QuotaLike[]): Array<QuotaLike & { group?: string; prefixes?: string[] }> {
  const byRecord = new Map<string, QuotaLike & { group?: string; prefixes?: string[]; _prefixes: Set<string> }>()
  for (const row of rows) {
    const record = quotaRecordFor(index, row.prefix)
    const prev = byRecord.get(record)
    if (!prev) {
      const groupId = index.byPrefix.get(normalizePrefix(row.prefix))
      const group = groupId ? index.groups.find((g) => g.id === groupId) : undefined
      byRecord.set(record, {
        ...row,
        ...(group ? { group: group.id } : {}),
        ...(group ? { prefixes: group.prefixes.map(normalizePrefix) } : {}),
        _prefixes: new Set([normalizePrefix(row.prefix)]),
      })
      continue
    }
    // 合并：额度取大（同一份额度的不同读法），到期取保守（最早），其余保留首个
    prev._prefixes.add(normalizePrefix(row.prefix))
    prev.remaining = Math.max(prev.remaining, row.remaining)
    prev.total = Math.max(prev.total, row.total)
    if (typeof row.expires_at === 'number' && row.expires_at > 0) {
      prev.expires_at = typeof prev.expires_at === 'number' && prev.expires_at > 0 ? Math.min(prev.expires_at, row.expires_at) : row.expires_at
    }
    if (!prev.display && row.display) prev.display = row.display
  }
  return [...byRecord.values()].map(({ _prefixes, ...rest }) => ({ ...rest, prefixes: [..._prefixes] }))
}
