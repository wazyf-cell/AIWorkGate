# magpie-bridge · aiworkgate 插件宿主桥（P1）

把 magpie 生态的 OpenCode 插件（npm 包 `@magpie-community/opencode-*-auth`，Node ESM，
`.mjs` 自包含、只依赖 `node:` 内置模块，见计划 F-01）加载进来，**以 aiworkgate 插件契约
的身份**对宿主暴露全部 P1 端点：`/meta`、`/health`、`/v1/models`、`/v1/chat/completions`
（SSE + 非流式）、`/accounts`、`/checkin`、`/admin/*`。

本目录是 P1 的三张卡的产出合集：桥核心（task-1）+ chat 泳道（task-2）+ 管理面（task-3）。
**P1-5 生产挂载**（task-6）把三条泳道全部接到 `src/main.ts` 这一个真实入口上——
以前 `src/server.ts` 只有三个只读端点、chat 只在 `chat/dev-server.ts` 里验过，
现在起一个桥进程就能对外提供全部端点。

---

## 1. 目录

```
opchost/bridge/
├── package.json          # type=module，无第三方依赖
├── tsconfig.json         # 仅编辑器/可选 tsc 用；运行靠 Node 24 类型擦除
├── README.md
└── src/
    ├── main.ts           # 入口：参数/环境变量解析、凭据库、bootstrap、优雅退出
    ├── server.ts         # node:http 服务、Bearer 鉴权闸门、全端点分发 + 生产挂载
    ├── loader.ts         # 插件目录扫描、动态 import、provider 注册表、凭据存储适配
    ├── types.ts          # **全桥共享类型契约**（chat/admin 两条泳道都 import 它）
    ├── meta.ts           # GET /meta（含能力位推导）
    ├── health.ts         # GET /health
    ├── models.ts         # GET /v1/models
    ├── chat/             # chat 泳道（task-2）：handler/route/sse/upstream/aggregate
    │   ├── dev-server.ts # 仅供 chat 自检的独立服务器（非生产入口）
    │   └── mock-plugin/  # 形状同 trae 的最小 mock 插件
    └── admin/            # 管理面泳道（task-3/task-7）：router/signin/credentials/
                          #   manifest/quotagroups/accounts/ledger/switches + wiring 接线
```

运行时只用 Node 内置模块：`node:http` / `node:fs` / `node:fs/promises` / `node:path` /
`node:crypto` / `node:url` / `node:os`。**不装任何第三方依赖**（含 `@types/node`）。

---

## 2. 启动

三种方式等价（同一份 TS 源码）：

```pwsh
# a) 系统 Node（v24.15.0 实测，直接跑 .ts，无需编译）
node src/main.ts --plugin-dir "<插件目录>" --port 18086 --token "<bearer>" --data-dir "<数据目录>"

# b) magpie 自带的 bun 实体（1.4.4，实测通过）
& "$env:USERPROFILE\.cache\magpie\bun\1.4.4\magpie-bun.exe" src/main.ts <同上>

# c) 单文件编译产物（实测 110 MB，0.5s 内完成）
& "$env:USERPROFILE\.cache\magpie\bun\1.4.4\magpie-bun.exe" build --compile src/main.ts --outfile magpie-bridge.exe
```

> 注意：PATH 上的 `bun` 是 npm shim（1.3.14），**不是** magpie 的 bun 实体。要测 `--compile`
> 请用上面的绝对路径。

### 2.1 参数（**环境变量优先，CLI 只作 standalone 回退**）

宿主 `manifest.Entry` 只有 `{exe, args}`，**没有 env 字段**——宿主是通过**环境变量**注入的
（`internal/plugins/supervisor.go` / `env.go`）。所以桥必须同时接受两种来源。

> **优先级为什么是 env 优先而不是 CLI 优先**
> supervisor **每次重启都会重新 `allocatePort`** 分配端口（`supervisor.go:147`），而
> `manifest.entry.args` 在**安装时冻结、不随重启更新**。如果 CLI 覆盖 env，重启后桥就会
> 监听到 args 里的**旧端口**，宿主永远连不上。因此：**env 优先，CLI 只在 env 缺失时生效**
> （用于 standalone 手跑调试）。

| CLI（回退） | 环境变量（优先） | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `--plugin-dir` | **（不支持，只走 CLI）** | 是 | — | 插件目录（含 `opencode.mjs`/`index.mjs`），或直接指向单个 `.mjs`。宿主把它写在 `entry.args` 里 |
| `--token` | `AIWORKGATE_PLUGIN_TOKEN` | 是 | — | 宿主 Bearer；空串**拒绝启动**（否则桥等于裸奔） |
| `--port` | `AIWORKGATE_PLUGIN_PORT` | 否 | `18086` | `0` = 系统分配 |
| `--data-dir` | `AIWORKGATE_PLUGIN_DATA_DIR` | 否 | `<os.tmpdir()>/magpie-bridge` | 凭据目录 |
| — | `AIWORKGATE_PLUGIN_ID` | 否 | `magpie-bridge` | `/meta`、`/health` 的 `id`（对齐宿主 manifest 的插件 id） |
| — | `AIWORKGATE_RULES_FILE` | 否 | — | 规则包路径，本卡只记录，留给 chat 泳道 |
| — | `AIWORKGATE_EGRESS_FILE` | 否 | — | 出口配置路径，本卡只记录，留给 egress 泳道 |
| — | `AIWORKGATE_API_LEVEL` | 否 | — | 宿主声明的契约级别（1..2），越界忽略并记日志 |

`-h` / `--help` 打印用法。绑定地址固定 `127.0.0.1`（公网永远经反代，见计划 D-15）。

宿主实际形态（与 host-opchost 对齐）：`entry.args` 只有 `--plugin-dir <目录>`，
其余 port/token/data-dir/id 全由环境变量给。

### 2.1.1 复用 magpie 已有登录态

启动时若桥的凭据库缺少某个已加载 provider 的记录，会从 magpie 的 `~/.config/magpie/plugin-auth.json`
导入该 provider 的登录条目（`access`/`refresh`/`expires` 等），并按桥的加密库落盘：

- 桥自己的登录/续期记录**优先**，导入不覆盖已有记录；
- 只打印导入的 provider id，不打印任何令牌；
- `MAGPIE_AUTH_FILE` 可指定其它路径；`AIWORKGATE_MAGPIE_AUTH_IMPORT=0` 关闭导入；
- 导入的是 magpie 当前登录态；若该凭据已被厂商判定失效，chat 仍会返回 `401 needs_sign_in`，需在桥管理面重新登录。

### 2.2 退出

`SIGINT` / `SIGTERM` → 关闭监听，3s 后强制退出兜底。

---

## 3. 端点契约（宿主视角）

### 3.0 生产挂载时序（`src/server.ts` 的分发顺序，**顺序即安全边界**）

```
GET  /health                    ← 公开（无鉴权）
        ↓
   —— Bearer 鉴权闸门 ——         ← 此后一切都要票，缺/错一律 401
        ↓
GET  /meta                      meta.ts
GET  /v1/models                 models.ts
POST /v1/chat/completions       chat/handler.ts（方法校验归 chat：非 POST → 405）
        ↓
await admin.handle(req, res)    admin/router.ts → 管理面 + /accounts + /checkin
        ↓ true = 已处理 / false = 继续
GET  /v1/... 未知路由 → 404
```

`server.on("close")` → `admin.close()`，回收挂起的登录会话与 127.0.0.1 回调监听。

### 3.1 端点总表

| 端点 | 鉴权 | 归属 | 宿主行为 |
|---|---|---|---|
| `GET /health` | **开放** | 桥核心 | 2s 超时；启动后 15s 内必须可用；运行期每 10s 探活；连续 2 次失败即重启 |
| `GET /meta` | Bearer | 桥核心 | 60s 超时；必含 `id` / `version` / `api_level` / `account_count` |
| `GET /v1/models` | Bearer | 桥核心 | 宽容解析：`{"data":[…]}` / `{"models":[…]}` / 裸数组 |
| `POST /v1/chat/completions` | Bearer | chat | SSE 流式 + 非流式聚合；无凭据 → 401 `needs_sign_in` |
| `GET /accounts` | Bearer | admin | 宿主调度枚举账号：`{"accounts":[Account]}` |
| `PATCH /accounts/{id}` | Bearer | admin | `{enabled:bool}` 局部更新 |
| `DELETE /accounts/{id}` | Bearer | admin | 2xx/204 |
| `POST /checkin` | Bearer | admin | D-19 桥侧聚合：enabled 记账、disabled 进 `skipped` |
| `POST /accounts/{id}/checkin` | Bearer | admin | D-19 契约端点，响应体即 `CheckinResult` |
| `POST /accounts/{id}/refresh` | Bearer | **未实现** | 契约有定义（`client.go:116`），桥尚未接线 |
| `GET/POST/DELETE /admin/*` | Bearer + 仅回环 | admin | signin / credentials / manifest / quotagroups / checkin/switches |

所有 `/admin/**` 只接受回环来源（`127.0.0.1 / ::1 / ::ffff:127.0.0.1`），其余 403。

**鉴权**：除 `/health` 外全部端点校验 `Authorization: Bearer <token>`；令牌缺失、格式错、
值错一律 **401**（`{"error":{"message":…,"type":"authentication_error","code":"invalid_token"}}`
+ `WWW-Authenticate: Bearer`）。比较方式：两边各取 SHA-256 后 `timingSafeEqual`，
不泄露长度、不短路。

**其他**：方法不符 405（带 `Allow`）；未知路由 404；`Cache-Control: no-store`。

### 3.2 `/meta` 样例

```json
{
  "id": "magpie-bridge",
  "name": "Magpie Bridge",
  "version": "0.1.0",
  "api_level": 2,
  "capabilities": ["accounts", "checkin", "quota", "upstream"],
  "account_count": 0,
  "uptime_s": 3,
  "started_at": 1793894656,
  "bridge": "magpie-bridge",
  "providers": [{"id":"trae-cn","name":"Trae CN","models":6},
                {"id":"trae-global","name":"Trae Global","models":5}],
  "plugin_dir": "…\\@magpie-community\\opencode-trae-auth",
  "warnings": []
}
```

- `account_count` 来自 `--data-dir` 里已落盘的凭据条数（当前 trae 未登录 → 0）。
- `providers` / `plugin_dir` / `warnings` / `bridge` 是**桥扩展字段**，宿主
  `json.Unmarshal` 会忽略，仅用于排障与验收。
- `capabilities` = **桥实际能应答的契约端点**（见 §3.3）。

### 3.3 能力位口径（Lead 裁定基线，2026-11-06，禁止改回插件钩子推导式）

**基线 = [`upstream`, `accounts`, `checkin`]**（桥恒定能应答的契约端点）；
**`quota` 仅当任一插件确有 `auth.usage` 钩子时附加**；
**`refresh` / `login_*` / `identity_*` 一律不声明**（端点未接线，声明即骗宿主）。

| 能力 | 依据 | 状态 |
|---|---|---|
| `upstream` | `POST /v1/chat/completions` 已挂 chat 泳道 | ✅ 基线 |
| `accounts` | `GET /accounts` 已由 admin 落（`admin/router.ts` `handleAccounts`） | ✅ 基线 |
| `checkin` | `/checkin` + `POST /accounts/{id}/checkin` 已落（D-19 委派） | ✅ 基线 |
| `quota` | 额度经 `/accounts` 的 `quota` 字段透出 | ⚪ 附加条件：插件有 `auth.usage` 才声明（trae 有 → `quota` 在；mock 无 → 不在） |
| `refresh` | `POST /accounts/{id}/refresh` | ❌ **不声明**（端点未接线） |
| `login_*` | 契约端点 `/accounts/login*` | ❌ **不声明**（登录走管理面 `/admin/signin/start`，属管理面能力不是宿主契约能力） |
| `identity_request` / `identity_os_switch` | `POST /accounts/{id}/identity/reset` | ❌ **不声明**（D-57 透传待补） |

**与 Go 侧 `DefaultBridgeCapabilities=[accounts, checkin]` 的差异**：桥的基线多了
`upstream`、`quota` 是条件项。宿主 `HasCapability` 读的是 manifest（Go 生成器仍写
`[accounts, checkin]`）而不是 `/meta`，所以**不影响调度**；若宿主侧也要统一口径，
归宿主侧与 task-9 处理。

规则：**只声明真正能调通的端点**——声明了宿主就会用，用不通就是骗宿主。
`src/meta.ts` 的 `bridgeCapabilities()`（基线常量 `BASELINE_CAPABILITIES`）是唯一来源，
`server.ts` 已把同一个函数传进 wiring override，`/admin/manifest` 与 `/meta` 一致
（wiring 的默认推导含 `refresh`/`identity`，但在生产路径不生效；其收尾归 task-9）。

### 3.4 `/v1/models` 样例

```json
{"object":"list","data":[
  {"id":"trae-cn/DeepSeek-V4-Flash","name":"DeepSeek V4 Flash","context_length":128000,
   "tools":true,"vision":false,"vendor":"trae-cn","local_id":"DeepSeek-V4-Flash","provider":"trae-cn"}
]}
```

**id 前缀策略**：`<providerId>/<localId>`。理由：一个桥实例会聚合多个 provider
（trae-cn 与 trae-global 有同名模型时无前缀会撞车），且计划 D-20 明确允许「一个插件导出多个
模型前缀」。未加前缀的原 id 保留在 `local_id`。若宿主希望无前缀，只改 `src/models.ts`
里的 `modelId()` 一处。

字段双写说明：桥输出**规范名**（`name` / `context_length` / `vision`），宿主解析器同时兼容
`display` / `context_window` / `context_length_max` / `supports_image` / `rate` /
`supported_efforts` 等别名——两者都对，桥选规范名。

---

## 4. 插件加载器

1. **选入口文件**（不是把所有 `.mjs` 当插件）：`opencode.mjs` → `index.mjs` →
   `package.json` 的 `main` → `exports["."]` → 全量扫描。
   真实包常带多个门面：`opencode.mjs` 是 OpenCode 1.x / magpie 宿主加载的门面，
   `index.mjs` 是自包含实现，`opencode2.mjs` 是 OpenCode 2.x 门面（`default = {id, setup}`，
   不是插件工厂）。全量扫描会误调 2.x 门面并产生重复 provider。
2. **识别工厂导出**：模块里每个函数导出依次 `await fn({ client })`；返回值满足
   `typeof value.provider.id === "string"` 即认定为插件实例（`_` 前缀与 `default` 跳过）。
3. **取静态模型表**：先跑 `config(cfg)` 钩子拿 `cfg.provider[<id>]`（名字/包名/baseURL/模型表），
   再调 `provider.models(entry, { auth: null })`——无凭据时插件按契约回落静态表
   （trae 包 L3606-3615 即此语义）；两者都空则用 `config` 的模型表兜底。
4. **宿主 client**：只实现插件真正用到的 `client.auth.set`（trae 包 L3132，且是可选链）。
5. **provider 去重**：同 id 只保留第一次，其余记 `warnings`。

### 4.1 凭据存储（全桥**只有一份**）

凭据的唯一正源是**管理面的加密库**（`src/admin/credentials.ts`）：
`<data-dir>/credentials.json`（AES-256-GCM 密文）+ `<data-dir>/bridge.key`（32B，`0600`）。
`src/server.ts` 的 `bridgeCredentialStore()` 把它适配成加载器/插件要看的
`CredentialStore`（provider 级），三条路径共用同一个实例：

| 路径 | 入口 | 落点 |
|---|---|---|
| 插件回写（续期） | `client.auth.set` → `loader.ts` 注入的 `client` | `bridgeCredentialStore.save` |
| chat 取凭据 | `chat/handler.ts` 的 `deps.getAuth` | `bridgeCredentialStore.get` |
| 管理面读写 | `/admin/credentials`（GET/POST/DELETE/export） | `adminStore` |

查找口径按 `accountId` 命中、忽略 `pluginId`（与 `admin/wiring.ts` 的 `chatAuthStore` 一致），
所以「登录写入 → chat 读取 → 插件回写」看到的是同一份凭据。
**任何日志与端点都不输出凭据明文**；`/admin/credentials` 只回脱敏投影
（`keys` / `present` / `expires_at` / `refreshable`），导出也不给密文。

> `loader.ts` 里仍保留了一个明文 `createCredentialStore`，现在**不再被 `main.ts` 使用**
> （生产路径一律走上面的加密库）。保留它只是因为 loader 目录不属于本卡作用域，
> 避免为了删一行改别人的文件。

---

## 5. 生产挂载验收（实测命令与回显）

### 5.1 trae 真包（无凭据）

```pwsh
$bridge='F:\Code\DSH-code\program\program5-aigate\opchost\bridge'
$trae="$env:USERPROFILE\.config\magpie\plugins\node_modules\@magpie-community\opencode-trae-auth"
node src/main.ts --plugin-dir "$trae" --port 18100 --token testtok --data-dir "$env:TEMP\fd-t6-trae"
```

```
[bridge] magpie-bridge v0.1.0 as "magpie-bridge" listening on http://127.0.0.1:18100
[bridge] providers:  trae-cn(6), trae-global(5) | plugins=2
[bridge] endpoints:  /health(open) /meta /v1/models /v1/chat/completions /accounts /checkin /admin/*
```

| # | 端点 | 结果 |
|---|---|---|
| A1 | `GET /health` | `{"status":"ok",…,"models":11,…} HTTP 200` |
| A2 | `GET /v1/models` 无 token | `HTTP 401` |
| A3 | `GET /meta` | `capabilities=["accounts","checkin","quota","upstream"]`（**含 accounts + checkin**）|
| A4 | `GET /v1/models` | `object=list count=11` |
| A5 | `GET /accounts` | `{"accounts":[{…,"status":"expired","credential":{"present":false}}, …]}` 合法 JSON |
| A6 | `GET /admin/manifest?plugin_id=magpie-bridge` | `HTTP 200`，capabilities 与 /meta 一致 |
| A7 | `POST /checkin` | `HTTP 200`（`skipped`，原因 `pluginCheckins off`——D-19④ 继承开关）|
| A8 | `POST /v1/chat/completions`（trae 无凭据）| **`HTTP 401`**，`code:"needs_sign_in"`，不是 404 |
| A9 | `POST /v1/chat/completions` 无 token | `HTTP 401` |
| A10 | `GET /v1/chat/completions` | `HTTP 405` |
| A11 | 未知路由 | `HTTP 404` |

A8 回显原文（关键证据：`/v1/chat/completions` 已挂载，无凭据返回语义错误而非 404）：

```json
{"error":{"message":"插件「trae-cn」尚未登录或登录已过期，无法发起对话请求。请先在管理面板完成「登录」：POST /admin/signin/start 拿到登录 URL，在本机浏览器完成授权后凭据会写入桥的 DATA_DIR，随后重试本请求。","type":"authentication_error","code":"needs_sign_in","hint":"…"}}
HTTP 401
```

### 5.2 mock 插件（凭据注入后 chat 全通）

```pwsh
node src/main.ts --plugin-dir "opchost\bridge\src\chat\mock-plugin" --port 18101 --token mocktok --data-dir "$env:TEMP\fd-t6-mock"
```

| # | 端点 | 结果 |
|---|---|---|
| B1 | `GET /health` | `HTTP 200`，`providers=2 models=16` |
| B2 | `GET /meta` | `capabilities=["accounts","checkin","upstream"]`（mock 无 `auth.usage` → 无 `quota`，符合 §3.3 规则）|
| B3 | `GET /v1/models` | `count=16`，前缀 `mock` + `mock-nocred` |
| B4 | `POST /v1/chat/completions` 无凭据 | `HTTP 401` `needs_sign_in` |
| B5 | `POST /admin/credentials` 注入 | `HTTP 200`，`{"ok":true,"stored":true,"credential":{…,"present":true}}` |
| B6 | `GET /meta` | `account_count=1`（凭据真的进了同一份库）|
| B7 | `GET /accounts` | `mock` → `status:"ok"` / `credential.present:true`；`mock-nocred` → `status:"expired"` |
| B8 | `POST /v1/chat/completions` `stream:false` | **`HTTP 200`**，`{"id":"chatcmpl-mock-json","object":"chat.completion",…,"choices":[…],"usage":{…}}` |
| B9 | `POST /v1/chat/completions` `stream:true` | **`HTTP 200`**，`Content-Type: text/event-stream`，3 帧正文 + finish + usage + `data: [DONE]` |
| B10 | `model` 不带 provider 前缀（双 provider） | `HTTP 404`，`code:"ambiguous_model_route"`（chat 路由的明确报错，非桥兜底）|
| B11 | `model=mock/mock-401` | `HTTP 401`，上游 401 映射成 `code:"needs_sign_in"` |

B9 的 SSE 帧（节选，帧边界与 `[DONE]` 齐全）：

```
HTTP/1.1 200 OK
content-type: text/event-stream; charset=utf-8
cache-control: no-cache, no-transform

data: {"id":"chatcmpl-mock-6061f449","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-mock-6061f449","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" from "},"finish_reason":null}]}

data: {"id":"chatcmpl-mock-6061f449","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"mock(mock-chat)"},"finish_reason":null}]}

data: {"id":"chatcmpl-mock-6061f449","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-mock-6061f449","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}

data: [DONE]
### 5.3 启动参数与只读端点回归（task-1 基线，挂载后未回归）

挂载 chat + admin 之后，`/health` `/meta` `/v1/models` 与三个只读端点的 401/404/405 语义全部保持（见 §3.0 时序）：

| 检查 | 结果 |
|---|---|
| `GET /health`（无 token） | `HTTP 200` |
| `GET /v1/models` 无 token / 错 token | `HTTP 401` |
| `GET /meta` 无 token | `HTTP 401` |
| `GET /v1/models` 带 token | `HTTP 200` + 11 条 |
| `GET /meta` 带 token | `HTTP 200` + providers |
| 未知路由 | `HTTP 404` |
| `POST /meta`（方法不符） | `HTTP 405` |

宿主形态启动（`entry.args` 只给 `--plugin-dir`，其余全走环境变量，实测通过）：

```pwsh
$env:AIWORKGATE_PLUGIN_ID='trae-bridge'; $env:AIWORKGATE_PLUGIN_PORT='18087'
$env:AIWORKGATE_PLUGIN_TOKEN='envtok';   $env:AIWORKGATE_PLUGIN_DATA_DIR="$env:TEMP\bridge-data-env"
$env:AIWORKGATE_RULES_FILE="$env:TEMP\rules.json"; $env:AIWORKGATE_EGRESS_FILE="$env:TEMP\egress.json"
$env:AIWORKGATE_API_LEVEL='2'
node src/main.ts --plugin-dir "$env:USERPROFILE\.config\magpie\plugins\node_modules\@magpie-community\opencode-trae-auth"
# → [bridge] magpie-bridge v0.1.0 as "trae-bridge" listening on http://127.0.0.1:18087
#   /meta 的 id 为 trae-bridge；rules-file / egress-file / host api_level 均被读到
```

**优先级回归**（env 与 CLI 冲突时必须走 env）：

```pwsh
$env:AIWORKGATE_PLUGIN_PORT='18091'; $env:AIWORKGATE_PLUGIN_TOKEN='envtok'
node src/main.ts --plugin-dir "<dir>" --port 18090 --token clitok
# → 实际监听 18091、接受 envtok、拒绝 clitok（证明 env 覆盖 CLI）
```

---

## 6. 边界与已知限制

- **已挂载**：chat（`/v1/chat/completions`）与 admin（`/admin/*`、`/accounts`、`/checkin`）
  全部接进 `src/main.ts`（P1-5）。`src/chat/dev-server.ts` 只是 chat 自检用的独立服务器，
  **不是生产入口**；生产一律走 `src/main.ts`。
- **未接线的契约端点**（对应能力位**不声明**，见 §3.3）：
  `POST /accounts/{id}/refresh`、`/accounts/login*`（四件套）、`POST /accounts/{id}/identity/reset`。
  这些是 aiworkgate 契约里已有、桥尚未实现的——别以为没报 404 就是安全，
  而是能力位故意没声明。
- SSE 约定（宿主侧）：宿主按响应 `Content-Type` 含 `text/event-stream` 判定流式，
  然后按 32 KiB 分块逐块 Flush；宿主自己每 15s 注入 keep-alive，桥**不必**发心跳，
  但必须逐块 flush、不缓冲整包（chat 泳道已按此实现，实测帧边界正确）。
- **trae 当前未登录**（`plugin-auth.json` 无 trae 凭据）→ `account_count=0`、
  `/accounts` 显示 `status:"expired"`、`provider.models` 只给静态表；真实模型列表/chat
  要用户先在本机完成一次浏览器登录（`POST /admin/signin/start` 拿 URL）。
- **无凭据时不崩**：`/v1/models` 正常返回静态表；chat 无凭据返回 `HTTP 401` +
  `code:"needs_sign_in"` 的可读文案，不是 500、也不是 404。
- `tsc --noEmit` **未验**：本目录不装 `@types/node`，`process`/`Buffer` 会报未定义——环境缺包，
  不是源码错误。运行时校验以 `node src/main.ts` / `bun build --compile` 为准（均已实测）。
- `provider.models` 若是「必须带凭据才能列」的实现，本卡只能拿到 `config` 里的静态表；
  接新插件时逐个核（计划 P1 风险第 1 条：逐包验证）。
- 凭据文件是**密文**（AES-256-GCM，`<data-dir>/bridge.key`），不是明文——这比 P1-1 时的占位更好。
  但仍别把带凭据的 `--data-dir` 拷到公网机器上。
- 调用方注意：`--plugin-dir` 指向目录时，加载器只挑**入口文件**（`opencode.mjs` → `index.mjs` →
  `package.json` 的 `main`/`exports["."]` → 全扫），不会把所有 `.mjs` 当插件重复加载
  （真包里 `opencode.mjs`/`opencode2.mjs`/`index.mjs` 是同一实现的三个门面）。

---

## 7. 三条泳道的分工与接线方式（已接完，供后续泳道续写）

`src/types.ts` 是唯一契约源，**不要**在 chat/admin 里另立同名字段。已就位的关键类型：

- 宿主侧：`MetaResponse`、`HealthResponse`、`ModelsResponse`、`ErrorShape` / `errorBody()`、
  `PluginManifest`、`PluginStatus`、`CredentialRecord`、`CredentialStore`
- 插件侧：`OpenCodePlugin`、`OpenCodeProvider`、`OpenCodeAuth`、`PluginModelMap`、
  `PluginModelEntry`、`HostClient`
- 泳道：`ChatRequest` / `ChatMessage` / `ChatChunk`（SSE 单帧）/ `ChatCompletion`（非流式，
  `model_requested` 与 `model_served` 分离，D-26）/ `ChatUsage`
- 运行时：`BridgeConfig`、`BridgeContext`（含 `registry` 与 `credentials`）、`ENV_KEYS`

**三条泳道的接入点**（都在 `src/server.ts` 里，后续只调用、不重写）：

| 泳道 | 入口 | 依赖怎么给 |
|---|---|---|
| chat | `createChatHandler({ deps })` → 挂 `POST /v1/chat/completions` | `ChatDeps`：`listProviderIds()` / `getProvider(id)` / `getAuth(id)` / `saveAuth(id,v)` / `log` / `config`。由 `buildChatDeps(ctx, adminStore)` 造好，provider 直接用 `registry.providers[id].plugin.plugin`（magpie 的 OpenCodePlugin） |
| admin | `createAdminRouter(adminDepsFromBridge(ctx, { pluginName, capabilities }))` → `admin.handle(req,res)` | `AdminDeps`：`plugins`（`registryFromLoad`）+ `dataDir` + `credentials`（同一个 `adminStore`） |
| 桥核心 | `buildMeta` / `buildHealth` / `buildModels` + Bearer 闸门 | `BridgeContext`：`config` / `startedAt` / `registry` / `credentials` |

**凭据只有一份**（`src/admin/credentials.ts` 的加密库）：`server.ts` 的
`bridgeCredentialStore(adminStore, pluginId)` 把它适配成 `CredentialStore`，
加载器（插件 `client.auth.set`）、chat（`getAuth`）、管理面三处共用。
**新增任何要读凭据的泳道，一律 `ctx.credentials`，不要再起自己的文件库。**

**新增端点的挂法**：在 `handle()` 里按 §3.0 的顺序插分支（鉴权闸门之后、admin 之前），
注意 `/v1/*` 与 `/admin/*` 不重叠但 admin 必须放在 chat 之后，避免 chat 的 fallback 先吃掉请求。

---

## 8. P8 出口白名单接线（task-14，实现见 src/egress/，规则正本 docs/p8/01）

桥内三层防线中的第二层（L7-2）：插件上游请求**默认**过白名单，越界拒绝且审计落盘。
`src/egress/` 本身由 bridge-chat 交付（selftest 12/12），本目录只负责接线。

**接线点**：

| 环节 | 位置 | 做的事 |
|---|---|---|
| 装配 | `main.ts` → `loadEgressFromEnv(process.env, {log}, dataDir)` | strict 读宿主 `AIWORKGATE_EGRESS_FILE`（缺省 false = 放行+记账）；审计落 `<data-dir>/egress-audit.jsonl` |
| 加载期声明 | `loader.ts` → `guard.declare` | 插件 SITES（`_internal.SITES`，入口门面没有就回退同目录 `index.mjs`）递归采域名 → **全部 provider 取并集**，以 `pluginId` 一个键注册（guard 以 plugin 为主键，逐 provider 会互相覆盖）；`egress.json/rules.json` 的文件级域名也挂到 `pluginId` 下 |
| 运行期拦截 | `server.ts` `wrapLoaderFetch` | chat 的 `getProvider` 在**插件 auth.loader 边界**把返回的 `fetch` 换成 `guard.wrapFetch({plugin, provider})`——越界拒绝、逐跳重定向复核、审计 |
| 403 语义透传 | `chat/upstream.ts` `callUpstream` catch | `EgressError`（403 + `EGRESS_*`）原样转成 ChatError 403（code=`egress_*`），不被 `networkError` 吞成 502 |
| 启动告警 | `main.ts` 启动末 | `reportDeclarationGaps(declarations, actualHosts)` 点名「声明了域但实际请求别处」；本桥声明与 SITES 同源，正常为空 |
| 管理面 | `server.ts` `sites` resolver | SITES 摊平成顶层 URL 形态交给 admin，`/admin/manifest` 的 `network_domains` 从此非空（trae 实测 11 域） |

**strict 语义**：`true`（服务器必须）= 未声明 → 拒绝一切外联；`false`（本机默认）= 未声明 → 放行 + WARN 记账。两者都记审计。

**实测回显（mock 插件，`mock.invalid` 上游）**：

```
A) strict=false → chat HTTP 200；审计行 {"action":"allowed","reason":"EGRESS_DOMAIN_NOT_DECLARED",...}
B) strict=true 且未声明 → chat HTTP 403 {"error":{"message":"出口白名单：插件「magpie-bridge」strict=true 且未声明任何 network_domains，按策略拒绝一切外联（mock.invalid:443）","code":"egress_no_declaration"}}
   审计行 {"action":"denied","reason":"EGRESS_NO_DECLARATION","host":"mock.invalid","port":443,...}
C) strict=true 且 egress.json 声明 "mock.invalid" → chat HTTP 200（命中白名单，无 WARN/denied 行）
```

**注意**：桥内拦截防不了插件用 `node:tls` / 原生 socket 直连（那由宿主 CONNECT 监听器 + L3 nftables 兜底，见 egress/README 部署注意）；`blockPrivate` 默认开（SSRF 层），生产**必须保持**。
