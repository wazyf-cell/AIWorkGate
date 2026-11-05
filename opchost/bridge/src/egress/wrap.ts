// 出口拦截包装器：把白名单判定缝进两种常见出口（docs/p8/01 §5.2）。
//   - wrapFetch：包 undici/globalThis.fetch；手动跟随重定向、每一跳重新判定（矩阵 #17）；
//   - wrapRequest：包 node:http(s).request 风格的同步返回函数；越界同步抛 EgressError。
// guard.ts 负责策略（声明/strict/审计），本文件只负责「怎么拦」。

import { EgressError, describeDeny } from "./egress-error.ts";
import type { AuditLog } from "./audit.ts";
import type { EgressReason } from "./whitelist.ts";

export interface WrapperDeps {
  audit: AuditLog;
  strictOf(plugin: string): boolean;
  syncDeny(params: { plugin: string; host: string; port: number }): EgressReason | null;
  assertAllowed(params: {
    plugin: string;
    provider: string;
    host: string;
    port?: number;
  }): Promise<void>;
  maxRedirectHops: number;
}

export function createWrappers(deps: WrapperDeps): {
  wrapFetch(params: { plugin: string; provider: string; fetchImpl?: typeof fetch }): typeof fetch;
  wrapRequest(params: {
    plugin: string;
    provider: string;
    original: (...args: unknown[]) => unknown;
  }): (...args: unknown[]) => unknown;
} {
  return {
    wrapFetch: ({ plugin, provider, fetchImpl }) => {
      const inner = fetchImpl ?? globalThis.fetch;
      return (async (input: Parameters<typeof fetch>[0], init?: Parameters<typeof fetch>[1]) => {
        let current = extractURL(input, plugin, provider);
        let hops = 0;
        let method = init?.method ?? "GET";
        let body: BodyInit | null | undefined = init?.body;
        for (;;) {
          const target = new URL(current);
          await deps.assertAllowed({ plugin, provider, host: target.hostname, port: portOfURL(target) });
          const res = await inner(target.href, { ...init, method, body, redirect: "manual" } as RequestInit);
          const status = res.status;
          if (status >= 300 && status < 400) {
            const location = res.headers.get("location");
            // 释放 3xx 响应体，避免 undici keep-alive 连接悬挂
            try {
              await res.body?.cancel();
            } catch {
              // 无关紧要
            }
            if (++hops > deps.maxRedirectHops) {
              throw new EgressError({
                reason: "EGRESS_BAD_TARGET",
                host: target.hostname,
                port: portOfURL(target),
                plugin,
                provider,
                message: `重定向超过 ${deps.maxRedirectHops} 跳，中止（出口白名单逐跳复核）`,
              });
            }
            if (!location) return res;
            const next = new URL(location, target.href);
            // 重定向不重新授权（矩阵 #17）：下一跳也要判定
            await deps.assertAllowed({ plugin, provider, host: next.hostname, port: portOfURL(next) });
            current = next.href;
            if (status === 303) {
              method = "GET";
              body = undefined;
            } else if ((status === 301 || status === 302) && method === "POST") {
              method = "GET";
              body = undefined;
            }
            continue;
          }
          return res;
        }
      }) as typeof fetch;
    },
    wrapRequest: ({ plugin, provider, original }) => {
      return (...args: unknown[]): unknown => {
        const target = extractRequestTarget(args);
        if (!target) {
          throw new EgressError({
            reason: "EGRESS_BAD_TARGET",
            host: "?",
            port: 0,
            plugin,
            provider,
            message: "无法解析请求目标（host 缺失）",
          });
        }
        const host = target.host.toLowerCase();
        const port = target.port;
        const denied = deps.syncDeny({ plugin, host, port });
        if (denied) {
          deps.audit.record({ plugin, provider, host, port, action: "denied", reason: denied });
          throw new EgressError({
            reason: denied,
            host,
            port,
            plugin,
            provider,
            message: describeDeny(denied, host, port, plugin, deps.strictOf(plugin)),
          });
        }
        // 同步层放行（域名级白名单命中 + 端口合法；SSRF 私网复查由调用方 async check 兜底）
        return original(...args);
      };
    },
  };
}

// ---- 目标解析 ---------------------------------------------------------------

function extractURL(input: Parameters<typeof fetch>[0], plugin: string, provider: string): string {
  if (typeof input === "string") return input;
  if (input instanceof URL) return input.href;
  const req = input as Request;
  if (req && typeof req.url === "string" && req.url) return req.url;
  throw new EgressError({
    reason: "EGRESS_BAD_TARGET",
    host: "?",
    port: 0,
    plugin,
    provider,
    message: "不支持的 fetch input 类型",
  });
}

function portOfURL(u: URL): number {
  if (u.port) return Number(u.port);
  return u.protocol === "https:" ? 443 : 80;
}

/** node:http.request 参数形态 (url, options?, cb?) / (options, cb?) → {host, port}。 */
function extractRequestTarget(args: unknown[]): { host: string; port: number } | null {
  let urlValue: unknown;
  let optsValue: unknown;
  if (typeof args[0] === "string" || args[0] instanceof URL) {
    urlValue = args[0];
    optsValue = args[1];
  } else {
    optsValue = args[0];
  }
  if (urlValue !== undefined) {
    try {
      const u = new URL(String(urlValue));
      return { host: u.hostname, port: portOfURL(u) };
    } catch {
      return null;
    }
  }
  const o = (optsValue ?? {}) as Record<string, unknown>;
  const host =
    typeof o.hostname === "string" && o.hostname
      ? o.hostname
      : typeof o.host === "string" && o.host
        ? o.host
        : "";
  if (!host) return null;
  const protocol = typeof o.protocol === "string" && o.protocol ? o.protocol : "https:";
  const rawPort = o.port !== undefined ? Number(o.port) : protocol === "http:" ? 80 : 443;
  const port = Number.isFinite(rawPort) && rawPort > 0 ? rawPort : protocol === "http:" ? 80 : 443;
  return { host, port };
}