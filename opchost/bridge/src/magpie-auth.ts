// Import the user's existing magpie plugin logins into the bridge credential
// store, so "install the bridge" also means "it works with the login magpie
// already has" (P2 acceptance). A record the bridge already holds (from its own
// sign-in or refresh) always wins; the import never overwrites it.
//
// Secrets are read from magpie's plugin-auth.json but never logged: only the
// provider ids that were imported are returned.

import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

import type { CredentialStore } from "./types.ts";

const IMPORT_ENV = "AIWORKGATE_MAGPIE_AUTH_IMPORT";
const PATH_ENV = "MAGPIE_AUTH_FILE";

/** magpieAuthFile returns MAGPIE_AUTH_FILE when set, else magpie's default. */
export function magpieAuthFile(): string {
  const override = (process.env[PATH_ENV] ?? "").trim();
  if (override) return override;
  return join(homedir(), ".config", "magpie", "plugin-auth.json");
}

/**
 * importMagpieAuth copies provider logins the bridge does not have yet.
 * AIWORKGATE_MAGPIE_AUTH_IMPORT=0 disables the import.
 *
 * task-84: besides the exact provider ids, magpie's multi-account convention
 * `<provider>#<suffix>` keys are imported too (each suffix account keeps its
 * own full credential record; the credential-store key is the complete magpie
 * key so the plugin/scheduler can address it). A `#` key whose provider prefix
 * is not a known provider is ignored — unknown channels stay invisible.
 */
export async function importMagpieAuth(
  credentials: CredentialStore,
  providers: string[],
): Promise<string[]> {
  if (process.env[IMPORT_ENV] === "0") return [];
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(magpieAuthFile(), "utf8"));
  } catch {
    return [];
  }
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return [];
  const file = raw as Record<string, unknown>;
  const known = new Set(providers);
  const candidates: string[] = [];
  for (const key of Object.keys(file)) {
    if (known.has(key)) {
      candidates.push(key);
      continue;
    }
    // 多账号约定：<provider>#<suffix>；provider 前缀必须是已知渠道。
    const hash = key.indexOf("#");
    if (hash > 0 && known.has(key.slice(0, hash))) {
      candidates.push(key);
    }
  }
  const imported: string[] = [];
  for (const provider of candidates) {
    if (credentials.get(provider)) continue;
    const entry = file[provider];
    if (!entry || typeof entry !== "object" || Array.isArray(entry)) continue;
    const record = entry as Record<string, unknown>;
    // Two importable shapes: the OAuth pair (access/refresh) and the api-key
    // public-credential form `{"type":"api","key":"…"}` used by free channels
    // like opencode-zen-free (task-77). Anything else is not a login.
    const hasOAuth = !!record.access || !!record.refresh;
    const hasApiKey =
      record.type === "api" && typeof record.key === "string" && record.key.trim() !== "";
    if (!hasOAuth && !hasApiKey) continue;
    await credentials.save(provider, record);
    imported.push(provider);
  }
  return imported;
}
