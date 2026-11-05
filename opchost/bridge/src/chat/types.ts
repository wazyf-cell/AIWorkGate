// 桥 chat 泳道（task-2）本地类型契约。
//
// 【待并入 ../types.ts】bridge-core（task-1）产出 src/types.ts 后，本文件应改为
// 从 '../types.ts' re-export，这里只保留 chat 专属类型（请求/响应/依赖注入）。
// 现在独立声明是为了不阻塞：本卡不得创建 ../types.ts。
//
// 设计依据（只读核实，不是臆造）：
// - %USERPROFILE%\.config\magpie\plugins\node_modules\@magpie-community\opencode-trae-auth\index.mjs
//   插件工厂 = `async ({ client }) => hooks`（L3031 makePlugin）
//   hooks.config(cfg) 写出 cfg.provider[siteId] = { name, npm, api, models }（L3581-3591）
//   hooks.auth.loader(getAuth) → { baseURL, apiKey, headers?, fetch }（L3641-3762）
//   getAuth() 的凭据形状 = toAuth()：{ type:'oauth', access, refresh, expires, uid, accountId, ... }（L400-421）
//   无凭据时 loader 返回 {}（L3642-3643 `if (!a0) return {}`）→ 桥据此判「需登录」
// - 宿主契约：docs/插件格式规范.md §6（`POST /v1/chat/completions` 流式原样透传、
//   非 2xx 即失败、优先取 error.message）、internal/plugins/client.go（errorMessage 同口径）

export type LogLevel = "debug" | "info" | "warn" | "error";

// ---- 入站请求 -------------------------------------------------------------

export interface ChatMessage {
  role: string;
  content?: unknown;
  name?: string;
  tool_call_id?: string;
  tool_calls?: unknown[];
  [key: string]: unknown;
}

/** 校验并归一化后的 chat 请求。`raw` 保留原始字段供白名单透传。 */
export interface ChatRequest {
  /** 原始 model 串（可能带 `provider/` 前缀） */
  model: string;
  messages: ChatMessage[];
  stream: boolean;
  temperature?: number;
  top_p?: number;
  max_tokens?: number;
  max_completion_tokens?: number;
  raw: Record<string, unknown>;
}

// ---- 插件侧形状（只读契约的 TS 侧镜像） -----------------------------------

/** getAuth() 的返回值。type='oauth' 时 access 是主凭据；type='api' 时 key 是。 */
export interface AuthValue {
  type?: string;
  access?: string;
  key?: string;
  refresh?: string;
  expires?: number;
  accountId?: string;
  uid?: string;
  [key: string]: unknown;
}

export type UpstreamFetch = (
  input: string | URL,
  init?: Record<string, unknown>,
) => Promise<Response>;

/** hooks.auth.loader 的返回值。`fetch` 存在时必须用它发请求（trae 的协议转换就在里面）。 */
export interface LoaderOptions {
  baseURL?: string;
  apiKey?: string;
  headers?: Record<string, string>;
  fetch?: UpstreamFetch;
}

export interface PluginConfig {
  provider: Record<string, PluginProviderConfig>;
}

export interface PluginProviderConfig {
  name?: string;
  npm?: string;
  api?: string;
  models?: Record<string, unknown>;
  [key: string]: unknown;
}

/** 插件暴露的 hooks 子集：chat 泳道只关心这三块。 */
export interface PluginHooks {
  config?: (config: PluginConfig) => void | Promise<void>;
  provider?: {
    id?: string;
    models?: (provider: unknown, options: { auth?: AuthValue | null }) => unknown;
  };
  auth?: {
    provider?: string;
    loader?: (
      getAuth: () => Promise<AuthValue | null>,
    ) => LoaderOptions | Promise<LoaderOptions>;
    methods?: unknown[];
    refresh?: (auth: AuthValue) => Promise<Partial<AuthValue> | undefined>;
  };
  "chat.headers"?: (...args: unknown[]) => unknown;
}

/** 一个已加载插件的绑定：providerId + hooks。 */
export interface ProviderBinding {
  providerId: string;
  label?: string;
  hooks: PluginHooks;
}

// ---- 依赖注入 -------------------------------------------------------------

export interface ChatConfig {
  /** 等上游响应头的超时（含流式首字节）。宿主契约：90s。 */
  headerTimeoutMs?: number;
  /** 非流式整体超时。 */
  nonStreamTimeoutMs?: number;
  /** 入站请求体上限（宿主网关同为 8 MiB 量级）。 */
  maxBodyBytes?: number;
}

/** chat 泳道对桥核心的全部依赖。bridge-core 只需实现这个接口即可挂载。 */
export interface ChatDeps {
  listProviderIds(): string[] | Promise<string[]>;
  getProvider(
    providerId: string,
  ): ProviderBinding | undefined | Promise<ProviderBinding | undefined>;
  getAuth(providerId: string): AuthValue | null | Promise<AuthValue | null>;
  saveAuth?(providerId: string, auth: AuthValue): void | Promise<void>;
  log?(level: LogLevel, message: string, meta?: Record<string, unknown>): void;
  config?: ChatConfig;
}

/** 解析后的路由：哪个 provider、上游真实 model 名、客户端原本要的 model。 */
export interface ResolvedRoute {
  providerId: string;
  modelId: string;
  requested: string;
}

// ---- 出站 OpenAI 形状 -----------------------------------------------------

export interface OpenAIUsage {
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  [key: string]: unknown;
}

export interface OpenAIToolCall {
  id?: string;
  type?: string;
  index?: number;
  function?: { name?: string; arguments?: string };
  [key: string]: unknown;
}

export interface OpenAIChatChoice {
  index: number;
  message?: {
    role?: string;
    content?: string | null;
    reasoning_content?: string;
    tool_calls?: OpenAIToolCall[];
    [key: string]: unknown;
  };
  delta?: {
    role?: string;
    content?: string;
    reasoning_content?: string;
    tool_calls?: OpenAIToolCall[];
    [key: string]: unknown;
  };
  finish_reason?: string | null;
  [key: string]: unknown;
}

export interface OpenAIChatCompletion {
  id: string;
  object: "chat.completion";
  created: number;
  model: string;
  choices: OpenAIChatChoice[];
  usage?: OpenAIUsage;
  system_fingerprint?: string;
  [key: string]: unknown;
}

/** OpenAI 风格错误体：{ error: { message, type, code } }（+ 桥自加的 hint）。 */
export interface OpenAIErrorBody {
  error: {
    message: string;
    type: string;
    code?: string | null;
    hint?: string;
  };
}
