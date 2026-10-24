# AIWorkGate

AIWorkGate 是 Windows 上的一个本地 AI 网关：把 **AI Work 助手（A）** 的内置网关与**可独立安装的插件**聚合成一个 OpenAI 兼容入口。

- 客户端只填一个地址、一把 Key：`http://127.0.0.1:7865/v1`；
- 裸模型名（如 `gpt-4o`）转发给 A；`插件id/模型名`（如 `hello/hello-chat`）转发给插件；
- 插件是独立进程，可单独安装 / 更新 / 停用 / 卸载，账号数据在更新时保留；
- 自带托盘、嵌入式 Vue 管理页、请求日志、签到调度、代理池与账号级出网；
- 账号健康与冷却：签到失败按原因记录（登录失效 / 已封禁永久，套餐限制 12 小时，设备已签到到次日 00:05，限流 / 额度不足到重置或按 30 分钟翻倍、封顶 6 小时），冷却中的账号不参与签到与网关调度，账号页显示徽标与倒计时；
- 用量页「模型」排行：按模型的请求数 / Token / 积分，附 P50、P95、TTFT 分位；
- 通知：签到失败、签到完成、账号失效、额度不足会弹出桌面系统通知；配置了地址时同时推送到通用 Webhook / Bark / Server酱 / 企业微信群机器人（地址 DPAPI 加密保存，未配置地址时仅桌面通知）；
- `GET /health` 无鉴权探活，返回 `{"status":"ok","running":true}`。
- `GET /healthz` 返回可调度账号数（`selectable/total`），无号时 503 `no_account`；
- 请求日志按时间/模型/来源/账号/Key/状态/流式筛选，可导出 CSV（UTF-8 BOM，单次 5 万行）；
- 自定义模型支持 `chat/completions`、`messages`、`responses`、`completions` 四个对话端点（流式与非流式）；
- 连通性检测：资源页模型目录、A 网关卡片、自定义模型行均可一键探测并显示毫秒延迟，历史记录保留 90 天。

宿主是单个 `aiworkgate.exe`（Go 1.26.5，CGO 关闭，SQLite 为纯 Go 实现），SPA 通过 `go:embed` 打进 exe。

## 仓库结构

```
cmd\aiworkgate\          程序入口（单实例 + 托盘消息循环）
internal\
  app\                   启动顺序、生命周期、管理 API 接线
  config\                config.json（%APPDATA%\AIWorkGate）
  secrets\               DPAPI 加密存储、客户端 Key（agk_）
  store\                 SQLite：请求日志、签到、额度、Key、代理、规则包
  logging\               API / 宿主 / 插件日志与保留清理
  plugins\               清单校验、安装/更新/卸载、进程守护、插件 HTTP 客户端
  gateway\               /v1 鉴权、模型合并、路由、并发闸、SSE 透传
  egress\                代理池、本地 HTTP CONNECT 转发器、egress.json 生成
  rules\                 共享规则包与 runtime\<id>\rules.json
  schedule\              每日签到、补签、一键全签、额度轮询
  hosts\                 A 的只读发现（数据目录/端口）与 CLI 任务
  tray\ webui\ winutil\  托盘、管理页与 Windows 工具
web\                     Vue 3 + Vite SPA（构建输出到 internal\webui\dist）
plugins\                 12 个渠道插件（每个自带 go.mod，可单独构建/安装）
examples\plugin-template\ 最小插件模板（hello，自带 go.mod，无依赖）
scripts\                 build.ps1 / dev.ps1 / pack-plugin.ps1 / test-plugins.ps1
docs\                    用户手册、插件格式规范、插件开发指南
```

## 快速开始（构建）

需要 Go 1.26.x；构建 SPA 还需要 Node.js（npm）。

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build.ps1 -Version 0.1.0
```

脚本会：`npm ci`（仅在 `web\node_modules` 缺失时）→ `npm run build` → 以
`-ldflags "-s -w -X aiworkgate/internal/app.Version=0.1.0"` 编译 → 生成可分发目录：

```
build\AIWorkGate-0.1.0\
  aiworkgate.exe
  plugins\            # 插件 zip/目录的暂存处（宿主实际从 %APPDATA%\AIWorkGate\plugins 加载）
  docs\
  README.md
  SHA256SUMS.txt
```

单独跳过前端重建（要求 `internal\webui\dist` 已存在）：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build.ps1 -SkipWeb -Version 0.1.0-dev
```

运行 `build\AIWorkGate-0.1.0\aiworkgate.exe`，浏览器会自动打开管理页。数据目录为 `%APPDATA%\AIWorkGate`；端口 7865 被占用时顺延到 7866–7875，实际端口写入 `run\gateway.port`。

### 发布包与插件

Release 里的 zip 只包含**宿主 exe + 产品文档**，不含任何插件。`plugins\` 下的 12 个渠道插件源码保留在仓库里供参考和自行构建，但它们仍在完善中，不作为本版的发布物；需要时按插件目录逐个 `go build`，再在「插件」页安装该目录或 zip。宿主本身不依赖任何插件即可启动、查看管理页和接入 A。

## 开发

```powershell
# 终端 1：SPA dev server（http://127.0.0.1:5173，/api 代理到 7865）
cd web
npm ci
npm run dev

# 终端 2：宿主
go run ./cmd/aiworkgate
```

辅助脚本：`scripts\dev.ps1`（自动在后台起 Vite，再前台跑宿主）。Vite 的 `/api` 代理写死 7865，宿主端口顺延时会指错，见脚本注释。

测试：

```powershell
go test ./...
powershell -ExecutionPolicy Bypass -File .\scripts\test-plugins.ps1   # 12 个插件 module 逐个跑
```

## 文档

- [用户手册](docs/用户手册.md)——安装、接入 A、客户端 Key、插件管理、代理、签到、日志、常见错误、卸载清理。
- [插件开发指南](docs/插件开发指南.md)——清单字段、HTTP 契约、生命周期、`egress.json` / `rules.json`、身份、验收清单、调试。
- [插件生成契约](docs/插件生成契约.md)——从源码到可安装 zip 的标准步骤、打包器、签名、安装门禁。
- [外部 Agent 接入](docs/外部Agent接入.md)——把网关当本地 OpenAI 端点用：Key、模型命名三规则、`/v1/completions` 与 `/v1/responses` 转译、curl 示例、SSE 事件、错误码。
- [管理接口](docs/管理接口.md)——每条 `/api/*` 的调用方：哪些页面在用、哪 5 条只给脚本/运维、哪些已删除，以及新增路由的规矩。
- [插件格式规范](docs/插件格式规范.md)——清单字段与打包格式的当前基线（api_level 1–2）。
- 可直接改的起点：`examples\plugin-template\`；打包契约：`docs\插件生成契约.md`。

## 说明

- 插件 `api_level` 接受 `1`–`2`：`1` 整池调度，`2` 允许宿主指定账号（`X-Aiworkgate-Account`）。
- 宿主不会读取或导入 A 的 Key（A 存在 Stronghold 里），需在设置页粘贴一次，DPAPI 加密保存。
- 账号级代理：代理账密不下发插件，插件只拿到 `http://<临时令牌>@127.0.0.1:<临时端口>`，监听器校验该令牌；失败不回退直连。

## 致谢与借鉴

本项目的部分设计与算法借鉴自开源项目 **AI Work 助手 / TraeWorkAssistant**（MIT License，Copyright © 2026 朱天伟，`https://github.com/smart-open/TraeWorkAssistant`），代码均按 Go / Vue 重写，未直接复制 Rust 源码。具体包括：

- 协议转译：`/v1/responses`、`/v1/messages` 的请求/响应/SSE 映射规则（对应其 `api_server/payload.rs`、`wb_sse.rs`、`wb_responses.rs`）；
- 网关容错：按状态码分级的重试与退避、首字超时对冲、冷却时长口径（`retry.rs`、`routes.rs`）；
- 模型目录：人工覆盖层优先、同步不覆盖人工值、倍率 0 即免费（`unified_catalog.rs`、`models_sync.rs`）；
- Key 管理：作用域（绑池/模型/账号）与日配额（`api_keys.rs`）；
- 账号健康：`SessionDead / PlanLimit / DeviceSigned` 分类冷却与设备维度签到去重（`pool.rs`、`*_checkin.rs`、`checkin_results.rs`）；
- 观测：TTFT 与延迟分位、按 Key 用量口径（`usage.rs`、`api_logger.rs`）。

## 许可证

本仓库**没有**附带 `LICENSE` 文件。在作者添加正式许可证之前，代码版权归作者所有，请勿默认按 MIT 或其他开源协议分发或商用；如需授权请与作者联系。
