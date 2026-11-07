// 接线层：把 bridge-core 的 LoadResult / CredentialStore 适配成管理面的 PluginRegistry，
// 使 server.ts 只需两行即可挂上 /admin/*（server.ts:104 的挂载点）。
//
//   const admin = createAdminRouter(adminDepsFromBridge(ctx, { sites }))
//   // 在 createBridgeServer 的分发里：
//   //   if (await admin.handle(req, res)) return
//
// sites 由加载器给出（插件的模块级 `_internal.SITES`，真包 trae/workbuddy 都有）。
// 拿不到 SITES 时 /admin/manifest 的 network_domains 会是空数组并带 warning，
// 绝不猜域名。

import type {
  Capability,
  CredentialStore as SharedCredentialStore,
  LoadResult,
  OpenCodePlugin,
  BridgeContext,
  JsonValue,
} from '../types.ts'
import type { HookTracker } from '../hooks.ts'
import type { AdminCredentialStore, AdminDeps, PluginCheckinResult, PluginHandle, PluginRegistry, PluginSigninStart, PluginSite, SitesResolver } from './types.ts'

/** 共享 CredentialRecord → 插件 getAuth() 看到的 auth 对象（magpie 形状）。 */
function authOf(record: Record<string, unknown> | undefined): Record<string, unknown> | undefined {
  if (!record) return undefined
  if (record.type === 'api') return { type: 'api', key: record.access ?? record.key }
  return {
    type: 'oauth',
    access: record.access,
    refresh: record.refresh,
    expires: record.expires,
    // 桌面端带出来的凭据标 source:"desktop"（真包用它决定是否可续期）
    source: record.label === 'desktop' ? 'desktop' : record.source ?? 'desktop',
  }
}

function firstOAuthMethod(plugin: OpenCodePlugin) {
  const methods = plugin.auth?.methods
  if (!Array.isArray(methods) || !methods.length) return undefined
  return methods.find((m) => m.type === 'oauth' && typeof m.authorize === 'function') ?? methods.find((m) => typeof m.authorize === 'function')
}

/**
 * LoadResult 的嵌套形状：providers[<id>] 是 ProviderRecord，其 `.plugin` 是
 * LoadedPlugin，再往里一层 `.plugin` 才是 magpie 的 OpenCodePlugin：
 *   load.providers[x]           ProviderRecord
 *   load.providers[x].plugin    LoadedPlugin
 *   load.providers[x].plugin.plugin  OpenCodePlugin   ← auth / methods 在这里
 */
function openCodeOf(prov: { plugin?: { plugin?: OpenCodePlugin } }): OpenCodePlugin | undefined {
  return prov?.plugin?.plugin
}

export interface RegistryOptions {
  /** 宿主注入的插件 id（BridgeConfig.pluginId），缺省用目录名 */
  pluginId?: string
  pluginName?: string
  /** 插件模块级 SITES；拿不到就不导出域名 */
  sites?: SitesResolver
  /** 覆盖能力集；缺省按插件实际钩子推导 */
  capabilities?: Capability[]
  warnings?: string[]
  /** D-58：桥运行时 hooks 实际面计数（authorize/checkin 调用点）。可选。 */
  hooks?: HookTracker
}

/** LoadResult（bridge-core）→ 管理面 PluginRegistry（一个桥 = 一个插件句柄）。 */
export function registryFromLoad(load: LoadResult, credentials: SharedCredentialStore, opts: RegistryOptions = {}): PluginRegistry {
  const providers = Object.values(load.providers)
  const pluginId = opts.pluginId ?? (providers[0]?.plugin?.exportName ? `magpie:${
    // 与 host 注入的 pluginId 不同时只作兜底标识
    providers[0].plugin.exportName
  }` : 'magpie-bridge')

  const warnings = [...(opts.warnings ?? []), ...(load.warnings ?? [])]

  // 能力口径（task-9 裁定，与 meta.ts bridgeCapabilities 同基线）：
  //   基线 = [upstream, accounts, checkin] —— 桥实际能应答的契约端点
  //   quota 仅当有 provider 的插件真带 auth.usage 时附加（额度经 /accounts 的 quota 字段透出）
  // 刻意不声明（端点未实现，声明了就是骗宿主）：
  //   refresh（/accounts/{id}/refresh 未接线）、login_*（登录走管理面 /admin/signin，不是
  //   宿主契约 /accounts/login*）、identity_request/identity_os_switch（D-57 指纹透传未接线，
  //   声明 = 谎称桥能 reset identity）。
  const derived: Capability[] = ['upstream', 'accounts', 'checkin']
  const hasUsage = providers.some((p) => typeof openCodeOf(p)?.auth?.usage === 'function')
  if (hasUsage) derived.push('quota')
  derived.sort()

  const getAuthFor = (providerId: string) => async (): Promise<Record<string, unknown> | undefined> => {
    const rec = credentials.get(providerId) as unknown as Record<string, unknown> | undefined
    return authOf(rec)
  }

  const handle: PluginHandle = {
    id: pluginId,
    name: opts.pluginName ?? pluginId,
    providerIds: providers.map((p) => p.id),
    capabilities: () => opts.capabilities ?? derived,
    sites(): Record<string, PluginSite> {
      if (!opts.sites) {
        warnings.push('no SITES resolver supplied; network_domains will be empty')
        return {}
      }
      const merged: Record<string, PluginSite> = {}
      for (const p of providers) Object.assign(merged, opts.sites(p.id) ?? {})
      return merged
    },
    async authorize(providerId: string, o: { redirectUri?: string } = {}): Promise<PluginSigninStart> {
      const prov = load.providers[providerId]
      if (!prov) throw new Error(`unknown provider ${providerId}`)
      const method = firstOAuthMethod(openCodeOf(prov) as OpenCodePlugin)
      if (!method?.authorize) throw new Error(`provider ${providerId} declares no browser sign-in method`)
      // D-58 实际面：methods[].authorize 真实调用过一次。
      opts.hooks?.count('auth.methods')
      // 真包 authorize() 不收参数；桥自持回调时把 redirectUri 一起给过去（支持的插件会用）
      const start = (await method.authorize(o.redirectUri ? { redirectUri: o.redirectUri } : undefined)) as PluginSigninStart
      if (!start || typeof start.url !== 'string' || !start.url) throw new Error(`provider ${providerId} returned no sign-in URL`)
      return start
    },
    async checkin(providerId: string, _accountId: string): Promise<PluginCheckinResult> {
      const prov = load.providers[providerId]
      if (!prov) throw new Error(`unknown provider ${providerId}`)
      const fn = openCodeOf(prov)?.auth?.checkin
      if (typeof fn !== 'function') throw new Error(`provider ${providerId} has no check-in hook`)
      // D-58 实际面：auth.checkin 真实调用过一次。
      opts.hooks?.count('auth.checkin')
      const out = (await fn(getAuthFor(providerId))) as PluginCheckinResult | undefined
      if (!out || typeof out.outcome !== 'string') throw new Error(`provider ${providerId} returned no outcome`)
      return out
    },
    // 桥的凭据是 provider 级的，宿主看到的一个账号 = 一个 provider
    async accounts(providerId: string) {
      const prov = load.providers[providerId]
      if (!prov) return []
      return [{ id: providerId, label: prov.name }]
    },
  }

  return {
    list: () => [handle],
    resolve: (id: string) => (id === pluginId ? handle : undefined),
  }
}

/** BridgeContext 直接构造管理面 deps（server.ts 挂载点用）。 */
export function adminDepsFromBridge(ctx: BridgeContext, opts: RegistryOptions = {}): AdminDeps {
  return {
    dataDir: ctx.config.dataDir,
    plugins: registryFromLoad(ctx.registry, ctx.credentials, {
      pluginId: ctx.config.pluginId,
      // D-58：桥运行时 hooks 实际面（authorize/checkin 计数）随之进入管理面。
      ...(ctx.hooks ? { hooks: ctx.hooks } : {}),
      ...opts,
    }),
  }
}

/**
 * 管理面凭据库 → chat 泳道的 AuthStore（chat/credentials.ts 的 AuthStore 形状）。
 * 让 chat 与管理面共用同一份（加密落盘的）凭据，而不是各写一份 auth.json。
 */
export interface ChatAuthStore {
  get(providerId: string): Record<string, unknown> | null
  set(providerId: string, auth: Record<string, unknown>): void
  path(): string
}

/**
 * 管理面凭据库 → chat 泳道的 AuthStore（chat/credentials.ts 的 AuthStore 形状）。
 * 让 chat 与管理面共用同一份（加密落盘的）凭据，而不是各写一份 auth.json。
 *
 * pluginId 缺省 'magpie-bridge' 是 standalone 模式的标识；网关托管时传入
 * BridgeConfig.pluginId（如 bridge-wb），读写都落在宿主看到的那个插件名下，
 * 否则 /accounts 的 provider 行会一直查不到凭据。
 */
export function chatAuthStore(store: AdminCredentialStore, pluginId = 'magpie-bridge', fileHint = 'admin-credentials'): ChatAuthStore {
  return {
    get(providerId: string): Record<string, unknown> | null {
      const rec = store.get(pluginId, providerId) ?? store.list().find((r) => r.accountId === providerId)
      return rec ? { ...rec.secret } : null
    },
    set(providerId: string, auth: Record<string, unknown>): void {
      store.put({ pluginId, accountId: providerId, secret: auth as JsonValue as Record<string, unknown> })
    },
    path(): string {
      return fileHint
    },
  }
}
