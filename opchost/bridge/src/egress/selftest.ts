// egress 泳道自检：覆盖 docs/p8/01 测试矩阵 ≥10 条 + 审计口径 + 启动告警 + 真 fetch 包装。
//
//   node opchost/bridge/src/egress/selftest.ts
//
// 覆盖清单（矩阵行号 ↔ 断言）：
//   §4.1 语法：#19 裸通配 LOADFAIL、#20 *.ai LOADFAIL、#21 scheme/path LOADFAIL，
//         另加：空串/带端口/标签超长/IDN/单标签/连续点/去重上限；
//   §4.2 匹配：#1 精确、#2 大小写、#3 尾点、#4 子域拒绝、#5/#6 通配一级/多级、#7 通配不含根、
//         #8 未声明拒绝+记账、#9 空声明+strict 拒绝、#10 空声明+非 strict 放行记账；
//   端口/IP：#11 8443 拒、#12 80 放行、#13 声明域解析到私网拒（SSRF）、#14 169.254.169.254 拒、
//         #15 IP 字面量精确放行、#16 通配不匹配 IP；
//   行为：#17 重定向到未声明域在第二跳被拒（真 fetch）、#22 洪泛抑制 1 条/分钟、
//         #23 审计行字段白名单、#24 5 分钟 >10 拒绝产生告警；
//   附加：wrapRequest 同步拒绝、启动期「声明了域但请求了别处」告警、宿主 env 装载。

import { createServer } from "node:http";
import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createEgressGuard, EgressError } from "./guard.ts";
import { loadEgressConfig } from "./config.ts";
import { reportDeclarationGaps } from "./index.ts";

let passed = 0;
const failures: string[] = [];

// 每个用例独立临时目录（进程内序号递增，避免同毫秒碰撞导致误删）
let dirSeq = 0;
function freshDir(): string {
  const dir = join(tmpdir(), `egress-selftest-${process.pid}-${dirSeq++}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}
// 匹配类用例统一注入「不解析」的 resolver：避免真实 DNS 延迟/挂起句柄导致进程退出崩溃
const NO_DNS = { resolve: async (): Promise<string[] | null> => null };

function check(cond: boolean, message: string): void {
  if (!cond) throw new Error(`断言失败：${message}`);
}
function checkEq(actual: unknown, expected: unknown, what: string): void {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  check(a === e, `${what} 期望 ${e}，实际 ${a}`);
}
function checkThrows(fn: () => unknown, name: string, code?: string): unknown {
  let err: unknown;
  try {
    fn();
  } catch (e) {
    err = e;
  }
  check(!!err, `${name}：应当抛错但没抛`);
  if (code && err instanceof EgressError) {
    checkEq(err.reason, code, `${name}：reason`);
  }
  return err;
}
async function checkRejects(fn: () => Promise<unknown>, name: string, code?: string): Promise<unknown> {
  let err: unknown;
  try {
    await fn();
  } catch (e) {
    err = e;
  }
  check(!!err, `${name}：应当 reject 但没 reject`);
  if (code && err instanceof EgressError) {
    checkEq(err.reason, code, `${name}：reason`);
  }
  return err;
}

const CASES: { name: string; run: () => Promise<void> }[] = [];

CASES.push({
  name: "矩阵 #19/#20/#21 + §4.1：坏声明整组 LOADFAIL",
  run: async () => {
    const g = createEgressGuard({ strict: true });
    const bad = [
      "*",
      "*.ai",
      "https://api.trae.ai/v1",
      "api.trae.ai:8443",
      "",
      "   ",
      "-bad.example.com",
      "bad-.example.com",
      "a.repeat...example.com",
      "中文.example",
      "localhost",
      "a.*.com",
      "*trae.ai",
      "203.0.113.*",
      "api.trae.ai@evil.example",
    ];
    const r = g.declare({ plugin: "bad", provider: "bad", domains: bad });
    checkEq(r.ok, false, "整组应拒绝");
    check(r.errors.length >= 15, `错误数应覆盖全部非法项，实际 ${r.errors.length}`);
    checkEq(g.declaredOf("bad").length, 0, "坏组不得注册任何条目");
  },
});

CASES.push({
  name: "矩阵 #1/#2/#3：精确匹配 + 大小写 + 尾点归一",
  run: async () => {
    const g = createEgressGuard({ strict: true, ...NO_DNS });
    g.declare({ plugin: "p1", provider: "trae-cn", domains: ["api.trae.ai"] });
    const a = await g.check({ plugin: "p1", provider: "trae-cn", host: "api.trae.ai", port: 443 });
    checkEq(a.allowed, true, "精确匹配");
    const b = await g.check({ plugin: "p1", provider: "trae-cn", host: "API.Trae.AI", port: 443 });
    checkEq(b.allowed, true, "大小写归一");
    const c = await g.check({ plugin: "p1", provider: "trae-cn", host: "api.trae.ai.", port: 443 });
    checkEq(c.allowed, true, "尾点归一");
  },
});

CASES.push({
  name: "矩阵 #4/#5/#6/#7：子域拒绝 / 通配一级 / 通配多级 / 通配不含根",
  run: async () => {
    const g = createEgressGuard({ strict: true, ...NO_DNS });
    g.declare({ plugin: "p2", provider: "trae-cn", domains: ["trae.ai", "*.trae.ai"] });
    const a = await g.check({ plugin: "p2", provider: "trae-cn", host: "evil.trae.ai" });
    checkEq(a.allowed, true, "*.trae.ai 命中一级子域");
    const b = await g.check({ plugin: "p2", provider: "trae-cn", host: "a.b.trae.ai" });
    checkEq(b.allowed, true, "*.trae.ai 命中多级子域");
    const c = await g.check({ plugin: "p2", provider: "trae-cn", host: "trae.ai" });
    checkEq(c.allowed, true, "精确项 trae.ai 命中根");
    // 只声明 trae.ai（无通配）的子域请求必须拒
    const g2 = createEgressGuard({ strict: true, ...NO_DNS });
    g2.declare({ plugin: "p3", provider: "trae-global", domains: ["trae.ai"] });
    const d = await g2.check({ plugin: "p3", provider: "trae-global", host: "evil.trae.ai" });
    checkEq(d.allowed, false, "子域不被精确项放行");
    checkEq(d.reason, "EGRESS_DOMAIN_NOT_DECLARED", "拒绝码");
    // 只声明 *.trae.ai（无精确项）的根请求必须拒
    const g3 = createEgressGuard({ strict: true, ...NO_DNS });
    g3.declare({ plugin: "p4", provider: "trae-cn", domains: ["*.trae.ai"] });
    const e = await g3.check({ plugin: "p4", provider: "trae-cn", host: "trae.ai" });
    checkEq(e.allowed, false, "通配不含根域");
  },
});

CASES.push({
  name: "矩阵 #8：未声明域名 → 拒绝 + 审计 1 条",
  run: async () => {
    const dir = freshDir();
    const g = createEgressGuard({ strict: true, auditFile: join(dir, "audit.jsonl"), ...NO_DNS });
    g.declare({ plugin: "p5", provider: "trae-cn", domains: ["api.trae.ai"] });
    const d = await g.check({ plugin: "p5", provider: "trae-cn", host: "telemetry.evil.example", port: 443 });
    checkEq(d.allowed, false, "应拒绝");
    checkEq(d.reason, "EGRESS_DOMAIN_NOT_DECLARED", "拒绝码");
    checkEq(g.audit.stats.denied, 1, "拒绝计 1");
    const lines = readFileSync(join(dir, "audit.jsonl"), "utf8").trim().split("\n").filter(Boolean);
    checkEq(lines.length, 1, "审计落盘 1 行");
    rmSync(dir, { recursive: true, force: true });
  },
});

CASES.push({
  name: "矩阵 #9/#10：空声明 strict 拒 / 非 strict 放行+记账",
  run: async () => {
    const dir = freshDir();
    const gs = createEgressGuard({ strict: true, auditFile: join(dir, "s.jsonl"), ...NO_DNS });
    const da = await gs.check({ plugin: "p6", provider: "trae-cn", host: "anywhere.example.com" });
    checkEq(da.allowed, false, "strict 空声明应拒一切");
    checkEq(da.reason, "EGRESS_NO_DECLARATION", "拒绝码");

    const go = createEgressGuard({ strict: false, auditFile: join(dir, "o.jsonl"), ...NO_DNS });
    const db = await go.check({ plugin: "p7", provider: "trae-global", host: "anywhere.example.com" });
    checkEq(db.allowed, true, "非 strict 空声明放行");
    checkEq(go.audit.stats.allowedWarn, 1, "放行也要记账（WARN）");
    const lines = readFileSync(join(dir, "o.jsonl"), "utf8").trim().split("\n").filter(Boolean);
    checkEq(lines.length, 1, "WARN 记账落盘");
    rmSync(dir, { recursive: true, force: true });
  },
});

CASES.push({
  name: "矩阵 #11/#12/#15/#16：端口越界拒 / 80 放行 / IP 精确放行 / 通配不匹配 IP",
  run: async () => {
    const g = createEgressGuard({ strict: true, ...NO_DNS });
    g.declare({
      plugin: "p8",
      provider: "trae-cn",
      domains: ["api.trae.ai", "203.0.113.10", "*.example.com"],
    });
    const a = await g.check({ plugin: "p8", provider: "trae-cn", host: "api.trae.ai", port: 8443 });
    checkEq(a.allowed, false, "8443 应拒");
    checkEq(a.reason, "EGRESS_PORT_FORBIDDEN", "拒绝码");
    const b = await g.check({ plugin: "p8", provider: "trae-cn", host: "api.trae.ai", port: 80 });
    checkEq(b.allowed, true, "80 放行");
    const c = await g.check({ plugin: "p8", provider: "trae-cn", host: "203.0.113.10", port: 443 });
    checkEq(c.allowed, true, "IP 字面量精确匹配放行");
    const d = await g.check({ plugin: "p8", provider: "trae-cn", host: "198.51.100.7", port: 443 });
    checkEq(d.allowed, false, "通配不匹配 IP");
    checkEq(d.reason, "EGRESS_DOMAIN_NOT_DECLARED", "拒绝码");
  },
});

CASES.push({
  name: "矩阵 #13/#14：声明域解析到私网拒（SSRF）/ 元数据地址拒",
  run: async () => {
    const g = createEgressGuard({
      strict: true,
      resolve: async (host) => (host === "internal.example" ? ["10.1.2.3"] : null),
    });
    g.declare({ plugin: "p9", provider: "trae-cn", domains: ["internal.example"] });
    const a = await g.check({ plugin: "p9", provider: "trae-cn", host: "internal.example" });
    checkEq(a.allowed, false, "声明域解析到私网应拒");
    checkEq(a.reason, "EGRESS_PRIVATE_ADDRESS_BLOCKED", "拒绝码");
    const b = await g.check({ plugin: "p9", provider: "trae-cn", host: "169.254.169.254", port: 80 });
    checkEq(b.allowed, false, "云元数据地址应拒");
    checkEq(b.reason, "EGRESS_PRIVATE_ADDRESS_BLOCKED", "拒绝码");
  },
});

CASES.push({
  name: "矩阵 #17：重定向到未声明域 → 第二跳被拒（真 fetch）",
  run: async () => {
    const server = createServer((req, res) => {
      if (req.url === "/ok") {
        res.writeHead(200, { "content-type": "text/plain" });
        res.end("ok");
        return;
      }
      if (req.url === "/redir") {
        res.writeHead(302, { location: "http://evil.test/elsewhere" });
        res.end();
        return;
      }
      res.writeHead(404);
      res.end();
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const addr = server.address() as { port: number };
    const port = addr.port;

    const g = createEgressGuard({
      strict: true,
      blockPrivate: false, // 测试用 127.0.0.1 直连；生产保持 true（见 README 说明）
      allowedPorts: [80, 443, port],
    });
    g.declare({ plugin: "mock", provider: "mock", domains: ["127.0.0.1"] });
    const guardedFetch = g.wrapFetch({ plugin: "mock", provider: "mock" });

    const ok = await guardedFetch(`http://127.0.0.1:${port}/ok`);
    checkEq(ok.status, 200, "第一跳命中白名单应 200");
    checkEq(await ok.text(), "ok", "响应体透传");

    await checkRejects(
      () => guardedFetch(`http://127.0.0.1:${port}/redir`),
      "重定向到未声明域应在第二跳被拒",
      "EGRESS_DOMAIN_NOT_DECLARED",
    );
    server.close();
  },
});

CASES.push({
  name: "wrapRequest：越界同步抛 EgressError，命中放行",
  run: async () => {
    const g = createEgressGuard({ strict: true, blockPrivate: false, allowedPorts: [80, 443, 18099] });
    g.declare({ plugin: "mock", provider: "mock", domains: ["127.0.0.1"] });
    let called = 0;
    const original = (..._args: unknown[]): unknown => {
      called += 1;
      return "client-request";
    };
    const guarded = g.wrapRequest({ plugin: "mock", provider: "mock", original });
    checkThrows(() => guarded("http://evil.test/x"), "未声明域名同步抛", "EGRESS_DOMAIN_NOT_DECLARED");
    checkEq(called, 0, "越界时原函数不得被调");
    const out = guarded("http://127.0.0.1:18099/path");
    checkEq(out, "client-request", "命中白名单放行并调原函数");
    checkEq(called, 1, "放行时原函数调用 1 次");
  },
});

CASES.push({
  name: "矩阵 #22/#23/#24：洪泛抑制 1 条/分钟 + 字段白名单 + >10 次告警",
  run: async () => {
    let now = 1_700_000_000_000;
    const dir = freshDir();
    const file = join(dir, "audit.jsonl");
    const g = createEgressGuard({
      strict: true,
      auditFile: file,
      now: () => now,
      ...NO_DNS,
    });
    g.declare({ plugin: "p10", provider: "trae-cn", domains: ["api.trae.ai"] });
    for (let i = 0; i < 100; i += 1) {
      now += 1;
      await g.check({ plugin: "p10", provider: "trae-cn", host: "flood.evil.example" });
    }
    checkEq(g.audit.stats.denied, 100, "拒绝计 100");
    checkEq(g.audit.stats.detailLines, 1, "明细 ≤1 条/分钟");
    checkEq(g.audit.stats.suppressed, 99, "其余只累加计数");
    const lines = readFileSync(file, "utf8").trim().split("\n").filter(Boolean);
    checkEq(lines.length, 1, "文件明细 1 行");
    const parsed = JSON.parse(lines[0]) as Record<string, unknown>;
    const allowedKeys = new Set(["ts", "plugin", "provider", "host", "port", "action", "reason", "count"]);
    for (const k of Object.keys(parsed)) {
      check(allowedKeys.has(k), `审计行不得出现字段 ${k}（#23 无 path/query/header/凭据）`);
    }
    check(Object.keys(parsed).every((k) => !/token|key|secret|access|refresh|header|query|path/i.test(k)), "审计行无任何凭据/内容字段");

    // 跨分钟窗口推进：再拒绝 12 次 → 触发 #24 告警
    for (let i = 0; i < 12; i += 1) {
      now += 60_001;
      await g.check({ plugin: "p10", provider: "trae-cn", host: "flood2.evil.example" });
    }
    check(g.audit.stats.alarms.length >= 1, `5 分钟 >10 次未声明拒绝应产生告警，实际 ${g.audit.stats.alarms.length}`);
    check(g.audit.stats.alarms[0].includes(">10"), "告警文案含阈值说明");
    rmSync(dir, { recursive: true, force: true });
  },
});

CASES.push({
  name: "启动期告警：声明了 network_domains 但实际请求了别处",
  run: async () => {
    const warnings = reportDeclarationGaps(
      [
        { plugin: "trae-cn", domains: ["api.trae.cn", "*.trae.cn"], strict: true },
        { plugin: "trae-global", domains: [], strict: true },
      ],
      {
        "trae-cn": ["api.trae.cn", "api-sg-central.trae.ai"],
        "trae-global": ["growsg-normal.trae.ai"],
      },
    );
    check(warnings.length >= 2, `应产出 ≥2 条告警，实际 ${warnings.length}`);
    check(warnings.some((w) => w.includes("api-sg-central.trae.ai")), "告警点名未覆盖的实际目标");
    check(warnings.some((w) => w.includes("trae-global")), "告警点名 strict 空声明的插件");
  },
});

CASES.push({
  name: "宿主 env 装载：egress.json strict + 坏文件降级 + rules 容错",
  run: async () => {
    const dir = freshDir();
    writeFileSync(join(dir, "egress.json"), JSON.stringify({ version: 1, strict: true, default: { mode: "blocked" } }));
    writeFileSync(join(dir, "rules.json"), JSON.stringify({ packs: ["wb"], rules: [{ from: "a", to: "b" }] }));
    const env: Record<string, string> = {
      AIWORKGATE_EGRESS_FILE: join(dir, "egress.json"),
      AIWORKGATE_RULES_FILE: join(dir, "rules.json"),
      AIWORKGATE_PLUGIN_DATA_DIR: join(dir, "data"),
    };
    const c = loadEgressConfig(env, (p) => readFileSync(p, "utf8"));
    checkEq(c.strict, true, "strict 来自 egress.json");
    check(c.auditFile.endsWith("egress-audit.jsonl"), `审计文件应在 data-dir：${c.auditFile}`);
    checkEq(c.warnings.length, 0, "好文件不应有告警");

    const c2 = loadEgressConfig(
      { AIWORKGATE_EGRESS_FILE: join(dir, "missing.json"), AIWORKGATE_RULES_FILE: join(dir, "garbage.json") },
      () => {
        throw new Error("no such file");
      },
    );
    checkEq(c2.strict, false, "缺失 egress.json → 非 strict");
    check(c2.warnings.length >= 2, "缺失文件应有告警");
    rmSync(dir, { recursive: true, force: true });
  },
});

async function main(): Promise<void> {
  for (const c of CASES) {
    try {
      await c.run();
      passed += 1;
      console.log(`PASS  ${c.name}`);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      failures.push(`${c.name}: ${msg}`);
      console.log(`FAIL  ${c.name}`);
      console.log(`      ${msg}`);
    }
  }
  console.log(`\n结果: ${passed}/${CASES.length} 通过（覆盖矩阵 #1-24 中的 ${passed} 个场景分组）`);
  if (failures.length > 0) {
    console.log("失败清单:");
    for (const f of failures) console.log(` - ${f}`);
    process.exit(1);
  }
}

main().catch((err: unknown) => {
  console.error(`selftest 崩溃: ${String(err)}`);
  process.exit(1);
});