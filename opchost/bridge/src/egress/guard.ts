// 出口强制层：策略（声明/strict/判定/审计）+ 包装器装配。
//
// 判定语义见 docs/p8/01-出口白名单设计与测试矩阵.md；传输拦截见 wrap.ts；
// 拒绝错误见 egress-error.ts。本文件只保留：声明注册、运行期 check/assert、
// strict 语义、以及把 wrap.ts 的两条拦截通道与策略接起来的装配。

import { AuditLog } from "./audit.ts";
import { describeDeny, EgressError } from "./egress-error.ts";
import { createWrappers } from "./wrap.ts";
import {
  anyMatch,
  defaultResolver,
  isIPv4Literal,
  isPrivateLiteral,
  normalizeHost,
  resolvesToPrivate,
  validateDeclarations,
  type DomainEntry,
  type Resolver,
  type EgressReason,
} from "./whitelist.ts";

export { describeDeny, EgressError } from "./egress-error.ts";

export interface DeclarationInput {
  plugin: string;
  provider: string;
  domains: string[];
}

export interface EgressOptions {
  strict?: boolean;
  blockPrivate?: boolean;
  maxDeclarations?: number;
  allowedPorts?: number[];
  maxRedirectHops?: number;
  auditFile?: string;
  resolve?: Resolver;
  now?: () => number;
  log?: (level: "info" | "warn" | "error", message: string) => void;
}

export interface EgressDecision {
  allowed: boolean;
  reason?: EgressReason;
  host: string;
  port: number;
}

const DEFAULT_PORTS = [80, 443];

interface PluginDeclaration {
  provider: string;
  entries: DomainEntry[];
}

export interface EgressGuard {
  readonly audit: AuditLog;
  configureStrict(strict: boolean): void;
  /** 加载期注册声明；整组非法返回 errors（LOADFAIL 语义），不注册该组。 */
  declare(input: DeclarationInput): { ok: boolean; errors: string[] };
  strictOf(plugin: string): boolean;
  declaredOf(plugin: string): string[];
  check(params: { plugin: string; provider: string; host: string; port?: number }): Promise<EgressDecision>;
  assertAllowed(params: { plugin: string; provider: string; host: string; port?: number }): Promise<void>;
  wrapFetch(params: { plugin: string; provider: string; fetchImpl?: typeof fetch }): typeof fetch;
  wrapRequest(params: { plugin: string; provider: string; original: (...args: unknown[]) => unknown }): (...args: unknown[]) => unknown;
}

export function createEgressGuard(opts: EgressOptions = {}): EgressGuard {
  let strictDefault = opts.strict ?? false;
  const blockPrivate = opts.blockPrivate ?? true;
  const resolve: Resolver = opts.resolve ?? defaultResolver;
  const allowedPorts = opts.allowedPorts ?? DEFAULT_PORTS;
  const audit = new AuditLog({
    file: opts.auditFile,
    now: opts.now,
    log: opts.log,
  });

  const declarations = new Map<string, PluginDeclaration>();
  const strictOverrides = new Map<string, boolean>();

  const strictOf = (plugin: string): boolean => strictOverrides.get(plugin) ?? strictDefault;

  const portAllowed = (port: number): boolean => allowedPorts.includes(port);

  /** 同步快速拒绝（wrapRequest 用，不依赖 DNS）。返回 null = 未在同步层拒绝。 */
  function syncDeny(params: { plugin: string; host: string; port: number }): EgressReason | null {
    const host = params.host;
    const port = params.port;
    if (!portAllowed(port)) return "EGRESS_PORT_FORBIDDEN";
    if (blockPrivate && isPrivateLiteral(host)) return "EGRESS_PRIVATE_ADDRESS_BLOCKED";
    const entries = declarations.get(params.plugin)?.entries ?? [];
    if (entries.length === 0) {
      return strictOf(params.plugin) ? "EGRESS_NO_DECLARATION" : null;
    }
    if (!anyMatch(host, entries)) {
      return strictOf(params.plugin) ? "EGRESS_DOMAIN_NOT_DECLARED" : null;
    }
    return null;
  }

  async function check(
    params: { plugin: string; provider: string; host: string; port?: number },
  ): Promise<EgressDecision> {
    const rawHost = normalizeHost(params.host);
    const port = params.port !== undefined && params.port > 0 ? params.port : 443;

    const sync = syncDeny({ plugin: params.plugin, host: rawHost, port });
    if (sync) {
      audit.record({ plugin: params.plugin, provider: params.provider, host: rawHost, port, action: "denied", reason: sync });
      return { allowed: false, reason: sync, host: rawHost, port };
    }

    const entries = declarations.get(params.plugin)?.entries ?? [];
    const domainsMatch = entries.length > 0 && anyMatch(rawHost, entries);

    // 域名未声明但非 strict：PASS + WARN 记账（矩阵 #10）
    if (!domainsMatch) {
      audit.record({ plugin: params.plugin, provider: params.provider, host: rawHost, port, action: "allowed", reason: "EGRESS_DOMAIN_NOT_DECLARED" });
      return { allowed: true, host: rawHost, port };
    }

    // 命中白名单的域名：SSRF 复查（解析到私网即拒，#13）——IP 字面量已在 syncDeny 处理
    if (blockPrivate && !isIPv4Literal(rawHost) && !rawHost.includes(":")) {
      const private_ = await resolvesToPrivate(rawHost, resolve);
      if (private_) {
        audit.record({ plugin: params.plugin, provider: params.provider, host: rawHost, port, action: "denied", reason: "EGRESS_PRIVATE_ADDRESS_BLOCKED" });
        return { allowed: false, reason: "EGRESS_PRIVATE_ADDRESS_BLOCKED", host: rawHost, port };
      }
    }
    return { allowed: true, host: rawHost, port };
  }

  async function assertAllowed(
    params: { plugin: string; provider: string; host: string; port?: number },
  ): Promise<void> {
    const d = await check(params);
    if (!d.allowed) {
      const reason = d.reason as EgressReason;
      throw new EgressError({
        reason,
        host: d.host,
        port: d.port,
        plugin: params.plugin,
        provider: params.provider,
        message: describeDeny(reason, d.host, d.port, params.plugin, strictOf(params.plugin)),
      });
    }
  }

  const wrappers = createWrappers({
    audit,
    strictOf,
    syncDeny,
    assertAllowed,
    maxRedirectHops: opts.maxRedirectHops ?? 5,
  });

  return {
    audit,
    configureStrict(strict: boolean): void {
      strictDefault = strict;
      strictOverrides.clear();
    },
    declare(input: DeclarationInput): { ok: boolean; errors: string[] } {
      const result = validateDeclarations(input.domains, opts.maxDeclarations ?? 64);
      if (!result.ok) return { ok: false, errors: result.errors };
      declarations.set(input.plugin, { provider: input.provider, entries: result.entries });
      return { ok: true, errors: [] };
    },
    strictOf,
    declaredOf(plugin: string): string[] {
      return (declarations.get(plugin)?.entries ?? []).map((e) => (e.wildcard ? `*.${e.value}` : e.value));
    },
    check,
    assertAllowed,
    wrapFetch: wrappers.wrapFetch,
    wrapRequest: wrappers.wrapRequest,
  };
}

export type { AuditAction } from "./audit.ts";