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
 * 能力口径（Lead 2026-11-06 裁定，P1 收尾-1；禁止再改回插件钩子推导式）：
 *
 * **基线 = [upstream, accounts, checkin]**，恒声明：
 *   upstream → POST /v1/chat/completions        （server.ts 挂的是 chat 泳道）
 *   accounts → GET /accounts                     （admin/router.ts handleAccounts）
 *   checkin  → /checkin + POST /accounts/{id}/checkin（admin D-19 委派）
 *
 * **quota 附加条件**：仅当任一插件确有 `auth.usage` 钩子才声明
 *   （额度经 /accounts 的 quota 字段透出；没有钩子就没有数据可透）。
 *
 * **D-57 identity 附加条件（task-35）**：仅当任一插件确有 `auth.identity` 钩子才声明
 *   identity_request；该钩子声明 identityOS（permissions.os_identity 语义）时再附加
 *   identity_os_switch。没有钩子就不声明——端点 /accounts/{id}/identity/reset 已实现，
 *   但只会对没有钩子的插件回 501，声明了就是骗宿主。
 *
 * **一律不声明**（端点没有接线，声明了就是骗宿主）：
 *   refresh            → POST /accounts/{id}/refresh 未实现
 *   login_api_key / login_oauth / login_paste_code / login_local_state / login_cookie
 *                      → 契约端点 /accounts/login* 未实现；登录实际走管理面
 *                        /admin/signin/start（管理面能力，不是宿主契约能力）
 *
 * 与 Go 侧 DefaultBridgeCapabilities=[accounts, checkin] 的差异（桥多了 upstream、
 * 条件 quota）记在 README §3.3，宿主 HasCapability 读的是 manifest 而非 /meta，不影响调度。
 */
export const BASELINE_CAPABILITIES: readonly Capability[] = [
  CAP.upstream,
  CAP.accounts,
  CAP.checkin,
] as const;

export function bridgeCapabilities(ctx: BridgeContext): Capability[] {
  const caps = new Set<Capability>(BASELINE_CAPABILITIES);
  for (const rec of Object.values(ctx.registry.providers)) {
    const auth = rec.plugin.plugin.auth;
    if (auth && typeof auth.usage === "function") caps.add(CAP.quota);
    // D-57（task-35）：插件真带 identity 钩子才声明；OS 级还需钩子声明 identityOS
    if (auth && typeof auth.identity === "function") {
      caps.add(CAP.identityRequest);
      if (auth.identityOS === true) caps.add(CAP.identityOS);
    }
  }
  return [...caps].sort();
}

export function buildMeta(ctx: BridgeContext): MetaResponse {
  const uptimeS = Math.max(0, Math.floor((Date.now() - ctx.startedAt) / 1000));
  const providers: ProviderSummary[] = Object.values(ctx.registry.providers)
    .map((p) => ({ id: p.id, name: p.name, models: Object.keys(p.models).length }))
    .sort((a, b) => a.id.localeCompare(b.id));
  // D-58 hooks 受控面摘要（宿主忽略未知字段；全量在 GET /admin/hooks）。
  const hooksSurface = ctx.hooks?.snapshot();
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
    ...(hooksSurface
      ? { hooks: { declared: hooksSurface.declared, actual: hooksSurface.actual } }
      : {}),
  };
}
