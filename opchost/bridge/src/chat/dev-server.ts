// chat 泳道的测试/自检服务器（非生产入口；生产入口是 bridge-core 的 src/main.ts）。
//
// 它做三件事：
//   1. 用最小 loader 加载插件 .mjs（mock-plugin 或真实 trae 包），照真包形状取
//      hooks.config 得到 provider 表 → 组 ProviderBinding；
//   2. 用 chat/credentials.ts 提供 getAuth（无凭据即 null → 触发「需登录」）；
//   3. 把 createChatHandler 挂到 POST /v1/chat/completions 上，另带 /health、/v1/models。
//
// 用法：
//   node opchost/bridge/src/chat/dev-server.ts --plugin ./mock-plugin/index.mjs \
//        --auth-provider mock --port 18080
//   node opchost/bridge/src/chat/dev-server.ts \
//        --plugin "$env:USERPROFILE/.config/magpie/plugins/node_modules/@magpie-community/opencode-trae-auth/index.mjs" \
//        --port 18081 --data-dir ./.tmp/trae-empty
//
// 真实 trae 包在没有凭据时正好验证「需登录」分支（不联网）。

import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { fileAuthStore } from "./credentials.ts";
import { createChatHandler, sendJson } from "./handler.ts";
import type { ChatDeps, PluginConfig, ProviderBinding } from "./types.ts";

interface Args {
  plugins: string[];
  port: number;
  host: string;
  token: string;
  dataDir: string;
  authProviders: string[];
}

function parseArgs(argv: string[]): Args {
  const args: Args = {
    plugins: [],
    port: 18080,
    host: "127.0.0.1",
    token: "",
    dataDir: "",
    authProviders: [],
  };
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i];
    const value = argv[i + 1];
    switch (flag) {
      case "--plugin":
        if (value) args.plugins.push(value);
        i += 1;
        break;
      case "--port":
        if (value) args.port = Number(value);
        i += 1;
        break;
      case "--host":
        if (value) args.host = value;
        i += 1;
        break;
      case "--token":
        if (value) args.token = value;
        i += 1;
        break;
      case "--data-dir":
        if (value) args.dataDir = value;
        i += 1;
        break;
      case "--auth-provider":
        if (value) args.authProviders.push(value);
        i += 1;
        break;
      default:
        break;
    }
  }
  return args;
}

type PluginFn = (ctx: { client: unknown }) => Promise<Record<string, unknown>>;

/** 加载一个 .mjs，按「函数导出即插件」的规则实例化（与 magpie 宿主同口径）。 */
async function loadPluginFile(
  file: string,
  onAuthSaved: (providerId: string, auth: Record<string, unknown>) => void,
): Promise<ProviderBinding[]> {
  const mod = (await import(pathToFileURL(resolve(file)).href)) as Record<string, unknown>;
  const client = {
    auth: {
      async set(arg: { path?: { id?: string }; body?: unknown }): Promise<{ data: true }> {
        const id = arg?.path?.id;
        const body = arg?.body;
        if (id && body && typeof body === "object") {
          onAuthSaved(id, body as Record<string, unknown>);
        }
        return { data: true };
      },
      async get(): Promise<{ data: undefined }> {
        return { data: undefined };
      },
    },
    app: { log: async (): Promise<{ data: true }> => ({ data: true }) },
  };

  const bindings: ProviderBinding[] = [];
  for (const [name, value] of Object.entries(mod)) {
    if (typeof value !== "function") continue;
    let hooks: Record<string, unknown>;
    try {
      hooks = await (value as PluginFn)({ client });
    } catch (err) {
      console.error(`[dev-server] plugin export ${name} 实例化失败：${String(err)}`);
      continue;
    }
    if (!hooks || typeof hooks !== "object") continue;
    const config: PluginConfig = { provider: {} };
    try {
      const fn = hooks.config as ((c: PluginConfig) => Promise<void> | void) | undefined;
      if (typeof fn === "function") await fn(config);
    } catch (err) {
      console.error(`[dev-server] ${name}.config() 抛错：${String(err)}`);
    }
    const providerIds = Object.keys(config.provider);
    if (providerIds.length === 0) continue;
    for (const providerId of providerIds) {
      bindings.push({
        providerId,
        label: config.provider[providerId]?.name ?? providerId,
        hooks: hooks as unknown as ProviderBinding["hooks"],
      });
      console.log(`[dev-server] 已加载 provider「${providerId}」（来自 ${name}）`);
    }
  }
  return bindings;
}

function buildDeps(
  bindings: ProviderBinding[],
  store: ReturnType<typeof fileAuthStore>,
): ChatDeps {
  const byId = new Map(bindings.map((b) => [b.providerId, b]));
  return {
    listProviderIds: () => bindings.map((b) => b.providerId),
    getProvider: (id) => byId.get(id),
    getAuth: (id) => store.get(id),
    saveAuth: (id, auth) => store.set(id, auth),
    log: (level, message, meta) => {
      const at = new Date().toISOString();
      console.log(`[chat][${level}] ${at} ${message}${meta ? " " + JSON.stringify(meta) : ""}`);
    },
  };
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2));
  if (args.plugins.length === 0) {
    console.error("用法: node dev-server.ts --plugin <插件 .mjs> [--auth-provider <id>] [--port 18080]");
    process.exit(2);
  }
  const dataDir = args.dataDir || resolve(process.cwd(), "opchost/bridge/.devdata");
  mkdirSync(dirname(dataDir), { recursive: true });
  mkdirSync(dataDir, { recursive: true });
  const store = fileAuthStore(dataDir);
  console.log(`[dev-server] DATA_DIR=${dataDir} authFile=${store.path()}`);

  const bindings: ProviderBinding[] = [];
  for (const file of args.plugins) {
    const loaded = await loadPluginFile(file, (id, auth) => {
      console.log(`[dev-server] 插件回写凭据 provider=${id}（内容不打印）`);
      store.set(id, auth);
    });
    bindings.push(...loaded);
  }
  if (bindings.length === 0) {
    console.error("[dev-server] 没有任何插件 provider 被加载");
    process.exit(3);
  }

  // 自测用：给指定 provider 塞一份假凭据（真 trae 不塞 → 保留「无凭据」语义）
  for (const id of args.authProviders) {
    store.set(id, {
      type: "oauth",
      access: "dev-access-token",
      refresh: "dev-refresh-token",
      expires: Date.now() + 3_600_000,
      accountId: "dev@example.invalid",
    });
    console.log(`[dev-server] 已为 provider「${id}」注入开发用假凭据（非真实 token）`);
  }

  const deps = buildDeps(bindings, store);
  const chat = createChatHandler({ deps });

  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    const path = (req.url ?? "/").split("?")[0];
    void (async () => {
      if (args.token) {
        const auth = req.headers.authorization ?? "";
        if (auth !== `Bearer ${args.token}`) {
          sendJson(res, 401, { error: { message: "unauthorized", type: "authentication_error" } });
          return;
        }
      }
      if (path === "/health") {
        sendJson(res, 200, { status: "ok", providers: bindings.map((b) => b.providerId) });
        return;
      }
      if (path === "/v1/models" && req.method === "GET") {
        sendJson(res, 200, { object: "list", data: [] });
        return;
      }
      if (path === "/v1/chat/completions") {
        await chat(req, res);
        return;
      }
      sendJson(res, 404, { error: { message: `no route ${req.method} ${path}`, type: "invalid_request_error" } });
    })().catch((err: unknown) => {
      if (!res.headersSent) {
        sendJson(res, 500, { error: { message: String(err), type: "api_error" } });
      } else if (!res.writableEnded) {
        res.end();
      }
    });
  });

  server.listen(args.port, args.host, () => {
    console.log(`[dev-server] listening http://${args.host}:${args.port}  POST /v1/chat/completions`);
  });
}

main().catch((err: unknown) => {
  console.error(`[dev-server] 启动失败：${String(err)}`);
  process.exit(1);
});
