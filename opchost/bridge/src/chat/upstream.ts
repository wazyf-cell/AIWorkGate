// 上游调用：凭据 → loader → 构造 OpenAI 兼容请求 → fetch → 错误映射。
//
// 关键事实（只读核实）：trae 插件的聊天能力不是 provider 上的独立方法，而是
// `hooks.auth.loader(getAuth)` 返回的 `{ baseURL, apiKey, headers, fetch }`：
// 由 loader 的 fetch 拦截 `{baseURL}/chat/completions` 并完成 Trae 协议转换。
// 所以桥必须：
//   1. 调 loader(getAuth) 拿选项；
//   2. loader 给出 fetch 就用它的 fetch（不用全局 fetch）；
//   3. baseURL 缺失 → 判定「需登录」（loader 无凭据时返回 {}）。
//
// 宿主契约：非 2xx 即失败；流式原样透传；90s 头超时。

import { ChatError, networkError, needsLoginError, timeoutError, upstreamError } from "./errors.ts";
import type {
  ChatDeps,
  ChatRequest,
  LoaderOptions,
  ProviderBinding,
  ResolvedRoute,
} from "./types.ts";

/** 出站请求体白名单：OpenAI chat 标准字段 + reasoning_effort；不透传桥内部字段。 */
const FORWARD_KEYS = new Set([
  "messages",
  "model",
  "stream",
  "temperature",
  "top_p",
  "top_k",
  "max_tokens",
  "max_completion_tokens",
  "stop",
  "presence_penalty",
  "frequency_penalty",
  "logit_bias",
  "seed",
  "n",
  "response_format",
  "tools",
  "tool_choice",
  "parallel_tool_calls",
  "reasoning_effort",
  "reasoningEffort",
  "user",
  "stream_options",
  "metadata",
  "service_tier",
]);

export function joinURL(baseURL: string, path: string): string {
  const base = baseURL.replace(/\/+$/, "");
  return `${base}/${path.replace(/^\/+/, "")}`;
}

/**
 * 解析 loader 选项；无 baseURL 视为无凭据 → 401 需登录。
 * loader 可能抛 Expired 类错误（trae 的 `Expired` 会从 fresh/getAuth 冒出来），
 * 一并归为需登录。
 */
export async function resolveLoaderOptions(
  binding: ProviderBinding,
  deps: ChatDeps,
): Promise<LoaderOptions> {
  const loader = binding.hooks.auth?.loader;
  if (typeof loader !== "function") {
    throw new ChatError({
      status: 501,
      message: `插件「${binding.providerId}」没有实现 auth.loader，无法发起对话。`,
      code: "loader_missing",
    });
  }
  let options: LoaderOptions;
  try {
    options = await loader(() => deps.getAuth(binding.providerId));
  } catch (err) {
    const expired = err instanceof Error && /sign-in|expired|登录|not signed in/i.test(err.message);
    if (expired) throw needsLoginError(binding.providerId, err.message);
    throw new ChatError({
      status: 502,
      message: `插件「${binding.providerId}」的 auth.loader 抛错：${err instanceof Error ? err.message : String(err)}`,
      code: "loader_failed",
      cause: err,
    });
  }
  const baseURL = typeof options.baseURL === "string" ? options.baseURL.trim() : "";
  if (!baseURL) {
    // loader 拿不到凭据时（如 trae L3642 `if (!a0) return {}`）返回空对象 → 需登录
    throw needsLoginError(binding.providerId);
  }
  const apiKey = typeof options.apiKey === "string" ? options.apiKey : "";
  const hasOwnFetch = typeof options.fetch === "function";
  // 有自定义 fetch（协议转换）时 apiKey 可能只是占位符；没有 fetch 且没有 key 视为凭据不全
  if (!hasOwnFetch && !apiKey) {
    throw needsLoginError(binding.providerId, "loader 返回了 baseURL 但没有凭据 key");
  }
  return options;
}

/** 构造上游请求：model 换成去前缀的真实名；其余字段走白名单。 */
export function buildUpstreamRequest(
  route: ResolvedRoute,
  options: LoaderOptions,
  req: ChatRequest,
  stream: boolean,
): { url: string; init: Record<string, unknown> } {
  const body: Record<string, unknown> = {};
  for (const key of Object.keys(req.raw)) {
    if (FORWARD_KEYS.has(key) && req.raw[key] !== undefined) body[key] = req.raw[key];
  }
  body.model = route.modelId;
  body.stream = stream;
  if (req.temperature !== undefined) body.temperature = req.temperature;

  const headers: Record<string, string> = {
    "content-type": "application/json",
    accept: stream ? "text/event-stream" : "application/json",
  };
  if (options.apiKey) headers.authorization = `Bearer ${options.apiKey}`;
  for (const [k, v] of Object.entries(options.headers ?? {})) {
    headers[k.toLowerCase()] = v;
  }

  return {
    url: joinURL(options.baseURL as string, "/chat/completions"),
    init: { method: "POST", headers, body: JSON.stringify(body) },
  };
}

/**
 * 发起上游调用。返回已就绪的 Response（可能是 SSE 流）。
 * 超时：非流式给整体超时；流式给头超时（首字节），之后交给调用方持续读。
 */
export async function callUpstream(
  route: ResolvedRoute,
  options: LoaderOptions,
  req: ChatRequest,
  stream: boolean,
  config: { headerTimeoutMs: number; nonStreamTimeoutMs: number },
): Promise<Response> {
  const { url, init } = buildUpstreamRequest(route, options, req, stream);
  const ac = new AbortController();
  const budget = stream ? config.headerTimeoutMs : config.nonStreamTimeoutMs;
  const timer = setTimeout(() => ac.abort(), budget);
  const fetchImpl = (options.fetch ?? globalThis.fetch).bind(globalThis) as typeof fetch;

  let res: Response;
  try {
    res = await fetchImpl(url, { ...init, signal: ac.signal });
  } catch (err) {
    clearTimeout(timer);
    if (ac.signal.aborted) throw timeoutError(budget);
    throw networkError(route.providerId, err);
  } finally {
    if (!stream) clearTimeout(timer);
  }

  if (res.status < 200 || res.status >= 300) {
    clearTimeout(timer);
    throw await mapNon2xx(route, res);
  }
  if (stream) {
    // 流式已拿到 200：清掉头超时，后续按上游节奏读
    clearTimeout(timer);
  } else {
    clearTimeout(timer);
  }
  return res;
}

/** 从上游非 2xx 响应提取可读 message（宿主契约：error.message → 正文前 256 字节 → 状态文本）。 */
async function mapNon2xx(route: ResolvedRoute, res: Response): Promise<ChatError> {
  let text = "";
  try {
    text = (await res.text()).trim();
  } catch {
    // 读失败就用状态文本
  }
  let message = "";
  try {
    const parsed = JSON.parse(text) as { error?: { message?: string } };
    if (parsed?.error?.message) message = String(parsed.error.message);
  } catch {
    // 非 JSON
  }
  if (!message) message = text.slice(0, 256) || `HTTP ${res.status}`;
  const signInHeader = res.headers.get("x-magpie-sign-in")?.toLowerCase() === "expired";
  return upstreamError(route.providerId, res.status, message, signInHeader);
}

/** 读取一个非流式响应的文本，做整体超时兜底（供 handler 在 fetch 已完成后用）。 */
export async function readBodyWithTimeout(
  res: Response,
  budgetMs: number,
): Promise<{ text: string; contentType: string }> {
  const contentType = res.headers.get("content-type") ?? "";
  const text = await Promise.race([
    res.text(),
    new Promise<string>((_resolve, reject) =>
      setTimeout(() => reject(new ChatError({ status: 504, message: "读取上游响应体超时", code: "upstream_timeout" })), budgetMs),
    ),
  ]);
  return { text, contentType };
}
