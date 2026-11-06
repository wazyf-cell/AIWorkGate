# `/me` 按 Key 自助页（P14，D-59）

给拿到客户端 Key 的人一个只读页面：看自己的限额、用量和最近请求。它与管理面完全分离。

## 入口

`https://域名/me/`（Caddy 样例已放行 `/me/*`；管理页 `/` 与 `/api/*` 仍只走 SSH 隧道）。

## 登录

- 输入客户端 Key，**无注册、无密码、无 OIDC**。
- 登录成功设置 `awg_me_session`（HttpOnly、SameSite=Lax、`Path=/me`，HTTPS 下 Secure）。
- 会话 12 小时；**每次请求都会重新读取 Key 行**：
  - Key 过期 → 立即 401「Key 已过期（具体时间）」
  - Key 停用 / 删除 → 立即 401
  - 会话本身过期 → 401，需重新输入 Key

## 页面内容

- 限额卡：到期、每日配额、总请求上限、今日剩余、模型范围（`model_scope`）
- 用量：今日 / 近 7 天请求与 token
- 按模型（近 7 天）：请求数、输入/输出 token
- 最近 50 条请求：时间、模型、状态、耗时、tokens

## 安全边界（代码约束）

- `/me` 处理器只读 `client_keys` / `client_key_usage` / `api_logs` 的**元数据聚合**；不读取请求/响应正文、凭据、其它 Key 的行为。
- 任何响应都不含 `body_req/body_resp`、`hash`、token 或管理能力；测试 `TestMeOverviewScopedAndBodyFree` 锁定了这一点。
- 页面使用 `textContent` 渲染，避免 Key 名/模型名注入。
- 登录接口有 4 KB 体积上限；会话令牌为 32 字节随机数。

## 已知限制

- 模型范围目前直接展示 `model_scope` 原值；「与实际启用模型的交集」需要等 `/v1/models` 的完整启用列表在 `/me` 侧聚合（P13/P11 后再补）。
- 请求记录暂只展示当前 `api_logs.model`（D-31 的 requested/upstream/served 三栏未落库）；三栏迁移后这里直接补列。
- 登录失败没有额外的 IP 限速（可复用 P5-5 的网关限流），公网部署建议在 Caddy 加 `rate_limit`。
