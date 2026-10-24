# CodeArts 插件（AIWorkGate sidecar）

面向华为云 CodeArts（SNAP）的 AIWorkGate 插件：多账号 OAuth 登录、凭据续期、每日签到/领积分、额度上报、动态模型目录，以及 OpenAI 兼容的 `/v1/chat/completions` 上游（含流式、工具调用、排队重试与账号轮换）。

- 插件 id：`codearts`
- 版本：`0.3.0`
- 契约等级：`api_level: 2`（支持契约 v0.3 的强制账号调度；也能跑在不发新头的 v0.2 宿主上）
- 平台：Windows amd64（DPAPI 加密依赖 Windows）
- 可执行文件：`codearts-plugin.exe`

## 1. 启动方式

宿主会注入以下环境变量并以 `127.0.0.1:<port>` 拉起插件：

| 变量 | 说明 |
|---|---|
| `AIWORKGATE_PLUGIN_PORT` | 必填，监听端口 |
| `AIWORKGATE_PLUGIN_TOKEN` | 必填，所有请求需带 `Authorization: Bearer <token>`（`/health` 除外） |
| `AIWORKGATE_PLUGIN_DATA_DIR` | 私有数据目录；缺省 `./plugin-data` |
| `AIWORKGATE_RULES_FILE` | 宿主合并后的规则包（可选） |
| `AIWORKGATE_EGRESS_FILE` | 账号 → 本地出口转发器映射（可选） |

手动运行：

```powershell
$env:AIWORKGATE_PLUGIN_PORT="18901"
$env:AIWORKGATE_PLUGIN_TOKEN="test"
$env:AIWORKGATE_PLUGIN_DATA_DIR="$env:TEMP\codearts-plugin-data"
.\codearts-plugin.exe
```

访问示例：

```bash
curl -H "Authorization: Bearer test" http://127.0.0.1:18901/health
curl -H "Authorization: Bearer test" http://127.0.0.1:18901/meta
curl -H "Authorization: Bearer test" http://127.0.0.1:18901/v1/models
curl -H "Authorization: Bearer test" http://127.0.0.1:18901/accounts
```

## 2. 登录

插件只实现了浏览器 OAuth 与“粘贴回调”两种方式（`login_oauth`）。`manifest.json` 的 `login` 块向宿主声明：`methods:["oauth"]`、不读本机登录态、不要求官方客户端、支持自动续期（`auto_refresh:true`），说明文字见 manifest。

### 2.1 浏览器 OAuth

```bash
curl -X POST http://127.0.0.1:18901/accounts/login \
  -H "Authorization: Bearer test" -H "Content-Type: application/json" \
  -d '{"method":"oauth"}'
```

返回：

```json
{"session_id":"ls_...","status":"pending","method":"oauth",
 "url":"https://codearts.huaweicloud.com/portal/authorize?...",
 "instructions":"在浏览器完成 CodeArts 登录"}
```

插件会在 `127.0.0.1` 上监听一个临时端口（≥10000），并按参考实现构造授权 URL：PKCE S256、随机 ticket、`client_id=codearts-agent`、`plugin-name=snap_AIIDE`、`plugin-version=5.2.0`；同时打开默认浏览器。回调落在 `/oauth/callback`：

- `?code=`：用 PKCE + DPoP 调用 STS 换取 AK/SK/security_token；
- `?secret=`：旧版门户票据流程（轮询 `/snap-manager/v1/login/ticket`）。

浏览器回调完成后轮询：

```bash
curl -H "Authorization: Bearer test" http://127.0.0.1:18901/accounts/login/ls_xxx
# {"session_id":"ls_xxx","status":"success","account_id":"acc_..."}
```

会话 5 分钟未完成会变为 `status:"failed"`；`DELETE /accounts/login/{sid}` 可取消并关闭监听端口。

### 2.2 粘贴回调（备用）

适用于浏览器无法跳回本机、或需要手工复制地址的场景：

```bash
# 第一步：创建会话（返回 url 与字段 code）
curl -X POST http://127.0.0.1:18901/accounts/login \
  -H "Authorization: Bearer test" -H "Content-Type: application/json" \
  -d '{"method":"paste_code"}'

# 第二步：把浏览器地址栏中的完整回调 URL（或裸 code）提交回来
curl -X POST http://127.0.0.1:18901/accounts/login/ls_xxx/submit \
  -H "Authorization: Bearer test" -H "Content-Type: application/json" \
  -d '{"values":{"code":"http://127.0.0.1:12345/oauth/callback?code=..."}}'
```

也支持一次性提交：`{"method":"paste_code","code":"<回调 URL 或 code>"}`。

同一个华为账号重复登录会更新原账号（id 由 `domain_id + user_id` 稳定派生，形如 `acc_ab12cd34ef`），不会重复创建。

## 3. 数据与安全

所有文件位于 `AIWORKGATE_PLUGIN_DATA_DIR`：

| 文件 | 内容 | 保护 |
|---|---|---|
| `accounts.dat` | 账号、AK/SK/security_token、refresh_token、PKCE verifier、DPoP 私钥 JWK、额度/签到缓存 | **DPAPI（当前用户作用域）加密**，临时文件 + 原子重命名 |
| `state.json` | 规则命中计数、最近 reload 信息 | 明文（不含机密） |
| `models-cache.json` | 远端模型目录、benefit 模型集合、抓取时间 | 明文（不含机密） |

- 插件不打印任何 token、AK/SK、refresh_token、DPoP 私钥；日志只包含账号 id、状态码与上游错误摘要。
- `GET /accounts` 只返回账号标签、状态、额度与只读身份，不返回凭据。
- `accounts.dat` 换用户或换机器后无法解密，需要重新登录。
- 账号 id、标签、额度、签到状态在插件升级时保留（数据目录不随安装目录覆盖）。

账号接口：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/accounts` | 账号列表 + 内联额度 |
| PATCH | `/accounts/{id}` | `{"enabled":false}` 启停 |
| DELETE | `/accounts/{id}` | 删除账号与凭据 |
| POST | `/accounts/{id}/refresh` | 立即续期（无 refresh_token 时 400） |
| POST | `/accounts/{id}/checkin` | 签到/领积分 |
| POST | `/accounts/{id}/identity/reset` | `{"mode":"request"}` 轮换 Session-Id/Chat-Id；`os` 返回 `unsupported_identity_mode` |

## 4. 模型

- `GET /v1/models` 返回远端目录（`/api/v1/gateway/config` 的 `result.models` + `/v1/model/builtin`），已做与参考实现一致的归一化：去掉 `-NNNN` 版本后缀、丢弃 `-VL` 视觉模型、按 id 去重。
- 目录写入 `models-cache.json`，TTL 6 小时；启动时只读磁盘缓存/内置回退，**绝不阻塞启动**。网络失败时继续提供缓存；无缓存时使用内置回退（GLM-5.2、GLM-5.1、GLM-5、glm-5.3-flash、openPangu、deepseek-v4 系列等）。
- benefit 模型（网关目录中不带数字后缀的模型 + `glm-5.3-flash`、`deepseek-v4.1-flash` 回退）请求会附加并签名 `maas_type: benefit`；若上游返回 `InferHub.4004.200`，会自动去掉该头重试一次。
- `deepseek-v4-flash` / `deepseek-v4-pro` 不支持 OpenAI `tools`：插件会注入 DSML 系统提示词、省略 `tools`，并从流式文本中解析 `<｜DSML｜tool_calls>` 与 `<thought>`，转换成标准 `tool_calls` / `reasoning_content`。

## 5. 聊天与排队

`POST /v1/chat/completions` 兼容 OpenAI：流式 SSE（`delta.content`、`delta.reasoning_content`、`delta.tool_calls`、`finish_reason`、`usage`、`[DONE]`）、非流式聚合、tools 透传。

上游固定使用 `stream:true`，并携带 `prompt_cache_key`（账号级 Session-Id）、`include:["reasoning.encrypted_content"]`、`reasoning_summary:"auto"`、`tool_stream:true`，`max_tokens` 默认 65536（请求可覆盖）。`reasoning_effort:"off"/"none"` 会翻译为顶层 `thinking:{"type":"disabled"}`。

- 首 token 等待上限 300 秒，之后空闲上限 600 秒。
- 排队：上游返回 `TM.00001041` / 高峰限流时，每 10 秒查询一次 `/api/v1/queue/status?model=&task_id=<session_id>` 并重发，最长 30 分钟；`queue_full` / `error` 立即失败。
- 200 响应里的 SSE 内嵌错误（`error_code`/`error_msg`）不会被吞掉：未向客户端输出前返回 HTTP 错误；已开始输出后发送 `data: {"error":{...}}` 再以 `[DONE]` 结束。
- 限流/额度错误码（6004、4291、`InferHub.ModelArts.81111.429`、`APIG.0602`、14003）：先对当前账号刷新一次 token；仍失败且有其它健康账号时**只轮换一次**，否则原样返回上游错误。401/403 会强制续期一次。
- 可用请求头 `X-CodeArts-Account: acc_xxx` 指定账号（调试/测试用；宿主通常不发送，插件按顺序自动选号）。这是本插件的扩展，不属于宿主契约。

### 5.1 契约 v0.3：强制账号与回传头

宿主（调度层）选号后通过请求头下发，插件照做：

| 请求头 | 行为 |
|---|---|
| `X-Aiworkgate-Account: acc_xxx` | **只用该账号**。未知 / 已停用 / 无效 / 无可用凭据 → `409` `{"error":{"message":...,"type":"account_unavailable","code":"account_unavailable"}}`，**绝不换号** |
| `X-Aiworkgate-Request-Id` | 只写入日志（`req_id`），不参与路由 |
| `X-Aiworkgate-Hedge: 1` | 单次尝试：不轮换、不续期后重试、不排队重试、不丢弃 benefit 头重试；失败立即返回 |

| 响应头 | 说明 |
|---|---|
| `X-Aiworkgate-Account` | **始终**返回真实服务账号；流式在第一个 SSE 字节前写出 |
| `X-Aiworkgate-Account-Status` | `rate_limited`（429/额度失败时） |
| `X-Aiworkgate-Cooldown` | 429/额度失败时给出建议冷却秒数（到 UTC+8 次日 0 点） |
| `X-Aiworkgate-Credits` | **不发送**：CodeArts 不提供每请求积分明细，插件不猜（D92） |

- 带 `X-Aiworkgate-Account` 时不轮换：额度/限流直接返回上游错误，由宿主换号。
- 不带该头时行为与 v0.2 完全一致（自选 + 一次轮换），因此同一份 exe 也能跑在旧宿主上。
- 出网失败（`egress_missing` 等）仍按 §18 返回 403，优先于账号可用性判断。


## 6. 签到与额度

签到顺序与参考实现一致：

1. `GET /snap-manager/v1/statistics/plugin`：确认是积分套餐并读取积分余额；
2. `GET /v1/ops/delivery?channel=IDE`：找 `type=="USER_LOGIN"` 的活动；
3. 可领取时 `POST /v1/ops/claim {"campaignId":...,"channel":"IDE"}`；
4. 响应含 `id` 时再 `POST /v1/ops/confirm {"campaignId":...}`。

映射：本次领取成功 → `ok`（能拿到 `benefitAmount` 时返回 `credits_before/after`）；活动存在但不可领取（含已领取状态）→ `already`；HTTP/解析/非积分账户/无活动 → `fail` 并带消息。

额度内联在 `/accounts`：统计接口结果缓存 60 秒，只对启用且凭据可用的账号拉取，失败保留上次值。`today_checked_in` 只在今天确实执行过签到（`ok` 或 `already`）后为 true——CodeArts 没有单独的签到状态接口。

契约 v0.3 新增的可选字段：

| 字段 | 来源 |
|---|---|
| `quota.plan` | 统计接口 `package.package_name_cn`（为空时用 `package_name_en`）；解析不到就不出现 |
| `credential.present` | 是否有可用或可续期凭据 |
| `credential.expires_at` | 存储的 security_token 到期时间（Unix 秒） |
| `credential.refreshable` | 是否有 refresh_token + code_verifier + DPoP 私钥 |
| `cooldown_until` | 仅在账号被标记 `rate_limited` 时出现，等于配额重置时间（UTC+8 次日 0 点） |
| `model_cooldowns` | **不提供**：本插件没有模型级冷却 |
| `priority` / `weight` | **不提供**：由宿主 `account_meta` 覆盖层决定 |


## 7. 续期语义

- access token 距过期不足 1 小时时自动续期（聊天、签到、额度、模型拉取前都会检查）；后台每 5 分钟巡检一次。
- 续期使用保存的 `code_verifier` 与 DPoP 私钥，向 STS 发送 `grant_type=refresh_token`。
- **旧 refresh_token 在续期成功后立即失效**；并发续期会按账号串行化，避免同一个 refresh_token 被消费两次。
- `refresh_token` 失效（`invalid_grant` / `ExpiredRefreshToken`）会把账号标记为 `invalid`，需要重新登录。
- `POST /accounts/{id}/refresh` 是手动“立即续期”，即使距过期超过 1 小时也会真的请求 STS；后台自动续期则遵守 1 小时提前量。

## 8. 账号级出网（egress）

- 完全遵循契约 §18 与 mock 插件实现：读取 `AIWORKGATE_EGRESS_FILE`，按账号选择 `direct` / `proxy(本地转发端口)` / `blocked`。
- `strict:true` 时没有代理的账号（含 `default:direct`）一律返回 403 `egress_missing`；代理失败绝不回退直连。
- 模型的拉取、聊天、签到、额度、续期等所有上游请求都走账号绑定出口；**只有浏览器登录页面本身**不受代理控制。登录过程中的 STS/票据请求使用 `egress.json` 中的 `"login"` 账号项（通常回落到 `default`），因此严格模式下需要为 `login` 或默认项分配代理，否则登录/换取 token 会被拒绝。
- 修改 `egress.json` 后宿主调用 `POST /reload`，插件重读规则/出口并重建连接。

## 9. 规则包

`AIWORKGATE_RULES_FILE` 中的字面替换与正则剥离会应用到发往上游的所有 system/message 文本（工具参数与工具名不处理）。命中次数写入 `state.json`，`POST /reload` 后立即生效。`manifest.json` 默认 `rules.packs: []`，即不启用清洗。

## 10. 已知限制

- 仅支持 Windows；DPAPI 加密绑定当前 Windows 用户。
- `api_level: 2`：旧宿主（只接受 `api_level: 1`）会拒绝安装本版本，需要先升级宿主。
- `/v1/models` 不提供 `vendor` / `multiplier` / `thinking` / `free`：上游目录只有 id 与显示名，插件不猜。
- 仅接入 `cn-north-4` 区域的固定上游地址与 `codearts-agent` OAuth 客户端，区域/客户端硬编码。
- 签到只对积分套餐有意义；Token 计费账户会返回 `fail`（“不在积分活动范围”）。
- 粘贴的授权码必须来自本插件刚生成的授权 URL：`redirect_uri` 中的回调端口需与会话一致，其他客户端（如官方 IDE）产生的 code 无法直接使用。
- 没有 `/accounts/{id}/history` 历史回填；宿主日历从安装日起记录。
- 不声明、也不执行 OS 级身份切换（`identity_os_switch` 未声明）；`identity.reset` 的 `os` 模式返回 `unsupported_identity_mode`。
- 身份只上报账号级 `session_id`（`source:"derived"`）；CodeArts 没有可用的机器码/设备号请求头，插件不臆造。
- benefit 免费额度按 UTC+8 自然日结算；额度耗尽时错误信息里的重置时间为估计值。
- 队列最长等待 30 分钟；大文件 `write` 类工具调用依赖 `tool_stream` 分段传输。
- 非流式请求会在插件内聚合整条上游流，超长响应占用更多内存（上游默认最大输出 65536 token）。
- 模型目录是账号无关的；插件用第一个可用账号拉取并写入磁盘缓存。
- 账号 id 优先由 STS 响应中的 `domain_id + user_id` 稳定派生；若响应不含这两个字段，则回退到 `access_key_id` 派生，此时同一用户重新登录可能生成新账号（上游未提供稳定用户标识时无法可靠合并）。

## 11. 构建与测试

```powershell
cd plugins\codearts
$env:CGO_ENABLED="0"
go vet ./...
go test -count=1 ./...
go build -o codearts-plugin.exe .
```

测试全部使用 `httptest` 假上游与假时钟，不会发起真实登录、签到或聊天；DPAPI 往返测试只在本机用户作用域内加密一段测试字符串。

## 12. 接口总览

| 方法 | 路径 |
|---|---|
| GET | `/health`（免鉴权）、`/meta`、`/v1/models`、`/accounts` |
| POST | `/v1/chat/completions`、`/accounts/login`、`/accounts/login/{sid}/submit`、`/accounts/{id}/checkin`、`/accounts/{id}/refresh`、`/accounts/{id}/identity/reset`、`/reload` |
| GET | `/accounts/login/{sid}` |
| DELETE | `/accounts/login/{sid}`、`/accounts/{id}` |
| PATCH | `/accounts/{id}` |

除 `/health` 外所有接口都要求 `Authorization: Bearer <AIWORKGATE_PLUGIN_TOKEN>`；错误体统一为 `{"error":{"message":"...","type":"...","code":"..."}}`。
