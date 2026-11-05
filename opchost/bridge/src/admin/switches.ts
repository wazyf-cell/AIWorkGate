// D-19④ 签到开关迁移：magpie settings.pluginCheckins 的关闭状态必须原样继承，
// 否则「接管了但没开」。
//
// 实测形状（2026-11-05，%USERPROFILE%\.config\magpie\settings.json）：
//   "pluginCheckins": { "codearts": true, "loomy": true, "officeace": true,
//                       "raccoon": true, "zcode": true }
// 键是插件/provider id，值是布尔。缺键 = 没开（与 magpie 一致的默认关闭）。
//
// 桥侧覆盖（可选，<data-dir>/checkins.json，如 {"workbuddy": true}）优先级最高：
// 新面板里显式打开的渠道不必回头改 magpie 的设置文件。

import { existsSync, readFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'

export interface CheckinSwitchIndex {
  /** 某插件/前缀今天是否该签到 */
  enabled(pluginId: string, providerId?: string): boolean
  /** 判断依据（诊断用，不含凭据） */
  source: { magpie?: string; override?: string; warning?: string }
  /** 合并后的开关表，供 /admin/manifest 与面板展示 */
  table(): Record<string, boolean>
}

function readBoolMap(file: string): Record<string, boolean> | undefined {
  if (!existsSync(file)) return undefined
  try {
    const raw = JSON.parse(readFileSync(file, 'utf8')) as unknown
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return {}
    const out: Record<string, boolean> = {}
    for (const [k, v] of Object.entries(raw as Record<string, unknown>)) {
      if (typeof v === 'boolean') out[k] = v
    }
    return out
  } catch {
    return undefined
  }
}

export function loadCheckinSwitches(opts: { settingsPath?: string; dataDir?: string; overridePath?: string } = {}): CheckinSwitchIndex {
  const settingsPath = opts.settingsPath ?? join(homedir(), '.config', 'magpie', 'settings.json')
  const overridePath = opts.overridePath ?? (opts.dataDir ? join(opts.dataDir, 'checkins.json') : undefined)

  const source: CheckinSwitchIndex['source'] = {}
  let magpie: Record<string, boolean> = {}
  let override: Record<string, boolean> = {}

  if (existsSync(settingsPath)) {
    try {
      const raw = JSON.parse(readFileSync(settingsPath, 'utf8')) as { pluginCheckins?: Record<string, boolean> }
      magpie = raw.pluginCheckins && typeof raw.pluginCheckins === 'object' ? { ...raw.pluginCheckins } : {}
      source.magpie = settingsPath
    } catch (e) {
      source.warning = `settings.json unreadable: ${e instanceof Error ? e.message : String(e)}`
    }
  } else {
    source.warning = `no magpie settings at ${settingsPath}; check-ins default to off`
  }

  if (overridePath) {
    const o = readBoolMap(overridePath)
    if (o) {
      override = o
      source.override = overridePath
    }
  }

  const table = (): Record<string, boolean> => ({ ...magpie, ...override })

  return {
    source,
    table,
    enabled(pluginId: string, providerId?: string): boolean {
      // 桥侧显式覆盖 > magpie 的插件键 > magpie 的 provider 键 > 关
      if (pluginId in override) return override[pluginId]
      if (providerId && providerId in override) return override[providerId]
      if (pluginId in magpie) return magpie[pluginId] === true
      if (providerId && providerId in magpie) return magpie[providerId] === true
      return false
    },
  }
}
