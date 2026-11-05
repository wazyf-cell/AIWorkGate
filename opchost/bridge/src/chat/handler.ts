// /v1/chat/completions 处理编排：读体 → 校验 → 路由 → 凭据 → 上游 → 回写。
//
// 挂载方式（供 bridge-core 用）：
//   const chat = createChatHandler({ deps });
//   httpServer.on("request", (req, res) => {
//     if (req.method === "POST" && urlPath(req) === "/v1/chat/completions") chat(req, res);
//     ...
//   });
// 非 /v1/chat/completions 的路径由桥核心的 server.ts 决定，本模块不越界。

import type { IncomingMessage, ServerResponse } from "node:http";
import { aggregateChunks, newCompletionId, normalizeCompletion } from "./aggregate.ts";
import {
  ChatError,
  badRequest,
  notFound,
  statusOf,
  toErrorBody,
  upstreamError,
} from "./errors.ts";
import { bodyTooLarge, parseChatRequestBody } from "./parse.ts";
import { resolveModelRoute } from "./route.ts";
import { parseSSEStream } from "./sse.ts";
import { pipeUpstreamStream, SSEWriter } from "./stream-pipe.ts";
import type { ChatDeps, ChatRequest } from "./types.ts";
import { callUpstream, readBodyWithTimeout, resolveLoaderOptions } from "./upstream.ts";

export interface ChatHandlerOptions {
  deps: ChatDeps;
  /** 无前缀 model 且多 provider 时的默认归属（通常来自 /v1/models 的默认值） */
  defaultProviderId?: string;
}

const DEFAULT_HEADER_TIMEOUT_MS = 90_000;
const DEFAULT_NONSTREAM_TIMEOUT_MS = 300_000;
const DEFAULT_MAX_BODY_BYTES = 8 << 20;

export function createChatHandler(options: ChatHandlerOptions) {
  const deps = options.deps;
  const defaultProviderId = options.defaultProviderId;

  return async function chatHandler(
    req: IncomingMessage,
    res: ServerResponse,
  ): Promise<void> {
    const cfg = deps.config ?? {};
    const maxBody = cfg.maxBodyBytes ?? DEFAULT_MAX_BODY_BYTES;
    try {
      if (req.method !== "POST") {
        throw new ChatError({
          status: 405,
          message: "仅支持 POST /v1/chat/completions",
          code: "method_not_allowed",
        });
      }
      const rawText = await readBody(req, maxBody);
      let rawJson: unknown;
      try {
        rawJson = JSON.parse(rawText);
      } catch {
        throw badRequest("请求体不是合法 JSON", "bad_json");
      }
      const request = parseChatRequestBody(rawJson);
      deps.log?.("info", `chat: ${request.model} stream=${request.stream} messages=${request.messages.length}`, {
        model: request.model,
        stream: request.stream,
      });
      await serve(request, deps, res, defaultProviderId);
    } catch (err) {
      sendError(res, err);
    }
  };
}

async function serve(
  request: ChatRequest,
  deps: ChatDeps,
  res: ServerResponse,
  defaultProviderId?: string,
): Promise<void> {
  const providerIds = await deps.listProviderIds();
  if (providerIds.length === 0) {
    throw new ChatError({
      status: 503,
      message: "桥尚未加载任何插件 provider，无法路由 chat 请求。",
      code: "no_providers",
    });
  }
  const route = resolveModelRoute(request.model, providerIds, defaultProviderId);
  const binding = await deps.getProvider(route.providerId);
  if (!binding) {
    throw notFound(
      `provider「${route.providerId}」不存在（已加载：${providerIds.join(", ")}）`,
      "unknown_provider",
    );
  }

  // 凭据探测：loader 拿不到凭据 → 401 需登录（本卡验收 ⑤）
  const options = await resolveLoaderOptions(binding, deps);

  const cfg = deps.config ?? {};
  const headerTimeoutMs = cfg.headerTimeoutMs ?? DEFAULT_HEADER_TIMEOUT_MS;
  const nonStreamTimeoutMs = cfg.nonStreamTimeoutMs ?? DEFAULT_NONSTREAM_TIMEOUT_MS;

  const upstream = await callUpstream(route, options, request, request.stream, {
    headerTimeoutMs,
    nonStreamTimeoutMs,
  });

  if (request.stream) {
    const writer = new SSEWriter(res);
    const created = Math.floor(Date.now() / 1000);
    await pipeUpstreamStream(upstream, writer, {
      model: request.model,
      chunkId: newCompletionId(),
      created,
    });
    return;
  }

  await serveNonStream(upstream, request, nonStreamTimeoutMs, res);
}

async function serveNonStream(
  upstream: Response,
  request: ChatRequest,
  nonStreamTimeoutMs: number,
  res: ServerResponse,
): Promise<void> {
  const contentType = (upstream.headers.get("content-type") ?? "").toLowerCase();

  if (contentType.includes("text/event-stream")) {
    // 上游无视 stream=false 仍回流：把帧聚合回非流式（本卡验收 ③ 的兜底路径）
    const payloads: string[] = [];
    if (upstream.body) {
      for await (const event of parseSSEStream(upstream.body)) {
        payloads.push(event.data);
      }
    }
    const completion = aggregateChunks(payloads, {
      model: request.model,
      messages: request.messages,
    });
    sendJson(res, 200, completion);
    return;
  }

  const { text } = await readBodyWithTimeout(upstream, nonStreamTimeoutMs);
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new ChatError({
      status: 502,
      message: `上游返回了无法解析的 JSON：${text.slice(0, 200)}`,
      code: "bad_upstream_body",
    });
  }
  const obj = (typeof parsed === "object" && parsed !== null ? parsed : {}) as Record<
    string,
    unknown
  >;
  if (obj.error) {
    // 200 但带 error 体（trae 在流内错误前也会这么表达）
    const msg = typeof obj.error === "object"
      ? String((obj.error as Record<string, unknown>).message ?? JSON.stringify(obj.error))
      : String(obj.error);
    throw upstreamError(request.model, 502, msg);
  }
  const completion = normalizeCompletion(parsed, {
    model: request.model,
    messages: request.messages,
  });
  sendJson(res, 200, completion);
}

async function readBody(req: IncomingMessage, maxBytes: number): Promise<string> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of req) {
    const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as Uint8Array);
    size += buf.length;
    if (bodyTooLarge(size, maxBytes)) {
      throw new ChatError({
        status: 413,
        message: `请求体超过上限（${maxBytes} 字节）`,
        code: "payload_too_large",
      });
    }
    chunks.push(buf);
  }
  return Buffer.concat(chunks).toString("utf8");
}

export function sendJson(
  res: ServerResponse,
  status: number,
  body: unknown,
): void {
  if (res.headersSent || res.writableEnded) return;
  const text = JSON.stringify(body);
  res.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "cache-control": "no-cache",
  });
  res.end(text);
}

/** 统一错误出口：未发响应头 → 完整 JSON 错误；已发（流中）→ 错误帧 + [DONE]。 */
export function sendError(res: ServerResponse, err: unknown): void {
  const status = statusOf(err, 500);
  const body = toErrorBody(err);
  if (res.headersSent || res.writableEnded) {
    try {
      res.write(`data: ${JSON.stringify(body)}\n\n`);
      res.write("data: [DONE]\n\n");
      res.end();
    } catch {
      // 客户端已断开
    }
    return;
  }
  sendJson(res, status, body);
}
