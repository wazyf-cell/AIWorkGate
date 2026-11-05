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
import type { AdminCredential, AdminCredentialStore } from "./admin/index.ts";
import { createChatHandler, sendError } from "./chat/index.ts";
import type { AuthValue, ChatDeps, ProviderBinding } from "./chat/index.ts";
import { buildHealth } from "./health.ts";
import { bridgeCapabilities, buildMeta } from "./meta.ts";
import { buildModels } from "./models.ts";
import type { BridgeContext, CredentialRecord, CredentialStore, JsonValue } from "./types.ts";
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
  const auth = chatAuthStore(adminStore);
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

function buildChatDeps(ctx: BridgeContext, adminStore: AdminCredentialStore): ChatDeps {
  const auth = chatAuthStore(adminStore);
  return {
    listProviderIds: () => Object.keys(ctx.registry.providers),
    getProvider: (providerId: string): ProviderBinding | undefined => {
      const rec = ctx.registry.providers[providerId];
      if (!rec) return undefined;
      // ProviderRecord.plugin 是 LoadedPlugin，其 .plugin 才是 magpie 的 OpenCodePlugin。
      return {
        providerId: rec.id,
        label: rec.name,
        hooks: rec.plugin.plugin as unknown as ProviderBinding["hooks"],
      };
    },
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
}

export function createBridgeServer(ctx: BridgeContext, deps: BridgeServerDeps = {}): Server {
  const adminStore = deps.adminStore ?? createFileCredentialStore(ctx.config.dataDir);
  const log = (line: string): void => {
    console.log(line);
  };

  // 管理面：/admin/*、/accounts、/accounts/{id}（PATCH/DELETE）、/checkin、
  // /accounts/{id}/checkin。build 阶段只创建路由，不监听端口。
  // capabilities 用与 /meta 同一个函数，保证「宿主看到的桥能力」两处不打架。
  const admin = createAdminRouter({
    ...adminDepsFromBridge(ctx, {
      pluginName: BRIDGE_NAME,
      capabilities: bridgeCapabilities(ctx),
    }),
    credentials: adminStore,
  });

  // chat 泳道：POST /v1/chat/completions（SSE 与非流式聚合）
  const defaultProviderId = soleProviderId(ctx);
  const chat = createChatHandler({
    deps: buildChatDeps(ctx, adminStore),
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
