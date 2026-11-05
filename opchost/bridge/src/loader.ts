// 插件加载器（loader.ts）
//
// 职责：
//   1. 扫描 --plugin-dir 下的 .mjs/.js，动态 import（node:url 的 file:// URL）。
//   2. 识别形如 `TraeCNAuthPlugin = makePlugin(SITES[...])` 的工厂导出——
//      即「async ({ client }) => ({ provider, auth, config, … })」形状的函数。
//   3. 实例化后经 config 钩子 + provider.models 构建静态模型表（无凭据回落到
//      site.models，trae 包 L3606-3615 语义）。
//   4. 提供最小 CredentialStore：凭据落 <data-dir>/credentials.json（明文，
//      加密与吊销是 P4 的职责，本卡只占位）。
//
// 兼容性证据（trae 真包，F-01）：
//   - 模块只 import node:crypto/node:fs/promises/node:http/node:os/node:path；
//   - 宿主 client 只用到 client?.auth?.set?.({path:{id},body})（L3132，可选链）；
//   - 构造期不发起任何网络或 IO。

import { readFileSync, statSync } from "node:fs";
import { readdir, readFile, writeFile, rename, mkdir } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

import type {
  CredentialRecord,
  CredentialStore,
  HostClient,
  JsonValue,
  LoadResult,
  LoadedPlugin,
  MutableProviderConfig,
  OpenCodePlugin,
  PluginModelEntry,
  PluginModelMap,
  ProviderRecord,
  ProviderConfigEntry,
} from "./types.ts";

// ---------------------------------------------------------------------------
// CredentialStore（占位实现，P4 前为明文 JSON，权限 0600）
// ---------------------------------------------------------------------------

const CREDENTIALS_FILE = "credentials.json";

interface CredentialFileShape {
  version: number;
  records: Record<string, CredentialRecord>;
}

function sanitizeRecord(provider: string, body: JsonValue): CredentialRecord {
  const b =
    body && typeof body === "object" && !Array.isArray(body)
      ? (body as Record<string, JsonValue>)
      : {};
  const str = (v: JsonValue | undefined): string | undefined =>
    typeof v === "string" && v.length > 0 ? v : undefined;
  const num = (v: JsonValue | undefined): number | undefined =>
    typeof v === "number" && Number.isFinite(v) ? v : undefined;
  return {
    provider,
    type: str(b.type),
    access: str(b.access),
    refresh: str(b.refresh),
    expires: num(b.expires),
    label: str(b.label) ?? str(b.name),
    updated_at: Date.now(),
  };
}

export function createCredentialStore(dataDir: string): CredentialStore {
  const file = path.join(dataDir, CREDENTIALS_FILE);

  const read = (): Record<string, CredentialRecord> => {
    try {
      const raw = JSON.parse(readFileSync(file, "utf8")) as CredentialFileShape;
      if (raw && typeof raw === "object" && raw.records && typeof raw.records === "object") {
        return raw.records as Record<string, CredentialRecord>;
      }
    } catch {
      // 文件缺失或损坏：按空库处理，不抛（不因凭据文件异常拖垮启动）。
    }
    return {};
  };

  let records = read();

  const persist = async (): Promise<void> => {
    const data: CredentialFileShape = { version: 1, records };
    try {
      await mkdir(dataDir, { recursive: true, mode: 0o700 });
      const tmp = file + ".tmp";
      await writeFile(tmp, JSON.stringify(data, null, 2), { encoding: "utf8", mode: 0o600 });
      await rename(tmp, file);
    } catch (err) {
      // 落盘失败不崩溃；调用方（登录/续期）自行决定是否重试。
      console.error(`[bridge] credentials persist failed: ${String(err)}`);
    }
  };

  return {
    count: () => Object.keys(records).length,
    list: () => Object.values(records).filter((r) => !r.revoked),
    get: (provider: string) => records[provider],
    save: async (provider: string, body: JsonValue) => {
      const record = sanitizeRecord(provider, body);
      records = { ...records, [provider]: record };
      await persist();
      return record;
    },
  };
}

// ---------------------------------------------------------------------------
// 插件目录扫描
// ---------------------------------------------------------------------------

function rankFile(name: string): number {
  const lower = name.toLowerCase();
  if (lower === "index.mjs") return 0;
  if (lower === "index.js") return 1;
  return 2;
}

/** 目录下全部 .mjs/.js（按 index 优先排序）。 */
async function allPluginFiles(dir: string): Promise<string[]> {
  const entries = await readdir(dir, { withFileTypes: true });
  return entries
    .filter((e) => e.isFile() && /\.(mjs|js)$/i.test(e.name))
    .map((e) => e.name)
    .sort((a, b) => rankFile(a) - rankFile(b) || a.localeCompare(b))
    .map((n) => path.join(dir, n));
}

/**
 * 选定**入口文件**，而不是把所有 .mjs 一起当插件加载。
 *
 * 真实 npm 包里同一实现常有多个门面：
 *   - opencode.mjs  = OpenCode 1.x 门面（magpie 宿主正是按这个加载，见其文件头注释）
 *   - index.mjs     = 自包含实现
 *   - opencode2.mjs = OpenCode 2.x 门面（default = {id, setup(ctx)}，不是插件工厂）
 * 全量扫描会把 2.x 门面也当工厂调用，产生无意义告警与重复 provider。
 *
 * 优先级：opencode.mjs → index.mjs → package.json main → exports["."] → 全量扫描。
 */
async function pickEntryFiles(target: string): Promise<string[]> {
  let stat;
  try {
    stat = statSync(target);
  } catch {
    return [];
  }
  if (stat.isFile()) {
    return /\.(mjs|js)$/i.test(target) ? [target] : [];
  }

  const candidates = ["opencode.mjs", "index.mjs"];
  try {
    const pkgRaw = await readFile(path.join(target, "package.json"), "utf8");
    const pkg = JSON.parse(pkgRaw) as {
      main?: unknown;
      exports?: unknown;
    };
    if (typeof pkg.main === "string") candidates.push(pkg.main);
    const dot = pkg.exports;
    if (typeof dot === "string") candidates.push(dot);
    else if (dot && typeof dot === "object" && typeof (dot as Record<string, unknown>)["."] === "string") {
      candidates.push((dot as Record<string, string>)["."]);
    }
  } catch {
    // 无 package.json 或不可解析：按文件名优先级走。
  }
  for (const rel of candidates) {
    const abs = path.resolve(target, rel);
    try {
      if (statSync(abs).isFile()) return [abs];
    } catch {
      continue;
    }
  }
  return allPluginFiles(target);
}

// ---------------------------------------------------------------------------
// 实例化与模型表
// ---------------------------------------------------------------------------

/** 从 config 钩子 + provider.models 提取 provider 名与静态模型表。 */
async function describePlugin(inst: OpenCodePlugin): Promise<{ name: string; models: PluginModelMap }> {
  const id = inst.provider.id;
  const cfg: MutableProviderConfig = {};
  if (typeof inst.config === "function") {
    try {
      await inst.config(cfg);
    } catch {
      // config 钩子失败只影响名字/缺省表，不终止加载。
    }
  }
  const entry: ProviderConfigEntry | undefined = cfg.provider?.[id];
  const models: PluginModelMap = {};

  const pm = inst.provider.models;
  if (typeof pm === "function") {
    try {
      // 无凭据 → 插件回落到静态表（trae L3607：auth.type !== "oauth" → provider.models）。
      const got = await pm(entry ?? { id, models: {} }, { auth: null });
      if (got && typeof got === "object") {
        for (const [k, v] of Object.entries(got)) {
          if (v && typeof v === "object") models[k] = v as PluginModelEntry;
        }
      }
    } catch {
      // 抛错则用 config 表兜底。
    }
  } else if (pm && typeof pm === "object") {
    Object.assign(models, pm as PluginModelMap);
  }
  if (Object.keys(models).length === 0 && entry?.models) {
    Object.assign(models, entry.models ?? {});
  }
  return { name: entry?.name ?? id, models };
}

function isPluginInstance(value: unknown): value is OpenCodePlugin {
  if (!value || typeof value !== "object") return false;
  const provider = (value as OpenCodePlugin).provider;
  return !!provider && typeof provider === "object" && typeof provider.id === "string";
}

// ---------------------------------------------------------------------------
// 入口
// ---------------------------------------------------------------------------

export interface LoadOptions {
  /** --plugin-dir 解析后的绝对路径。 */
  dir: string;
  credentials: CredentialStore;
}

export async function loadPluginDir(opts: LoadOptions): Promise<LoadResult> {
  const warnings: string[] = [];
  const providers: Record<string, ProviderRecord> = {};
  const plugins: LoadedPlugin[] = [];

  const client: HostClient = {
    auth: {
      set: async ({ path: p, body }) => {
        const providerId = p?.id;
        if (!providerId || body === undefined) return;
        try {
          await opts.credentials.save(providerId, body);
        } catch {
          // 落盘失败已被 store 消化；此处不重复报警。
        }
      },
    },
  };

  const files = await pickEntryFiles(opts.dir);
  if (files.length === 0) {
    warnings.push(`no .mjs/.js plugin file found under ${opts.dir}`);
    return { dir: opts.dir, providers, plugins, warnings };
  }

  for (const file of files) {
    let mod: Record<string, unknown>;
    try {
      mod = (await import(pathToFileURL(file).href)) as Record<string, unknown>;
    } catch (err) {
      warnings.push(`import failed: ${file}: ${String(err)}`);
      continue;
    }

    const exported = Object.entries(mod);
    for (const [name, value] of exported) {
      if (name === "default" || name.startsWith("_")) continue;
      if (typeof value !== "function") continue;

      let inst: unknown;
      try {
        inst = await value({ client });
      } catch (err) {
        warnings.push(`${file}#${name}: factory threw: ${String(err)}`);
        continue;
      }
      if (!isPluginInstance(inst)) continue; // 不是插件工厂导出，静默跳过

      const id = inst.provider.id;
      if (providers[id]) {
        warnings.push(`provider "${id}" already registered by ${providers[id].plugin.file}; skip ${file}#${name}`);
        continue;
      }

      const { name: providerName, models } = await describePlugin(inst);
      const loaded: LoadedPlugin = {
        file,
        exportName: name,
        providerId: id,
        providerName,
        plugin: inst,
      };
      providers[id] = { id, name: providerName, models, plugin: loaded };
      plugins.push(loaded);
    }
  }

  return { dir: opts.dir, providers, plugins, warnings };
}

/** 供 main.ts 校验目录用：目录必须存在且可读。 */
export function ensurePluginDir(raw: string): string {
  const abs = path.resolve(raw);
  try {
    const st = statSync(abs);
    if (!st.isDirectory() && !st.isFile()) throw new Error(`${abs} is neither file nor directory`);
  } catch (err) {
    throw new Error(`--plugin-dir ${abs} is not readable: ${String(err)}`);
  }
  return abs;
}
