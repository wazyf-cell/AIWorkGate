# OfficeAce 插件（AIWorkGate）

把本机 **OfficeAce 1.3.4** 的登录态接入 AIWorkGate：宿主看到的是标准 OpenAI 兼容的
`officeace/<模型>`，实际请求由插件直连华为云 MaaS 网关。插件同时提供 OfficeAce 账号、
积分/额度、领取（映射为宿主签到）与 STS 续期能力。

- 插件 id：`officeace`
- 版本：`0.3.1`
- 契约等级：`api_level: 2`（支持契约 v0.3 的强制账号调度；也能跑在不发新头的 v0.2 宿主上）

> 本插件是**只读**的。它不会写入 OfficeAce 的安装目录、`data\storage.sqlite`、
> `.office-claw` 或 `C:\Users\<你>\.office-claw` 中的任何文件，也不会修改 OfficeAce
> 的会话、登录状态或数据库。新版 OfficeAce 把 providerState 放进 Windows 凭据管理器
> 时，插件只用 `CredReadW` 读取，不新增、不修改、不删除凭据条目。

## 前置条件

1. 已安装 **OfficeAce**（当前验证版本 1.3.4）。
2. 已在 OfficeAce 中**登录**，并在 OfficeAce 中配置好可用的华为云 MaaS 模型。
3. OfficeAce 至少启动过一次（需要 `data\storage.sqlite` 中存在会话记录）。
4. 查看积分、领取积分需要 OfficeAce **正在运行**（本机 API `https://127.0.0.1:3004`）。
   仅聊天不依赖它：插件从 SQLite 读取凭据后直连 MaaS。

## 安装与配置

把整个 `plugins/officeace` 目录（含 `manifest.json`、`officeace-plugin.exe`）交给
AIWorkGate 安装，宿主会注入端口、token、数据目录、规则与出网文件。

安装目录探测顺序：

1. 环境变量 `OFFICEACE_HOME`
2. 插件数据目录下 `config.json` 的 `{"officeace_home":"..."}`
3. `E:\Program\OfficeAce\program\OfficeAce`
4. `%LOCALAPPDATA%\Programs\OfficeAce`
5. `%PROGRAMFILES%\OfficeAce`

以上目录（或其父目录 `...\OfficeAce`）下必须存在 `data\storage.sqlite`。

### 非默认安装路径

两种方式任选：

- 设置环境变量 `OFFICEACE_HOME` 指向 OfficeAce 的 Electron 根目录（包含
  `data\storage.sqlite` 的那一层）。
- 在宿主登录页选择 OfficeAce，若提示找不到安装目录，提交表单（插件控制端口由宿主
  动态分配，token 为宿主注入的 `AIWORKGATE_PLUGIN_TOKEN`）：

```bash
curl -X POST "http://127.0.0.1:$AIWORKGATE_PLUGIN_PORT/accounts/login/ls_xxx/submit" \
  -H "Authorization: Bearer $AIWORKGATE_PLUGIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"values":{"officeace_home":"D:\\Apps\\OfficeAce\\program\\OfficeAce"}}'
```

插件会把它写入自己的数据目录 `config.json`，更新插件不会覆盖。

## 读取了什么（全部只读）

| 位置 | 内容 | 用途 |
|---|---|---|
| `<OfficeAce>\data\storage.sqlite` | `oc_kv_string` 中 `office-claw:oc:session:id:*` 的会话 JSON | 会话 id、用户身份、`model_auth_info`、`model_api_url_base`、IAM AK/SK、STS token、刷新令牌与过期时间 |
| `<OfficeAce>\data\storage.sqlite` | 同上，只取 `lastSeenAt` 最新的记录 | 单账号 |
| Windows 凭据管理器 | 会话 JSON 里 `providerStateRef: wincred://...` 指向的条目（分片 + `base64`） | 新版 OfficeAce 把 providerState 移出 SQLite 后，用 `CredReadW` 只读重组成同样的字段 |
| `%USERPROFILE%\.office-claw\users\<id>\model.json` | 模型目录 | 本机 API 不可用时的模型兜底 |
| `https://127.0.0.1:3004` | `/api/subscription/current`、`/api/subscription/claim-bonus`、`/api/maas-models`、`/api/model-config-profiles`、`/api/islogin` | 额度、领取、模型目录 |
| 华为云 MaaS | `https://<model_api_url_base>/v2/chat/completions` | 聊天（Basic 优先，401/403 时 SDK-HMAC 重试） |
| 华为云 IAM | `https://iam.myhuaweicloud.com/v3.0/OS-CREDENTIAL/securitytokens` | STS 过期后的续期 |

SQLite 以 `mode=ro` + `query_only(1)` 打开，绝不建表、迁移或写入。数据库被占用时
会短暂等待并返回明确错误。Windows 凭据管理器只用 `CredReadW` 读取，不新增、不修改、
不删除条目。

## 从不写入

- OfficeAce 安装目录、`storage.sqlite` 及其 `-wal/-shm`
- `C:\Users\<你>\.office-claw`（含 `session.cookie`、`provider-profiles.json`、
  `users\...\model.json`）
- OfficeAce 的登录态、cookie、模型配置

插件自己的数据目录只保存：`state.json`（账号 id、启用状态、签到记账、计数）、
`config.json`（`officeace_home`、可选的 `auto_refresh`）、`models-cache.json`
（模型目录缓存，6 小时 TTL）。**凭据永不落盘**；STS 续期结果只驻留内存。

## 账号与登录

- 账号 id 为 `acc_<hash(用户标识)>`，用户不变则重启后稳定。
- 宿主登录接口：

```bash
# 发起（OfficeAce 已登录时通常直接 success）
curl -X POST "http://127.0.0.1:$AIWORKGATE_PLUGIN_PORT/accounts/login" \
  -H "Authorization: Bearer $AIWORKGATE_PLUGIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"method":"local_state"}'

# 轮询
curl "http://127.0.0.1:$AIWORKGATE_PLUGIN_PORT/accounts/login/ls_xxx" \
  -H "Authorization: Bearer $AIWORKGATE_PLUGIN_TOKEN"
```

返回 `pending` 且 `instructions` 为“请打开 OfficeAce 并登录后重试”时，按提示操作后
重新轮询即可；插件每次都会重新只读 SQLite。

`manifest.json` 的 `login` 块向宿主声明：`methods:["local_state"]`、读本机登录态、不强制要求官方客户端在运行、**默认不自动续期**（`auto_refresh:false`）。

## 契约 v0.3：强制账号与回传头

宿主（调度层）选号后通过请求头下发。OfficeAce 是**单账号**插件，因此只有"就是它"或"409"两种结果：

| 请求头 | 行为 |
|---|---|
| `X-Aiworkgate-Account: acc_xxx` | 与本机会话派生的账号一致 → 正常处理；不一致 / 账号已停用 / 无可用凭据 / 读不到会话 → `409` `{"error":{"message":...,"type":"account_unavailable","code":"account_unavailable"}}`，**绝不换号** |
| `X-Aiworkgate-Request-Id` | 只写入日志（`req_id`） |
| `X-Aiworkgate-Hedge: 1` | **禁止续期**（`auto_refresh` 即使为 true 也不生效），单次尝试 |

| 响应头 | 说明 |
|---|---|
| `X-Aiworkgate-Account` | 真实服务账号；流式在第一个 SSE 字节前写出 |
| `X-Aiworkgate-Account-Status` | `rate_limited`（MaaS 返回 429 时） |
| `X-Aiworkgate-Cooldown` | 不发送：OfficeAce 没有可推算的冷却时长 |
| `X-Aiworkgate-Credits` | 不发送：MaaS 只返回 token 用量，没有每请求积分 |

- `X-Aiworkgate-Hedge:1` 下若 Basic 认证被拒，仍会按既有逻辑改用 SDK-HMAC 再试一次（同账号、同请求、未产生生成），**不会**续期、不会重放。
- 不带 `X-Aiworkgate-Account` 时行为与 v0.2 完全一致。
- `/accounts` 的 v0.3 字段：`credential{present,expires_at,refreshable}`（`refreshable` 表示存在未过期的 refresh_token，不代表允许自动续期）、`quota.plan`（本机订阅 `planName`，取不到就省略）；不提供 `cooldown_until` / `model_cooldowns` / `priority` / `weight`。

## 支持的端点

`GET /meta`、`GET /health`、`GET /v1/models`、`POST /v1/chat/completions`、
`GET /accounts`、`POST /accounts/login`、`GET|DELETE /accounts/login/{sid}`、
`POST /accounts/login/{sid}/submit`、`PATCH|DELETE /accounts/{id}`、
`POST /accounts/{id}/checkin`、`POST /accounts/{id}/refresh`、
`GET /accounts/{id}/history?days=30`、`POST /reload`。

除 `/health` 外均要求 `Authorization: Bearer <AIWORKGATE_PLUGIN_TOKEN>`。

## 限制（请务必阅读）

- **OfficeAce 没有“每日签到”**。本插件把「一键领取赠送积分」映射为宿主签到：
  领取到积分 → `ok`；无可领取 → `already`；请求/服务失败 → `fail`。
  赠送积分可能已经领过，`already` 是正常结果。
- **额度与领取依赖本机 OfficeAce 运行**。服务未运行时额度字段省略，
  `last_error` 会说明；领取返回 `fail`（“OfficeAce 未运行，无法领取”）。
- **模型目录**优先本机 API，其次用户 `model.json`，最后是 6 小时缓存。
  未知能力字段不会被编造：模型未明确声明视觉能力时，图片会替换为文本占位并记录 warning。
- **STS 过期**：默认**不自动续期**，因为续期可能轮换 `refresh_token`，让 OfficeAce
  自己保存的会话失效。过期时账号标记为 `expired`，打开 OfficeAce 重新登录即可；
  确认愿意承担轮换风险时，可在插件数据目录的 `config.json` 写入
  `{"auto_refresh": true}`（或调用 `POST /accounts/{id}/refresh` 手动续期一次）。
- **Basic 认证优先**：与 OfficeAce 1.3.4 自身一致；仅当 MaaS 返回 401/403 时改用
  SDK-HMAC-SHA256（`X-Sdk-Date` / `X-Security-Token` / `X-Project-ID`）。
- 每个账号固定代理，代理失败即失败，绝不回退直连；本机 `127.0.0.1:3004` 调用始终
  绕过代理。严格模式下未分配代理会返回 `egress_missing`。
- 账号数固定为 1（最新会话）。`DELETE /accounts/{id}` 只删除插件记账，OfficeAce
  重新登录态会在下一次读取时再次出现。
- `api_level: 2`：旧宿主（只接受 `api_level: 1`）会拒绝安装本版本，需要先升级宿主。
- `/v1/models` 不提供 `vendor` / `multiplier` / `thinking` / `free`：本机目录没有这些字段，插件不猜。

## curl 示例

宿主的浏览器界面通过 `/api/accounts`、`/api/checkin/all` 等接口间接调用插件；下面的
示例用于**直接调试插件**。控制端点使用宿主注入的端口与 token；聊天走宿主网关，使用
客户端 Key（`agk_...`）。

```bash
PLUGIN="http://127.0.0.1:$AIWORKGATE_PLUGIN_PORT"
TOKEN="$AIWORKGATE_PLUGIN_TOKEN"
GATEWAY="http://127.0.0.1:7865"
AGK="agk_xxx"

# 健康（免 token）/ 元信息
curl "$PLUGIN/health"
curl "$PLUGIN/meta" -H "Authorization: Bearer $TOKEN"

# 模型：插件直连 或 宿主合并列表
curl "$PLUGIN/v1/models" -H "Authorization: Bearer $TOKEN"
curl "$GATEWAY/v1/models" -H "Authorization: Bearer $AGK"

# 非流式聊天（经宿主）
curl "$GATEWAY/v1/chat/completions" \
  -H "Authorization: Bearer $AGK" -H "Content-Type: application/json" \
  -d '{"model":"officeace/glm-5.3","messages":[{"role":"user","content":"你好"}]}'

# 流式聊天
curl -N "$GATEWAY/v1/chat/completions" \
  -H "Authorization: Bearer $AGK" -H "Content-Type: application/json" \
  -d '{"model":"officeace/kimi-k3-1019","stream":true,"messages":[{"role":"user","content":"写一句诗"}]}'

# 账号（含内联额度）
curl "$PLUGIN/accounts" -H "Authorization: Bearer $TOKEN"

# 领取（宿主签到）
curl -X POST "$PLUGIN/accounts/acc_xxx/checkin" -H "Authorization: Bearer $TOKEN"

# 主动续期
curl -X POST "$PLUGIN/accounts/acc_xxx/refresh" -H "Authorization: Bearer $TOKEN"
```

## 构建

```bash
cd plugins/officeace
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test -count=1 ./...
CGO_ENABLED=0 go build -o officeace-plugin.exe .
```

纯 Go（`modernc.org/sqlite`），不依赖 CGO；目标 Windows amd64。
