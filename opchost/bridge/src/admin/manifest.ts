// GET /admin/manifest —— 从插件 SITES 表导出 network_domains（供宿主生成 manifest）。
//
// 依据计划 P1：「/admin/manifest（从插件 SITES 表导出 network_domains，供 manifest 生成）」，
// 与 internal/model/model.go 的 Permissions.NetworkDomains 对齐（P8 出口白名单用它）。
// 同时带出 D-20 的 quota_groups 定义、D-19 的签到能力声明、identity 能力声明（D-57）。

import type { IncomingMessage, ServerResponse } from 'node:http'
import type { PluginHandle, PluginSite, QuotaGroup } from './types.ts'
import { isLoopback, rejectRemote, sendJSON } from './util.ts'

/** 从任意字段值里抽出域（只认绝对 http(s) URL 的 hostname）。 */
export function hostOf(value: unknown): string | undefined {
  if (typeof value !== 'string' || !value) return undefined
  if (!/^https?:\/\//i.test(value)) return undefined
  try {
    const h = new URL(value).hostname.toLowerCase()
    return h || undefined
  } catch {
    return undefined
  }
}

/** 站点记录里所有可能带域名的字段（URL 形式的才采）。 */
function siteDomains(site: PluginSite): string[] {
  const out = new Set<string>()
  for (const v of Object.values(site)) {
    const h = hostOf(v)
    if (h) out.add(h)
  }
  return [...out]
}

export interface SiteView {
  id: string
  name: string
  endpoint?: string
  platform?: string
  /** model prefixes this site serves */
  models_hint?: string
  domains: string[]
}

export interface DerivedManifest {
  /** 插件身份（供宿主写 manifest.json） */
  plugin: { id: string; name: string; version?: string }
  /** 插件导出、宿主会当作 model 前缀的 provider 列表 */
  providers: string[]
  /** 结构：每个站点一行，便于审查 */
  sites: SiteView[]
  /** 去重后的出口域名白名单（P8） */
  network_domains: string[]
  capabilities: string[]
  /** D-19：签到委派能力声明 */
  checkin: { supported: boolean; delegated_to_plugin: true }
  /** D-57：指纹交给插件自持，管理面不透出明文 */
  identity: { delegated_to_plugin: true; exposed_in_admin: false }
  /** D-20：共享额度组 */
  quota_groups: QuotaGroup[]
  /** 该插件的 model 前缀 → 额度记录键 */
  quota_records: Record<string, string>
}

export function deriveManifest(handle: PluginHandle, quotaGroups: QuotaGroup[], prefixToRecord: Map<string, string>): DerivedManifest {
  const sites = handle.sites()
  const siteViews: SiteView[] = Object.entries(sites).map(([key, site]) => ({
    id: String(site.id ?? key),
    name: String(site.name ?? site.id ?? key),
    ...(site.endpoint ? { endpoint: String(site.endpoint) } : {}),
    ...(site.platform ? { platform: String(site.platform) } : {}),
    domains: siteDomains(site),
  }))

  const domains = new Set<string>()
  for (const s of siteViews) for (const d of s.domains) domains.add(d)

  const capabilities = handle.capabilities()
  const quotaRecords: Record<string, string> = {}
  for (const p of handle.providerIds) quotaRecords[`${p}/`] = prefixToRecord.get(p) ?? p

  return {
    plugin: { id: handle.id, ...(handle.name ? { name: handle.name } : {}), ...(handle.version ? { version: handle.version } : {}) },
    providers: handle.providerIds,
    sites: siteViews,
    network_domains: [...domains].sort(),
    capabilities,
    checkin: { supported: capabilities.includes('checkin'), delegated_to_plugin: true },
    identity: { delegated_to_plugin: true, exposed_in_admin: false },
    quota_groups: quotaGroups,
    quota_records: quotaRecords,
  }
}

export async function handleManifest(
  req: IncomingMessage,
  res: ServerResponse,
  pathname: string,
  resolveHandle: (id: string) => PluginHandle | undefined,
  build: (handle: PluginHandle) => DerivedManifest,
): Promise<boolean> {
  if (!isLoopback(req)) {
    rejectRemote(res)
    return true
  }
  if (pathname !== '/admin/manifest') return false
  const url = new URL(req.url ?? '/', 'http://127.0.0.1')
  const id = url.searchParams.get('plugin_id') ?? url.searchParams.get('id') ?? ''
  if (!id) {
    sendJSON(res, 400, { error: { message: 'plugin_id query parameter is required', type: 'invalid_request' } })
    return true
  }
  const handle = resolveHandle(id)
  if (!handle) {
    sendJSON(res, 404, { error: { message: `unknown plugin ${id}`, type: 'not_found' } })
    return true
  }
  sendJSON(res, 200, { ok: true, manifest: build(handle) })
  return true
}
