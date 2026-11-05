# 桥 egress 泳道（P8 出口白名单强制，task-10）

桥内进程级出口白名单：插件只能访问其声明过的域名，越界拒绝 + data-dir 审计。
规则与测试矩阵的唯一正本：`docs/p8/01-出口白名单设计与测试矩阵.md`。本目录是实现，不发明语义。

## 职责

| 能力 | 说明 |
| --- | --- |
| 加载期校验 | `declare({plugin, provider, domains})`：整组语法校验（§4.1），任一项非法 → 整组拒绝（LOADFAIL），坏条目绝不进白名单 |
| 运行期判定 | `check()/assertAllowed()`：host 匹配（§4.2 精确/`*.S` 通配/大小写/尾点）、端口策略（80/443）、IP 字面量精确匹配、私网/保留地址 SSRF 拦截 |
| 传输拦截 | `wrapFetch()`：包 undici fetch，手动跟随重定向并在**每一跳**重新判定（矩阵 #17）；`wrapRequest()`：包 `node:http(s).request`，越界**同步抛** `EgressError`(403) |
| 审计 | data-dir `egress-audit.jsonl` 追加 JSON 行 `{ts,plugin,provider,host,port,action,reason,count}`；**不记** path/query/头/正文/凭据；同 key 每分钟 1 条明细，其余计数（矩阵 #22/#23）；私网拦截即告警、5 分钟 >10 次越界告警（#24） |
| strict 语义 | `strict=true`（服务器必须）：未声明 = 拒绝一切；`false`（本机默认）：未声明 = 放行 + WARN 记账（矩阵 #9/#10） |
| 启动告警 | `reportDeclarationGaps()`：插件声明了 network_domains 但 SITES 表的实际请求目标不在白名单 → 启动日志告警 |

## 文件

| 文件 | 作用 |
| --- | --- |
| `whitelist.ts` | 语法校验 + 匹配引擎 + 私网/保留地址判定（含 IPv4/IPv6 常见块） |
| `egress-error.ts` | `EgressError`（reason 枚举 + HTTP 403）+ 可读文案 |
| `audit.ts` | JSONL 审计 + 1/分钟去重 + 告警事件 |
| `guard.ts` | 策略层：声明注册 / check / assert / strict，装配两条拦截通道 |
| `wrap.ts` | 传输拦截：wrapFetch（逐跳重定向复核）/ wrapRequest（同步拒绝） |
| `config.ts` | 宿主 env 装载：`AIWORKGATE_EGRESS_FILE`（strict）、`AIWORKGATE_RULES_FILE`（容错）、`AIWORKGATE_PLUGIN_DATA_DIR`（审计路径） |
| `index.ts` | 公共出口 + `loadEgressFromEnv()` + 启动告警 |
| `selftest.ts` | 矩阵断言（见下）+ 真 fetch 集成 + 审计样例 |

## 接线（需要 Lead 协调 bridge-core，本卡未改 server.ts/loader.ts）

1. **加载期**（loader.ts）：每个插件实例化后，用插件 SITES 表的 `network_domains` 调
   `guard.declare({plugin, provider, domains})`；`ok:false` 的插件按 LOADFAIL 处理（警告并禁外联）。
2. **运行期**（chat 泳道 / 插件 fetch）：插件发起的任何上游请求换成
   `guard.wrapFetch({plugin, provider})`（chat 的 `deps.getProvider(...)` 上游调用点）；
   `node:https(s).request` 换成 `guard.wrapRequest(...)`。
3. **启动末**：`reportDeclarationGaps(declarations, 各插件 SITES 实际 host)` 打告警。
4. 宿主终态：`loadEgressFromEnv(process.env)` 一次性装配（strict 来自宿主写入的 egress.json）。

## 部署注意

- `blockPrivate`（默认 true）在生产**必须保持**：这层是 SSRF 防护（§7-④）。selftest 用
  `false` 是为了在 127.0.0.1 起真服务器验证 fetch 包装，不代表生产建议。
- `allowedPorts` 默认 `[80,443]`：端口越界属 `EGRESS_PORT_FORBIDDEN`（矩阵 #11/#12）。
- 桥内拦截（L7-2）只是三层之一：L7-1 宿主 CONNECT 监听器记账 + L3 内核 nftables 按 uid 阻断
  （见 01 号 §5）。桥内层把「愿意」变成「默认」，防不了 `node:tls`/原生 socket 直连——那由 L3 兜底。

## 自检

```powershell
node opchost/bridge/src/egress/selftest.ts
```

覆盖矩阵 #1-24 中的 12 组场景：精确/大小写/尾点归一（#1-3）、子域与通配（#4-7）、
未声明拒绝+记账（#8）、strict 空声明（#9/#10）、端口（#11/#12）、SSRF 私网/元数据（#13/#14）、
IP 字面量（#15/#16）、重定向逐跳复核（#17，真 fetch）、LOADFAIL 语法（#19/#20/#21）、
洪泛抑制与字段白名单与告警（#22/#23/#24），外加 wrapRequest 同步拒绝、启动期声明差距告警、
宿主 env 装载降级。