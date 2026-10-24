# 视觉基线 · 2026-10-23（12 个页面 + 资源详情，真实数据）

> 用途：真实账号、真实日志下的一份人工可读参照。**它不参与自动比对**——自动门禁见下节。
> 拍摄时间：2026-10-23 15:40–15:44（北京时间），宿主 `0.1.0-dev`，线上 exe sha256 前缀 `203485d0`（25,536,512 B，构建于 2026-10-22 22:43，进程启动于 2026-10-23 15:17），12/12 插件在跑。

## 自动门禁在哪（2026-10-23 晚补建）

同一天晚些时候加了真正逐像素比对的门禁，基线在 [`web/visual-baseline/`](../../../web/visual-baseline)：

| 项 | 值 |
|---|---|
| 命令 | `npm run check:visual`（`build` 里也已串上，差一像素就让构建失败）；确认是有意的改动后用 `npm run check:visual:update` 重拍 |
| 数据 | 固定 fixture，不走真实后端，所以每轮数字、时间、随机数都不变 |
| 覆盖 | **52 张** = 12 个页面 × 浅色/暗色/窄屏(1024) 共 36 张，加 5 个交互/错误态 × 两皮肤共 10 张（`accounts-drawer` 抽屉、`scheduler-explain` 判定结果、`logs-raw` 原始行、`dashboard-error`、`accounts-error`），再加 18:23 新增的 6 张（`logs-page2` 分页加载、`logs-debug` 调试正文提醒、`pool-detail` 资源详情设置，各浅/深两皮肤）。`usage-token`（Token 标签页）算在那 12 页里 |
| 交互态怎么拍 | 每页可声明 `prepare`（先点开抽屉/展开原始行/注入失败响应）与 `settled`（断言目标元素真的出现了，8 秒不出现就判失败，不会把".点开"的截图当成基线） |
| 稳定性 | 冻结 `Date`/`Math.random`/`crypto.randomUUID`，注入 CSS 关掉过渡动画，引擎锁文件防止并发抢浏览器 |

本文这条基线仍然有用：门禁用 fixture 覆盖结构，这里覆盖真实数据下的观感（长账号名、真实日志行、7 行签到结果）。比对时请以门禁为准，不拿这批图做逐像素 diff。

## 拍摄条件（复拍必须一致，否则 diff 无意义）

| 项 | 值 |
|---|---|
| 视口 | **1440 × 900**（`resize_page`），截图模式 `fullPage=true` |
| 主题 | **浅色**（`<html>` 未置 `data-theme=dark`） |
| 浏览器 | Qoder 内置 Chrome（Chrome DevTools MCP） |
| 认证 | 页内 `document.cookie='dsh_admin_session=…; path=/'` 后 reload（宿主重启会轮换，旧值失效） |
| 数据 | **真实数据**：8 个账号、12 插件、242 个模型、真实日志行 |

## 文件

| 文件 | 路由 | 页面 |
|---|---|---|
| `01-dashboard.png` | `/` | 概览 |
| `02-plugins.png` | `/plugins` | 插件 |
| `03-accounts.png` | `/accounts` | 账号 |
| `04-checkin.png` | `/checkin` | 签到 |
| `05-usage.png` | `/usage` | 用量 |
| `06-scheduler.png` | `/scheduler` | 调度 |
| `07-pools.png` | `/pools` | 资源（插件列表） |
| `08-pool-officeace.png` | `/pools/officeace` | 资源详情（含模型目录） |
| `09-rules.png` | `/rules` | 规则 |
| `10-proxies.png` | `/proxies` | 代理 |
| `11-logs.png` | `/logs` | 日志 |
| `12-settings.png` | `/settings` | 设置 |

共 12 张、2.7 MB。拍摄时的真实状态（截图里能看到、下一轮会变）：

- 签到页：**成功 1 / 已签 5 / 失败 7**，7 行账号（aichat、autoglm×2、codearts×2、officeace×2）；
- 代理页只有 1 个「系统代理」`127.0.0.1:7897`（拍摄时该端口**不可达**，属异常态）；
- 日志页是实时数据。

## 这一轮有意没做的

- 不把这批**真实数据图**接进自动比对（数据每轮都变，遮罩规则会越写越脆；结构回归交给 fixture 门禁）；
- 不收集每页的控制台 error/warn（只拍了图，控制台问题靠运行时日志看）。

上面两条之外，原先缺的暗色、窄屏、弹层、抽屉、错误态都已进了门禁；`/meta` 与历史记录两条契约也在同一批处理掉了（`/meta` 接进了插件页自述面板，`history` 删除）。
