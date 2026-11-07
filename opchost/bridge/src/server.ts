// HTTP 服务、鉴权与**生产挂载**（server.ts）
//
// 端点与时序（顺序即安全边界，勿调换）：
//   1. GET /health                公开（宿主 2s 超时的探活）
//   2. —— Bearer 鉴权闸门 ——      此后一切都要票
//   3. GET /meta                  meta.ts
//   4. GET /v1/models             models.ts
//   5. POST /v1/chat/completions  chat 泳道（chat/index.ts 的 createChatHandler）
//   6. admin.handle(req,res)      管理面 + /accounts + /checkin（admin/index.ts）；
//                                 返回 true = 已处理，false 落到下一步
//   7. 兜底 404
//
// 关停：server.on("close") → admin.close()，回收挂起的登录会话与 127.0.0.1 回调监听。
//
// 凭据只有**一份**：admin 的加密库（<data-dir>/credentials.json + bridge.key）。
// 加载器（插件 client.auth.set）、chat（getAuth）、管理面三处共用同一个实例，
// 避免加载器的明文库与管理面的密文库争抢同名文件。

import { createHash, timingSafeEqual } from "node:crypto";
import { createServer } from "node:http";
import type { IncomingMessage, Server, ServerResponse } from "node:http";

import {
  adminDepsFromBridge,
  chatAuthStore,
  createAdminRouter,
  createFileCredentialStore,
} from "./admin/index.ts";
import type { AdminCredential, AdminCredentialStore, PluginSite, SitesResolver } from "./admin/index.ts";
import { createChatHandler, sendError } from "./chat/index.ts";
import type { AuthValue, ChatDeps, ProviderBinding } from "./chat/index.ts";
import type { PluginHooks } from "./chat/types.ts";
import type { EgressGuard } from "./egress/index.ts";
import { buildHealth } from "./health.ts";
import { buildHooksSurface } from "./hooks.ts";
import { bridgeCapabilities, buildMeta } from "./meta.ts";
import { buildModels } from "./models.ts";
import type { LoadResultWithSites } from "./loader.ts";
import type { BridgeContext, CredentialRecord, CredentialStore, JsonValue, OpenCodePlugin, PluginModelMap } from "./types.ts";
import { BRIDGE_NAME, errorBody } from "./types.ts";

const CHAT_PATH = "/v1/chat/completions";

// ---------------------------------------------------------------------------
// 凭据：admin 加密库 ↔ 共享 CredentialStore 的适配
// ---------------------------------------------------------------------------

function str(v: unknown): string | undefined {
  return typeof v === "string" && v.length > 0 ? v : undefined;
}

function num(v: unknown): number | undefined {
  return typeof v === "number" && Number.isFinite(v) ? v : undefined;
}

/** AdminCredential（account 级，secret 为原始材料）→ 共享 CredentialRecord（provider 级）。 */
function recordOf(cred: AdminCredential): CredentialRecord {
  const secret = (cred.secret ?? {}) as Record<string, unknown>;
  return {
    provider: cred.accountId,
    type: str(secret.type),
    access: str(secret.access) ?? str(secret.key),
    refresh: str(secret.refresh),
    expires: num(secret.expires) ?? (cred.expiresAt !== undefined ? cred.expiresAt * 1000 : undefined),
    label: cred.accountId,
    updated_at: cred.updatedAt,
  };
}

/**
 * 让加载器 / 插件看到的是**管理面那一个**加密库。
 * 查找口径与 admin/wiring.ts 的 chatAuthStore 一致（按 accountId 命中、忽略 pluginId），
 * 这样「登录写入 → chat 读取 → 插件回写续期」三条路径看到同一份凭据。
 */
export function bridgeCredentialStore(
  adminStore: AdminCredentialStore,
  pluginId: string,
): CredentialStore {
  const auth = chatAuthStore(adminStore, pluginId);
  const find = (provider: string): AdminCredential | undefined =>
    adminStore.list().find((c) => c.accountId === provider);

  return {
    count: () => adminStore.list().length,
    list: () => adminStore.list().map(recordOf),
    get: (provider: string) => {
      const cred = find(provider);
      return cred ? recordOf(cred) : undefined;
    },
    save: async (provider: string, body: JsonValue) => {
      auth.set(provider, (body ?? {}) as Record<string, unknown>);
      const cred = find(provider);
      if (cred) return recordOf(cred);
      // 存储实现若做了归一化导致落盘后仍取不到，返回入参投影而不抛：
      // 插件续期路径不能因为桥侧投影失败而整条中断。
      return recordOf({
        pluginId,
        accountId: provider,
        secret: (body ?? {}) as Record<string, unknown>,
        updatedAt: Date.now(),
      });
    },
  };
}

// ---------------------------------------------------------------------------
// chat 依赖注入（chat/ 只给接口，实现在这里）
// ---------------------------------------------------------------------------

/**
 * P8（task-14）：把插件的 auth.loader 返回的 fetch 包上 egress 白名单守卫。
 * 包装发生在 loader 边界（server.ts 的 getProvider），而不是 chat/upstream.ts 的
 * fetch 调用点——后者需要同步改 chat/handler.ts（chat 作用域）才能把守卫传进去，
 * 属于「更大的改动」；在边界包一次对 chat 完全透明，等价于「上游调用点换 wrapFetch」。
 */
function wrapLoaderFetch(
  plugin: OpenCodePlugin,
  pluginId: string,
  providerId: string,
  egress?: EgressGuard,
  hooks?: import("./hooks.ts").HookTracker,
): PluginHooks {
  // D-58：auth.loader 只有在插件真的带这个钩子时才会被包/被调用。
  if (!plugin.auth || typeof plugin.auth.loader !== "function") {
    return plugin as unknown as PluginHooks;
  }
  const innerLoader = plugin.auth.loader as (
    getAuth: () => Promise<unknown>,
  ) => Promise<{ fetch?: unknown; [key: string]: unknown } | undefined>;
  return {
    ...plugin,
    auth: {
      ...plugin.auth,
      loader: async (getAuth) => {
        // D-58 实际面：chat 每次解析上游选项都会走到 auth.loader。
        hooks?.count("auth.loader");
        const options = await innerLoader(getAuth);
        if (options && typeof options.fetch === "function") {
          // D-58 实际面：loader 给了自定义 fetch（协议转换点，审查重点）。
          hooks?.count("fetch_override");
          options.fetch = egress.wrapFetch({
            plugin: pluginId,
            provider: providerId,
            fetchImpl: options.fetch as typeof fetch,
          });
        }
        return options;
      },
    },
  } as unknown as PluginHooks;
}

/** 模型自带 api.url（workbuddy 形状）：loader 空 baseURL 时用它做上游地址。 */
/**
 * chat 泳道的上游基址解析（loader 返回空 baseURL 时用，workbuddy 形状）：
 *   1. 模型条目自带 api.url（modelOf 形状，登录后的动态列表）；
 *   2. provider 级 api（config 钩子写的 chat 基址，如 <endpoint>/v2——
 *      无凭据的静态表没有 per-model URL，OpenCode 自己也这么取）；
 *   3. 都没有 → undefined（上游报 501 no_upstream_url）。
 * Messages(Anthropic) 形状的模型（provider.npm = @ai-sdk/anthropic）明确不兜底：
 * OpenAI chat 泳道发过去对不上协议，宁可 501 也不打错地方。
 */
function modelApiUrlOf(
  providers: Record<string, { models?: PluginModelMap; api?: string }>,
  providerId: string,
  modelId: string,
): string | undefined {
  const rec = providers[providerId];
  const entry = rec?.models?.[modelId];
  if (entry) {
    if (typeof entry.api === "string" && entry.api.trim()) return entry.api.trim();
    if (entry.api && typeof entry.api === "object") {
      const url = (entry.api as { url?: unknown }).url;
      if (typeof url === "string" && url.trim()) return url.trim();
    }
    const pnpm = (entry as { provider?: { npm?: unknown } }).provider?.npm;
    if (typeof pnpm === "string" && /anthropic/i.test(pnpm)) return undefined;
  }
  const providerApi = typeof rec?.api === "string" ? rec.api.trim() : "";
  return providerApi || undefined;
}

function buildChatDeps(
  ctx: BridgeContext,
  adminStore: AdminCredentialStore,
  egressGuard?: EgressGuard,
): ChatDeps {
  const auth = chatAuthStore(adminStore, ctx.config.pluginId);
  return {
    listProviderIds: () => Object.keys(ctx.registry.providers),
    getProvider: (providerId: string): ProviderBinding | undefined => {
      const rec = ctx.registry.providers[providerId];
      if (!rec) return undefined;
      // ProviderRecord.plugin 是 LoadedPlugin，其 .plugin 才是 magpie 的 OpenCodePlugin。
      return {
        providerId: rec.id,
        label: rec.name,
        hooks: wrapLoaderFetch(
          rec.plugin.plugin,
          ctx.config.pluginId,
          rec.id,
          egressGuard,
          ctx.hooks,
        ),
      };
    },
    getModelApiUrl: (providerId: string, modelId: string) =>
      modelApiUrlOf(ctx.registry.providers, providerId, modelId),
    getAuth: (providerId: string) => auth.get(providerId) as AuthValue | null,
    saveAuth: (providerId: string, value: AuthValue) => {
      auth.set(providerId, value as unknown as Record<string, unknown>);
    },
    log: (level, message, meta) => {
      console.log(`[chat][${level}] ${message}${meta ? " " + JSON.stringify(meta) : ""}`);
    },
  };
}

/** 只有一个 provider 时让无前缀 model 直接落到它；多 provider 才要求前缀。 */
function soleProviderId(ctx: BridgeContext): string | undefined {
  const ids = Object.keys(ctx.registry.providers);
  return ids.length === 1 ? ids[0] : undefined;
}

// ---------------------------------------------------------------------------
// 鉴权与响应工具
// ---------------------------------------------------------------------------

function digest(value: string): Buffer {
  return createHash("sha256").update(value, "utf8").digest();
}

/**
 * 恒定时间比较 Bearer token。
 * 先各自 sha256 再 timingSafeEqual：长度差异不会提前返回，也不泄露 token 长度。
 */
export function authorize(header: string | undefined, expected: string): boolean {
  if (typeof header !== "string") return false;
  const match = /^Bearer[ \t]+(.+)$/i.exec(header.trim());
  if (!match) return false;
  const presented = match[1].trim();
  if (presented.length === 0) return false;
  return timingSafeEqual(digest(presented), digest(expected));
}

function sendJson(
  res: ServerResponse,
  status: number,
  body: unknown,
  extraHeaders?: Record<string, string>,
): void {
  if (res.headersSent || res.writableEnded) return;
  const payload = JSON.stringify(body);
  res.writeHead(status, {
    "Content-Type": "application/json; charset=utf-8",
    "Content-Length": Buffer.byteLength(payload),
    "Cache-Control": "no-store",
    ...extraHeaders,
  });
  res.end(payload);
}

function methodNotAllowed(res: ServerResponse, allowed: string): void {
  sendJson(
    res,
    405,
    errorBody(`method not allowed; use ${allowed}`, "invalid_request_error", "method_not_allowed"),
    { Allow: allowed },
  );
}

// ---------------------------------------------------------------------------
// 服务
// ---------------------------------------------------------------------------

export interface BridgeServerDeps {
  /**
   * 管理面凭据库。main.ts 传入以保证全桥只有**一个**实例
   * （加载器的 auth.set、chat 的 getAuth、/admin/credentials 共用）。
   */
  adminStore?: AdminCredentialStore;
  /**
   * P8 出口白名单守卫（task-14）。chat 上游 fetch 经它包装；未给则不启用
   * （selftest / 单测可省，生产必经 main.ts 传入）。
   */
  egressGuard?: EgressGuard;
}

export function createBridgeServer(ctx: BridgeContext, deps: BridgeServerDeps = {}): Server {
  const adminStore = deps.adminStore ?? createFileCredentialStore(ctx.config.dataDir);
  const log = (line: string): void => {
    console.log(line);
  };

  // P8：loader 已按 provider 采好 SITES（services 采集，含嵌套 hosts 摊平），
  // 这里把站点变成「顶层即 URL/裸 host」的平面形态交给 admin 的 sites resolver；
  // admin/manifest.ts 的 siteDomains 只扫顶层 http(s) URL，嵌套 hosts 不摊平会漏成空。
  const loaded = ctx.registry as unknown as LoadResultWithSites;
  const sites: SitesResolver = (providerId: string) => {
    const raw = loaded.sitesByProvider?.[providerId];
    if (!raw) return {};
    const hosts = (raw as { hosts?: Record<string, unknown> }).hosts;
    const flat: Record<string, unknown> = { id: raw.id ?? providerId, name: raw.name ?? providerId };
    if (hosts && typeof hosts === "object") {
      for (const [key, url] of Object.entries(hosts)) flat[`host_${key}`] = url;
    }
    if (typeof raw.webHost === "string" && raw.webHost) {
      flat.web_host = `https://${raw.webHost}/`;
    }
    return { [providerId]: flat as unknown as PluginSite };
  };

  // 管理面：/admin/*、/accounts、/accounts/{id}（PATCH/DELETE）、/checkin、
  // /accounts/{id}/checkin。build 阶段只创建路由，不监听端口。
  // capabilities 用与 /meta 同一个函数，保证「宿主看到的桥能力」两处不打架。
  const admin = createAdminRouter({
    ...adminDepsFromBridge(ctx, {
      pluginName: BRIDGE_NAME,
      capabilities: bridgeCapabilities(ctx),
      sites,
    }),
    credentials: adminStore,
  });

  // chat 泳道：POST /v1/chat/completions（SSE 与非流式聚合）
  const defaultProviderId = soleProviderId(ctx);
  const chat = createChatHandler({
    deps: buildChatDeps(ctx, adminStore, deps.egressGuard),
    ...(defaultProviderId ? { defaultProviderId } : {}),
  });

  const handle = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const startedAt = Date.now();
    const method = (req.method ?? "GET").toUpperCase();
    let pathname = "/";
    try {
      pathname = new URL(req.url ?? "/", "http://127.0.0.1").pathname;
    } catch {
      // 畸形 URL 按 404 处理，不抛。
    }
    res.on("finish", () => {
      log(`[bridge] ${method} ${pathname} ${res.statusCode} ${Date.now() - startedAt}ms`);
    });

    // ---- 1. 公开端点 ----
    if (pathname === "/health") {
      if (method !== "GET") return methodNotAllowed(res, "GET");
      return sendJson(res, 200, buildHealth(ctx));
    }

    // ---- 2. 鉴权闸门：此后所有端点都必须带正确 Bearer ----
    if (!authorize(req.headers.authorization, ctx.config.token)) {
      return sendJson(
        res,
        401,
        errorBody("missing or invalid bearer token", "authentication_error", "invalid_token"),
        { "WWW-Authenticate": "Bearer" },
      );
    }

    // ---- 3/4. 只读元信息 ----
    if (pathname === "/meta") {
      if (method !== "GET") return methodNotAllowed(res, "GET");
      return sendJson(res, 200, buildMeta(ctx));
    }

    if (pathname === "/v1/models") {
      if (method !== "GET") return methodNotAllowed(res, "GET");
      // ?refresh=1 在接上插件实时模型表前无实义，保持静默兼容。
      return sendJson(res, 200, buildModels(ctx));
    }

    // ---- 5. chat 泳道（方法校验归 chat handler：非 POST → 405）----
    if (pathname === CHAT_PATH) {
      return chat(req, res);
    }

    // ---- 5b. D-58 hooks 受控面（先于 admin.handle：/admin/ 前缀会被其 404 兜住）----
    if (pathname === "/admin/hooks") {
      if (method !== "GET") return methodNotAllowed(res, "GET");
      return sendJson(res, 200, buildHooksSurface(ctx));
    }

    // ---- 6. 管理面 + /accounts + /checkin（async；true = 已处理）----
    if (await admin.handle(req, res)) return;

    // ---- 7. 兜底 ----
    return sendJson(
      res,
      404,
      errorBody(`no route for ${method} ${pathname}`, "invalid_request_error", "not_found"),
    );
  };

  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    void handle(req, res).catch((err: unknown) => {
      // 与 chat 泳道同一套错误出口：未发头 → JSON 错误；已发头（流中）→ 错误帧 + [DONE]
      try {
        sendError(res, err);
      } catch {
        if (!res.writableEnded) res.end();
      }
    });
  });

  // 关停时序：先停接入，再回收管理面的临时回环监听与挂起的登录会话
  server.on("close", () => {
    admin.close();
  });

  server.on("clientError", (_err, socket) => {
    if (socket.writable) socket.end("HTTP/1.1 400 Bad Request\r\n\r\n");
  });

  return server;
}
