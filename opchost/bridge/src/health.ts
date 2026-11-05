// GET /health（health.ts）
//
// 宿主 supervisor 每 10s 带 Bearer 探一次（internal/plugins/supervisor.go），
// 只以 2xx 判定健康；本端点允许无鉴权访问（与任务书一致：/health 不校验）。

import type { BridgeContext, HealthResponse } from "./types.ts";
import { BRIDGE_VERSION } from "./types.ts";

export function buildHealth(ctx: BridgeContext): HealthResponse {
  const uptimeS = Math.max(0, Math.floor((Date.now() - ctx.startedAt) / 1000));
  const providers = Object.keys(ctx.registry.providers).length;
  const models = Object.values(ctx.registry.providers).reduce(
    (acc, p) => acc + Object.keys(p.models).length,
    0,
  );
  return {
    status: providers > 0 ? "ok" : "degraded",
    id: ctx.config.pluginId,
    version: BRIDGE_VERSION,
    uptime_s: uptimeS,
    plugins_loaded: ctx.registry.plugins.length,
    providers,
    models,
    warnings: ctx.registry.warnings,
  };
}
