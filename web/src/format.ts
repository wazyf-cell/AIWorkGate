/** Small formatting helpers shared by the views. */

export function toDate(value: number | string | Date | undefined | null): Date | null {
  if (value === undefined || value === null || value === '') return null
  if (value instanceof Date) return Number.isNaN(value.getTime()) ? null : value
  if (typeof value === 'number') {
    if (!Number.isFinite(value) || value <= 0) return null
    return new Date(value < 1e12 ? value * 1000 : value)
  }
  const text = String(value).trim()
  if (/^\d+$/.test(text)) {
    const numeric = Number(text)
    return new Date(numeric < 1e12 ? numeric * 1000 : numeric)
  }
  const parsed = new Date(text)
  return Number.isNaN(parsed.getTime()) ? null : parsed
}

export function formatTimestamp(value: number | string | Date | undefined | null): string {
  const date = toDate(value)
  if (!date) return '—'
  return date.toLocaleString('zh-CN', { hour12: false })
}

export function formatTimeOnly(value: number | string | Date | undefined | null): string {
  const date = toDate(value)
  if (!date) return '—'
  return date.toLocaleTimeString('zh-CN', { hour12: false })
}

export function formatDate(value: number | string | Date | undefined | null): string {
  const date = toDate(value)
  if (!date) return '—'
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
}

export function formatDay(value: number | string | Date | undefined | null): string {
  return formatDate(value)
}

export function formatNumber(value: number | undefined | null): string {
  if (value === undefined || value === null || Number.isNaN(value)) return '—'
  return value.toLocaleString('zh-CN', { maximumFractionDigits: 2 })
}

export function formatLatency(ms: number | undefined | null): string {
  if (ms === undefined || ms === null || Number.isNaN(ms)) return '—'
  if (ms < 1000) return `${Math.round(ms)} ms`
  return `${(ms / 1000).toFixed(2)} s`
}

export function formatTokens(prompt?: number, completion?: number): string {
  if (!prompt && !completion) return '—'
  return `${formatNumber(prompt ?? 0)} / ${formatNumber(completion ?? 0)}`
}

export function pad(value: number): string {
  return value < 10 ? `0${value}` : String(value)
}

/** Local date in YYYY-MM-DD (used for check-in ranges). */
export function dateISO(date: Date): string {
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
}

export function monthBounds(month: Date): { from: string; to: string; days: number } {
  const first = new Date(month.getFullYear(), month.getMonth(), 1)
  const last = new Date(month.getFullYear(), month.getMonth() + 1, 0)
  return { from: dateISO(first), to: dateISO(last), days: last.getDate() }
}

export function monthLabel(month: Date): string {
  return `${month.getFullYear()} 年 ${month.getMonth() + 1} 月`
}

const WEEKDAYS = ['日', '一', '二', '三', '四', '五', '六']

export function weekdayLabel(month: Date, day: number): string {
  const date = new Date(month.getFullYear(), month.getMonth(), day)
  return WEEKDAYS[date.getDay()]
}

export function percent(part?: number | null, total?: number | null): number {
  if (!part || !total || total <= 0) return 0
  return Math.max(0, Math.min(100, Math.round((part / total) * 100)))
}
