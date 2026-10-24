# aichat · AI Work 助手转发插件

一句话：**这是本机独立 GUI「AI Work 助手」(A) 的转发插件——只转发，不托管 A 的进程，A 的 base_url 与 api_key 由插件自己持有。**

## 边界（先读，别误改）

| 事实 | 依据 |
|---|---|
| A 是**独立 GUI**，由 `HKCU\...\Run` 登录自启，父进程是 explorer | `docs/_scout-s9b.md` §3.1 实测 |
| A 的端口（实测 17864）来自它自己的 sqlite kv `api_gateway_settings` | 同上 |
| A **不校验** `Bearer <plugin token>`，也**不接受**宿主注入端口 | 同上：supervisor 六个前提 A 一条都不满足 |
| 所以本插件**不启停 A**、**不改 A 的端口**、**不做健康握手**；A 挂了这个插件就报上游不可达 | 本文件 |

## 凭据归属（Q18）

- `base_url` + `api_key` 存在 **`AIWORKGATE_PLUGIN_DATA_DIR\accounts.dat`**，用 **DPAPI**（`CryptProtectData`，当前 Windows 用户）封装，原子写 + `0600`。
- 宿主**不用**环境变量把 key 传进来（那只是把硬编码换个位置）。
- 本插件**不读** `%APPDATA%\AIWorkAssistant\conf\vault.stronghold`（别人的加密库，算法未验证，属越权读取）。
- `secrets.dat` 里的 `a_api_key` 本轮**不删**，保留作备份。

## HTTP 端点

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/health` | **免 token** | 宿主每 100ms/30s 轮询；只回 liveness，不泄露凭据 |
| GET | `/meta` | token | 含 `api_level`、`sealing`（DPAPI 还是降级）、`upstream_base_url`（不含 key） |
| GET | `/v1/models` | token | 代调 A 的 `base_url + /v1/models` 并**原样**返回 |
| POST | `/v1/chat/completions` | token | 透传，**SSE 逐块 flush，不缓冲** |
| POST | `/v1/completions` `/v1/responses` `/v1/messages` | token | 同上，路径原样拼到 base_url 后 |
| GET | `/accounts` | token | 回空列表（本插件只有一个上游凭据，没有账号池） |
| POST | `/accounts/{id}/checkin` `/accounts/{id}/refresh` | token | **501**，本骨架未实现 |
| POST | `/admin/upstream` | token | 写入 `{base_url, api_key}`，响应只回 `key_present` + `key_fingerprint`（sha256 前 4 字节），**绝不回显 key** |
| POST | `/reload` | token | 回 200 `{"rules":0,"egress":0}`（本插件不声明规则包） |

## 编译与测试

```powershell
cd plugins\aichat
go build ./...          # 零依赖，只标准库
go vet ./...
go test ./... -count=1
```

产物：`go build -o aichat-plugin.exe .`（`manifest.json` 的 `entry.exe` 就是这个名字）。

## 手工跑一次（不经宿主）

```powershell
$env:AIWORKGATE_PLUGIN_PORT = '17864'   # 注意不要和正在跑的 A 抢端口，换一个空闲端口
$env:AIWORKGATE_PLUGIN_TOKEN = 'dev-token'
$env:AIWORKGATE_PLUGIN_DATA_DIR = "$env:TEMP\aichat-dev"
.\aichat-plugin.exe
```

```powershell
# 先写入上游（A 的 key 用真实值；下面输出不回显它）
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:<port>/admin/upstream -Headers @{Authorization='Bearer dev-token'} -ContentType 'application/json' -Body '{"base_url":"http://127.0.0.1:17864","api_key":"<redacted>"}'
Invoke-RestMethod -Uri http://127.0.0.1:<port>/health
```

## 还没做完（骨架 ≠ 能端到端跑）

1. **R1 key 迁移没做**：宿主侧还没有「把 `secrets.a_api_key` 搬进 `accounts.dat`」的一次性动作。现在只能手工调 `/admin/upstream`。
2. **A 的端口发现没做**：插件不读 A 的 sqlite kv，`base_url` 要人填；A 换端口不会自愈（宿主的 `aResolvePort` 也不再有替代品）。
3. **签到/续期未实现**：`trae/wb/qoder` 三项签到还挂在宿主 `checkinAllA` 上，没有迁进来（Q18 之后的下一轮）。
4. **`api_level` 写的是 2**：v0.4 的 `embeddings`/`images` 能力**没写**，因为宿主 `handlePassThrough` 对任何插件模型一律 501，声明了就是假的。要等宿主接线后才能删 `forwardA`（见决策台账 Q18 之后「由此推导的两条执行约束」）。
