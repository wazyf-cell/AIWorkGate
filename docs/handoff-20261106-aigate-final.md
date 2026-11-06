# AIWorkGate 交接（2026-11-06 最终版）

> 这是给下一个 agent 的唯一入口文档。旧文档 `handoff-20261106-aigate-swarm-rounds.md` 保留历史细节，但**以本文 + `git log` + 变更日志为准**。
> 用户已明确：本仓库按**单代理**推进，不要把旧文档 §6 的“蜂群/team 板”当成可用能力；如你确有多子代理能力，可自行使用，但不要假设看板存在。

## 1. 项目与当前状态

- **项目**：AIWorkGate，Windows 上的 Go 本地 AI 网关（`aiworkgate.exe`），Vue3 管理页经 `go:embed` 内嵌；对外 `/v1` OpenAI/Gemini/Anthropic 兼容，管理面 `/api`，自助页 `/me/`。
- **仓库**：`F:\Code\DSH-code\program\program5-aigate`，分支 `master`（推送到 GitHub `main`）。
- **当前提交**：`dcb5391`（本文所在提交的父提交；最新以 `git log` 为准）。
- **远端**：`https://github.com/wazyf-cell/AIWorkGate`，`main` 已同步。
- **工作区**：干净（仅 `.qoder-credits/` 未跟踪，忽略）。
- **预览实例**：运行在 `127.0.0.1:7865`，隔离数据目录 `runtime/preview-appdata`，管理员启动令牌见 `runtime/preview-admin-url.txt`，客户端 Key 见 `runtime/preview-key.txt`（两者均在 .gitignore 的 runtime/ 下，不提交）。

## 2. 本轮之后的重大变化（务必先读）

1. **内置 A 网关已退役（D-60）**
   - 删除：`config.a`、A 适配器/端口探测/A Key、`forwardA`、裸模型名 A 兜底、`raw=="a"`、HostStatus A 字段、概览/设置 A 面板。
   - 裸模型名现在只走 `gateway.default_plugin`；未配置 → **404 `model_not_found`**。设置页有「默认插件（裸模型名）」。
   - 历史 `source="a"` 日志行保留、可查询；不要重写历史。
2. **P3 插件退役**：11 个示例插件在 `_archive/plugins-20261106/`（git mv），`plugins/` 只剩 **aichat**（用户明确保留）。整仓备份：`F:\Code\DSH-code\program\aiworkgate-backup-20261106.zip`，SHA-256 `95429fdc2cca72c4ebec85ce4f4e230319a5e33c46401f45565021b2a1346f47`。
3. **桥可复用 magpie 登录态**：`opchost/bridge/src/magpie-auth.ts` 启动时从 `~/.config/magpie/plugin-auth.json` 导入桥尚未持有的 provider；`AIWORKGATE_MAGPIE_AUTH_IMPORT=0` 关闭，`MAGPIE_AUTH_FILE` 换路径。桥自检 **78/78**。
4. **Key 创建/编辑 bug 已修**：`createKeySpec` / `updateKey` 曾丢弃 `expires_at/allowed_ips/owner/max_requests_total/share`；已补齐并有回归测试 `TestCreateKeyPersistsLimits`、`TestUpdateKeyPersistsLimits`。
5. **无浏览器开关**：`AIWORKGATE_NO_BROWSER=1`（1/true/yes/on）禁止首次启动/托盘/二次启动自动打开系统浏览器。用户不希望弹浏览器（影响打游戏）；做 UI 检查请用 **Qoder 内置浏览器（browser-use）**，不要拉起系统 Chrome。
6. **Git 推送已静默**：仓库级 credential helper 读取 `C:\Users\zyf\.config\aiworkgate\github-token`（0600，明文 PAT）。推送不再弹账户选择；轮换 token 直接替换该文件。**不要在日志/回复里打印 token。**

## 3. 13 阶段完成状态

| 阶段 | 状态 | 证据/入口 |
|---|---|---|
| P1 桥/宿主 | **本地验收完成**：真包 `/meta` 两 provider、11 模型；无凭据 401；pluginCheckins 迁移；supervisor 崩溃重启；**真实 trae chat 非流式 200（31.27s）/ 流式 200（34.01s）** | `docs/验收/2026-11-06-真实登录验收.md`、桥自检 78/78 |
| P2 市场/凭据 | 目录、双 provider、31 模型通过；**workbuddy 真实 chat 需重新登录** | 同上 + 变更日志 |
| P3 插件退役 | **完成**（11 归档、aichat 保留） | `docs/验收/2026-11-06-P3退役.md` |
| P5 网关安全 | 本地运行态全过：过期 401、无 Key 401、伪造 XFF 不计、分享 Key 到期必填、allowed_ips、whitelist/属主 Key；Caddy/服务器项待外部 | 变更日志 P5 段 |
| P6 协议 | Gemini 入站含工具调用双向翻译；SystemOne 本地测试过；**真实 JEV key 未验** | `internal/gateway/gemini_inbound_test.go`、`docs/p6/` |
| P7 部署 | 脚本/演练手册完成；Linux amd64/arm64 ELF 构建与 deploy/rollback dry-run 通过；**服务器实跑未做** | `docs/p7/` |
| P9/P10/P11/P12 | 本地实现与测试完成；P11 真实 JEV 意图判定待 key | 各包测试、`docs/p11/` |
| P13 用量/导出 | **全部完成**：D-31 十列、粒度/拆段（model/key/account/timeseries）、`cache_read`/`cache_write`、清空事件、runway 估算、Prompt 六段、47 列清单 | `docs/p13/`、`docs/验收/2026-11-06-价格与导出.md` |
| P14 `/me` | 有效 Key 登录、过期/吊销会话即时失效、无正文，均运行态实测 | 变更日志 P14 段 |

## 4. 未完成（仅外部依赖，本地无阻塞项）

1. **workbuddy 真实 chat**：magpie 存量 `workbuddy` 凭据被桥本地判定 `needs_sign_in`（5ms 返回，非上游拒绝）；需要用户在桥管理面重新登录（`POST /admin/signin/start`）。`workbuddy-ai` 在 magpie 侧无条目。
2. **真实 JEV Key**：P6 SystemOne 三例（`noul`/`choice`/`score`）与 P11 意图规则一例（难题→强、简单→便宜）。
3. **P4/P7 服务器**：凭据同步、Caddy 反代、手机/朋友 Key、本机断电 48h 续期等，需要服务器与域名。

**接手顺序**：workbuddy 登录验收 → JEV 两处验收 → P4/P7。不要提前做 P3（已完成）。

## 5. 必须知道的命令

```bash
# 后端
go build ./...
go test -ldflags=-buildid= ./internal/...        # 24 包；-buildid= 必带

# 前端（typecheck + 样式债 + 视觉门禁 65/65）
cd web && npm run build

# 桥自检（78/78）
node opchost/bridge/src/admin/testdata/selftest.mjs --exit

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
```

> PowerShell 里不要用 `$env:...` 拼接后直接 `Start-Process` 而不验证：曾因 `$` 被 shell 吃掉导致实例落到真实 APPDATA 并抢占单实例锁。用 bash 环境变量前缀最稳。

## 6. 已知坑（避免重复踩）

- **Windows TempDir 抖动**：整包连跑偶发 `TempDir RemoveAll: directory is not empty` / timing 用例失败；用 `-run` 或重复跑隔离验证，别改代码。
- **`api_logs` 加列**：`apiLogColumns`/`NoBodies` 与所有 Scan（timeline/raw/since/conversations/me）必须同步；全量测试才会抓到。
- **周粒度**：周桶必须用 `substr(ts,1,10)` 再算周一，直接用 `date(ts,…)` 会被 `+08:00` 折算到上一周。
- **A 退役后的裸名**：任何“裸名转 A”的旧代码/文档都过期；裸名没有默认插件就是 404。
- **桥凭据**：桥自己的加密库优先；magpie 导入只补空缺。真实 chat 用 `/v1/chat/completions`，模型名如 `trae-cn/glm-5`。
- **不要** `gofmt` 全仓、不要 `go mod tidy`、不要随意启停用户正在用的宿主进程（预览实例重启除外）。
- 中途不要 `git add -A`（会带上 `.qoder-credits/`）；按路径 add。

## 7. 真源与文档

- 计划台账：`plans/2026-11-05-magpie接入与公网部署计划.md`（D-01…D-60；D-60=A 退役）
- 变更日志：`docs/变更日志/变更日志-2026-11-05.md`（每批改动、验证证据、未通过项）
- 验收：`docs/验收/`（价格与导出、真实登录、P3 退役、README 模板）
- 旧交接：`docs/handoff-20261106-aigate-swarm-rounds.md`（§12 快照 + 完成审计；蜂群段落仅历史）
- 用户偏好：中文回复；界面改动要自己逐屏验收（用内置浏览器，不弹系统浏览器）；提交推送必须静默。

## 8. 给下一个 agent 的第一句话

先跑 `git log --oneline -10` 与 `git status`，读本文 §3/§4；然后只做 §4 的下一项，完成后更新本文 §3/§4、变更日志，并按路径提交（推送已静默）。
