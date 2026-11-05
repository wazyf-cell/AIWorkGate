# 桥 chat 泳道（P1-2）— /v1/chat/completions

OpenAI 兼容聊天端点的实现与自检。只依赖 Node 内置模块，可被 `bun`/`node` 直接运行。

## 职责

| 输入 | 输出 |
| --- | --- |
| `POST /v1/chat/completions`（OpenAI chat 请求体） | 非流式：标准 OpenAI `chat.completion` JSON；流式：`text/event-stream`，`data: {chunk}\n\n` … `data: [DONE]\n\n` |

- 按 model 前缀（`trae-cn/xxx`）路由到对应插件 provider；无前缀且唯一 provider 时直接收；多 provider 无前缀判歧义（404 可读提示）。
- 聊天通道 = 插件的 `hooks.auth.loader(getAuth)` → `{ baseURL, apiKey, headers, fetch }`：
  loader 给了 fetch 就用它的 fetch（trae 的协议转换在 fetch 内部）；`baseURL` 缺失 = 无凭据 = **401「需登录」**（绝不 500）。
- 上游 4xx 保留原状态、5xx 收成 502 但文案原样带出；超时 504；网络失败 502；流内错误分片 `data: {"error":...}` 后立即 `[DONE]`。
- 上游忽略 `stream` 标志时双向兜底：流式请求收到 JSON → 转单帧流；非流式请求收到 SSE → 聚合回 JSON。

## 文件

| 文件 | 作用 |
| --- | --- |
| `types.ts` | 本地共享类型（**待并入 `../types.ts`**，bridge-core 产出后改 re-export） |
| `errors.ts` | `ChatError` 统一错误模型 + OpenAI 错误体/错误帧 |
| `parse.ts` | 请求体校验（model/messages/stream/temperature/max_tokens） |
| `route.ts` | model 前缀路由 |
| `upstream.ts` | loader 解析（凭据探测）→ 请求构造 → fetch → 错误映射 |
| `stream-pipe.ts` | SSE 回写：惰性头、保序、错误帧、[DONE] |
| `aggregate.ts` | 非流式归一 + SSE→JSON 聚合 |
| `handler.ts` | `createChatHandler(deps)` 编排（挂载点） |
| `credentials.ts` | 最小凭据文件 store（管理面落地前自用；admin 泳道落地后由 deps 提供） |
| `dev-server.ts` | 自检服务器（测试基建，非生产入口） |
| `mock-plugin/index.mjs` | 最小 mock 插件（形状同 trae） |
| `verify.ts` | 自检断言脚本 |

## 挂载（bridge-core）

```ts
import { createChatHandler, fileAuthStore } from "./chat/index.ts";
const chat = createChatHandler({ deps: { listProviderIds, getProvider, getAuth, saveAuth, log, config } });
// 在 server.ts 里：POST /v1/chat/completions → chat(req, res)
```

`deps.getAuth` 应由 admin 泳道的凭据库实现（`fileAuthStore` 只是自用回落）。

## 自检

```powershell
# mock：非流式 + 流式 + 错误 + 无凭据
node opchost/bridge/src/chat/dev-server.ts --plugin ./opchost/bridge/src/chat/mock-plugin/index.mjs --auth-provider mock --port 18080 --data-dir "$env:TEMP\aigate-chat-dev"
node opchost/bridge/src/chat/verify.ts --base http://127.0.0.1:18080 --suite mock

# 真实 trae 包（无凭据 → 需登录）
node opchost/bridge/src/chat/dev-server.ts --plugin "$env:USERPROFILE\.config\magpie\plugins\node_modules\@magpie-community\opencode-trae-auth\index.mjs" --port 18081 --data-dir "$env:TEMP\aigate-chat-trae-empty"
node opchost/bridge/src/chat/verify.ts --base http://127.0.0.1:18081 --suite trae
```

## 契约出处

- 插件聊天形态：`~/.config/magpie/plugins/node_modules/@magpie-community/opencode-trae-auth/index.mjs`
  （`makePlugin` L3031；`config` L3581；`auth.loader` L3641；无凭据返回 `{}` L3642）
- 宿主侧：`docs/插件格式规范.md` §6（`POST /v1/chat/completions` 流式透传、非 2xx 即失败、优先 `error.message`）
- 错误口径：`internal/plugins/client.go` `errorMessage`（error.message → 正文前 256 字节 → 状态文本）
