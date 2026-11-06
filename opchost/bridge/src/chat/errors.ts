// chat 泳道的错误模型：一处定义，handler/upstream/stream 共用。
//
// 口径来自宿主契约（docs/插件格式规范.md §6「错误约定」）：
//   非 2xx 即失败；宿主优先取 `error.message`，其次正文前 256 字节，再其次状态文本。
// 因此桥对宿主/客户端暴露的错误体一律是 OpenAI 形状 { error: { message, type, code } }。

import type { OpenAIErrorBody } from "./types.ts";

export interface ChatErrorInit {
  status: number;
  message: string;
  /** OpenAI error.type：invalid_request_error / authentication_error / rate_limit_error / api_error */
  type?: string;
  code?: string | null;
  needsLogin?: boolean;
  /** 可读的补救提示（例如「如何登录」），会作为 error.hint 一起返回 */
  hint?: string;
  cause?: unknown;
}

/** 桥内统一错误。status 即要回给客户端的 HTTP 状态码。 */
export class ChatError extends Error {
  status: number;
  type: string;
  code: string | null;
  needsLogin: boolean;
  hint?: string;

  constructor(init: ChatErrorInit) {
    super(init.message);
    this.name = "ChatError";
    this.status = init.status;
    this.type = init.type ?? typeForStatus(init.status);
    this.code = init.code ?? null;
    this.needsLogin = init.needsLogin ?? false;
    if (init.hint) this.hint = init.hint;
    if (init.cause !== undefined) (this as { cause?: unknown }).cause = init.cause;
  }
}

function typeForStatus(status: number): string {
  if (status === 400 || status === 404 || status === 422) return "invalid_request_error";
  if (status === 401 || status === 403) return "authentication_error";
  if (status === 429) return "rate_limit_error";
  return "api_error";
}

/**
 * 「需登录」——本卡验收 ⑤ 的核心语义：插件没有可用凭据时给 401 + 可读文案，
 * 绝不让它变成 500 崩溃。
 */
export function needsLoginError(providerId: string, detail?: string): ChatError {
  const why = detail ? `（${detail}）` : "";
  return new ChatError({
    status: 401,
    type: "authentication_error",
    code: "needs_sign_in",
    needsLogin: true,
    message:
      `插件「${providerId}」尚未登录或登录已过期${why}，无法发起对话请求。` +
      `请先在管理面板完成「登录」：POST /admin/signin/start 拿到登录 URL，` +
      `在本机浏览器完成授权后凭据会写入桥的 DATA_DIR，随后重试本请求。`,
    hint:
      `登录入口：GET /accounts（确认账号状态）→ POST /admin/signin/start` +
      `（{"plugin_id":"…","provider":"…"}）→ 在本机浏览器打开返回的 url 完成授权。` +
      `凭据缺失时 /v1/models 仍可列出模型，只有 chat 需要登录态。`,
  });
}

export function badRequest(message: string, code?: string): ChatError {
  return new ChatError({ status: 400, message, code: code ?? null });
}

export function notFound(message: string, code?: string): ChatError {
  return new ChatError({ status: 404, message, code: code ?? null });
}

/** 上游/网络超时：504。 */
export function timeoutError(ms: number, what = "上游响应"): ChatError {
  return new ChatError({
    status: 504,
    message: `${what}超时（>${ms}ms），已中止本次请求。上游可能限流或网络不通。`,
    code: "upstream_timeout",
  });
}

/** 网络层失败（DNS/连接/重置）：502。 */
export function networkError(providerId: string, err: unknown): ChatError {
  const msg = err instanceof Error ? err.message : String(err);
  const aborted = /abort|cancel/i.test(msg);
  return new ChatError({
    status: aborted ? 499 : 502,
    message: aborted
      ? `请求已中止（插件「${providerId}」）：${msg}`
      : `无法连接插件「${providerId}」的上游：${msg}`,
    code: aborted ? "client_closed_request" : "upstream_unreachable",
    cause: err,
  });
}

/**
 * 上游非 2xx 的映射：4xx 保留原状态（那是用户错误，不该被吞成 502），
 * 5xx 收成 502 但把上游文案原样带出（宿主契约要求优先取 error.message）。
 */
export function upstreamError(
  providerId: string,
  status: number,
  message: string,
  signInExpired = false,
): ChatError {
  const text = message.trim();
  if (signInExpired || status === 401) {
    return new ChatError({
      status: 401,
      type: "authentication_error",
      code: "needs_sign_in",
      needsLogin: true,
      message: text || `插件「${providerId}」的上游判定登录态已失效（HTTP ${status}），请重新登录。`,
      hint: "登录入口：POST /admin/signin/start（管理面，本机回环可用）。",
    });
  }
  const mapped = status >= 500 ? 502 : status;
  return new ChatError({
    status: mapped >= 400 ? mapped : 502,
    message: text || `插件「${providerId}」的上游返回 HTTP ${status}。`,
    code: status >= 500 ? "upstream_error" : null,
  });
}

/** 把 ChatError（或未知错误）翻译成 OpenAI 错误体。 */
export function toErrorBody(err: unknown): OpenAIErrorBody {
  if (err instanceof ChatError) {
    const body: OpenAIErrorBody = {
      error: { message: err.message, type: err.type, code: err.code },
    };
    if (err.hint) body.error.hint = err.hint;
    return body;
  }
  const message = err instanceof Error ? err.message : String(err);
  return { error: { message, type: "api_error", code: null } };
}

/** 流式错误分片：`data: {"error":...}\n\n`（随后由调用方补 `data: [DONE]`）。 */
export function errorFrameFor(err: unknown): string {
  return `data: ${JSON.stringify(toErrorBody(err))}\n\n`;
}

export function statusOf(err: unknown, fallback = 500): number {
  return err instanceof ChatError ? err.status : fallback;
}

export function isAbort(err: unknown): boolean {
  return (
    err instanceof Error &&
    (err.name === "AbortError" || /abort|cancel/i.test(err.message))
  );
}
