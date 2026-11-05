# admin/ —— magpie 桥的管理面

实现 aiworkgate 插件契约里的管理面端点与 D-19 签到委派。只写本目录；`../types.ts`
是共享契约正源，本目录只做「管理面独有类型 + 适配」。

## 挂载（属 bridge-core 的 `src/server.ts` 作用域）

**顺序很重要**：`/admin/*` 必须挂在 **Bearer 鉴权闸门之后**。宿主
`internal/plugins/client.go` 对每个插件调用都带 `Authorization: Bearer <token>`，
签到与凭据端点同样带；挂在闸门前会让本机任意进程无票触及管理面。

```ts
// server.ts（示意，仅供参考，改动归 bridge-core）
import { createAdminRouter } from "./admin/index.ts";
import { adminDepsFromBridge } from "./admin/wiring.ts";

export function createBridgeServer(ctx: BridgeContext): Server {
  const admin = createAdminRouter(
    adminDepsFromBridge(ctx, {
      // 插件模块级 _internal.SITES；拿不到就留空，manifest 会明确报空并带 warning
      sites: (providerId) => sitesOf(providerId),
    }),
  );

  const server = createServer((req, res) => {
    // ... /health（公开）...
    // ... Bearer 鉴权闸门 ...
    // ... /meta、/v1/models ...
    // ... chat handler（/v1/chat/completions）...

    // 管理面 + 签到契约端点；handle 返回 true 表示已处理
    void admin.handle(req, res).then((handled) => {
      if (handled) return;
      sendJson(res, 404, errorBody(`no route for ${req.method} ${pathname}`));
    });
  });

  // 关停时序：先停接入，再回收管理面的临时回环监听与挂起的登录会话
  server.on("close", () => admin.close());
  return server;
}
```

要点：
- `admin.handle()` 是 **async**，返回 `true|false`；未命中返回 `false`，交给后面的 404。
- `admin.close()` 会：取消所有 pending 登录会话、关闭 bridge 自持的 127.0.0.1 回调监听。
  登录会话只存在内存，进程重启即全部作废（这是刻意的：token 不落日志/不落临时文件）。
- 路径与 chat 不重叠（`/admin/*`、`/checkin`、`/accounts/{id}/checkin` vs `/v1/*`），
  但 admin 应放在 chat 之后、兜底 404 之前，避免 chat 的 fallback 先吃掉请求。

## 端点

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/admin/signin/start` | `{plugin_id, provider?, mode?: "plugin"\|"bridge"}` → 登录 URL；`bridge` 模式额外返回 `redirect_uri`（只绑 127.0.0.1） |
| GET | `/admin/signin/{sid}` | 会话状态（pending/success/failed/cancelled/timeout），**不含 token** |
| POST | `/admin/signin/{sid}/cancel` | 取消并回收回环监听 |
| GET | `/admin/credentials` | 脱敏列表（keys/present/expires_at/refreshable） |
| POST | `/admin/credentials` | `{plugin_id, account_id, credentials:{…}}` 注入，磁盘 AES-256-GCM |
| POST | `/admin/credentials/export` | **纯脱敏导出**（`export:"redacted"`），永不含明文或密文 |
| DELETE | `/admin/credentials?plugin_id=&account_id=` | 删除 |
| GET | `/admin/manifest?plugin_id=` | SITES → `network_domains`、`quota_groups`、identity 声明 |
| GET | `/admin/quotagroups` | D-20 共享组定义 |
| GET | `/admin/checkin/switches` | D-19④ 开关（继承 magpie `settings.pluginCheckins`） |
| GET | `/accounts` | 宿主契约端点：`{"accounts":[Account]}`，字段对齐 `model.go:176-194` |
| PATCH | `/accounts/{id}` | `{enabled:bool}` 局部更新，落 `<data-dir>/accounts.json` |
| DELETE | `/accounts/{id}` | 写墓碑（幂等 2xx），GET 不再列出；连同凭据一起删 |
| POST | `/accounts/{id}/checkin` | 宿主契约端点（`internal/plugins/client.go:106`），响应体即 `CheckinResult` |
| POST | `/checkin` | 桥侧聚合：enabled 记账、disabled 进 `skipped` |

所有 `/admin/**`、`/accounts*`、`/checkin` 只接受回环来源（`127.0.0.1 / ::1 / ::ffff:127.0.0.1`），其余 403。

## 账号（`/accounts`，P1-6）

宿主调度**必须**靠它枚举账号。条目来源 = provider 注册表（每个 provider 一条，id = provider id）
∪ 凭据库（登录产生的 account 级凭据，id = accountId）。两条来源按 id 去重。

- `status`：无凭据或凭据已过期 → `expired`（D-19：未登录即 expired）；否则 `ok`。
- `credential`：`{present, expires_at, refreshable}`，**永不含 token**。
- `quota`：只带确有其值的字段——目前是签到日账给出的
  `today_checked_in` / `checked_in_at`；`remaining/total/...` 属 P9 多 window 模型，未接入前省略而不伪造。
- `identity`：**整体省略**（D-57 指纹留插件侧；Go 侧是 `*Identity omitempty`，省略合法）。
- `enabled` 与墓碑落 `<data-dir>/accounts.json`，重启仍生效；DELETE 会同时删该账号凭据。

## 四条口径（裁决已定，勿改）

1. **签到结果字段**：`result` 只出 `ok|already|fail`；`skipped` 由宿主生成，桥不返回。
   `kind` 取 `normal|rate_limited|session_dead|plan_limit|device_signed|quota|banned`，
   归不出就省略，不瞎填。`day` 不在契约里（日期只出现在 `/checkin` 聚合信封的 `date`）。
2. **关闭渠道的签到**：单账号端点返回 **409 `checkin_disabled`**，不伪造 `fail` 行
   （否则 `checkin_results` 日账每天多一条假失败）；聚合 `/checkin` 进 `skipped`。
3. **凭据**：任何响应只有键名/存在性/生命周期；导出也不给密文——服务器侧注入走 POST。
   指纹（D-57）留在插件侧，管理面恒不透出。
4. **能力位口径**（task-9，与 meta.ts bridgeCapabilities 同基线）：基线
   `[upstream, accounts, checkin]`，`quota` 仅当某 provider 的插件带 `auth.usage` 时附加，
   排序输出；**不声明** `refresh` / `login_*` / `identity_request` / `identity_os_switch`——
   对应宿主契约端点未接线，声明了就是骗宿主（D-57 指纹透传接线后再声明）。

## 落盘文件（`--data-dir`）

| 文件 | 内容 |
| --- | --- |
| `credentials.json` | 凭据密文（AES-256-GCM） |
| `bridge.key` | 本地 32B 密钥，0600 |
| `checkin-ledger.json` | 签到日账（`(plugin, account, 当天)` 只记一次成功） |
| `accounts.json` | 账号账本：`enabled` / `added_at` / 墓碑（DELETE） |
| `checkins.json` | 可选：桥侧签到开关覆盖，如 `{"workbuddy": true}`，优先于 magpie |
| `quota-groups.json` | 可选：D-20 共享组覆盖（缺省用内置 workbuddy 组） |

## 自检

```bash
node src/admin/testdata/selftest.mjs            # 69 项断言 + 常驻服务供 curl
node src/admin/testdata/selftest.mjs --exit     # 只跑断言后退出
```

`testdata/mock-plugin.mjs` 是与真包同形的 mock（SITES / `auth.checkin` / `auth.methods[].authorize`），
不引入任何第三方依赖。
