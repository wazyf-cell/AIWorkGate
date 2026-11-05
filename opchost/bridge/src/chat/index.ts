// chat 泳道公共出口（bridge-core 与 dev-server 从这里 import）。
//
// 接入注意：本文件里的类型在 bridge-core 产出 src/types.ts 后应改为
// re-export 它（见 types.ts 头部注释），避免两套契约漂移。

export {
  createChatHandler,
  sendError,
  sendJson,
} from "./handler.ts";
export type { ChatHandlerOptions } from "./handler.ts";

export {
  fileAuthStore,
  memoryAuthStore,
} from "./credentials.ts";
export type { AuthStore } from "./credentials.ts";

export {
  ChatError,
  badRequest,
  needsLoginError,
  networkError,
  notFound,
  timeoutError,
  toErrorBody,
  upstreamError,
} from "./errors.ts";
export type { ChatErrorInit } from "./errors.ts";

export {
  parseChatRequestBody,
} from "./parse.ts";
export {
  resolveModelRoute,
} from "./route.ts";
export {
  parseSSEStream,
} from "./sse.ts";
export {
  aggregateChunks,
  normalizeCompletion,
} from "./aggregate.ts";
export {
  buildUpstreamRequest,
  callUpstream,
  joinURL,
  resolveLoaderOptions,
} from "./upstream.ts";
export {
  SSEWriter,
  pipeUpstreamStream,
} from "./stream-pipe.ts";

export type {
  AuthValue,
  ChatConfig,
  ChatDeps,
  ChatMessage,
  ChatRequest,
  LoaderOptions,
  LogLevel,
  OpenAIErrorBody,
  OpenAIChatCompletion,
  PluginConfig,
  PluginHooks,
  ProviderBinding,
  ResolvedRoute,
  UpstreamFetch,
} from "./types.ts";
