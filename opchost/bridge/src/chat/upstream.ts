// 上游调用：凭据 → loader → 构造 OpenAI 兼容请求 → fetch → 错误映射。
//
// 关键事实（只读核实）：trae 插件的聊天能力不是 provider 上的独立方法，而是
// `hooks.auth.loader(getAuth)` 返回的 `{ baseURL, apiKey, headers, fetch }`：
// 由 loader 的 fetch 拦截 `{baseURL}/chat/completions` 并完成 Trae 协议转换。
// 所以桥必须：
//   1. 调 loader(getAuth) 拿选项；
//   2. loader 给出 fetch 就用它的 fetch（不用全局 fetch）；
//   3. baseURL 缺失分两种：无自定义 fetch → 无凭据，判「需登录」（loader 无凭据
//      时返回 {}）；有自定义 fetch → workbuddy 形状（真包 L3753-3762 刻意返回
//      空 baseURL），上游地址改用模型自带的 api.url。
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
 * 解析 loader 选项；无凭据 → 401 需登录。
 * loader 可能抛 Expired 类错误（trae 的 `Expired` 会从 fresh/getAuth 冒出来），
 * 一并归为需登录。
 *
 * 空 baseURL 有两种形状，不能一概判「需登录」：
 *   - 无凭据（trae L3642 `if (!a0) return {}`）：没有自定义 fetch → 需登录；
 *   - workbuddy（真包 L3753-3762）：有凭据时刻意返回 `baseURL: ""`，上游地址由
 *     模型自带的 api.url 决定（注释原文 "no baseURL: a model's own api.url says
 *     where it is served"），其自定义 fetch 会原样请求传入的 URL → 合法，
 *     上游地址在 buildUpstreamRequest 用 deps.getModelApiUrl 解析。
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
  const apiKey = typeof options.apiKey === "string" ? options.apiKey : "";
  const hasOwnFetch = typeof options.fetch === "function";
  if (!baseURL) {
    // 没有自定义 fetch 的空 baseURL 才是「无凭据」（loader 拿不到凭据时返回空对象）
    if (!hasOwnFetch) {
      throw needsLoginError(binding.providerId);
    }
    return options;
  }
  // 有自定义 fetch（协议转换）时 apiKey 可能只是占位符；没有 fetch 且没有 key 视为凭据不全
  if (!hasOwnFetch && !apiKey) {
    throw needsLoginError(binding.providerId, "loader 返回了 baseURL 但没有凭据 key");
  }
  return options;
}

/** 构造上游请求：model 换成去前缀的真实名；其余字段走白名单。
 *
 * 上游地址：优先 loader 的 baseURL；为空时（workbuddy 形状）用模型自带
 * 的 api.url（deps.getModelApiUrl 解析后传入）。两者都没有 → 501。
 */
export function buildUpstreamRequest(
  route: ResolvedRoute,
  options: LoaderOptions,
  req: ChatRequest,
  stream: boolean,
  modelApiUrl?: string,
): { url: string; init: Record<string, unknown> } {
  const baseURL = typeof options.baseURL === "string" ? options.baseURL.trim() : "";
  const base = baseURL || (modelApiUrl ?? "").trim();
  if (!base) {
    throw new ChatError({
      status: 501,
      message:
        `模型「${route.modelId}」没有可用的上游地址：loader 未返回 baseURL，` +
        `模型配置里也没有可用的 api.url（若该模型走 Anthropic Messages 协议，` +
        `桥的 chat 泳道暂不支持）。`,
      code: "no_upstream_url",
    });
  }
  // workbuddy 形状（空 baseURL、地址取模型/provider 配置）：上游实测只收流式
  // 请求（11101 Non-stream chat request is currently not supported）。客户端要
  // 非流式时向上游强制 stream:true，回流 SSE 由 serveNonStream 聚合回 JSON。
  const usedModelUrl = !baseURL;
  const upstreamStream = stream || usedModelUrl;

  const body: Record<string, unknown> = {};
  for (const key of Object.keys(req.raw)) {
    if (FORWARD_KEYS.has(key) && req.raw[key] !== undefined) body[key] = req.raw[key];
  }
  body.model = route.modelId;
  body.stream = upstreamStream;
  if (req.temperature !== undefined) body.temperature = req.temperature;

  const headers: Record<string, string> = {
    "content-type": "application/json",
    accept: upstreamStream ? "text/event-stream" : "application/json",
  };
  if (options.apiKey) headers.authorization = `Bearer ${options.apiKey}`;
  for (const [k, v] of Object.entries(options.headers ?? {})) {
    headers[k.toLowerCase()] = v;
  }

  return {
    url: joinURL(base, "/chat/completions"),
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
  modelApiUrl?: string,
): Promise<Response> {
  const { url, init } = buildUpstreamRequest(route, options, req, stream, modelApiUrl);
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
    // P8（task-14）：出口白名单拒绝（egress 的 EgressError，status=403、reason=EGRESS_*）
    // 原样透传成 403，别被 networkError 吞成 502/500——这是策略拒绝，不是网络故障。
    const egress = err as { status?: number; reason?: string } | null;
    if (egress && egress.status === 403 && typeof egress.reason === "string" && egress.reason.startsWith("EGRESS_")) {
      throw new ChatError({
        status: 403,
        code: egress.reason.toLowerCase(),
        message: err instanceof Error ? err.message : String(err),
      });
    }
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
