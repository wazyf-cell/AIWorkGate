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
  const imported: string[] = [];
  for (const provider of providers) {
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
