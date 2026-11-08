# AIWorkGate 交接（2026-11-07 计划清零轮 · 给下一个 agent 的唯一入口）

> **2026-11-08 增量（以此为准，更新本文部分结论）**：
> ① **P6/P11 真实验收已完成**（真实 JEV key，TokenRhythm）：三题型 + 意图分流端到端全 PASS，过程中修复 3 个协议 bug（questions 对象形状 / intent 缺 model 字段 / parseIntentLabel 对象 answers），详见 `docs/变更日志/变更日志-2026-11-08.md` 第 20 轮与 `docs/p6/00-SystemOne路由.md` §3.1——§3.1 的「真实 JEV key 最小请求未跑」与 P11 §「未实现/已知限制 1」**均已过时**。
> ② §3.1 的「api_logs.attempt 写入侧」实际已随第 8 轮 D-32⑥ 接线（`gateway.go:1866 Attempt: rec.attempt` + `retry_test.go:307`），该条**作废**。
> ③ **预览实例隔离目录只能用 `APPDATA=<repo>\runtime\preview-appdata` 前缀启动**（§6 命令是对的）——**不存在 `AIWORKGATE_DATA_DIR` 这个 env**，设了无效且会静默落到真实用户数据目录（2026-11-08 实测踩坑）。
> ④ 第 11~20 轮增量提交（33f0e53…7ca7dec，含市场/热力图/RouteStage/六维一二期/内置表编辑/P6P11 验收）已记录在变更日志第 11~20 轮段并推送 main；custom 连接模型暂不进路由组成员源（路由组二期 task-72 修复中）。
> ⑤ 剩余外部依赖不变：P7 服务器实跑、P4 实机推送（同一台服务器）、桥 OAuth 回调（可选）。

> **2026-11-09 增量（最新，优先于上面 ①~⑤）**：
> ⑥ **插件体系已整体切换**：11 个半成品原生 Go 插件全部卸载、保留 aichat；改由 **magpie 生态真包 × 共享桥** 接入 7 渠道（trae/workbuddy/zcode/zen-free/raccoon/codearts/loomy）——预览与正式双实例均 8 插件 healthy、15 账号（含 4 个 `#` 后缀备用账号）、magpie 登录态自动导入。安装配方在 `scripts/install-bridge-plugin.ps1`（`-Target preview|formal`），备份在 `runtime/backups/`。上面 ④ 的「custom 连接模型暂不进路由组成员源」**已作废**：task-72 已让 custom 进组 universe/members，task-83 实现全 custom 组直发（调度层零改动）+ 混合组保存校验 400。
> ⑦ **桥凭据导入三形态**：OAuth（access/refresh）、`type=api`+key 公共凭据（zen-free）、`#` 后缀多账号（task-84，selftest 105/105）；边界复核 8/8 PASS + 14/14 exe/manifest 一致性。
> ⑧ **构建链**：`scripts/build-bridge.ps1 -Target <windows|linux|linux-arm64|all>`（bun 1.4.4 交叉编译 ELF，x64/arm64 已落产物 + sha256 + e_machine 字节校验）；mtime 守卫防「旧 exe 静默缺功能」（Go 宿主 exe 同坑，见变更日志第 23 轮）；**Linux ELF 未实机跑过**，首次部署需先 /health + 一条 chat。
> ⑨ **宿主→桥凭据转发已打通**：`POST /api/plugins/{id}/credentials/sync`（task-90，P4 库按渠道拆出 → 桥 /admin/credentials），P7 部署文档 §2.5 已固化布局与注入链路。
> ⑩ **正式实例端口 17865（非 7865）**，宿主有单实例 mutex——预览在跑时启正式会静默退出；验证需先停预览再恢复。
> ⑪ **本地项已清零、只差服务器**：剩余 = P7 实跑 + P4 实推（需用户 6 项：OS/架构、SSH user@host、域名+DNS、安全组、出口代理、githubToken 确认）、zcode 的 Kaspersky `*.z.ai` 扫描排除（用户本机杀软设置）、zen-free 公共额度（订阅或等恢复）。上面 ⑤ 的「桥 OAuth 回调」不再是阻塞项（存量凭据有效）。详见变更日志第 22~24 轮与 `docs/p7/00-部署演练.md` §2.5/§10。

> 本文取代 `handoff-20261106-aigate-final.md` 成为**唯一入口**（旧文保留作历史）。
> 更旧的两份（`handoff-20261105-aigate-third-round.md`、`handoff-20261106-aigate-swarm-rounds.md`）只是历史，不要按它们行动。
> 逐批改动与验证证据在 `docs/变更日志/变更日志-2026-11-05.md` 的「第 7 轮（接手轮）」与「第 8 轮（计划清零轮）」两个段落——**那是事实的原始记录，本文只做索引与导航**。

---

## 1. 项目是什么（30 秒版）

AIWorkGate 是 Windows 上的 Go 本地 AI 网关（`aiworkgate.exe`），Vue3 管理页经 `go:embed` 内嵌：

- **对客户端**：`/v1`（OpenAI 兼容 chat/completions、completions、responses、embeddings、images）、`/v1/messages`（Anthropic 兼容 + `count_tokens`）、`/v1beta/models/{m}:generateContent`（Gemini 兼容）、`/v1/systemone`（=`/v1/decision` 别名，JEV 结构化决策）、`/me/`（按 Key 自助页）
- **对管理端**：`/api/*`（会话 cookie，启动令牌换发）
- **对上游**：插件宿主桥（opchost/bridge，TypeScript on Node/bun，加载 magpie 生态 OpenCode 插件）+ 自定义连接（多协议槽）
- **调度**：internal/scheduler——池/账号/模型绑定/路由组/意图规则/六策略/冷却分级/亲和
- **存储**：SQLite（internal/store，迁移链当前 **v26**），凭据 DPAPI/AES-GCM 加密（internal/secrets）

仓库 `F:\Code\DSH-code\program\program5-aigate`，分支 `master` → 推送 GitHub `main`（`https://github.com/wazyf-cell/AIWorkGate`，当前 `94ab8e3` 同步）。

## 2. 当前状态（2026-11-07 收口时点）

- **HEAD**：`94ab8e3`（docs handoff）。工作区干净（仅 `.qoder-credits/` 未跟踪，**永远不要 `git add -A`**，按路径 add）。
- **测试**：`go test -ldflags=-buildid= ./internal/...` **24 包全绿**（`-buildid=` 必带）。app/schedule 包偶发 Windows TempDir 抖动（`TempDir RemoveAll: directory is not empty`），**不同用例随机轮换、隔离重跑必绿**——这是已知环境抖动，别改代码凑绿（交接 §6 沿用）。
- **前端**：`cd web && npm run build` = typecheck 0 错 + 样式债 0 + 视觉门禁 **65/65**。UI 改动导致像素差时用 `npm run build -- --update` 更新基线并提交 png。
- **桥自检**：`node opchost/bridge/src/admin/testdata/selftest.mjs --exit` → **84/84**（第 8 轮新增 5 个 hooks 检查）。chat 泳道：dev-server + `verify.ts --suite mock` **16/16**。
- **预览实例**：`127.0.0.1:7865` 在跑（PID 见 tasklist，含 2026-11-07 全部修复），隔离数据目录 `runtime/preview-appdata`。重启命令见 §6。
- **本轮 9 个提交**（全部已推送 GitHub main）：
  `fbcede3`（F-09/D-41/D-42+文档）→ `7043092`（限流四件套+D-21 接线）→ `9545483`（D-14 题数限制）→ `3f79885`（冷却分级/Weight/半衰期/usage_known）→ `17cfc41`（model_disabled/目录元数据/me 选择器）→ `cdbf868`（count_tokens/协议槽/失败去重/兜底链/脱敏 UI/告警规则）→ `147740c`（D-43 亲和/P4 同步器/D-58 hooks/D-30/32 用量面）→ `bdcb163`（变更日志）→ `94ab8e3`（交接）。

## 3. 四份计划的最终核销状态

四份计划都在 `plans/2026-11-05-*.md`。第 8 轮做了全量逐项审计（三类证据：代码 grep+测试实跑、文档验收记录、运行态实测），把**全部本地缺口清零**。逐项结论不再赘述——查这三处即可：

1. **变更日志第 8 轮段**：按 7 个提交批次列出每个 D/S/F 编号做了什么、回归测试名是什么。
2. **`plans/2026-11-05-magpie接入与公网部署计划.md`**：D-01…D-60 台账行内已有 2026-11-07 修订括注（codearts 退役改判、端口 7865、Q1 额度裁决）。
3. **`docs/handoff-20261106-aigate-final.md`**（历史版）：第 7 轮的桥 bug 纠偏记录——**重要结论**：workbuddy 5ms 401 的根因是桥两个 bug（空 baseURL 误判未登录 + 登录凭据落错键位），**不是** magpie 凭据失效；已修复并实测双模式真实 chat 200，workbuddy **不需要重新登录**。

### 3.1 诚实留待清单（计划内但未做，非阻塞）

| 项 | 现状 | 为什么留 |
|---|---|---|
| 模型目录 4 视图中的「供应商列表」「仅看用量」 | Pool 页已实现「仅所选」「每系最新」两视图 | 供应商列表与目录页语义重复；仅看用量需跨页 join 无数据源 |
| `api_logs.attempt` 写入侧 | 迁移 v26 已落库、5 类 Scan 全通、查询透出；**gateway dispatch 的 attempt 号透传未接，值恒 0** | gateway 包是高耦合核心，动 emit 链需要专门的回归窗口；`internal/model/model.go` 的 Attempt 字段注释标明了接线点 |
| 凭据异常时段告警（S5-4 规则 3） | 设计写进 `internal/notify/rules.go` 头注释 | 需要 P4 同步落库后的 last_used_at **历史序列**（现在只有最近一次），数据到位才能实现 |

## 4. 待用户/服务器（全部剩余工作，无一本地阻塞）

| # | 事项 | 依赖 | 到位后怎么做 |
|---|---|---|---|
| 1 | **P6 SystemOne 三例**（`noul`/`choice`/`score`） | 用户提供真实 JEV key | 配 `decision.upstreams`（base_url/api_key/model_id/protocol）→ 按 `docs/p6/00-SystemOne路由.md` 逐例发 `/v1/systemone`，对照 F-12 契约（1~16 题/带图单题已本地强校验） |
| 2 | **P11 意图规则一例**（难题→强、简单→便宜） | 同上 JEV key | `docs/p11/00-路由分组.md` 的意图规则 + JEV 判据模型，跑一例验证 |
| 3 | **P7 服务器实跑** | 租服务器 + 域名 + DNS | `docs/p7/00-部署演练.md`：先过 §8 服务器安全验收清单（UFW/SSH/fail2ban/ss -tlnp/nmap/rkhunter/AIDE/auditd/Lynis/unattended-upgrades，每条带命令）→ `scripts/build-linux.sh` → `deploy-linux.sh user@host` → §3 上线检查单（**显式确认启用项**：redact.enabled、plugins.strict_egress、rate_limit_*、plugins.sandbox_user 默认全关）→ §5 SSE 实测 |
| 4 | **P4 实机推送验收** | 服务器（可与 #3 同台） | 本地代码已全量：服务器端 `POST /api/internal/credentials`（仅回环+`X-Sync-Token`，token 存 secrets extra `credential_sync.token`）；本机 `POST /api/credentials/sync {target, dry_run?}` 走 ssh 流式（token 经 stdin 不进 argv/日志）。先 dry-run 再真推，按 `internal/app/credential_sync_test.go` 的场景核 |
| 5 | （可选）桥 OAuth 回调登录全程验 | 用户配合浏览器 | 存量凭据本来就有效（workbuddy 到 2026-12-06），只在需要新渠道时才做；流程在旧交接 §2 的「待用户」段与变更日志第 7 轮 |

## 5. 第 7+8 轮做了什么（按主题索引，细节在变更日志）

**修的 bug（接手轮，第 7 轮）**：`/v1beta` Gemini 入站未挂载到宿主路由（405）；桥把 workbuddy「空 baseURL」误判未登录（真包刻意返回空 baseURL，地址在模型/provider 配置上）且上游只收流式（非流式由桥聚合）；登录凭据落错键位（settle 现在同时写 provider 键位）；`/v1/messages` 畸形 JSON 伪装 404 → 改 400；部署脚本三处 footgun（`--binary` 缺值静默退出、裸 `--dry-run` 会真实 ssh、install 非原子）。

**计划缺口清零（第 8 轮，7 个代码提交）**：
- 安全：F-09 fail-closed（scope/quota 读错误 503）、D-41 CORS（拒 `*`/拒 `Origin: null`）、D-42 认证失败限速（同 IP 10 次/5 分钟 → 429）
- P5 限流四件套：账号级 RPM（重试计入、全暂停等 2 分钟）、排队深度上限、排队等待可配（0=一直等；迁移 v22）
- P9：D-21③ 告警运行态接线（quotaAlertLoop 每 10 分钟、按插件阈值、经 notify 通道）
- 调度：D-24/D-45 冷却七类分级+Why/By+手动解除；D-29 Weight 选取（Priority 严格先行）；D-25② 用量半衰期；D-43 Turn/Within/strict 亲和 + StickyWhy 七态 + 90s 首字截止换号；D-46 失败去重（NoPenalty）+ 策略 panic 回落
- 协议：D-14 题数边界本地拒；D-51 count_tokens（本地估算，不计费）；D-56 自定义连接三协议槽；D-18⑦ usage_known（迁移 v23）
- 目录/市场：D-33 glob 批量禁用 + 400 model_disabled（未知仍 404）；D-34/D-38 元数据列+48h 宽限（迁移 v24/v25）；D-49/D-27⑤ models.dev 客户端+快照兜底链接线；D-04 GitHub 源真下载；D-36 预设字段
- UI：P12 批量测试入口；P14-4 /me 选择器+越权 403；D-30 相对新鲜度/搜索/绿标/只选可用/两视图；D-32 热力图/思考档拆分/保留期提示/attempt 列（迁移 v26）；P10 脱敏管理面（/api/redact+热重建）
- 运营：S5-4/S7-5 告警规则引擎（auth_spike/usage_spike，可订阅默认不推）；P4 同步器本地全量；D-58 桥 /admin/hooks

## 6. 必须知道的命令

```bash
# 后端（-buildid= 必带）
go build ./...
go test -ldflags=-buildid= ./internal/...          # 24 包

# 前端
cd web && npm run build                             # typecheck+样式债+视觉 65/65
cd web && npm run build -- --update                 # 基线有意变更时更新

# 桥
node opchost/bridge/src/admin/testdata/selftest.mjs --exit   # 84/84
# chat 泳道（两步）：
node opchost/bridge/src/chat/dev-server.ts --plugin opchost/bridge/src/chat/mock-plugin/index.mjs \
  --auth-provider mock --auth-provider mock-wb --auth-provider mock-wb2 \
  --port 18080 --data-dir <临时目录>
node opchost/bridge/src/chat/verify.ts --base http://127.0.0.1:18080 --suite mock   # 16/16

# Linux 交叉编译 + 演练
VERSION=0.1.0-dev bash scripts/build-linux.sh
ARCH=arm64 VERSION=0.1.0-dev bash scripts/build-linux.sh
bash scripts/deploy-linux.sh user@host --dry-run
bash scripts/rollback-linux.sh user@host --dry-run

# 预览实例（先 taskkill 旧 PID，再 bash 前缀起，别用 PowerShell $env 拼接）
APPDATA="F:/Code/DSH-code/program/program5-aigate/runtime/preview-appdata" \
AIWORKGATE_ADMIN_TOKEN="preview-2f7c9a1b4d6e8f0a1b3c5d7e9f2a4b6c" \
AIWORKGATE_NO_BROWSER=1 ./build/AIWorkGate-preview.exe

# 预览实例客户端 Key 的正确读法（3 行卡片格式，整文件 cat 会 401）
CLIENT_KEY=$(awk 'NR==2{print $2}' runtime/preview-key.txt)

# git（静默推送，token 在 helper 里，永不打印）
git push https://github.com/wazyf-cell/AIWorkGate.git master:main
```

## 7. 已知坑（接手轮新踩过的在前）

1. **preview-key.txt 是 3 行卡片格式**：真 Key 在第 2 行第 2 列。整文件 cat/读 → 错值 → 一律 401，别误判「Key 失效」。
2. **needs_sign_in ≠ 凭据失效**：401 且毫秒级返回，先怀疑桥本地判定（loader 空 baseURL/无自定义 fetch）或凭据落错键位，用 `/accounts` 与真包 loader 实现交叉核实，**别直接让用户重新登录**（第 7 轮的教训：根因是桥 bug，存量凭据本来就有效）。
3. **TempDir 抖动**：整包连跑时 app/schedule 包偶发 `TempDir RemoveAll` 失败，**失败用例每次不同**、隔离重跑必绿；隔离绿 + 整轮复跑绿即可判定环境抖动。别改代码凑绿。
4. **api_logs 加列**：`apiLogColumns`/`apiLogColumnsNoBodies` 与**所有** Scan（api_logs/log_rows/usage/me_queries/usage_conversations/usage_clear/usage_analytics）必须同步；漏一个全量测试才抓到。当前 v26 是最后一次加列的范例（usage_known v23、attempt v26）。
5. **store 迁移版本**：加列走 `columnMigration` 惯例（先自动备份）；**新迁移从 v27 起**。
6. **workbuddy 形状**：loader 空 baseURL + 自定义 fetch = 合法，地址从模型 `api.url` → provider 级 `api` 解析；上游只收流式（非流式桥聚合）；`provider.npm=@ai-sdk/anthropic`（claude-* 原生 Messages）OpenAI 泳道不支持（501 明示）。
7. **视觉基线**：UI 变更先 `npm run build` 看哪几页 differ，确认是预期变化再 `--update`，png 随代码一起提交。
8. **A 退役后的裸名**：裸模型名没有默认插件就是 404；已知但禁用的模型是 400 `model_disabled`（D-33 口径，与 D-60 的 404 分界写在 dispatch.go 注释里）。
9. **老坑沿用**：不要 `gofmt` 全仓、不要 `go mod tidy`、不要随意启停用户宿主进程（预览实例可重启）、`api_logs` 加列见 #4、周桶用 `substr(ts,1,10)`、中途不要 `git add -A`。
10. **多代理推进**：本环境可用并行子代理（第 7/8 轮共用了 10+ 个）；注意「quota limit」偶发——失败就重试或自己内联做。并行车道开工前必须声明禁改文件清单（gateway/store/logging 这类共享核心容易撞车），收工必须 `go build ./...` + 相关包测试合并验证。

## 8. 关键代码地图（按 2026-11-07 后的最新结构）

| 模块 | 位置 | 要点 |
|---|---|---|
| 路由 | `internal/app/app.go` routeHTTP | `/health` `/healthz` `/me` → 各处理器；`isGatewayPath`（/v1 + /v1beta）→ 网关；serveGateway 外包 auth 观察器（告警用） |
| 网关 | `internal/gateway/gateway.go` | 鉴权（D-42 失败限速在 authenticate 失败路径）→ scope fail-closed（keys_scope.go）→ 限流 → 路由 |
| 调度 | `internal/scheduler/scheduler.go` + `cooldown_policy.go` + `score_factors.go` + `sticky_why.go` | 选路（六策略+Weight+半衰期）、冷却七类、亲和七态、RPM 滑窗、PickWait |
| 桥 | `opchost/bridge/src/` | `chat/upstream.ts`（空 baseURL 解析+强制流式）、`admin/signin.ts`（settle 双键位）、`hooks.ts`（/admin/hooks）、`main.ts` 入口 |
| 市场 | `internal/market/` | npm/GitHub（github.go）/本地三源；extract.go 有 traversal/symlink 防护 |
| presets | `internal/presets/` | 兜底链 fallback.go + modelsdev.go + snapshot.go（原子写） |
| 同步器 | `internal/webui/credential_sync.go` + `internal/app/credential_sync.go` | 接收端点+推送端点+dry-run |
| notify | `internal/notify/` | dispatcher（节流）+ rules.go（auth_spike/usage_spike 引擎） |
| 迁移 | `internal/store/store.go` columnMigration | 当前 v26；v22 rpm / v23 usage_known / v24 max_output / v25 removed_at+upstream_name / v26 attempt |

## 9. 真源与文档

- **变更日志**：`docs/变更日志/变更日志-2026-11-05.md`（第 7 轮=接手轮、第 8 轮=计划清零轮，逐批证据）
- **计划台账**：`plans/2026-11-05-magpie接入与公网部署计划.md`（D-01…D-60，含 2026-11-07 修订括注）
- **安全契约**：`plans/2026-11-05-安全加固计划.md`（端口已修正 7865；服务器侧清单在 docs/p7 §8）
- **验收记录**：`docs/验收/`（真实登录、价格与导出、P3 退役）
- **阶段文档**：`docs/p5/`(Caddyfile) `p6/`(SystemOne) `p7/`(部署演练+三章新手册) `p8/`(沙箱) `p9/` `p11/` `p13/` `p14/`；`docs/外部Agent接入.md`（count_tokens 口径）
- **用户偏好**：中文回复；界面改动自己逐屏验收（内置浏览器，`AIWORKGATE_NO_BROWSER=1`，不弹系统浏览器）；提交推送必须静默；多代理推进。

## 10. 给下一个 agent 的第一句话

先跑 `git log --oneline -20` 与 `git status`，读本 §2/§4/§7；四份计划的本地缺口已全部清零，**剩余工作只有 §4 的 5 项外部依赖**。拿到 JEV key 就做 P6/P11 验收；服务器到位就按 docs/p7 §8 → P7 实跑 → P4 实机推送。遇到与本文不符的现状，以变更日志第 7/8 轮段为准并更新本文。
