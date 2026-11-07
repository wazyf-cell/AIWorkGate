# flaky 台账

目的：把「已知偶发失败」与其**复现条件**、**判定方法**沉淀下来，避免每次失败都当成回归去查一遍。
维护方：见每条「记录人」。新增条目按同一模板：**现象 → 位置 → 复现条件 → 判定方法 → 处置 → 状态**。

---

## 1. accounts-quota-windows overlay（visual 门）

| 项 | 内容 |
| --- | --- |
| 位置 | `web/scripts/visual/check.mjs` PAGES（`slug: 'accounts-quota-windows'`） |
| 现象 | `visual: page evaluation failed: Error: Alpha row missing`；全量视觉门退出码 2（构建失败），单跑 accounts 亦复现 |
| 根因 | `ready` 判据只看 `document.querySelectorAll('[class*="account"]').length > 0`——账号页外壳的类名在异步数据加载前就存在，导致数据未渲染就放行；随后 `prepare` 里查 `tbody tr` 找不到 fixture 的「示例账号 Alpha」行，直接抛错 |
| 复现条件 | 账号页 `/accounts` 的表格数据仍在异步加载时进入该 overlay 捕获（渲染慢/机器负载高时更易触发）；连跑 3 次 2 败 1 过（修复前） |
| 判定方法 | `node scripts/visual/check.mjs --only accounts` 连跑 3 次应全绿；若有 `Alpha row missing` 即为回归（说明 ready 又被放宽或行渲染结构变了） |
| 处置（2026-11-08，task-39） | `ready` 改为 `hasAccountRow()`：必须在表格里出现「示例账号 Alpha」行才放行（helper 已加在 `check.mjs` PAGES 之前）。`prepare` 里的行查找保留为兜底断言 |
| 状态 | **已修**（本轮）。后续若改 Accounts.vue 表格结构，需同步确认该判据仍能命中目标行 |
| 记录人 | bridge-admin（task-33 上报复现，task-39 修复） |

---

## 2. TestScheduledAttemptRecordedInLog（gateway）

| 项 | 内容 |
| --- | --- |
| 位置 | `internal/gateway/retry_test.go:312` |
| 现象 | task-34 报告：共享机偶发失败（本机未复现） |
| 测试语义 | 调度重试：acc1 返回 500 → 切 acc2 返回 200；断言上游恰好 2 次调用、日志恰好 1 行且 `Attempt == 2` |
| 复现条件 | 共享机负载高 / 与其它测试同时抢 CPU、调度或等待计数抖动时，可能出现调用次数 ≠2、日志行数 ≠1（尚未根因；本机 20/20 未复现，疑似调度/计时或包级共享状态的敏感度） |
| 判定方法 | 再次失败时先复跑 `go test ./internal/gateway/ -run '^TestScheduledAttemptRecordedInLog$' -count=10`（必要时 `-race`）排除机器抖动；若**稳定**失败 → 视为真实回归（检查调度 attempt 记录与账号切换逻辑） |
| 处置建议 | 共享机 CI 上降低对机器抖动的敏感度（如把调用次数断言放宽为 `{2,3}` 且仍要求 `Attempt == 2`），不改语义；修前不得凭偶发失败判定回归 |
| 状态 | **已知 flaky，未根因**。本机实测：`-count=20` ok、`-race` ok（task-39 记录） |
| 记录人 | bridge-admin（task-39 复测记录，来源 task-34 报告） |

---

## 3. TestSummaryCacheHitRate（usage）

| 项 | 内容 |
| --- | --- |
| 位置 | `internal/usage/service_test.go:657` |
| 现象 | task-34 报告：共享机偶发失败（本机未复现） |
| 测试语义 | SQLite 聚合的精确浮点断言：`300 / 1500 = 0.2`（容差 `1e-9`）；并断言「无 cached 行 → `cache_hit_rate` 缺省为 nil」「空范围 JSON 不泄漏」 |
| 复现条件 | 共享机负载 / 存储或计时抖动使聚合或缺省语义判断抖动（尚未根因；本机 20/20 未复现） |
| 判定方法 | 复跑 `go test ./internal/usage/ -run '^TestSummaryCacheHitRate$' -count=10`（必要时 `-race`）；稳定失败 → 真实回归（聚合、缺省或 JSON 泄漏语义） |
| 处置建议 | 若共享机持续偶发，把容差从 `1e-9` 放宽到 `1e-6`（不改变语义）；并把该测试与 CPU/IO 敏感测试在 CI 上隔离 |
| 状态 | **已知 flaky，未根因**。本机实测：`-count=20` ok、`-race -count=10` ok（task-39 记录） |
| 记录人 | bridge-admin（task-39 复测记录，来源 task-34 报告） |

---

## 判定原则（通用）

1. 先复跑（`-count=10`，必要时 `-race`）排除机器抖动；只有**稳定失败**才算真实回归。
2. flaky 与回归二分后，flaky 的处置不改变业务语义（放宽容差/解耦计时/复跑隔离）。
3. 新增 flaky 必须先确认「本机是否复现」再登记；复现数据写进「状态」行。

---

## 4.（环境级）visual 门 CDP 端口暴露失败 —— 非代码 flaky

| 项 | 内容 |
| --- | --- |
| 位置 | `web/scripts/visual/cdp.mjs` launchBrowser → `Page.getTargets` 轮询 |
| 现象 | `visual: browser never exposed a CDP page target on port <随机>`；全量视觉门退出码 2，且**无页面级 diff 输出**（与代码回归不同） |
| 复现条件 | 同一共享机上连续跑多次视觉门后（本轮观察到：三连跑出现 1 次失败）；机器上累积多个本工具启动的 Edge 实例（每次 check 可能留下 renderer 进程），启动竞态变差时发生 |
| 判定方法 | 与「代码回归」区分：出现该报错时没有 `differ`/`size changed` 行，重跑即过；若重跑**仍**失败且伴随页面 diff → 转代码回归排查 |
| 处置 | 不在本轮改代码（涉及浏览器启动策略，非本卡范围）；缓解：连续失败时间隔重试（本轮 6 次内 3 连绿达标） |
| 状态 | **已知环境级 flaky，未修**（task-39 记录；建议后续视觉门加启动重试或清理残留实例） |
| 记录人 | bridge-admin（task-39） |

---

## 台账历史

- 2026-11-08 · 初版（bridge-admin, task-39）：登记 4 条（① accounts-quota-windows overlay 已修；②③ 两 Go 测试未根因、本机未复现；④ 环境级 CDP 抖动未修）。
