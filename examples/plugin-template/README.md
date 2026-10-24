# Hello 模板插件

AIWorkGate 的最小可用插件：一个模型、非流式/流式对话、tools 回显、JSON 行日志。
纯标准库，无第三方依赖，`manifest.json` 与 `api_level = 1` 对齐宿主当前契约。

```
manifest.json   插件清单（id=hello，模型 hello-chat，按需启动）
main.go         读环境变量、绑定 127.0.0.1:<port>、信号退出
server.go       鉴权、访问日志、/health /meta /v1/models /reload
chat.go         /v1/chat/completions（JSON + SSE + tool_calls）
```

## 1. 构建

```powershell
cd examples\plugin-template
$env:CGO_ENABLED = "0"
go build -o hello-plugin.exe .
```

## 2. 打包与安装

用仓库自带的打包器（构建 + 按契约排版 + 自查一步完成）：

```powershell
powershell -ExecutionPolicy Bypass -File ..\..\scripts\pack-plugin.ps1 -PluginDir .
# 产物：build\hello.zip（manifest.json 与 hello-plugin.exe 都在 zip 根）
```

契约见 `docs\插件生成契约.md`。宿主会：

- 复制到 `%APPDATA%\AIWorkGate\plugins\hello\`
- 创建 `%APPDATA%\AIWorkGate\plugin-data\hello\`
- 因为清单是 `on_demand`，不会立刻启动；第一次请求时拉起

安装后模型名是 **`hello/hello-chat`**（宿主转发时会去掉 `hello/` 前缀）。

也可以用宿主客户端验证：

```powershell
curl.exe -s http://127.0.0.1:7865/v1/models -H "Authorization: Bearer agk_..."
```

## 3. 独立调试

```powershell
$env:AIWORKGATE_PLUGIN_PORT    = "18800"     # 必填
$env:AIWORKGATE_PLUGIN_TOKEN   = "dev-token" # 留空 = 不校验（仅调试）
$env:AIWORKGATE_PLUGIN_DATA_DIR = ".\dev-data"
.\hello-plugin.exe
```

另开一个终端：

```powershell
# 健康
curl.exe -s http://127.0.0.1:18800/health -H "Authorization: Bearer dev-token"

# 元信息
curl.exe -s http://127.0.0.1:18800/meta -H "Authorization: Bearer dev-token"

# 模型（注意：直连插件时用本地名 hello-chat，不带 hello/）
curl.exe -s http://127.0.0.1:18800/v1/models -H "Authorization: Bearer dev-token"

# 非流式
curl.exe -s http://127.0.0.1:18800/v1/chat/completions ^
  -H "Authorization: Bearer dev-token" -H "Content-Type: application/json" ^
  -d "{\"model\":\"hello-chat\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"

# 流式（SSE）
curl.exe -N http://127.0.0.1:18800/v1/chat/completions ^
  -H "Authorization: Bearer dev-token" -H "Content-Type: application/json" ^
  -d "{\"model\":\"hello-chat\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"

# tools 回显：hello_tool_call=true 时，若请求带 tools，就返回第一个工具的 tool_calls
curl.exe -s http://127.0.0.1:18800/v1/chat/completions ^
  -H "Authorization: Bearer dev-token" -H "Content-Type: application/json" ^
  -d "{\"model\":\"hello-chat\",\"hello_tool_call\":true,\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"get_time\",\"parameters\":{}}}],\"messages\":[{\"role\":\"user\",\"content\":\"time?\"}]}"

# 规则/出口文件重载（模板没有用到，返回 200）
curl.exe -s -X POST http://127.0.0.1:18800/reload -H "Authorization: Bearer dev-token"
```

通过宿主（`http://127.0.0.1:7865/v1`）访问时：

- `model` 换成 `hello/hello-chat`；
- 请求里不用带插件 token，宿主会换成插件 token 再转给你；
- `hello_tool_call` 是模板自定义字段，宿主会原样透传。

## 4. 宿主怎么启动/停止它

宿主注入（`internal/plugins/env.go` 与 `supervisor.go`）：

| 变量 | 说明 |
|---|---|
| `AIWORKGATE_PLUGIN_ID` | `hello` |
| `AIWORKGATE_PLUGIN_PORT` | 宿主动态分配的空闲端口，必须监听 |
| `AIWORKGATE_PLUGIN_TOKEN` | 每次调用都要校验的 Bearer token |
| `AIWORKGATE_PLUGIN_DATA_DIR` | `%APPDATA%\AIWorkGate\plugin-data\hello` |
| `AIWORKGATE_RULES_FILE` | `runtime\hello\rules.json`（模板未使用） |
| `AIWORKGATE_EGRESS_FILE` | `runtime\hello\egress.json`（模板未使用） |
| `AIWORKGATE_API_LEVEL` | `1` |
| `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` | 仅当宿主设置启用代理时注入，**不含代理账密** |

启动时宿主每 100 ms 调一次 `/health`，最多等 15 秒；运行中每 30 秒一次，连续 3 次失败就重启。
崩溃退避 1s/2s/5s/10s/30s，10 分钟内 5 次失败标记为 `failed` 并停止重试。

**停止机制（重要）**：Windows 上宿主先 `taskkill /PID <pid> /T`（不带 `/F`，只对窗口程序有效），等 5 秒后 `taskkill /F /T` 强杀整棵进程树。

- 宿主**不关闭 stdin**；子进程 stdin 指向 NUL，读会立刻 EOF。所以**绝对不要**把"读 stdin 失败/EOF"当作退出信号，否则插件启动即退出。
- 模板用 `signal.NotifyContext(os.Interrupt, syscall.SIGTERM)` 做优雅退出，只在手动运行（Ctrl+C）时可靠；被宿主强杀时不会执行。因此状态要写一次落一次（模板不写状态；需要持久化时用临时文件 + rename）。

## 5. 改成真实插件

1. 把 `buildReply` 换成真正的上游调用，把账号选择、签名、设备头放在这一层；
2. 若需要账号池：manifest 加 `accounts`、`checkin`、`quota` 等能力，实现 `/accounts`、`/accounts/login*`、`/accounts/{id}/checkin`；
3. 若需要账号级代理：manifest 加 `egress`，启动时和 `/reload` 时读取 `AIWORKGATE_EGRESS_FILE`，按账号选 `http://127.0.0.1:<port>` 的 `http.Transport`，**代理失败不得回退直连**，严格模式下拒绝 `direct`/`blocked`；
4. 若需要指纹清洗：manifest 的 `rules.packs` 声明规则包，读取 `AIWORKGATE_RULES_FILE`（`{packs, rules, strip}`），在发往上游前做字面替换 + 正则剥离；
5. 若声明 `identity_os_switch`：OS 级操作必须先关官方客户端、管理员权限、串行执行（宿主负责串行，但当前版本尚未开放系统级入口）；
6. 改 `manifest.json` 的 `id`、`name`、`version`、`models`、`entry.exe`；`api_level` 保持 1。

详细字段与端点见 `docs\插件开发指南.md`。
