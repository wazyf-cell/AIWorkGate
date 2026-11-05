// D-19 签到日账：按 (plugin, account, 当天) 只记一次成功。
//
// 「重复签到返回 already 不重复记账」的落点是这里：一旦当天已记过 ok/already，
// 再签到一律回 already，credits_after 不再累加；fail 不写账（否则会污染
// 「今天已经签过」的判断，让宿主第二天不再尝试）。

import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import type { CheckinLedgerRow } from './types.ts'
import { dayOf } from './util.ts'

export type CheckinLedger = ReturnType<typeof createCheckinLedger>

export function createCheckinLedger(dataDir: string, now: () => number = Date.now) {
  mkdirSync(dataDir, { recursive: true })
  const file = join(dataDir, 'checkin-ledger.json')
  const keyOf = (pluginId: string, accountId: string, date: string) => `${pluginId}\u0000${accountId}\u0000${date}`

  function load(): Record<string, CheckinLedgerRow> {
    if (!existsSync(file)) return {}
    try {
      return JSON.parse(readFileSync(file, 'utf8')) as Record<string, CheckinLedgerRow>
    } catch {
      return {}
    }
  }
  function save(rows: Record<string, CheckinLedgerRow>): void {
    writeFileSync(file, JSON.stringify(rows, null, 2), { mode: 0o600 })
  }

  return {
    path(): string {
      return file
    },
    today(): string {
      return dayOf(now())
    },
    get(pluginId: string, accountId: string, date = dayOf(now())): CheckinLedgerRow | undefined {
      return load()[keyOf(pluginId, accountId, date)]
    },
    /** 记一次成功；同一 (plugin, account, 当天) 只累加一次。 */
    record(pluginId: string, accountId: string, credit: number, date = dayOf(now())): CheckinLedgerRow {
      const rows = load()
      const k = keyOf(pluginId, accountId, date)
      const prev = rows[k]
      const row: CheckinLedgerRow = prev
        ? { ...prev, at: prev.at }
        : { plugin_id: pluginId, account_id: accountId, date, result: 'ok', credit_total: 0, at: Math.floor(now() / 1000) }
      if (!prev && credit > 0) {
        row.credit_total = credit
        row.credit_last = credit
      }
      rows[k] = row
      save(rows)
      return row
    },
    /** 只读快照（诊断/测试用）。 */
    all(): CheckinLedgerRow[] {
      return Object.values(load())
    },
  }
}
