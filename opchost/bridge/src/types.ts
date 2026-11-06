// 全桥共享类型契约（types.ts）
//
// 定位：本文件是 bridge 的**唯一契约源**，chat / admin 两条泳道必须 import 它，
// 不许各自定义同名字段。字段口径对齐 Go 侧：
//   - internal/model/model.go   （Manifest / ModelInfo / Account / Credential…）
//   - internal/plugins/client.go（GET /meta 的 Meta 结构）
//
// 约定：
//   1. 只用「可擦除」类型语法（interface / type / as const）。Node 24 直接跑 .ts
//      靠类型擦除，enum / namespace / 参数属性 一律不支持，禁止使用。
//   2. 与服务端 JSON 对齐的字段名一律 snake_case（Go json tag 原样），
//      桥内部字段用 camelCase，两边都在同名对象上时以注释标出。
//   3. 绝不把凭据字段写进日志或错误体。

// ---------------------------------------------------------------------------
// 基础
// ---------------------------------------------------------------------------

export type JsonPrimitive = string | number | boolean | null;
export type JsonValue = JsonPrimitive | JsonValue[] | { [key: string]: JsonValue };

/** 桥的身份（/meta 的 id/name/version，宿主据此识别插件）。 */
export const BRIDGE_ID = "magpie-bridge";
export const BRIDGE_NAME = "Magpie Bridge";
export const BRIDGE_VERSION = "0.1.0";

/** 契约级别：宿主 internal/model/model.go 的 MinAPILevel/MaxAPILevel = 1..2。 */
export const MIN_API_LEVEL = 1;
export const MAX_API_LEVEL = 2;
/** 桥的能力集含 identity 透传（D-57），必须 level 2。 */
export const BRIDGE_API_LEVEL = 2;

/** 能力位，逐字对齐 internal/model/model.go 的 Cap* 常量。 */
export const CAP = {
  upstream: "upstream",
  accounts: "accounts",
  checkin: "checkin",
  quota: "quota",
  loginApiKey: "login_api_key",
  loginOAuth: "login_oauth",
  loginPasteCode: "login_paste_code",
  loginIDEPlugin: "login_ide_plugin",
  loginLocalState: "login_local_state",
  loginCookie: "login_cookie",
  refresh: "refresh",
  egress: "egress",
  identityRequest: "identity_request",
  identityOS: "identity_os_switch",
} as const;

export type Capability = (typeof CAP)[keyof typeof CAP];

/** 账号健康状态，对齐 model.go 的 Account* / Checkin* 常量。 */
export const ACCOUNT_OK = "ok";
export const ACCOUNT_EXPIRED = "expired";
export const ACCOUNT_INVALID = "invalid";
export const ACCOUNT_RATE_LIMITED = "rate_limited";
export const ACCOUNT_UNKNOWN = "unknown";

export const CHECKIN_OK = "ok";
export const CHECKIN_ALREADY = "already";
export const CHECKIN_FAIL = "fail";
/** 宿主主动跳过（冷却中 / 该设备今天已签到），不是成功。 */
export const CHECKIN_SKIPPED = "skipped";

// ---------------------------------------------------------------------------
// 模型（GET /v1/models 的条目，宿主 model.ParseModelList 解析）
// ---------------------------------------------------------------------------

/**
 * ModelInfo 对齐 model.go:128。宿主解析器是宽容的：接受 `{data:[…]}`、
 * `{models:[…]}` 或裸数组三种外壳。
 *
 * 桥扩展字段（宿主会忽略，供桥自身 / 面板使用）：
 *   - local_id：未加 provider 前缀的插件内 id
 *   - provider：所属 provider id
 */
export interface ModelInfo {
  id: string;
  name?: string;
  context_length?: number;
  tools?: boolean;
  vision?: boolean;
  vendor?: string;
  multiplier?: number;
  thinking?: string[];
  free?: boolean;
  // —— 桥扩展 ——
  local_id?: string;
  provider?: string;
}

/** GET /v1/models 的响应外壳（OpenAI 兼容）。 */
export interface ModelsResponse {
  object: "list";
  data: ModelInfo[];
}

// ---------------------------------------------------------------------------
// 插件清单（manifest.json，宿主 model.Manifest）
// ---------------------------------------------------------------------------

export interface Permissions {
  read_paths?: string[];
  launch_apps?: string[];
  network_domains?: string[];
  use_proxy?: boolean;
  os_identity?: boolean;
  require_admin?: boolean;
}

export interface CheckinConfig {
  default_time?: string;
  supports_history?: boolean;
}

export interface QuotaConfig {
  /** null/缺省 = 契约默认 30 分钟；0 = 宿主不按定时器轮询。 */
  poll_minutes?: number | null;
}

export interface ConcurrencyConfig {
  suggested?: number;
}

export interface RulesConfig {
  packs?: string[];
}

export interface EgressConfig {
  strict?: boolean;
}

export interface StartupConfig {
  mode?: "always" | "on_demand";
  idle_timeout_s?: number;
}

export interface LoginConfig {
  methods?: string[];
  reads_local_state?: boolean;
  requires_app?: boolean;
  auto_refresh?: boolean;
  notes?: string;
}

/** 插件的启动入口。桥插件的 exe 指向共享的 magpie-bridge，args 指向插件目录。 */
export interface PluginEntry {
  exe: string;
  args?: string[];
}

export interface PluginManifest {
  id: string;
  name: string;
  version: string;
  api_level: number;
  description?: string;
  author?: string;
  homepage?: string;
  os?: string[];
  capabilities?: Capability[];
  permissions: Permissions;
  models?: ModelInfo[];
  checkin: CheckinConfig;
  quota: QuotaConfig;
  concurrency: ConcurrencyConfig;
  rules: RulesConfig;
  egress: EgressConfig;
  startup: StartupConfig;
  entry: PluginEntry;
  login?: LoginConfig;
}

/** 宿主侧看到的插件运行态（model.go:318）。 */
export interface PluginStatus {
  manifest: PluginManifest;
  dir: string;
  enabled: boolean;
  running: boolean;
  healthy: boolean;
  failed: boolean;
  port?: number;
  pid?: number;
  started_at?: number;
  installed_at?: number;
  last_error?: string;
}

// ---------------------------------------------------------------------------
// GET /meta（宿主 internal/plugins/client.go:85 的 Meta）
// ---------------------------------------------------------------------------

export interface ProviderSummary {
  id: string;
  name: string;
  models: number;
}

export interface MetaResponse {
  id: string;
  name?: string;
  version: string;
  api_level: number;
  capabilities?: Capability[];
  account_count: number;
  uptime_s: number;
  started_at?: number;
  // —— 桥扩展：宿主忽略，供排障与验收 ——
  /** 桥自身标识（插件 id 可能与宿主注入的不同）。 */
  bridge?: string;
  providers?: ProviderSummary[];
  plugin_dir?: string;
  warnings?: string[];
}

// ---------------------------------------------------------------------------
// GET /health（无鉴权，宿主 supervisor 每 10s 探一次，只看 2xx）
// ---------------------------------------------------------------------------

export interface HealthResponse {
  status: "ok" | "degraded";
  id: string;
  version: string;
  uptime_s: number;
  plugins_loaded: number;
  providers: number;
  models: number;
  warnings: string[];
}

// ---------------------------------------------------------------------------
// 错误体（OpenAI 风格；宿主 plugins/client.go:360 errorMessage 读 error.message）
// ---------------------------------------------------------------------------

export interface ErrorDetail {
  message: string;
  type?: string;
  code?: string | null;
  param?: string | null;
}

export interface ErrorShape {
  error: ErrorDetail;
}

export function errorBody(
  message: string,
  type: string = "invalid_request_error",
  code: string | null = null,
): ErrorShape {
  const detail: ErrorDetail = { message, type };
  if (code !== null) detail.code = code;
  return { error: detail };
}

// ---------------------------------------------------------------------------
// 聊天泳道（chat/ 目录使用；本卡只定契约，不实现）
// ---------------------------------------------------------------------------

export interface ChatContentPart {
  type: string;
  text?: string;
  image_url?: { url: string; detail?: string } | string;
  [key: string]: unknown;
}

export interface ChatToolCall {
  id?: string;
  type?: string;
  function?: { name: string; arguments: string };
}

export interface ChatMessage {
  role: "system" | "user" | "assistant" | "tool" | string;
  content: string | ChatContentPart[] | null;
  name?: string;
  tool_call_id?: string;
  tool_calls?: ChatToolCall[];
  [key: string]: unknown;
}

export interface ChatRequest {
  model: string;
  messages: ChatMessage[];
  stream?: boolean;
  temperature?: number;
  top_p?: number;
  max_tokens?: number;
  tools?: JsonValue[];
  tool_choice?: JsonValue;
  reasoning_effort?: string;
  [key: string]: unknown;
}

export interface ChatUsage {
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  [key: string]: unknown;
}

export interface ChatDelta {
  role?: string;
  content?: string | null;
  reasoning_content?: string;
  tool_calls?: JsonValue;
}

/** SSE 分片（`data: {…}` 的单帧载荷）。 */
export interface ChatChunk {
  id: string;
  object: "chat.completion.chunk";
  created: number;
  model: string;
  choices: Array<{
    index: number;
    delta: ChatDelta;
    finish_reason?: string | null;
  }>;
  usage?: ChatUsage;
}

/** 非流式聚合结果。model_requested / model_served 分离（D-26）。 */
export interface ChatCompletion {
  id: string;
  object: "chat.completion";
  created: number;
  model: string;
  model_requested?: string;
  model_served?: string;
  choices: Array<{
    index: number;
    message: ChatMessage;
    finish_reason?: string | null;
  }>;
  usage?: ChatUsage;
}

// ---------------------------------------------------------------------------
// 凭据（admin 泳道使用；本卡只提供落盘与计数）
// ---------------------------------------------------------------------------

/**
 * 一个 provider 的凭据记录。字段口径来自 magpie 的 plugin-auth.json：
 * `{access, refresh, expires, type}`。宿主永不接触明文指纹（D-57）。
 *
 * 安全：本对象**禁止**进入日志、错误体、/meta、/health、/v1/models。
 */
export interface CredentialRecord {
  provider: string;
  type?: string;
  access?: string;
  refresh?: string;
  /** Unix 毫秒。 */
  expires?: number;
  label?: string;
  updated_at: number;
  last_used_at?: number;
  revoked?: boolean;
}

export interface CredentialStore {
  count(): number;
  list(): CredentialRecord[];
  get(provider: string): CredentialRecord | undefined;
  /** 由插件 client.auth.set 调用；只接受已知字段，其余丢弃。 */
  save(provider: string, body: JsonValue): Promise<CredentialRecord>;
}

// ---------------------------------------------------------------------------
// OpenCode / magpie 插件运行时形状（F-01：只依赖 node: 内置模块）
// ---------------------------------------------------------------------------

/** 插件给出的单条模型配置（site.models 的条目，已并入 MODEL 默认值）。 */
export interface PluginModelEntry {
  id?: string;
  name?: string;
  limit?: { context?: number; output?: number };
  attachment?: boolean;
  tool_call?: boolean;
  reasoning?: boolean;
  temperature?: boolean;
  modalities?: { input?: string[]; output?: string[] };
  variants?: JsonValue;
  api?: JsonValue;
  [key: string]: unknown;
}

export type PluginModelMap = Record<string, PluginModelEntry>;

/**
 * provider.models 有两种形态：静态表，或 `(provider, {auth}) => map` 函数。
 * 无凭据时函数一律回落到静态表（trae 包 L3606-3615 即此语义）。
 */
export type PluginModelsHook = (
  provider: unknown,
  options: { auth?: unknown },
) => Promise<PluginModelMap> | PluginModelMap;

export interface OpenCodeProvider {
  id: string;
  models?: PluginModelsHook | PluginModelMap;
}

export interface PluginLoaderHandle {
  baseURL: string;
  apiKey?: string;
  fetch: (input: unknown, init?: unknown) => Promise<unknown>;
}

export interface LoginMethod {
  type: string;
  label: string;
  authorize?: (input?: unknown) => Promise<unknown> | unknown;
}

/** 插件的 auth 钩子：签到、用量、续期、登录方式。 */
export interface OpenCodeAuth {
  provider?: string;
  usage?: (options: unknown) => Promise<unknown>;
  checkin?: (options: unknown) => Promise<unknown>;
  refreshLead?: number;
  refresh?: (auth: unknown) => Promise<Partial<CredentialRecord> | undefined>;
  loader?: (getAuth: () => Promise<unknown>) => Promise<PluginLoaderHandle>;
  methods?: LoginMethod[];
  [key: string]: unknown;
}

/** config 钩子的入参形状（插件往 config.provider[<id>] 写自身配置）。 */
export interface ProviderConfigEntry {
  name?: string;
  npm?: string;
  api?: string;
  models?: PluginModelMap;
  [key: string]: unknown;
}

export interface MutableProviderConfig {
  provider?: Record<string, ProviderConfigEntry>;
  [key: string]: unknown;
}

/** 插件实例：magpie 的 `makePlugin(site)({client})` 产物。 */
export interface OpenCodePlugin {
  config?: (config: MutableProviderConfig) => Promise<void> | void;
  "chat.headers"?: (
    input: unknown,
    output: { headers: Record<string, string> },
  ) => Promise<void> | void;
  provider: OpenCodeProvider;
  auth?: OpenCodeAuth;
  [key: string]: unknown;
}

/** 插件工厂收到的宿主 client。trae 包只用到 client.auth.set（L3132）。 */
export interface HostClient {
  auth?: {
    set?: (payload: { path?: { id?: string }; body?: JsonValue }) => Promise<void>;
  };
}

// ---------------------------------------------------------------------------
// 加载结果与桥运行时上下文
// ---------------------------------------------------------------------------

export interface LoadedPlugin {
  /** 模块文件绝对路径。 */
  file: string;
  /** 模块导出名，如 TraeCNAuthPlugin。 */
  exportName: string;
  providerId: string;
  providerName: string;
  plugin: OpenCodePlugin;
}

export interface ProviderRecord {
  id: string;
  name: string;
  /** provider 级 chat 基址（config 钩子写的 api，如 workbuddy 的 <endpoint>/v2）。
   * 模型条目没有自带 api.url 时，chat 泳道用它兜底。 */
  api?: string;
  /** 静态模型表：插件内模型 id → 条目（未加前缀）。 */
  models: PluginModelMap;
  plugin: LoadedPlugin;
}

export interface LoadResult {
  /** 解析后的插件目录（绝对路径）。 */
  dir: string;
  providers: Record<string, ProviderRecord>;
  plugins: LoadedPlugin[];
  /** 加载期非致命问题：单个导出失败、provider 重名等。 */
  warnings: string[];
}

export interface BridgeConfig {
  /** 只从 CLI 的 --plugin-dir 来（宿主写在 manifest.entry.args），不读环境变量。 */
  pluginDir: string;
  port: number;
  token: string;
  dataDir: string;
  /**
   * 宿主注入的插件 id（env AIWORKGATE_PLUGIN_ID）。缺省用 BRIDGE_ID。
   * /meta 与 /health 的 id 字段用它，保证与宿主 manifest 里的插件 id 一致。
   */
  pluginId: string;
  /**
   * 宿主注入的规则/出口文件路径（env AIWORKGATE_RULES_FILE / AIWORKGATE_EGRESS_FILE）。
   * 本卡只记录，由 chat / egress 泳道消费。
   */
  rulesFile?: string;
  egressFile?: string;
  /** 宿主声明的契约级别（env AIWORKGATE_API_LEVEL）。 */
  hostApiLevel?: number;
}

/**
 * 宿主注入的环境变量名（supervisor.go / env.go）。
 *
 * **取值优先级：环境变量优先，CLI 参数只在环境变量缺失时生效。**
 * 原因：supervisor 每次重启都会重新 allocatePort（supervisor.go:147），而
 * manifest.entry.args 安装时冻结、不随重启更新；若 CLI 覆盖 env，重启后桥会监听
 * 旧端口，宿主永远连不上。唯一例外是 plugin-dir——它只走 CLI（见 BridgeConfig）。
 */
export const ENV_KEYS = {
  pluginId: "AIWORKGATE_PLUGIN_ID",
  port: "AIWORKGATE_PLUGIN_PORT",
  token: "AIWORKGATE_PLUGIN_TOKEN",
  dataDir: "AIWORKGATE_PLUGIN_DATA_DIR",
  rulesFile: "AIWORKGATE_RULES_FILE",
  egressFile: "AIWORKGATE_EGRESS_FILE",
  apiLevel: "AIWORKGATE_API_LEVEL",
} as const;

export interface BridgeContext {
  config: BridgeConfig;
  startedAt: number;
  registry: LoadResult;
  credentials: CredentialStore;
}
