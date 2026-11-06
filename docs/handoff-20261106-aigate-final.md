# AIWorkGate 交接（2026-11-06 最终版，2026-11-07 接手轮更新）

> 这是给下一个 agent 的唯一入口文档。旧文档 `handoff-20261106-aigate-swarm-rounds.md` 保留历史细节，但**以本文 + `git log` + 变更日志为准**。
> 用户已明确：本仓库按**单代理**推进，不要把旧文档 §6 的“蜂群/team 板”当成可用能力；如你确有多子代理能力，可自行使用，但不要假设看板存在。

## 1. 项目与当前状态

- **项目**：AIWorkGate，Windows 上的 Go 本地 AI 网关（`aiworkgate.exe`），Vue3 管理页经 `go:embed` 内嵌；对外 `/v1`（OpenAI/Anthropic 兼容）+ `/v1beta`（Gemini 兼容），管理面 `/api`，自助页 `/me/`。
- **仓库**：`F:\Code\DSH-code\program\program5-aigate`，分支 `master`（推送到 GitHub `main`）。
- **当前提交**：接手轮结束于 `9a57219` 之后（见 `git log --oneline -6`：66be7de /v1beta、d463a8b 桥、f1fe7a2 messages 400、9a57219 脚本）。
- **远端**：`https://github.com/wazyf-cell/AIWorkGate`，`main` 与 master 同步（推送为直接 URL push，静默 helper）。本地另有 `origin` 指向 `F:/Code/DSH-code/program/program-2-gateway/aiworkgate`（本地镜像，落后不管）。
- **工作区**：干净（仅 `.qoder-credits/` 未跟踪，忽略；**不要 `git add -A`**）。
- **预览实例**：`127.0.0.1:7865`，隔离数据目录 `runtime/preview-appdata`；管理员启动令牌固定为 env `AIWORKGATE_ADMIN_TOKEN`（交接 §5），客户端 Key 在 `runtime/preview-key.txt`——**该文件是 3 行卡片格式，真 Key 是第 2 行第 2 列**（`awk 'NR==2{print $2}'`），整文件 cat 会 401（见 §6）。

## 2. 本轮之后的重大变化（2026-11-07 接手轮，务必先读）

1. **workbuddy 真实 chat 已验收，无需用户重新登录**（原 §4 第 1 项关闭，原归因作废）
   - 5ms 401 needs_sign_in 的真实根因是桥的两个 bug（非 magpie 凭据失效，存量凭据 2026-12-06 才过期）：
     - **空 baseURL 误判**：真包 workbuddy loader 刻意返回 `baseURL: ""`（模型自带 api.url，chat 走 `<endpoint>/v2`），桥一律判「未登录」。已修：空 baseURL + 自定义 fetch = 合法形状，上游地址按 模型 `api.url` → provider 级 `api` 解析；都没有 → 501 `no_upstream_url`；`provider.npm=@ai-sdk/anthropic`（Messages 模型）明确不支持。
     - **登录凭据落错键位**：signin settle 按账号名落库，chat 按 providerId 读。已修：settle 同时写 provider 键位（对 trae 重新登录同样生效）。
   - 真包上游**只收流式**（非流式 400 11101）：模型驱动形状的请求向上游强制 `stream:true`，客户端非流式由桥聚合回 JSON。
   - 验收证据（standalone 真包桥 + magpie 导入存量凭据）：非流式 200（2.6s，finish=stop，content="2"）；流式 200（41 帧，真实 usage 32+41）。见变更日志 2026-11-07 段。
2. **Gemini 入站 `/v1beta/*` 挂载修复**：宿主路由此前只转 `/v1`，`/v1beta` 落 SPA 处理器（405）。`app.routeHTTP` 改用 `isGatewayPath`（镜像网关白名单），LAN 管理密码豁免同步。回归 `gateway_mount_test.go`。
3. **`/v1/messages` 畸形 JSON 改 400**：原来伪装成 404 model_not_found（空模型名）。与 OpenAI 面 invalid_json 语义对齐，错误体保持 Anthropic 形状。
4. **P7 部署脚本三处 footgun 修复**：`--binary` 缺值静默退出；裸 `--dry-run` 首参会**真实 ssh**（已拒）；`install` 直写 live 文件非原子（改 `.new`+`mv`，文档「原子替换」现在为真）。
5. **桥自检 78 → 79 项**（新增 `signin_writes_provider_level_credential`）；chat 泳道 verify **16/16**（新增 5 个 workbuddy 形状用例）。bridge 无单测框架，验证走 selftest + dev-server/verify。
6. 沿用：A 网关退役（D-60）、P3 退役、无浏览器开关 `AIWORKGATE_NO_BROWSER=1`、静默推送（token 文件 helper，不打印 token）。

## 3. 13 阶段完成状态

| 阶段 | 状态 | 证据/入口 |
|---|---|---|
| P1 桥/宿主 | **本地验收完成**；本轮另修 `/v1beta` 挂载与桥两 bug | `docs/验收/2026-11-06-真实登录验收.md`、桥自检 79/79、verify 16/16 |
| P2 市场/凭据 | 目录、双 provider、31 模型；**workbuddy 真实 chat 已验收（免登录，见 §2.1）** | 变更日志 2026-11-07 段 |
| P3 插件退役 | **完成**（11 归档、aichat 保留） | `docs/验收/2026-11-06-P3退役.md` |
| P5 网关安全 | 本地运行态全过（过期 401、无 Key 401、伪造 XFF 不计、分享 Key 到期必填、allowed_ips、whitelist/属主）；2026-11-07 复查仍绿；Caddy/服务器项待外部 | 变更日志 P5 段 |
| P6 协议 | Gemini 入站含工具调用双向翻译（`/v1beta` 现已真正可达）；SystemOne 本地测试过；**真实 JEV key 未验** | `internal/gateway/gemini_inbound_test.go`、`docs/p6/` |
| P7 部署 | 脚本/演练手册完成（本轮修 3 处 footgun）；Linux amd64/arm64 ELF + dry-run 复验通过；**服务器实跑未做** | `docs/p7/` |
| P9/P10/P11/P12 | 本地实现与测试完成；P11 真实 JEV 意图判定待 key | 各包测试、`docs/p11/` |
| P13 用量/导出 | **全部完成** | `docs/p13/`、`docs/验收/2026-11-06-价格与导出.md` |
| P14 `/me` | 有效 Key 登录、过期/吊销会话即时失效、无正文；2026-11-07 边界复查全过（畸形请求、并发、413、大 body） | 变更日志 P14 段 + 2026-11-07 段 |

## 4. 未完成（仅剩外部依赖）

1. **真实 JEV Key**：P6 SystemOne 三例（`noul`/`choice`/`score`）与 P11 意图规则一例（难题→强、简单→便宜）。需要用户提供 JEV key。
2. **P4/P7 服务器**：凭据同步、Caddy 反代、手机/朋友 Key、本机断电 48h 续期等，需要服务器与域名。

**接手顺序**：拿到 JEV key 后做 P6/P11 两处验收 → 服务器到位后做 P4/P7 实跑。本地无剩余阻塞项。

## 5. 必须知道的命令

```bash
# 后端
go build ./...
go test -ldflags=-buildid= ./internal/...        # 24 包；-buildid= 必带

# 前端（typecheck + 样式债 + 视觉门禁 65/65）
cd web && npm run build

# 桥自检（79/79）
node opchost/bridge/src/admin/testdata/selftest.mjs --exit

# 桥 chat 泳道（16/16）：先起 dev-server 再跑 verify，两步都要
node opchost/bridge/src/chat/dev-server.ts --plugin opchost/bridge/src/chat/mock-plugin/index.mjs \
  --auth-provider mock --auth-provider mock-wb --auth-provider mock-wb2 \
  --port 18080 --data-dir <临时目录>
node opchost/bridge/src/chat/verify.ts --base http://127.0.0.1:18080 --suite mock

# Linux 交叉编译 + 脚本演练
VERSION=0.1.0-dev bash scripts/build-linux.sh
ARCH=arm64 VERSION=0.1.0-dev bash scripts/build-linux.sh
bash scripts/deploy-linux.sh user@host --dry-run
bash scripts/rollback-linux.sh user@host --dry-run

# 预览实例（隔离数据、无浏览器）
# 先停旧进程，再：
APPDATA="F:/Code/DSH-code/program/program5-aigate/runtime/preview-appdata" \
AIWORKGATE_ADMIN_TOKEN="preview-2f7c9a1b4d6e8f0a1b3c5d7e9f2a4b6c" \
AIWORKGATE_NO_BROWSER=1 ./build/AIWorkGate-preview.exe

# 预览实例客户端 Key 的正确读法（3 行卡片格式）
CLIENT_KEY=$(awk 'NR==2{print $2}' runtime/preview-key.txt)
```

> PowerShell 里不要用 `$env:...` 拼接后直接 `Start-Process` 而不验证：曾因 `$` 被 shell 吃掉导致实例落到真实 APPDATA 并抢占单实例锁。用 bash 环境变量前缀最稳。

## 6. 已知坑（避免重复踩）

- **Windows TempDir 抖动**：整包连跑偶发 `TempDir RemoveAll: directory is not empty` / timing 用例失败；用 `-run` 或重复跑隔离验证，别改代码（2026-11-07 又见一次，复跑即绿）。
- **`preview-key.txt` 是 3 行卡片格式**：真 Key 在第 2 行第 2 列，整文件 cat/读出错值 → 一律 401，别误判「Key 失效」。
- **needs_sign_in ≠ 凭据失效**：401 且毫秒级返回，先怀疑桥本地判定（loader 空 baseURL / 无自定义 fetch），用 `/accounts` 与真包 loader 实现交叉核实，别直接让用户重新登录。
- **`api_logs` 加列**：`apiLogColumns`/`NoBodies` 与所有 Scan（timeline/raw/since/conversations/me）必须同步；全量测试才会抓到。
- **周粒度**：周桶必须用 `substr(ts,1,10)` 再算周一，直接用 `date(ts,…)` 会被 `+08:00` 折算到上一周。
- **桥凭据**：桥自己的加密库优先；magpie 导入只补空缺。真实 chat 用 `/v1/chat/completions`，模型名如 `workbuddy/glm-5.3-flash`（**上游只收流式**，非流式由桥聚合）。workbuddy 的 claude-* 原生 Messages 模型 OpenAI 泳道不支持（501 明示）。
- **A 退役后的裸名**：任何“裸名转 A”的旧代码/文档都过期；裸名没有默认插件就是 404。
- **不要** `gofmt` 全仓、不要 `go mod tidy`、不要随意启停用户正在用的宿主进程（预览实例重启除外）。
- 中途不要 `git add -A`（会带上 `.qoder-credits/`）；按路径 add。

## 7. 真源与文档

- 计划台账：`plans/2026-11-05-magpie接入与公网部署计划.md`（D-01…D-60；D-60=A 退役）
- 变更日志：`docs/变更日志/变更日志-2026-11-05.md`（每批改动、验证证据、未通过项；2026-11-07 段为接手轮全记录）
- 验收：`docs/验收/`（价格与导出、真实登录、P3 退役、README 模板）
- 旧交接：`docs/handoff-20261106-aigate-swarm-rounds.md`（§12 快照 + 完成审计；蜂群段落仅历史）
- 用户偏好：中文回复；界面改动要自己逐屏验收（用内置浏览器，不弹系统浏览器）；提交推送必须静默。

## 8. 给下一个 agent 的第一句话

先跑 `git log --oneline -10` 与 `git status`，读本文 §2/§3/§4；剩余项只有 JEV key（P6/P11）与服务器（P4/P7）两类外部依赖，其余本地闭环已全部完成。
