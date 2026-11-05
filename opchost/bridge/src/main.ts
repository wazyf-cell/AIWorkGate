// magpie-bridge 入口（main.ts）
//
// 取值优先级（**重要，别改反**）：
//   **环境变量优先，CLI 参数只作 standalone 调试的回退。**
//   理由：宿主 supervisor 每次重启都会 allocatePort 重新分配端口，而
//   manifest.entry.args 在安装时冻结、不随重启更新。若 CLI 覆盖 env，重启后桥
//   会监听到 args 里的旧端口，宿主将永远连不上（supervisor.go:147）。
//
//   --plugin-dir 是唯一例外：**只走 CLI**（宿主把它写在 entry.args 里），
//   不读环境变量，避免两个来源互相矛盾。
//
//   CLI                     环境变量（宿主注入，优先）        说明
//   --plugin-dir <dir>      （不支持，只走 CLI）              必填；插件目录或单个 .mjs
//   --port <n>              AIWORKGATE_PLUGIN_PORT            默认 18086；0 = 系统分配
//   --token <bearer>        AIWORKGATE_PLUGIN_TOKEN           必填；空串拒绝启动
//   --data-dir <dir>        AIWORKGATE_PLUGIN_DATA_DIR        默认 <os.tmpdir()>/magpie-bridge
//   （仅环境变量）           AIWORKGATE_PLUGIN_ID             /meta、/health 的 id
//   （仅环境变量）           AIWORKGATE_RULES_FILE            规则包文件（chat 泳道消费）
//   （仅环境变量）           AIWORKGATE_EGRESS_FILE           出口配置（egress 泳道消费）
//   （仅环境变量）           AIWORKGATE_API_LEVEL             宿主声明的契约级别
//
// 运行（standalone 调试）：node src/main.ts --plugin-dir <dir> --port 18086 --token <tok> --data-dir <dir>
// 也兼容 bun：bun src/main.ts <同上>
//
// 只依赖 Node 内置模块（F-01 已证实 trae 插件同样如此）。
// 日志里 token 一律打码，凭据永不输出。

import os from "node:os";
import path from "node:path";

import { createFileCredentialStore } from "./admin/index.ts";
import { loadPluginDir, ensurePluginDir } from "./loader.ts";
import { bridgeCredentialStore, createBridgeServer } from "./server.ts";
import type { BridgeConfig, BridgeContext } from "./types.ts";
import { BRIDGE_ID, BRIDGE_VERSION, ENV_KEYS, MAX_API_LEVEL, MIN_API_LEVEL } from "./types.ts";

const DEFAULT_PORT = 18086;
const LISTEN_HOST = "127.0.0.1";

const USAGE = `magpie-bridge ${BRIDGE_VERSION} — aiworkgate 插件宿主桥（P1 骨架）

用法:
  node src/main.ts --plugin-dir <dir> [--port ${DEFAULT_PORT}] [--token <bearer>] [--data-dir <dir>]

取值优先级（别改反）:
  port/token/data-dir：**环境变量优先**（宿主每次重启重新分配端口，而
  entry.args 安装时冻结），CLI 仅在环境变量缺失时生效（standalone 调试用）。
  --plugin-dir：**只走 CLI**，宿主把它写在 entry.args 里。

参数:
  --plugin-dir <dir>   必填。插件目录（含 opencode.mjs/index.mjs）或单个 .mjs。只走 CLI。
  --token <bearer>     宿主 Bearer，空串拒绝启动。        env ${ENV_KEYS.token}（优先）
  --port <n>           默认 ${DEFAULT_PORT}；0 = 系统分配。 env ${ENV_KEYS.port}（优先）
  --data-dir <dir>     凭据目录。                          env ${ENV_KEYS.dataDir}（优先）
  -h, --help           打印本帮助。

仅环境变量（宿主 supervisor 注入）:
  ${ENV_KEYS.pluginId} / ${ENV_KEYS.rulesFile} / ${ENV_KEYS.egressFile} / ${ENV_KEYS.apiLevel}

端点:
  GET /health     开放（宿主 2s 超时、10s 探活）
  GET /meta       Bearer（id/version/api_level/account_count…，60s 超时）
  GET /v1/models  Bearer（{"object":"list","data":[…]}）

契约级别: ${MIN_API_LEVEL}..${MAX_API_LEVEL}，桥标识 ${BRIDGE_ID}。`;

interface ParsedArgs {
  values: Record<string, string>;
  help: boolean;
}

function parseArgs(argv: string[]): ParsedArgs {
  const values: Record<string, string> = {};
  let help = false;
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === "-h" || arg === "--help") {
      help = true;
      continue;
    }
    if (!arg.startsWith("--")) {
      throw new Error(`unexpected argument: ${arg}`);
    }
    const body = arg.slice(2);
    const eq = body.indexOf("=");
    if (eq !== -1) {
      values[body.slice(0, eq)] = body.slice(eq + 1);
      continue;
    }
    const next = argv[i + 1];
    if (next === undefined || next.startsWith("--")) {
      throw new Error(`missing value for --${body}`);
    }
    values[body] = next;
    i += 1;
  }
  return { values, help };
}

/**
 * 取值优先级：**环境变量优先，CLI 只在环境变量缺失时生效**。
 *
 * 为什么不能反过来：宿主 supervisor 每次重启都会重新 allocatePort
 * （supervisor.go:147），而 manifest.entry.args 在安装时冻结、不随重启更新。
 * 一旦 CLI 覆盖 env，重启后桥会监听 args 里的旧端口，宿主永远连不上。
 * CLI 的定位只有两个：standalone 手跑调试、以及 future 的可执行文件自测。
 */
function pick(cli: Record<string, string>, key: string, envKey: string): string | undefined {
  const fromEnv = process.env[envKey];
  if (typeof fromEnv === "string" && fromEnv.length > 0) return fromEnv;
  const fromCli = cli[key];
  return typeof fromCli === "string" && fromCli.length > 0 ? fromCli : undefined;
}

function parsePort(raw: string | undefined): number {
  if (raw === undefined) return DEFAULT_PORT;
  const n = Number(raw);
  if (!Number.isInteger(n) || n < 0 || n > 65535) {
    throw new Error(`--port must be an integer in 0..65535, got ${raw}`);
  }
  return n;
}

function parseApiLevel(raw: string | undefined): number | undefined {
  if (raw === undefined) return undefined;
  const n = Number(raw);
  if (!Number.isInteger(n) || n < MIN_API_LEVEL || n > MAX_API_LEVEL) {
    // 宿主说了个我们不认识的级别：记录但不阻断（加载器仍按自身能力工作）。
    return undefined;
  }
  return n;
}

function resolveConfig(argv: string[]): BridgeConfig | null {
  const { values, help } = parseArgs(argv);
  if (help) {
    console.log(USAGE);
    return null;
  }

  // --plugin-dir 是唯一只走 CLI 的项：宿主把它写进 entry.args，且它不随重启变化。
  // 刻意不读环境变量，避免出现「args 一个目录、env 另一个目录」的双源矛盾。
  const pluginDirRaw = values["plugin-dir"];
  if (!pluginDirRaw) {
    throw new Error("--plugin-dir is required (host passes it in manifest.entry.args)");
  }
  const token = pick(values, "token", ENV_KEYS.token);
  if (!token) {
    throw new Error(
      `--token is required (or set ${ENV_KEYS.token}); an empty bearer would leave the bridge open`,
    );
  }

  const dataDirRaw =
    pick(values, "data-dir", ENV_KEYS.dataDir) ?? path.join(os.tmpdir(), "magpie-bridge");
  const pluginId = process.env[ENV_KEYS.pluginId] || BRIDGE_ID;

  return {
    pluginDir: ensurePluginDir(pluginDirRaw),
    port: parsePort(pick(values, "port", ENV_KEYS.port)),
    token,
    dataDir: path.resolve(dataDirRaw),
    pluginId,
    rulesFile: process.env[ENV_KEYS.rulesFile] || undefined,
    egressFile: process.env[ENV_KEYS.egressFile] || undefined,
    hostApiLevel: parseApiLevel(process.env[ENV_KEYS.apiLevel]),
  };
}

async function main(): Promise<void> {
  let config: BridgeConfig | null;
  try {
    config = resolveConfig(process.argv.slice(2));
  } catch (err) {
    console.error(`[bridge] bad arguments: ${String(err)}`);
    console.error(USAGE);
    process.exit(2);
    return;
  }
  if (config === null) return; // --help

  const startedAt = Date.now();
  // 凭据只有一份：管理面的加密库。加载器（插件 auth.set）、chat（getAuth）、
  // /admin/credentials 三处共用它，避免明文库与密文库争抢同名 credentials.json。
  const adminStore = createFileCredentialStore(config.dataDir);
  const credentials = bridgeCredentialStore(adminStore, config.pluginId);
  const registry = await loadPluginDir({ dir: config.pluginDir, credentials });

  const ctx: BridgeContext = { config, startedAt, registry, credentials };
  // 生产挂载：/health、Bearer 闸门、/meta、/v1/models、/v1/chat/completions、
  // admin.handle()（/admin/*、/accounts、/checkin）、兜底 404 —— 全在 server.ts 里。
  const server = createBridgeServer(ctx, { adminStore });

  server.on("error", (err) => {
    console.error(`[bridge] server error: ${String(err)}`);
    process.exit(1);
  });

  server.listen(config.port, LISTEN_HOST, () => {
    const address = server.address();
    const port = typeof address === "object" && address !== null ? address.port : config.port;
    const providers = Object.values(registry.providers)
      .map((p) => `${p.id}(${Object.keys(p.models).length})`)
      .join(", ");
    console.log(
      `[bridge] ${BRIDGE_ID} v${BRIDGE_VERSION} as "${config.pluginId}" listening on http://${LISTEN_HOST}:${port}`,
    );
    console.log(`[bridge] plugin-dir: ${config.pluginDir}`);
    console.log(`[bridge] data-dir:   ${config.dataDir}`);
    console.log(`[bridge] token:      <redacted>`);
    if (config.rulesFile) console.log(`[bridge] rules-file: ${config.rulesFile}`);
    if (config.egressFile) console.log(`[bridge] egress-file: ${config.egressFile}`);
    if (config.hostApiLevel !== undefined) {
      console.log(`[bridge] host api_level: ${config.hostApiLevel}`);
    }
    console.log(
      `[bridge] providers:  ${providers || "(none)"} | plugins=${registry.plugins.length}`,
    );
    console.log(
      "[bridge] endpoints:  /health(open) /meta /v1/models /v1/chat/completions /accounts /checkin /admin/*",
    );
    for (const warning of registry.warnings) {
      console.warn(`[bridge] warning: ${warning}`);
    }
    if (registry.plugins.length === 0) {
      console.warn("[bridge] no provider loaded: check --plugin-dir and the plugin factory exports");
    }
  });

  const shutdown = (signal: string): void => {
    console.log(`[bridge] ${signal} received, shutting down`);
    server.close(() => process.exit(0));
    // 兜底：连接挂住时 3s 后强制退出。
    setTimeout(() => process.exit(0), 3000).unref();
  };
  process.on("SIGINT", () => shutdown("SIGINT"));
  process.on("SIGTERM", () => shutdown("SIGTERM"));
}

main().catch((err) => {
  const stack = err instanceof Error && typeof err.stack === "string" ? err.stack : String(err);
  console.error(`[bridge] fatal: ${stack}`);
  process.exit(1);
});
