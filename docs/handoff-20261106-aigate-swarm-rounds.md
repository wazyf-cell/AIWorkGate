# Handoff: aiworkgate → program5-aigate 蜂群第 4~5 轮交付与截断交接（2026-11-06）

> **写给谁**：接手本工作的下一个 agent（可能从本截断点直接继续）。假设你**什么都不知道**，从零读起。
> **第一件事**：本文所有路径以仓库 `F:\Code\DSH-code\program\program5-aigate` 为准（注意：旧接手文档里的 `program-2-gateway\aiworkgate` 是回滚点，只读；`_magpie-src` 也在旧目录）。
> **第二件事**：本文件是**蜂群第 4~5 轮的完整交接**，与 `docs/handoff-20261105-aigate-third-round.md`（第三轮，必读前置）衔接。冲突时以本文为准（本文更新）。

---

## 0 · 30 秒版

你在接手一个 Go 本地 AI 网关（aiworkgate）的 magpie 化重构。**前 5 个蜂群轮次已完成**：P1 插件宿主桥（代码收口）、P8 沙箱与供应链（代码大半）、P2 市场全链、P10 脱敏（验收 8/8）、P12/P9 主体开工。**截断点**：两条在途泳道被用户要求截断——task-29（会话真聚合：store/usage 层已完成且测试绿，**剩 webui 接线 + 前端翻默认**）、task-30（脱敏去重已落，**剩 A 兜底分支实现**使 `TestRedactABranchSinglePass` 转绿）。当前 `go build ./...` exit 0；四包定向测试中仅 gateway 1 条在途规格测试红。**你的第一项工作大概率是把这两个截断卡收尾**（§6 有精确续接步骤）。

---

## 1 · 必读契约（按序）

| 序 | 文档 | 拿什么 |
|---|---|---|
| 1 | `plans/2026-11-05-magpie接入与公网部署计划.md`（528 行） | 决策台账 D-01~D-32/D-57~D-59（不可推翻）+ D-33~D-56 已定稿 19 条 + §1.1 用户裁决记录（2026-11-06）+ 14 阶段验收清单 |
| 2 | `docs/handoff-20261105-aigate-third-round.md` | 第三轮交付、9 条硬约束（§6，仍然有效）、关键路径速查 |
| 3 | `docs/变更日志/变更日志-2026-11-05.md` | 本蜂群 4~5 轮逐轮变更记录（三轮追加段落） |
| 4 | `docs/缺口分析/06-承接核对表.md` | 112 条缺口承接状态（已勘误三处计数） |
| 5 | `docs/p8/` 4 篇 + `docs/缺口分析/05-冷却档位规格.md` | P8 取舍细节（§4.4 四条路径）、冷却 10 常量 7 类 |

---

## 2 · 当前进度总览（截断时点 2026-11-06 13:40）

### 2.1 提交链（本蜂群 4 个提交）

```
f839d01  第 1 轮：TS 桥（core/chat/admin）+ 宿主 opchost 包 + P8 资产 + changelog（62 文件 +10955）
5eab08f  第 2 轮：能力口径统一 + egress 白名单接线 + internal/market 包 + Linux uid 隔离 + 台账 D-33~56 并入（47 文件 +4326）
b7011c3  第 3 轮：P2 市场接线全链 + P10 redact 规则层移植（166 测试）+ P8 更新回退 + build-bridge 脚本（192 文件 +19026）
590849b  第 4 轮：用户拍板入账 + P10-2 gateway 接线 + 密钥往返验证 + 更新定时器 + KPI 两格（37 文件 +1756）
[截断点] 第 5 轮 WIP 未提交：见 §5（task-25/26/28 已完成未提交 + task-29/30 截断 WIP）
```

### 2.2 十四阶段状态

| 阶段 | 状态 | 说明 |
|---|---|---|
| P1 插件宿主桥 | **代码收口** | TS 桥全端点 + 宿主 opchost + 生产挂载；**剩**：真实 trae 登录验收（用户）、supervisor 真实监管联调、tsc 类型检查、bun --compile 产物入部署物（脚本已备 scripts/build-bridge.ps1，产物 exe 不入库） |
| P8 沙箱供应链 | **代码大半** | 脚本资产 + uid 隔离（opt-in，config plugins.sandbox_user）+ 出口白名单桥内强制 + 更新回退原语 + build-bridge 哈希；**剩**：config 接线已做（sandbox_user/market_mirror/market_update_*）、Linux 实跑（待服务器）、checkin/refresh 调用点补 wrap、L3 nftables、宿主 forwarder.go:333 侧接线 |
| P2 市场 | **全链打通** | 三源包 + 接线 + 路由 + app 回调 + 市场页 + 更新定时器（6h/prompt/auto 走回退）；**剩**：workbuddy 接入即用（待用户登录）、镜像 failover、签名策略产品决策、models.dev 真实客户端 |
| P10 脱敏 | **验收 8/8** | 规则层逐字节移植（166 测试）+ gateway 四路径接线 + 密钥往返 hex 实证；redact.enabled 缺省 false；**剩小项**：双 MaskJSONBody 去重收尾（见截断卡 task-30）、耗时口径已裁决 a |
| P12 模型测试 | **两块落地** | 测试端点（D-39 分档 + otherAPI + 批量并行，真实 fakeplugin e2e）+ presets 四类（11 条内嵌）+ model_prefs v12（重命名/裁剪）+ 兜底链；**剩**：/v1/models 发布面（P12-3）、重命名/裁剪 UI、models.dev 客户端 |
| P9 额度告警 | **主体开工** | expiryRank 三态 + expiring_first 策略 + 告警三分独立于调度（全部实测）+ KPI 两格 + 会话聚合 store/usage 层（截断）；**剩**：见截断卡 task-29、windows[] 数据通道打通（桥侧→宿主）、D-44 补档、告警前端订阅同步 |
| P3~P7、P11、P13、P14 | 未开工 | 顺序见主计划 §7；P4 依赖用户服务器；P11 依赖 P12 |

### 2.3 编译/测试健康度（截断时实测）

```
go build ./...                        → exit 0（截断 WIP 可编译）
go test ./internal/redact/...          → ok（166+ 用例，Keep 113 全绿）
go test ./internal/store/...           → ok（含 usage_conversations 新层）
go test ./internal/usage/...           → ok
go test ./internal/gateway/...         → FAIL 仅 1 条：TestRedactABranchSinglePass（task-30 在途规格测试，见 §5）
其余包                                  → ok（task-25/26/28 完成时各自验证过）
```

---

## 3 · 用户拍板记录（全部已写入主计划 §1.1，2026-11-06）

1. **Q1（D-20 口径修订）**：workbuddy 与 workbuddy-ai 是两平台，**额度各记各**（桥 quotagroups 默认不配组）；codebuddy+workbuddy(国内) 按 D-20 原文配共享组；P1 验收清单措辞已修订。
2. **Q2-A**：presets 保留 remote 类定义（已落地 internal/presets）。
3. **Q3-A**：custom_models 一次性迁移 + 备份（未实施，属后续）。
4. **Q4-A**：模型测试按 D-39 分档（已落地 task-25）。
5. **Q5-A**：脱敏转发链路可逆占位符为主，Shape 仅落盘侧（已落地）。
6. **Q6-A**：manual 策略并入 D-30①，D-52 保持空号。
7. **githubToken（D-17 追加裁决）**：**不轮换**，复用到 aiworkgate（仅加密存储、绝不回显/落日志）。风险已记录：该 token 曾明文存在视为可能暴露，P4 同步链路必须加密，滥用首查此 token。

---

## 4 · 用户侧待办（用户说自己稍后做，agent 可监测）

1. magpie 登录 **trae** → 解锁 P1「真实登录→chat 成功」验收（监测法：读 `%USERPROFILE%\.config\magpie\plugin-auth.json` 顶层键，截断时仍无 trae 条目；注意该文件可能被 magpie 锁定导致 grep 超时）。
2. magpie 登录 **workbuddy**（国内/国外）→ 解锁 P2「接入即用」验收（plugin-auth.json 截断时无 workbuddy 条目）。
3. githubToken：用户裁决不轮换（见 §3.7），**不要再提醒轮换**，只在 P4 实现加密存储。

---

## 5 · 截断点精确定义（两条在途泳道，用户指示截断）

### 5.1 task-29 · 会话真聚合（owner: redact-fixer，被截断于 webui 接线中途）

**已完成（在工作区，测试绿）**：
- `internal/store/usage_conversations.go`（新）：`UsageConversations(f, modelFilter, limit, beforeTS, beforeConv)` —— 单查询 GROUP BY conversation_id（无会话 ID 行按 `'req:'||req_id` 兜底成组）、keyset 游标分页（`HAVING MAX(ts)< ? OR (= 且 conv<)`，LIMIT ?+1 判 hasMore，零 COUNT/OFFSET）、跨 provider 归并（source 进 GROUP_CONCAT）、DISTINCT 列表截断防超长。
- `internal/usage/conversations.go`（新）：`ConversationItem`/`ConversationsResponse` + 不透明游标（base64(lastTS+"\x00"+convID)）。
- `internal/store/usage.go`、`internal/usage/query.go`、`internal/usage/service.go`：配套修改。
- `web/src/api.ts`/`Usage.vue`/`styles.css`：聚合视图 UI + 切换 + keyset 前端游标栈（默认视图仍是逐请求——**未翻默认**）。

**剩余（续接步骤，按序）**：
1. webui 接线：`/usage/conversations` 端点（handler 新文件 + Options 字段 + 路由 + app 适配——redact-fixer 被截断时正在做这一步，其遗言「webui 接线（Options 字段 + 路由 + handler 新文件 + app 适配）」即此；检查是否有半成品）。
2. 前端默认视图翻为「会话聚合」+ 点亮 conversation_id 过滤。
3. 全量 `go build ./...` + `go test ./internal/...` + `npm run build`（基线仅 sessions 区域变化则重录附证据）。
4. complete task-29（expected_revision=3，owner redact-fixer——可改派给新队员）。
**设计稿全文**：redact-fixer 交付过完整设计（§usage/store 层 SQL 形状、keyset 语义、兜底成组、截断策略），在其阶段回执里；本文 §5.1 已摘要点。

### 5.2 task-30 · 脱敏清理（owner: bridge-admin，被截断于 A 兜底实现前）

**已完成（在工作区）**：
- 双重 MaskJSONBody 去重：`dispatch.go` 入口存 origBody、兜底传 origBody、A 路径只脱一遍；调用点合并（dispatch.go:421 一处 + gateway.go:1201/1232）。
- `redact_scheduled_test.go`/`redact_a_fallback_test.go`（两个规格测试新增）。
- `probe_test.go` 已删除（断言依赖进程内残留指纹不可锁定；同语义由 restore_test/roundtrip_key_test/gateway e2e 覆盖）。

**剩余（续接步骤）**：
1. 实现 A 兜底分支：`forwardScheduled` 遇 `errNotScheduled`（p1 池无模型）→ 用 `origBody` 走 `forwardA`（Legacy A 转发）→ 200。规格测试 `TestRedactABranchSinglePass`（redact_a_fallback_test.go:58）当前实测 503 upstream_unreachable——实现后应转绿。
2. 全量 `go build ./...` + `go test ./internal/...` 全绿（23+ 包）。
3. complete task-30（expected_revision=2，owner bridge-admin）。
**注意**：A 兜底实现属于 gateway 既有逻辑补全（原 forwardA 通道在第三轮前就存在，是 scheduled 重构后兜底丢了），不是新功能；先读 dispatch.go 现状与 git log 确认。

---

## 6 · 蜂群编成与看板注册表

### 6.1 队员 roster（6/10 槽，均可 send_message 续接）

| 队员 | 专长 | 本会话累计产出 |
|---|---|---|
| bridge-core | TS 桥核心/生产挂载/egress 接线/config+build 脚本 | task-1/6/8/18/20 |
| bridge-chat | chat 端点/redact 移植/gateway 接线 | task-2/10/16/21（后段出现长上下文疲劳，两轮空转后换人） |
| bridge-admin | admin 端点/账户/市场 UI/KPI 两格 | task-3/7/9/17/22/30(截断) |
| host-opchost | 宿主 Go：manifest/market/模型测试/定时器 | task-4/11/12/15/23/25 |
| p8-security | P8 资产/uid 隔离/更新回退/P9-2 额度告警 | task-5/13/19/28 |
| redact-fixer | 密钥往返结案/会话聚合（截断） | task-24/29(截断) |

**经验**：长上下文队员会疲劳空转（bridge-chat 案例）——两轮无产出即换新队员（roster 槽 6/10），交接 prompt 必须自包含 + 指出已落盘资产。

### 6.2 共享卡注册表（team 板，全部状态）

- task-1~20：**全 completed**（P1 五卡、P8 三卡、P2 四卡、收尾清理）。
- task-21：completed（P10-2 接线，口径 b 已入卡）。
- task-22：completed（KPI 两格）。
- task-23：completed（更新定时器）。
- task-24：completed（密钥往返结案）。
- task-25：completed（P12-1 模型测试端点，真实 fakeplugin e2e）。
- task-26：completed（P12-2 presets/model_prefs/兜底链）。
- task-27：**pending 未认领**（P9-1 桥侧多 window 透传 + Q1-A quotagroups 默认——**注意与 §5.1 无关**，是独立卡，opchost/bridge/src/admin/ + types.ts）。
- task-28：completed（P9-2 三态+告警）。
- task-29：**in_progress 被截断**（owner redact-fixer；见 §5.1）。
- task-30：**in_progress 被截断**（owner bridge-admin；见 §5.2）。

### 6.3 the agent harness 看板（workspace `e785fffd-d5d6-4f8a-82d7-aebd6f653d1a`）

- 根卡 `50fe8ffe`（总控，todo）· P1 `8a5859c5` running · P8 `1c5371be` running · P2 `b279763e` running · P10 `64082258` running（8/8 回填）。
- 规矩：手工流卡 todo→running→done；手工收尾不走验收闸门；证据回填进 description。

---

## 7 · 环境事实与坑（三条轮次的真实教训，全部实测）

1. **传输层显示掩蔽（最重要）**：蜂群会话的 grep/read 输出会把真实密钥显示为 `"sk-ant-"+"{{SECRET_xxx}}"` 拼接形、真实邮箱显示为 {{EMAIL_xxx}}，写回时还原——**文件真实内容没有占位符**。读含密钥形常量的代码必须 pwsh hex 转储固化。已归档灵枢 `mem_1793907816669_6f6a55`。
2. **bun 实体**：`C:\Users\zyf\.cache\magpie\bun\1.4.4\magpie-bun.exe`（PATH 上 bun 是 npm 1.3.14 shim）；`bun build --compile` 产物 ~110MB；node v24.15 可直跑 .ts。
3. **宿主启动契约**：manifest.Entry 只有 {Exe,Args} 无 env 字段；宿主经环境变量 AIWORKGATE_PLUGIN_PORT/TOKEN/DATA_DIR 注入；**env 优先于 CLI**（supervisor 每次重启重新 allocatePort）；--plugin-dir 只走 CLI。
4. **magpie 插件源**：`%USERPROFILE%\.config\magpie\plugins\node_modules\@magpie-community\opencode-*-auth\index.mjs`（trae 181KB 自包含）；magpie 源码在旧目录 `_magpie-src\magpie-main`（redact/redact 源已逐字节移植，勿再拷）。
5. **平台口径**：workbuddy=国内 copilot.tencent.com、workbuddy-ai=国际 www.workbuddy.ai；qoder=qoder.com、qoder-cn=qoder.cn（该插件另导出 qwenwork）；trae=trae-cn/trae-global。一律独立账号独立额度。
6. **并发写纪律**：动 internal/config、internal/store 前先确认没有其他在途卡写同文件（本会话两次靠 blockedBy 串行化化解）；收尾前必跑 `go build ./...` exit 0。
7. **测试红线**：internal/redact 的 113 个 Keep 反例与 166 用例是 D-23 硬闸，任何引擎改动不许破坏；gateway e2e 是回程还原硬线。
8. **长输出终端**：pty 终端有粘贴回显噪声，长命令回显会重复拼接——读结果看尾部即可，别被噪声骗。
9. **webui/dist**：前端 build 产物入库（dist 资产 rename 正常）；根 dist/（bridge exe）被 .gitignore 忽略、哈希记录文件入库。

---

## 8 · 硬约束（沿用第三轮 §6 全部 9 条 + 本轮新增）

原 9 条（第三轮 handoff §6）：只改授权文件+并发写检查、记变更日志、不 gofmt 全仓/不 go mod tidy/不启停进程、不回显凭据、前端改完必 build+核 mtime、跑测试后核 mtime、编译不过先看 mtime、SQL 前后 EXPLAIN、plans 命名沿用例外规则。

本轮新增：
10. 蜂群内 store migration 串行化（一轮只允许一个队员动 store/schema）；internal/config 同理。
11. 队员两轮无产出即换人（roster 槽位管理），交接 prompt 自包含。
12. 读含密钥代码先 hex 固化（见 §7.1）。
13. build-bridge.ps1 产物 exe 不入库（.gitignore），SHA256 记录文件入库，-Verify 校验。

---

## 9 · 下一步工作建议（按序，给下一个 agent 的执行清单）

1. **收尾两条截断卡**（§5.1/§5.2，预计各 ≤1 小时）：task-30 先（A 兜底实现，全量转绿）；task-29 后（webui 接线 + 翻默认）。完成后 commit。
2. **task-27**（pending 未认领）：桥侧 quota 多 window 透传 + Q1-A quotagroups 默认——P9 数据通道的关键一块。
3. **P12-3**：/v1/models 发布面（下发 D-35 八字段 + model_prefs 重命名/裁剪消费 ApplyInfo/ApplyEntry + structured:true 标记）。
4. **P9-3**：D-44 策略补档（free/pace/cheap）+ 告警前端订阅同步（quota_expiring/quota_resetting 已进后端闭集）。
5. **P3 旧插件退役**（破坏性，单独一阶段；依赖 P1 全绿即真实 trae 登录验收）。
6. **P4 凭据同步**（依赖用户服务器；githubToken 加密复用裁决在此实现）。
7. 用户完成 magpie 登录后：补 P1/P2 的真实登录验收项。
8. 每完成一个阶段：更新对应 DSH 卡（todo→running→done）+ 变更日志 + 必要时灵枢归档。

---

## 10 · 诚实声明（本蜂群未验/不确定项）

1. 真实 trae/workbuddy 登录后的端到端 chat 未验（用户未登录；凭据文件无条目）。
2. Linux 侧全部未实跑（uid 隔离/白名单脚本/systemd），仅编译级+语法级验证；服务器未提供。
3. tsc/bun --noEmit 类型检查未跑（零依赖纪律下无 @types/node；Node24 直跑 + 全量 go test 为替代验证）。
4. task-29 webui 接线是否有半成品文件未知（截断时正在做；接手者先 grep `conversations` 于 internal/webui/ internal/app/）。
5. task-30 的 A 兜底实现状态未知（规格测试红；bridge-admin 部分消息称「A 路径只脱一遍已改」但 503 说明兜底分支本身未接）。
6. 128KB 基准在共享机高负载下波动（4.5→12ms），判定非代码回归，未复测。

---

## 11 · 接手进度更新（2026-11-06 14:51，单代理执行，**本 agent 无蜂群**）

> §6（蜂群编成/看板）对本 agent 无效：没有 team 板、roster、send_message、DSH 卡。以下为实测进度，覆盖正文中的旧状态。

### 11.1 新增提交（接在 2ecddd0 之后）

```
aa32665  task-29 收尾：/api/usage/conversations 全链接线 + 测试 + Usage 默认会话聚合 + dist
18537f9  task-30：A 兜底规格测试改为真实快照竞态夹具；红/绿反证通过
a3e0213  task-27：quotas.json windows[] 桥→宿主透传 + Q1-A 配额组修正
06c85fd  P12-3：/v1/models 下发 D-35 八字段 + model_prefs 消费 + structured 标记
478311f  P9-3：free/pace 策略 + expiring_first 管理端校验修复 + 告警前端订阅同步
62ea967  P9：账号面板多窗口额度渲染 + 视觉基线（56/56）
94a49c3  P12-3：serves() 重命名存在性校验（只查本次设置的非空名称）
8e06171  P5/D-40：/v1 来源 IP 白名单（默认仅本机、鉴权前、不信任 XFF）
bc7c3a1  P5-1：客户端 Key 到期（schema v13）+ 过期 401 明确文案 + 编辑框
```

### 11.2 正文勘误

1. §2.1「第 5 轮 WIP 未提交」不成立：截断 WIP 已包含在 2ecddd0，接手时工作区干净。
2. §5.2 task-30 的诊断不成立：不是缺 A 兜底实现。`dispatch.go` 的 `errNotScheduled → forwardA` 分支一直存在；旧测试用 `p1/m1` + 存在 `p1` 池，`resolveModel` 必然接管，永远到不了兜底。真实触发窗口是 `hasBinding` 与 `scheduleModel` 两次快照读之间绑定消失。测试已改为计数 snapshot 复现该窗口，并做过「改回 `body` 则审计行断言变红」的反证。
3. §4「trae/workbuddy 未登录」已过时：`~/.config/magpie/plugin-auth.json` 现有 `trae-cn`、`workbuddy` 条目（14:23 更新）；仍缺 `workbuddy-ai`。**凭据存在 ≠ 端到端 chat 已验收**，P1/P2 验收仍未跑。
4. §5.1 task-29 剩余工作已全部完成（后端路由/handler/app 回调 + cursor 400 映射 + 默认视图翻转）。

### 11.3 验收证据（本次实测）

- `go build ./...` exit 0；`go test ./internal/...` **23 包全绿**（含 gateway）。
- 桥 admin selftest **74/74**（新增 windows 透传、Q1-A 组断言）。
- `npm run build`：typecheck + 样式债 0 + 视觉基线 **54/54**。
- task-30 反证：把 `forwardA(origBody)` 改回 `forwardA(body)` → `redact: no values masked` 断言变红；恢复后绿。

### 11.4 仍未做 / 阻塞

1. **P3 旧插件退役**：破坏性，需用户单独批准；前置是 P1 真实 trae 登录验收（未跑）。
2. **P4 凭据同步**：依赖用户服务器。
3. **P1/P2 真实登录验收**：需要启动桥并打真实上游；本轮遵守「不启停进程」未跑。
4. **P9-3 `cheap`**：D-44 原文依赖 D-31③ 价格引擎，当前仓内无价格数据；未做假实现。
5. P12-3 的 `serves()` 存活性校验、决策模型 chat 400 互斥（依赖 P13 systemone）未做。
6. P9 面板多窗口渲染、告警前端订阅之外的展示联动未做（数据通道已通）。
7. `probeEvery=10` 为首版常量，未做分布调参。

### 11.5 本轮追加进展（P9 面板 / P12 serves / P5）

- **P9 多窗口渲染**：账号详情新增「额度窗口」，逐条显示 vendor `display` 与重置/失效时间；新增 `accounts-quota-windows` 视觉态（浅/深两基线）。
- **P12-3 serves()**：重命名只校验“本次设置的非空名称”，目录/manifest 都无模型清单时 fail-open；清空名称与改其它字段不受影响。
- **P5/D-40**：`gateway.allow_ips` 默认仅本机、鉴权前拦截、不信任 `X-Forwarded-For`、保存即时生效；设置页可编辑。
- **P5-1**：客户端 Key 可设到期日（schema v13），过期 401 带具体时间；`expires_at=0` 不受影响。
- 全量验证：`go build ./...` + `go test ./internal/...` **23 包全绿**；`npm run build` + 视觉 **56/56**。
- P5 本轮已完成：Key 到期/总请求上限/Key 级 `allowed_ips`、全局 IP 白名单、`trusted_proxies`、服务模式三态 + 属主 Key、双维限流、Linux 交叉编译 + Caddy/systemd 样例（`docs/p5/`）。仍未做：Linux 实机启动与长期验证（无服务器）、分享 Key 的“到期必填”缺分享标记。

---

## 12 · 进度快照（2026-11-06 16:34，单代理）

> 本节覆盖 §1~§11 的旧状态；§6 蜂群编成对本 agent 仍无效（无蜂群）。

### 12.1 提交链（2ecddd0 之后，截至本次快照）

```
aa32665  task-29 收尾：/api/usage/conversations 全链 + Usage 默认会话聚合
18537f9  task-30：A 兜底规格测试改为真实快照竞态 + 红/绿反证
a3e0213  task-27：quota windows[] 桥→宿主 + Q1-A 配额组
06c85fd  P12-3：/v1/models D-35 八字段 + prefs + structured
478311f  P9-3：free/pace 策略 + expiring_first 修复 + 告警订阅
62ea967  P9：账号面板多窗口额度 + 视觉基线
94a49c3  P12-3：serves() 重命名校验
8e06171  P5/D-40：/v1 IP 白名单（默认本机）
bc7c3a1  P5-1：Key 到期
c155a89  P5-4：trusted_proxies
e6c69e8  P5-1：Key 级 allowed_ips
4fe1d5e  P5/D-11：服务模式三态 + 属主 Key
a82f325  P5-1：Key 总请求上限
72f949b  P5-5：双维滚动分钟限流
67eba0a  P5-6：Linux 构建 + Caddy/systemd + 无头管理员令牌
5eb25fd  P7：部署/回滚脚本 + 演练文档
923fca3  P6-1：Gemini 入站
f7defe0  P6：SystemOne/decision 路由
e29f084  P4/P5：public_hosts 反代 Host 白名单
72fae04  P14：/me 自助页
9f85a12  P14：/me 路由分流测试
aa2452b  P11：模式路由组
e9caf3e  P11：组规则表
a546bef  P11：组权重
6941076  P13/D-31：requested/upstream/served 三栏
0dd4fe0  P13：按 served 统计
9ae8453  P5-1：分享 Key 强制到期
d81b7f3  D-31：sync the remaining api_logs scans and refresh the handoff snapshot
855dc53  store: finish the api_logs query scans for the D-31 columns
3fd247b  P13/D-31：配置价表与 cost_usd 列
efd1707  P9：cheap 池间按配置价排序
ab189af  D-31：缓存写侧/reasoning/purpose 五列
dd7a976  P13：导出粒度/拆段 + Prompt 组成分析
f8d6027  P11：组编辑器 + 意图规则框架
e1b77c8  退役内置 A 网关（D-60）
```

远端：`wazyf-cell/AIWorkGate` 的 `main` 在每批提交后按需推送；旧库备份标签 `backup/github-main-20261106` 保留。本地 `origin` 仍是旧本地检出路径，推送需显式指向 GitHub。

### 12.2 阶段状态

| 阶段 | 状态 |
|---|---|
| P1/P2/P8/P10/P12-1/2 | 代码/本地验证完成；内置 A 网关已退役（D-60），裸模型名改由 `default_plugin`；真实登录与 Linux 实跑仍缺 |
| P5 | D-40/41(部分)/P5-1 全项/P5-4/P5-5/P5-6 完成；Key 分享标记强制到期完成 |
| P6 | Gemini 入站（含工具调用双向翻译）+ SystemOne 路由完成；真实 JEV key 未验 |
| P9 | 策略/告警/面板/告警订阅 + `cheap`（配置价）完成 |
| P11 | 模式组 + 规则表（含 Token 条件）+ 权重 + 意图规则框架 + `/groups` 编辑器完成；真实 JEV 判定未验收 |
| P13 | D-31 十列 + 按 served 统计 + 导出粒度/拆段 + Prompt 六段分析完成；47 列清单已冻结（`docs/p13/01-导出列清单.md`） |
| P14 | `/me` 完成（限制卡/用量/按模型/最近记录，无正文） |
| P7 | 构建/部署/回滚脚本与演练手册完成；**服务器实跑未做** |
| P3/P4 | 未开工（P3 破坏性；P4 依赖服务器） |

### 12.3 当前验证基线

- `go build ./...` exit 0。
- `go test ./internal/...`：23 包通过；app/logging 偶发 Windows `TempDir RemoveAll: directory is not empty`（隔离重跑通过，非断言失败）。
- `npm run build`：typecheck + 样式债 0 + 视觉 **63/63**（新增 `groups` / `groups-intent`；导航加「路由组」后全量重录）。
- Linux 交叉编译：amd64/arm64 通过（ELF），部署脚本 dry-run 通过。

### 12.4 接手首要事项

1. 服务器到位后按 `docs/p7/00-部署演练.md` 实跑，重点验收 `public_hosts` / `trusted_proxies` / 回滚。
2. 真实 trae/workbuddy 登录后补 P1/P2 验收；真实 JEV key 补 P6 三例与 P11 意图规则一例。
3. 下一步：真实登录验收（临时 Key + trae 首个模型，非流式 1 次 + 流式 1 次）→ P3 退役。P13 已全部收尾（列清单冻结于 `docs/p13/01-导出列清单.md`，价格/导出验收记录在 `docs/验收/2026-11-06-价格与导出.md`）。
4. 内置 A 网关已按 D-60 删除：`config.a`、A 面板/Key、`forwardA` 与裸名兜底都不存在；`aichat` 插件保留但需自行配置上游。历史 `source="a"` 行仍可查询。
5. 不要启用蜂群：本 agent 无多子代理能力。
