# Handoff: aiworkgate → program5-aigate 第三轮交付与第四轮开工指引（2026-11-05）

> **写给谁**：新工作区的接续者（也可能是未来的我自己）。本文假设你**什么都不知道**，从零接手。
> **第一件事**：本文所有路径都以新仓库 `F:\Code\DSH-code\program\program5-aigate` 为准——**不要再进旧目录** `program-2-gateway\aiworkgate`（那是回滚点，只读）。

---

## 0 · 30 秒版

你在接手一个 Go 写的本地 AI 网关（aiworkgate），它正被重构成「**能安装/加载/更新 magpie 生态插件的公网网关**」。本轮（第三轮）完成了：安全修复（P0-2 鉴权收口）、审计修复（T3/T5/T6）、契约定稿（**D-01~D-32 + D-57~D-59**，13 个阶段）、缺口分析（**112 条**，4 个子代理产出）、承接核对（零「待定」）、仓库迁移（B 方案）。

**你的第一项工作大概率是 P1（插件宿主桥）或 P10（脱敏）——见 §5。**

---

## 1 · 必读契约（按序，全在新仓库内）

| 序 | 文档 | 你要从中拿什么 |
|---|---|---|
| 1 | [`plans/2026-11-05-magpie接入与公网部署计划.md`](../../plans/2026-11-05-magpie接入与公网部署计划.md)（461 行） | **决策台账 D-01~D-32 + D-57~D-59（不可推翻）**、事实 F-01~F-14（每条带证据）、13 个阶段的「做什么/验收」 |
| 2 | [`docs/缺口分析/06-承接核对表.md`](../缺口分析/06-承接核对表.md)（288 行） | **112 条缺口的承接状态**（46 已承接 / 41 待承接已排阶段 / 5 建议新阶段 / 20 明确不做带复核条件）；**D-33~D-56 草案（24 条待用户定稿）** |
| 3 | [`docs/缺口分析/00-总索引.md`](../缺口分析/00-总索引.md) | P0 十二条汇总、修复成本排序、aiworkgate 的 9 条优势 |
| 4 | [`plans/2026-11-05-安全加固计划.md`](../plans/2026-11-05-安全加固计划.md) | S-1~S-7 分层加固（每层带验收命令）；P7 部署的安全前置 |
| 5 | [`plans/2026-11-05-magpie功能盘点与沿用清单.md`](../plans/2026-11-05-magpie功能盘点与沿用清单.md) | 36 个功能域逐项对照（有/无 + 优先级） |
| 6 | [`plans/2026-11-05-供应商配置面板字段规格.md`](../plans/2026-11-05-供应商配置面板字段规格.md) | 用户三张截图的 17 项字段规格（P12/P5 的验收基准） |
| 7 | [`docs/缺口分析/05-冷却档位规格.md`](../缺口分析/05-冷却档位规格.md) | magpie 冷却 **10 常量 7 类原因**（P9 改造的规格） |

**纪律**：台账与用户拍板冲突时以台账为准；台账没写的按本文 §6 硬约束；你发现的**新**缺口必须走「承接核对」（§4 的铁律），不许直接开工。

---

## 2 · 项目与仓库现状

### 2.1 项目是什么
- **aiworkgate**：Go 写的本地网关，把 12 个渠道（codearts/zcode/loomy…）以受监督子进程插件接入，对客户端暴露 OpenAI 兼容 API（`/v1/chat/completions` 等）+ 管理面（`/api`，Vue3 SPA）
- 构建产物单 exe（SPA 内嵌）；SQLite 纯 Go；Windows 本机运行（`%APPDATA%\AIWorkGate\`）

### 2.2 本轮重构的方向（为什么）
用户拍板：**舍弃自有 12 个 Go 插件的实现，改用 magpie 生态的 OpenCode 插件**（npm 包 `@magpie-community/opencode-*-auth`，Node ESM 模块），aiworkgate 改为内置 Node 运行时加载它们。然后部署到租用的公网服务器 7×24 服务，本机只负责登录。

### 2.3 仓库状态（`program5-aigate`）
```
git log:  77c0b48 ← 契约入库（本轮终点）
          fe26318 ← 迁移基线
go build ./...                    → exit 0
go test ./internal/... -count=1   → 20 包全 ok
新增目录: plans/（4 份契约）、docs/缺口分析/（7 份）
.gitignore 已加例外: !plans/2026-11-05-*.md
```
- **旧目录** `program-2-gateway\aiworkgate`：保留作回滚点，**只读**
- **`_magpie-src/`（150MB，magpie 源码）未迁入**——留在旧目录；需要时从 `E:\Alltools\Magpine\Code\magpie-main.zip` 重解压
- ⚠️ **DSH 工作区绑定**：`workspace.json` 的 `b68d0e74-...` 仍指旧路径。**开工前先在新目录确认/重建工作区**，否则会话日志写错地方

---

## 3 · 本轮已完工（有实证，可直接采信）

| 项 | 证据 |
|---|---|
| ACL 修复（工作区 WRITE_OWNER） | `_acl-recovery/` 有备份与回滚脚本 |
| P0-2 鉴权收口（一次性启动 token） | `webui/session.go`、guard 全方法校验；`go test ./internal/webui/` ok |
| T3 安装只收 multipart + 新装默认停用 | `plugins/install.go`；测试 ok |
| T5 日期范围走索引 / T6 清理分批 | `store/date_range.go`；EXPLAIN 实证 SEARCH vs SCAN |
| 契约定稿 D-01~D-32 + D-57~D-59 | 主计划 §1（497→461 行版本） |
| 缺口分析 112 条 | `docs/缺口分析/01~04`（3553 行，4 子代理产出） |
| 承接核对闭合 | `06-承接核对表.md`（脚本机械复核：46+41+5+20=112） |
| 仓库迁移（方案 B） | `77c0b48`；新仓 build/test 全绿 |

---

## 4 · 铁律（第三轮的真实教训，不许再犯）

1. **分析文档产出后必须「承接核对」**：逐条 grep 主计划，缺口没进 D-xx 的当场补——**不许等用户问**。这是本轮被用户三次点名纠正的核心教训（第 1 次漏字段、第 2 次漏整域、第 3 次「分析完没承接」）。
2. **先查证再定级**：初判 P0 后必须核实（子代理 01 把「禁用模型不拦截」初判 P0，核实后降 P1——依赖隐式前提，属「没全关」非「失效」）。
3. **`Account.Weight` 是死字段**：有声明/快照/持久化但 `selectAccountLocked` 六分支无一读它。修它（D-29）前，管理页的权重设置是无效的——别被 UI 骗了。
4. **magpie 冷却是 10 常量 7 类原因**（见 05 号文档），不是「半小时」一档。改冷却前先读 05 号。
5. **`p2c`/首字节对冲 hedge/池级熔断/延迟分位** 是 aiworkgate 独有优势（magpie 零命中）——**搬 magpie 时不要丢掉这些**。
6. **magpie 3425 端口无鉴权**（无 key/错 key 均 200，实测）——绝不可映射公网。
7. **公网不发布 `/api/*`**（只放 `/v1` + `/me` + `/healthz`）。
8. **绝不回显凭据**；`githubToken` 用户已承诺轮换（验证时若仍是 `ghp_r4v1...` 就再提醒）。

---

## 5 · 下一轮做什么（按用户已定的执行顺序）

```
P1 → P8 → P2 → P10 → P12 → P9 → P3 → P4 → P5 → P6 → P11 → P13 → P14 → P7
```

### 首选起点：P1 · 插件宿主桥（主计划 §P1，地基，3~5 天当量）
**做什么**：bun 单文件编译的 `magpie-bridge.exe`，启动参数 `--plugin-dir --port --token --data-dir`；动态 import 插件 `.mjs`（trae 包自包含，只依赖 node: 内置模块，**F-01 已证实**）；对宿主暴露 `/meta /health /v1/models /v1/chat/completions`（SSE+非流式）；对管理面暴露 `/admin/signin/start`、`/admin/credentials`、`/admin/manifest`；**D-19 签到委派**（`/checkin` + `/accounts/{id}/checkin`）；**D-20 配额共享组**（workbuddy 双前缀共用额度）；**D-57 指纹透传**（identity 能力，宿主不接触明文）。
**验收清单**在主计划 P1 节（8 条，含用 trae-auth 真包跑通、真实 trae 登录需用户配合）。
**注意**：P1 全绿才准进 P3（破坏性退役）。

### 备选起点（若用户想先见效）：
- **P10 脱敏**（独立、中等、直接减少第三方外发风险）——搬 `internal/redact` 7226 行 + **必须连 30+ 条「不该脱」反例一起搬** + `Route` 感知 + SSE Writer；验收含「客户端看不到 `{{KIND_指纹}}` 占位符」
- **安全三连洞**（D-40~D-42，约 2 天）——Key 过期 / IP 白名单鉴权前执行 / CORS 拒 `*`
- **零成本速赢**：P1-7 KPI 补「平均延迟/平均首字」两格（数据已算出，`service.go:118-125` 只透出 6 个）

### P1 之前的一次性准备（用户侧/环境侧）
- [ ] DSH 工作区改绑 `program5-aigate`
- [ ] 用户：轮换 githubToken、重登三个过期渠道、租服务器+域名、确认服务器 OS（建议 Ubuntu 22.04+）
- [ ] D-33~D-56 草案 24 条定稿（不阻塞 P1，但阻塞 P5/P9/P11 对应项）

---

## 6 · 硬约束（三轮沿用 + 本轮新增）

1. 只改被授权的文件；**开工前先确认没有别的会话在写**（`Get-ChildItem -Recurse -Include *.go,*.vue | Where LastWriteTime -gt (Get-Date).AddMinutes(-10)`，连查两轮为空再动手）——本轮实测：两条并行车道导致 19:03 编译失败、19:18 迟到写入使验收过期
2. 不许 `git init`（已是 git 仓库）；改动记 `docs/变更日志`（新仓库要**新建** 2026-11-05 之后的日志）
3. 不许 `gofmt -w` 全仓 / `go mod tidy`；不启停 `aiworkgate.exe` 与 magpie 进程
4. 绝不回显凭据（token/key/cookie 一律 `<redacted>`）
5. `internal/webui/dist` 是构建产物，前端改完必须 `npm run build`；**构建后用 mtime 确认 dist 晚于最新源码**
6. 跑完测试后**核对自己改过的文件 mtime 是否晚于测试时刻**，晚于就重跑（本轮真实踩坑）
7. **编译不过先看 mtime**，不要顺手「修」别人的文件
8. SQL 改动前后跑 `EXPLAIN QUERY PLAN` 验证
9. `plans/` 旧文件被 .gitignore 忽略是**历史遗留**；11-05 契约已有例外规则，新增契约文档命名请沿用 `2026-11-05-*` 或更新例外规则

---

## 7 · 关键路径速查

| 什么 | 在哪 |
|---|---|
| 网关入站路由 | `internal/gateway/gateway.go:628-663` |
| 调度器（策略/冷却/粘性/熔断） | `internal/scheduler/scheduler.go`（1925 行；策略 :33-38、冷却 :364-365、Pick :1117、**Weight 死字段** :101/:1539-1581、expiryRank :1796） |
| 插件契约 | `internal/model/model.go:103,170`、`internal/plugins/client.go`（/meta /v1/models /accounts /checkin） |
| 管理面鉴权（本轮已修） | `internal/webui/session.go`、`webui.go:434-463`、`app.go:314/:677` |
| magpie 插件样例 | `E:\Alltools\Magpine\Code\magpie-main.zip` 或旧目录 `_magpie-src/`（trae 的 index.mjs 181KB 自包含，import 仅 node: 内置） |
| magpie 运行态（活着，别动它） | `127.0.0.1:3425`（无鉴权！）、`~/.config/magpie/*.json` |
| 活库 | `%APPDATA%\AIWorkGate\aiworkgate.db`（20 张表；quota_snapshots 有 expires_at 列但大量为 0） |
| 冷库/规则 | `C:\Users\zyf\.dsh\AGENTS.md`（用户全局规矩，优先级高于本文） |

---

## 8 · 用户侧待办（不阻塞 P1/P10/P12，阻塞 P4/P7/P14）

1. ~~githubToken 轮换~~（用户已应允，开工时验证）
2. 本机重登三个过期渠道（用户自己操作）
3. 租服务器（建议 Ubuntu 22.04+ x86_64）+ 域名 + DNS
4. **DSH 工作区改绑新目录**（或新目录开新会话自动建）
5. 分享 Key 的默认策略数值（期限天数/每日配额/模型范围）——面板做成可配即可

---

## 9 · 本轮的诚实声明（哪些没验）

1. **P0-2 修复的运行时行为未验**：启动 exe 后「浏览器自动打开可用 / 无痕不带 `?t=` 全 403 / `curl /api/status` 403」三条**没做**（硬约束禁启停进程）——**你接手后第一次构建完就该做**
2. **bun 单文件编译未实测**：`bun build --compile` 出 exe 是推断（110MB 运行时实测存在），未真编过
3. **dev 模式（`npm run dev`）拿不到 cookie**（vite 代理不带 `?t=`）——开发前端要么走 build.ps1，要么手粘 token
4. 01 号缺口有 17 条原文只给 `#NN` 无行号，需精确证据时回 01 号 §1 对照表
5. `TestSchedulerStickyAdmin` 出现过一次 FAIL 后自愈，疑 flaky——复现再查
