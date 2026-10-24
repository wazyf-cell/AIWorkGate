# 外部 Agent 接入 AIWorkGate

这份文档只讲一件事：把 AIWorkGate 当成一个**本地 OpenAI 兼容端点**，给你的编码 agent 用。
命令都是在本机实测过的；`agk_xxx` 请换成你自己的 Key。

---

## 1. 基本信息

| 项 | 值 |
|---|---|
| Base URL | `http://127.0.0.1:7865/v1`（端口见「设置」，默认 7865） |
| 协议 | 仅 HTTP（无 TLS）。要跨机访问必须自己套反向代理 |
| 认证 | `Authorization: Bearer <Key>`，也接受 `x-api-key: <Key>`、`api-key: <Key>` |
| Key 形态 | `agk_` + 32 位；库内只存 SHA-256，明文只在创建时显示一次 |
| 模型清单 | `GET /v1/models` |
| 存活探针 | `GET /health`（根路径，不在 `/v1` 下，不校验 Key） |
| 跨源 | 默认**不发** CORS 头；浏览器里的网页要用，必须在「设置 → 跨源(CORS)」里加白名单 |

## 2. 创建 Key

界面：设置页「客户端 Key」→ 新建 → 弹出的明文只出现一次。

管理接口（`/api/*`）现在只接受**同源浏览器**请求，并要求宿主下发的会话 Cookie；`curl`/PowerShell 直接 `POST /api/keys` 会拿到 403 `admin_session_required`。这不是 bug，是防「你打开的任意网页可以替你在本机造 Key、装插件」的收口。要脚本化建 Key，请在浏览器控制台里用同源 `fetch` 调用，或直接用界面。

可用的管理调用（浏览器同源，自动带 Cookie）：

- `GET /api/keys` 列表（不含明文；带 `model_scope`、`bind_pool`、`daily_quota`、`used_today`、`quota_left`）
- `POST /api/keys` `{ "name": "codex" }` → 201，明文只在这次响应里
- `PUT /api/keys/{id}` 改 `name` / `enabled` / `model_scope` / `bind_pool` / `daily_quota`（缺省字段保持原值）
- `DELETE /api/keys/{id}`

### 2.1 作用域与每日配额

一把 Key 还可以带三个可选字段（界面上是新建/编辑对话框里的同名项）：

| 字段 | 含义 |
|---|---|
| `model_scope` | 允许的模型数组；每项是精确模型名或 `插件/*` 前缀，空数组 = 不限 |
| `bind_pool` | 绑定一个插件：裸模型名会改写成 `插件/模型` 再路由；带其他插件前缀直接 404 |
| `daily_quota` | 每自然日（宿主本地时区）的推理请求上限，0 = 不限 |

- 模型 id 比较会去掉首尾空白并忽略大小写（与 `/v1/models` 的合并去重同一规则），所以 `officeace/GLM-5.2` 与 `officeace/glm-5.2` 视为同一个；
- 配额在请求进入网关时就原子预占一个名额（SQLite 一条带条件的 UPSERT），并发突发不会超卖；因模型范围/绑定插件被拒、没到达模型的请求会把名额退回，不消耗配额；已通过准入的请求即使上游失败、或客户端中途断开（记录为 499）也算一次（请求数以准入为准）；
- 用尽返回 **429** `key_quota_exceeded`，`Retry-After` 是到次日 00:00 的秒数；超出范围的模型返回 **404** `model_not_found`。每日请求数之外没有其他配额或预留档位；

## 3. 模型名怎么写（三规则）

1. `插件id/模型id` —— 只在该插件的资源池里挑账号。例：`officeace/glm-5.2`、`codearts/GLM-5.2`。前缀大小写敏感。
2. **逻辑名** —— 在「调度 → 模型绑定」里把一个名字绑到多个 `插件/模型`，网关按顺序跨池挑，前一个失败自动换下一个。例：`kimi → officeace/kimi-k2.6, codearts/kimi-k2.6`。
3. **裸模型名** —— 交给 AI Work 助手（A）。A 没运行或没配 A Key 时会返回 502/401，日志页能看出原因。

`GET /v1/models` 里 `owned_by` 为 `plugin:<id>` 的是插件模型，`scheduler` 的是逻辑名。

## 4. 支持的端点

| 端点 | 插件模型 | A（裸模型名） |
|---|---|---|
| `GET /health`（不在 `/v1` 下） | ✅ 宿主存活探针，无需 Key | ✅ 同一实现 |
| `GET /healthz`（不在 `/v1` 下） | ✅ 可调度账号探针，无需 Key | ✅ 同一实现 |
| `GET /v1/models` | ✅ 合并插件 + 逻辑名 + A | ✅ |
| `POST /v1/chat/completions` | ✅（含 SSE 流式） | ✅ 原样透传 |
| `POST /v1/completions` | ✅ 内部转成 chat 再回译 | ✅ 原样透传 |
| `POST /v1/responses` | ✅ 内部转成 chat 再回译（含 SSE） | ✅ 原样透传 |
| `POST /v1/messages` | ✅ 内部转成 chat 再回译（Anthropic 协议，含 SSE） | ✅ 原样透传 |
| `/v1/embeddings`、`/v1/images*` | ❌ 501 `unsupported_endpoint` | ✅ 原样透传 |

`GET /health` 返回 `{"status":"ok","running":true}`（HTTP 200，`Cache-Control: no-store`，只表示宿主在监听），不需要客户端 Key，也不受局域网网页密码限制，不检查插件、账号或 A 的状态。`HEAD /health` 同样返回 200（无 body）；其他方法返回 405 并带 `Allow: GET, HEAD`。

`GET /healthz` 是「有没有号可调度」的探针：同样免 Key、在管理鉴权之前处理，`HEAD` 可用，响应带 `Cache-Control: no-store`，不含账号 ID 或凭据。至少一个账号可调度时返回 200 `{"status":"ok","selectable":N,"total":M}`；一个都没有时返回 503 `{"status":"unavailable","reason":"no_account"}`；启动宽限期或调度器尚未就绪时返回 200 `{"status":"ok","reason":"scheduler_unavailable"}`。看门狗可据此区分「进程活着」与「进程活着但没号」。

模型条目除 `id/owned_by` 外还会带目录元数据（上游有才带）：`vendor`、`multiplier`（倍率，未公布则不出现）、`thinking`（支持的思考档位，如 `["off","high"]`）、`free`、`context_length`、`vision`、`tools`；被人工覆盖的字段列在 `overridden` 里。模型 id 在合并时按「去首尾空白 + 忽略大小写」去重，输出保留第一个上报者的拼写。

Anthropic 客户端（Claude Code 等）把 `ANTHROPIC_BASE_URL` 指到本网关即可用插件模型；`/v1/messages` 的 `stop_reason` 映射为 `end_turn / max_tokens / tool_use / refusal`，SSE 序列是 `message_start → content_block_start → …delta → content_block_stop → message_delta → message_stop`，出错时只发 `event: error`，不会伪造成功。`/v1/messages` 的 `usage` 现在带 `cache_read_input_tokens` 与 `cache_creation_input_tokens`（上游没报时为 0），`/v1/responses` 的 `usage` 在插件上报时带 `input_tokens_details.cached_tokens` 与 `output_tokens_details.reasoning_tokens`。

图片输入：`/v1/messages` 的 `image` 块（`base64` 转成 `data:` URL，`url` 原样）和 `/v1/responses` 的 `input_image` 都会转成 chat 的 `image_url` 内容部分转发，是否可用取决于上游模型的视觉能力；识别不了的图片形态保留 `[image_url]` 占位文本。相邻同角色的纯文本消息会合并成一条（带 `tool_calls` 或 tool 结果的消息不合并），避免严格上游因连续同角色返回 400。

### 4.1 自定义模型

在「设置 → 自定义模型」里登记的条目是第四个模型来源，优先于插件与 A：

- 模型 id 就是登记时填的「模型名称」：最多 64 个字符、不含 `/`，匹配时去首尾空白并忽略大小写。直接把它填进客户端的 `model` 字段。
- 支持 `POST /v1/chat/completions`、`POST /v1/messages`、`POST /v1/responses`、`POST /v1/completions`（均含流式与非流式）。`chat/completions` 直达 `<base>/chat/completions`（base 以 `/v1` 结尾时不再补 `/v1`）；其余三个走与插件模型相同的转译层（Anthropic / Responses / completions 转 chat 再回译），上游仍是 chat 端点。都用宿主保存的 Key 认证，客户端发给 AIWorkGate 的 Key 不会转发给上游。
- `/v1/embeddings`、`/v1/images*` 不经过自定义模型，仍按普通路由处理（自定义条目对这些路径没有特殊支持）。
- `GET /v1/models` 中这些条目 `owned_by` 与 `vendor` 都是 `custom`；与插件模型或逻辑名同名时对方优先，该条目不出现在列表里、也不参与路由。
- 响应头与请求日志的来源是 `custom:<条目 id>`；Key 的模型范围照常生效；绑定了插件的 Key 不会命中自定义模型。

管理接口（与其余 `/api/*` 一样只允许同源会话、需要宿主下发的 Cookie）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/custom-models` | 列出条目，含 `key_set`；Key 本身从不返回 |
| POST | `/api/custom-models` | 新建：`name`、`base_url`、`upstream_model`、`api_key`、`enabled?`，成功 201 |
| PUT | `/api/custom-models/{id}` | 局部更新；不传 `api_key` 保留原 Key，传空串清除 |
| DELETE | `/api/custom-models/{id}` | 删除条目并清除保存的 Key |
| POST | `/api/custom-models/{id}/test` | 显式探测：`max_tokens=1` 的 ping，5 秒超时；失败 502，`error` 已抹掉 Key |

错误码：400 `invalid_request`（字段不合法）、409 `conflict`（名称重复）、404 `not_found`（条目不存在）、502（测试失败或上游失败）。

### 4.2 请求日志查询与导出

两个端点与其余 `/api/*` 一样只接受同源浏览器请求并需要宿主会话 Cookie：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/logs/requests` | 按筛选条件分页返回请求日志（`logs`、`total`） |
| GET | `/api/logs/requests/export` | 同一套筛选参数，返回 CSV 附件 |

参数：`from`、`to`（时间范围）、`q`（关键字）、`model`（大小写不敏感的子串）、`source`（或别名 `plugin_id`）、`account_id`（或 `account`）、`key_id`（或 `key`）、`status_class`（`2xx|4xx|5xx|error`，`error` 表示状态 0 或 ≥400）、`stream`、`limit`、`offset`。`status_class` 或 `stream` 取值非法时返回 400 `invalid_query`。

导出为带 UTF-8 BOM 的 CSV，表头「时间,请求ID,来源,模型,账号,Key,流式,状态,延迟ms,首字ms,输入Token,输出Token,缓存Token」，文件名 `requests-YYYYMMDD.csv`；单次最多 50,000 行，被截断时带 `X-Export-Truncated: true`。导出不包含请求/响应正文，也不包含任何凭据。

## 5. 可复制示例

列模型：

```bash
curl -s http://127.0.0.1:7865/v1/models -H "Authorization: Bearer agk_xxx" | head -c 400
```

非流式对话：

```bash
curl -s http://127.0.0.1:7865/v1/chat/completions \
  -H "Authorization: Bearer agk_xxx" -H 'Content-Type: application/json' \
  -d '{"model":"officeace/glm-5.2","messages":[{"role":"user","content":"一句话解释 goroutine"}]}'
```

流式：

```bash
curl -N http://127.0.0.1:7865/v1/chat/completions \
  -H "Authorization: Bearer agk_xxx" -H 'Content-Type: application/json' \
  -d '{"model":"officeace/glm-5.2","stream":true,"messages":[{"role":"user","content":"数到 3"}]}'
```

legacy completions（`prompt` 只支持单条字符串）：

```bash
curl -s http://127.0.0.1:7865/v1/completions \
  -H "x-api-key: agk_xxx" -H 'Content-Type: application/json' \
  -d '{"model":"officeace/glm-5.2","prompt":"把这句话翻成英文：今天很冷","max_tokens":64}'
```

Responses API（Codex 类客户端）：

```bash
curl -N http://127.0.0.1:7865/v1/responses \
  -H "Authorization: Bearer agk_xxx" -H 'Content-Type: application/json' \
  -d '{"model":"officeace/glm-5.2","stream":true,"instructions":"只回答一句话",
       "input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]}]}'
```

Python（OpenAI SDK）：

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:7865/v1", api_key="agk_xxx")
r = client.chat.completions.create(
    model="officeace/glm-5.2",
    messages=[{"role": "user", "content": "say hi"}],
)
print(r.choices[0].message.content)
```

## 6. 流式与错误

### 6.1 事件序列

- `/v1/chat/completions`：上游原样透传，最后一帧 `data: [DONE]`。
- `/v1/completions`：`data: {"object":"text_completion",...}`，`choices[0].text` 是增量；最后一帧带 `finish_reason`；再跟 `data: [DONE]`。
- `/v1/responses`：事件序列（每个都带自增 `sequence_number`）
  `response.created` → `response.in_progress` → `response.output_item.added` → `response.content_part.added` → 若干 `response.output_text.delta` → `response.output_text.done` → `response.content_part.done` → `response.output_item.done` → `response.completed` → `data: [DONE]`。
  有工具调用时追加 `function_call` 条目（`response.output_item.added` / `response.function_call_arguments.delta` / `.done` / `.output_item.done`）。
  上游的 `reasoning_content` 以 `type:"reasoning"` + `summary[].summary_text` 承载（不是官方逐字 reasoning 流，只有全文）。

### 6.2 保活

流式响应（`Content-Type: text/event-stream`）如果连续 **15 秒**没有可写的数据，网关会插入一帧 SSE 注释：

```
: keep-alive

```

它只在上一帧刚好结束于行边界时写入，不会切开事件；间隔固定 15 秒、不可配置，流结束即停止。SSE 客户端按规范忽略注释行，不需要处理。作用是防止反向代理/中间设备把长时间安静的连接回收。

### 6.3 上游中途失败

上游在已经开流之后报错时，网关按各自协议收尾，**不会伪造一个成功结果**：

- `/v1/messages`：先关掉当前打开的 `content_block`，再发一帧 `event: error`，`data` 是 Anthropic 错误信封（`{"type":"error","error":{"type":...,"message":...}}`），之后不再有 `message_delta` / `message_stop`；
- `/v1/responses`：发 `response.failed`（`response.status:"failed"`，`error.code` 为 `upstream_error`），不会发 `response.completed`；
- `/v1/completions`：发一帧 `data: {"error":{...}}`，之后不再发 `[DONE]`；
- `/v1/chat/completions`：上游字节原样透传，看到什么就是什么。

另外，转译端点不会把「什么都没有」当成成功：流式结束时既没有文本、也没有思考内容和工具调用，按失败处理（Responses / completions 的 `error.code` 是 `empty_response`，Messages 发 `event: error`）；非流式上游返回空 body 时报 `empty_response`。

### 6.4 截断与用量明细

- `/v1/responses`：上游 `finish_reason=length` 时，`status` 是 `"incomplete"`，并带 `incomplete_details:{"reason":"max_output_tokens"}`；流式对应的事件是 `response.incomplete`（不是 `response.completed`）。
- `/v1/responses` 的 `usage`：保留 `input_tokens / output_tokens / total_tokens`；插件上报缓存与推理明细时再加 `input_tokens_details.cached_tokens`、`output_tokens_details.reasoning_tokens`。
- `/v1/messages` 的 `usage`：`input_tokens / output_tokens` 之外总是带 `cache_read_input_tokens`、`cache_creation_input_tokens`（上游没报则为 0）。
- `/v1/chat/completions`、`/v1/completions` 的 `usage` 是上游原样字段。

## 7. 响应头

| 头 | 含义 |
|---|---|
| `X-AIWorkGate-Source` | `a` / `plugin:<id>` / `scheduler` |
| `X-Aiworkgate-Account` | 实际使用的账号 id（池调度时） |
| `X-Aiworkgate-Credits` | 插件回传的本次积分消耗，没有就不发 |
| `X-Aiworkgate-Downgraded` | 发生降档时返回 |
| `X-Aiworkgate-A-Stale` | A 模型列表来自缓存 |

## 8. 错误码

| HTTP | code | 含义 / 处理 |
|---|---|---|
| 401 | `invalid_api_key` | 没带 Key 或 Key 无效 |
| 404 | `model_not_found` | 模型名不存在、未在目录里「接入」，或不在该 Key 的模型范围内 |
| 400 | `invalid_json` / `unsupported_request` | body 非 JSON，或用了不支持的参数 |
| 405 | `cors_not_allowed` | 浏览器跨源但没配 CORS 白名单 |
| 409 | `account_unavailable` | 指定账号不可用（插件侧） |
| 429 | `concurrency_limit` | 插件并发满、排队超时 |
| 429 | `key_quota_exceeded` | 这把 Key 的每日配额用尽（`Retry-After` 到次日 00:00） |
| 503 | `no_available_account` / `plugin_unavailable` | 没有可用账号/插件起不来（带 `Retry-After`） |
| 502 | `upstream_unavailable` | 插件不可达，或 A 未运行 |
| 501 | `unsupported_endpoint` | 该端点只支持 A |

### 8.1 400 的重试分类（客户端可见行为）

上游 400 不一定都原样返回。网关只对三类正文做特殊处理，其余 400 一律直接返回、不换号：

| 上游 400 正文 | 网关行为 | 客户端最终看到 |
|---|---|---|
| 同时含 `thinking` 和 `signature`（签名块失效） | 同一账号等 200ms 重试一次 | 重试成功则正常结果；仍失败则原样返回这次 400 |
| 含 `"code": 11128`、`illegal api invocation` 或 `unapproved channel` | 同一账号等 200ms 重试一次（网关没有指纹清洗，只做这一次重试） | 同上 |
| 含 `insufficient credit`、`insufficient balance`、`credit balance`、`quota exceeded`、`余额不足`、`额度不足`、`积分不足` | 换下一个账号重试（该请求本身没问题，是账号没钱了） | 换号成功则正常结果；所有账号都失败时返回最后一次的 400 |
| 其他 400 | 不重试、不换号 | 原样返回 |

中间的重试不会作为额外响应暴露给客户端（网关先把错误缓冲下来再决定）；「日志 → 调度」里的 `retry decision` 记录每次的 `action`（`retry_same` / `switch_account` / `stop`）、等待毫秒和上游 `Retry-After`。同理，401/403 会直接换号，429 按 `Retry-After` 或 1s/2s/3s 等待重试三次后换号，503/529 按 10s/20s/40s 等待，502 只等 500ms 重试一次。

## 9. 已知限制

- 只有 `/v1/chat/completions` 的**完整**语义；`/v1/completions` 与 `/v1/responses` 是转译层，不支持这些字段：`echo`、`suffix`、`logit_bias`、`best_of`、`n>1`（completions）；`background`（responses）。`previous_response_id` 会被接受但忽略——网关无状态，请每轮把完整上下文放进 `input`（Codex 类客户端本来就是这样发的）；只给指针不给 `input` 时返回 400。多工具并行返回只取第一个 choice。
- Key 作用域只有「模型范围」和「绑定一个插件」两档，没有按上游账号、分组或标签的白名单。
- 模型目录来自插件上报、A 与自定义模型（可加人工覆盖与逻辑名绑定）；自定义模型是用户显式配置的上游，不是“手写一个上游并不存在的模型”。每日请求数是唯一的配额，没有按池/按模型的配额或预留。
- 请求级账号/模型冷却（429、5xx）会写入 SQLite 的 `scheduler_cooldowns`，宿主重启后恢复，429 的翻倍计数也一并恢复；这只影响调度选择，客户端看到的仍是原有的状态码与响应格式。签到维度的账号健康态（`account_meta` 的 `status_kind/cooldown_until`）另有持久化。
- 管理接口（`/api/*`）只允许同源浏览器访问，且需要宿主发的会话 Cookie；它**不是**认证，本机进程仍可访问。
- 无 TLS、无请求速率限制；跨机暴露前请自己加反代 + LAN 口令。

## 10. 排查顺序

1. `GET /v1/models` 有没有你要的模型 → 没有就去「资源」页点「立即同步」并勾上「接入」。
2. `X-AIWorkGate-Source` 是 `plugin:*` 还是 `a` → 判断路由是否如你预期。
3. 「日志 → 调度」看 `dispatch decision`（选了哪个账号、为什么淘汰别的）。
4. 「日志 → 请求」看状态码与错误正文；打开 Debug 正文可看请求/响应片段（与请求日志同样保留 30 天）。
