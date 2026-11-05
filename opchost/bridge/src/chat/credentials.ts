// 凭据存取（chat 泳道自用的最小实现）。
//
// 归属说明：管理面 /admin/credentials（task-3）才是凭据的正主，本文件只为让
// chat 能在「本机自用 + 测试」两种场景下跑起来，并提供 dev-server 的默认实现。
// 接入 bridge-core 后，deps.getAuth / deps.saveAuth 应由 admin 泳道的凭据库提供，
// 本文件即可不再被 chat 内部引用（只在 dev-server 与回落路径使用）。
//
// 文件形状：<DATA_DIR>/auth.json
//   { "<providerId>": { "type":"oauth", "access":"...", "refresh":"...", "expires":1700000000000 } }
// 写盘用 tmp + rename，避免半截文件；日志里绝不打印 access/refresh。

import { existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import type { AuthValue } from "./types.ts";

export const AUTH_FILE_NAME = "auth.json";

export interface AuthStore {
  get(providerId: string): AuthValue | null;
  set(providerId: string, auth: AuthValue): void;
  path(): string;
}

type AuthFile = Record<string, AuthValue>;

function readFileSafe(path: string): AuthFile {
  try {
    if (!existsSync(path)) return {};
    const parsed: unknown = JSON.parse(readFileSync(path, "utf8"));
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return {};
    return parsed as AuthFile;
  } catch {
    return {};
  }
}

/** 基于 DATA_DIR 的凭据文件store。内存缓冲 + 原子落盘。 */
export function fileAuthStore(dataDir: string): AuthStore {
  const file = join(dataDir, AUTH_FILE_NAME);
  let cache: AuthFile | null = null;

  const load = (): AuthFile => {
    if (cache) return cache;
    cache = readFileSafe(file);
    return cache;
  };

  return {
    get(providerId: string): AuthValue | null {
      const all = load();
      const value = all[providerId];
      return value && typeof value === "object" ? value : null;
    },
    set(providerId: string, auth: AuthValue): void {
      const all = load();
      all[providerId] = auth;
      cache = all;
      try {
        mkdirSync(dirname(file), { recursive: true });
        const tmp = `${file}.${process.pid}.tmp`;
        writeFileSync(tmp, JSON.stringify(all, null, 2), { encoding: "utf8", mode: 0o600 });
        renameSync(tmp, file);
      } catch {
        // 落盘失败不阻塞对话：内存里的续期仍生效（下次重启才丢）
      }
    },
    path(): string {
      return file;
    },
  };
}

/** 内存版（测试用）。 */
export function memoryAuthStore(initial?: Record<string, AuthValue>): AuthStore {
  const all: AuthFile = { ...(initial ?? {}) };
  return {
    get: (id) => (all[id] && typeof all[id] === "object" ? all[id] : null),
    set: (id, auth) => {
      all[id] = auth;
    },
    path: () => ":memory:",
  };
}
