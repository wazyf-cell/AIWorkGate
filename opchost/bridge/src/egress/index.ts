// egress 泳道公共出口：创建守卫、装载宿主 env、启动期声明差距告警。
//
// 接线说明（本卡不动 bridge-core 的 server.ts/loader.ts，由 Lead 协调）：
//   - 加载期：loader 逐个插件实例化后，把插件 SITES 表的 network_domains 用
//     guard.declare({plugin, provider, domains}) 注册；坏声明按 LOADFAIL 拒装；
//   - 运行期：插件上游 fetch 一律换成 guard.wrapFetch({plugin, provider})，
//     node:http(s).request 换成 guard.wrapRequest(...)，目标 host 越界即拒并审计；
//   - 启动末：reportDeclarationGaps(...) 打「声明了域但请求了别处」的告警日志。

import type { EgressError, EgressGuard, EgressOptions } from "./guard.ts";
import { createEgressGuard } from "./guard.ts";
import { applyEgressConfig, loadEgressConfig, type LoadedEgressConfig } from "./config.ts";
import { anyMatch, normalizeHost, type DomainEntry } from "./whitelist.ts";

export { AuditLog } from "./audit.ts";
export type { AuditAction, AuditLine, AuditStats } from "./audit.ts";
export { EgressError, createEgressGuard } from "./guard.ts";
export type { DeclarationInput, EgressDecision, EgressGuard, EgressOptions } from "./guard.ts";
export {
  anyMatch,
  defaultResolver,
  isIPv4Literal,
  isPrivateOrReservedIPv4,
  isPrivateLiteral,
  normalizeHost,
  resolvesToPrivate,
  validateDeclarations,
  validateDomainEntry,
} from "./whitelist.ts";
export type { DomainEntry, EgressReason, Resolver } from "./whitelist.ts";
export { applyEgressConfig, loadEgressConfig } from "./config.ts";
export type { EgressFileShape, LoadedEgressConfig, RulesFileShape } from "./config.ts";

export interface DeclarationForGapReport {
  plugin: string;
  domains: string[];
  strict: boolean;
}

/**
 * 装载 env + 建守卫。这是接线方的默认入口：
 *   const { guard, config } = loadEgressFromEnv(process.env);
 */
export function loadEgressFromEnv(
  env: Parameters<typeof loadEgressConfig>[0],
  opts: EgressOptions = {},
  dataDirOverride?: string,
): { guard: EgressGuard; config: LoadedEgressConfig } {
  const config = loadEgressConfig(env, undefined, dataDirOverride);
  const guard = createEgressGuard({ ...opts, auditFile: config.auditFile });
  applyEgressConfig(guard, config);
  for (const w of config.warnings) {
    if (opts.log) opts.log("warn", `egress config: ${w}`);
  }
  return { guard, config };
}

/**
 * 启动期告警（本卡必做 ③）：插件声明了 network_domains，但插件实际要请求的
 * host（来自其 SITES 表的 api/auth/pay/webHost 等）不在声明内 → 返回告警行。
 * 接线方在加载完成后调用并把结果打日志。strict 插件未声明任何域但确有外联目标
 * 也告警（该插件在 strict 下会拒绝一切外联）。
 */
export function reportDeclarationGaps(
  declarations: DeclarationForGapReport[],
  actualHosts: Record<string, string[]>,
): string[] {
  const out: string[] = [];
  for (const decl of declarations) {
    const entries: DomainEntry[] = decl.domains.map((d) => {
      const v = normalizeHost(d);
      return { value: v.startsWith("*.") ? v.slice(2) : v, wildcard: v.startsWith("*.") };
    });
    const hosts = actualHosts[decl.plugin] ?? [];
    for (const raw of hosts) {
      const h = normalizeHost(raw);
      if (!anyMatch(h, entries)) {
        out.push(
          `[egress] 启动告警：插件「${decl.plugin}」声明了 network_domains（${decl.domains.join(", ") || "无"}），` +
            `但实际请求目标 ${h} 不在白名单内，strict=${decl.strict} 时该请求会被拒绝`,
        );
      }
    }
    if (decl.strict && (decl.domains.length === 0) && hosts.length > 0) {
      out.push(
        `[egress] 启动告警：插件「${decl.plugin}」strict=true 且未声明任何 network_domains，` +
          `其外联目标（${hosts.join(", ")}）将全部被拒`,
      );
    }
  }
  return out;
}