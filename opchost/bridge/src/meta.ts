// GET /meta（meta.ts）
//
// 输出对齐宿主 internal/plugins/client.go:85 的 Meta 结构：
// {id, name, version, api_level, capabilities, account_count, uptime_s, started_at}。
// 宿主解码用 json.Unmarshal，多余字段一律忽略，所以可附 providers 摘要供排障。

import type {
  BridgeContext,
  Capability,
  MetaResponse,
  ProviderSummary,
} from "./types.ts";
import { BRIDGE_API_LEVEL, BRIDGE_ID, BRIDGE_NAME, BRIDGE_VERSION, CAP } from "./types.ts";

/**
 * 能力位 = **桥实际能应答的契约端点**，不是「插件碰巧有什么钩子」。
 *
 * 口径（与 Go 侧 DefaultBridgeCapabilities=[accounts, checkin] 对齐，见
 * internal/opchost/bridge.go）：
 *   upstream → POST /v1/chat/completions        （server.ts 挂的是 chat 泳道）
 *   accounts → GET /accounts                     （admin/router.ts handleAccounts）
 *   checkin  → /checkin + POST /accounts/{id}/checkin（admin D-19 委派）
 *   quota    → 额度经 /accounts 的 quota 字段透出；仅当插件确实有 auth.usage 时才声明
 *
 * **刻意不声明**（端点还没有，声明了就是骗宿主）：
 *   refresh            → POST /accounts/{id}/refresh 未实现
 *   login_api_key / login_oauth / login_paste_code / login_local_state / login_cookie
 *                      → 契约端点 /accounts/login* 未实现；登录实际走管理面
 *                        /admin/signin/start（管理面能力，不是宿主契约能力）
 *   identity_request / identity_os_switch
 *                      → POST /accounts/{id}/identity/reset 未实现（D-57 的透传待补）
 */
export function bridgeCapabilities(ctx: BridgeContext): Capability[] {
  const caps = new Set<Capability>([CAP.upstream, CAP.accounts, CAP.checkin]);
  for (const rec of Object.values(ctx.registry.providers)) {
    const auth = rec.plugin.plugin.auth;
    if (auth && typeof auth.usage === "function") caps.add(CAP.quota);
  }
  return [...caps].sort();
}

export function buildMeta(ctx: BridgeContext): MetaResponse {
  const uptimeS = Math.max(0, Math.floor((Date.now() - ctx.startedAt) / 1000));
  const providers: ProviderSummary[] = Object.values(ctx.registry.providers)
    .map((p) => ({ id: p.id, name: p.name, models: Object.keys(p.models).length }))
    .sort((a, b) => a.id.localeCompare(b.id));
  return {
    id: ctx.config.pluginId,
    name: BRIDGE_NAME,
    version: BRIDGE_VERSION,
    api_level: BRIDGE_API_LEVEL,
    capabilities: bridgeCapabilities(ctx),
    account_count: ctx.credentials.count(),
    uptime_s: uptimeS,
    started_at: Math.floor(ctx.startedAt / 1000),
    bridge: BRIDGE_ID,
    providers,
    plugin_dir: ctx.registry.dir,
    warnings: ctx.registry.warnings,
  };
}
