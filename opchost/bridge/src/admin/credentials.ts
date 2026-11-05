// /admin/credentials —— 凭据注入/导出（服务器用）。
//
// 安全口径（D-57 + model.go:208-214 + Lead 2026-11-05 修正）：
//   · 磁盘上 AES-256-GCM 加密（密钥为 <data-dir>/bridge.key，0600，随机 32B）；
//   · 响应一律脱敏：只给键名 / 是否存在 / 过期时间 / 更新时间；
//   · **导出同样只回脱敏投影**——宿主永远看不到凭据明文（model.go:208-214 注释）；
//     真正的服务器同步走 P4 的独立通道，不经过本端点；
//   · 任何响应都不含指纹（identity）字段 —— 指纹能力留在插件侧。

import { createCipheriv, createDecipheriv, randomBytes } from 'node:crypto'
import { existsSync, readFileSync, writeFileSync, chmodSync, mkdirSync, renameSync } from 'node:fs'
import { join } from 'node:path'
import type { IncomingMessage, ServerResponse } from 'node:http'
import type { AdminCredential, AdminCredentialStore, AdminCredentialView } from './types.ts'
import { http400, http404, isLoopback, readJSON, rejectRemote, sendJSON } from './util.ts'

const AES = 'aes-256-gcm'

/** File-backed encrypted credential store. */
export function createFileCredentialStore(dataDir: string): AdminCredentialStore {
  mkdirSync(dataDir, { recursive: true })
  const keyFile = join(dataDir, 'bridge.key')
  const dbFile = join(dataDir, 'credentials.json')

  function loadKey(): Buffer {
    if (existsSync(keyFile)) return readFileSync(keyFile)
    const key = randomBytes(32)
    writeFileSync(keyFile, key, { mode: 0o600 })
    try {
      chmodSync(keyFile, 0o600)
    } catch {
      // Windows 上 chmod 语义有限，忽略
    }
    return key
  }

  function load(): Record<string, Buffer> {
    if (!existsSync(dbFile)) return {}
    try {
      const raw = JSON.parse(readFileSync(dbFile, 'utf8')) as { iv: string; tag: string; data: string }
      const key = loadKey()
      const decipher = createDecipheriv(AES, key, Buffer.from(raw.iv, 'base64'))
      decipher.setAuthTag(Buffer.from(raw.tag, 'base64'))
      const plain = Buffer.concat([decipher.update(Buffer.from(raw.data, 'base64')), decipher.final()])
      const recs = JSON.parse(plain.toString('utf8')) as Record<string, AdminCredential>
      return recs
    } catch {
      // 密钥轮换/损坏时视为空库，绝不把损坏的密文当凭据用
      return {}
    }
  }

  function save(recs: Record<string, AdminCredential>): void {
    const key = loadKey()
    const iv = randomBytes(12)
    const cipher = createCipheriv(AES, key, iv)
    const plain = Buffer.from(JSON.stringify(recs), 'utf8')
    const enc = Buffer.concat([cipher.update(plain), cipher.final()])
    const tmp = `${dbFile}.tmp`
    writeFileSync(tmp, JSON.stringify({ iv: iv.toString('base64'), tag: cipher.getAuthTag().toString('base64'), data: enc.toString('base64') }), { mode: 0o600 })
    renameSync(tmp, dbFile)
  }

  const keyOf = (p: string, a: string) => `${p}\u0000${a}`

  return {
    list(pluginId?: string): AdminCredential[] {
      const recs = load()
      return Object.values(recs).filter((r) => !pluginId || r.pluginId === pluginId)
    },
    get(pluginId, accountId) {
      return load()[keyOf(pluginId, accountId)]
    },
    put(rec) {
      const recs = load()
      const k = keyOf(rec.pluginId, rec.accountId)
      const existing = recs[k]
      const full: AdminCredential = {
        ...rec,
        updatedAt: rec.updatedAt ?? Date.now(),
        ...(existing?.expiresAt !== undefined && rec.expiresAt === undefined ? { expiresAt: existing.expiresAt } : {}),
        ...(existing?.refreshable !== undefined && rec.refreshable === undefined ? { refreshable: existing.refreshable } : {}),
      }
      recs[k] = full
      save(recs)
      return full
    },
    delete(pluginId, accountId) {
      const recs = load()
      const k = keyOf(pluginId, accountId)
      if (!recs[k]) return false
      delete recs[k]
      save(recs)
      return true
    },
  }
}

/** 脱敏投影：只有键名 / 存在性 / 生命周期。绝不含指纹（D-57）。 */
export function toView(rec: AdminCredential): AdminCredentialView {
  return {
    plugin_id: rec.pluginId,
    account_id: rec.accountId,
    keys: Object.keys(rec.secret).sort(),
    present: true,
    updated_at: rec.updatedAt,
    ...(typeof rec.expiresAt === 'number' ? { expires_at: rec.expiresAt } : {}),
    ...(typeof rec.refreshable === 'boolean' ? { refreshable: rec.refreshable } : {}),
  }
}

/** 导出即脱敏投影：只给键名/存在性/生命周期，绝不含 secret 值（Lead 修正 #4）。 */
export function exportView(rec: AdminCredential): AdminCredentialView {
  return toView(rec)
}

export async function handleCredentials(
  req: IncomingMessage,
  res: ServerResponse,
  store: AdminCredentialStore,
  pathname: string,
): Promise<boolean> {
  if (!isLoopback(req)) {
    rejectRemote(res)
    return true
  }

  if (pathname === '/admin/credentials' && req.method === 'GET') {
    const url = new URL(req.url ?? '/', 'http://127.0.0.1')
    const pluginId = url.searchParams.get('plugin_id') ?? undefined
    const recs = store.list(pluginId).map(toView)
    sendJSON(res, 200, { ok: true, count: recs.length, credentials: recs })
    return true
  }

  if (pathname === '/admin/credentials' && req.method === 'POST') {
    const body = await readJSON(req)
    const pluginId = String(body.plugin_id ?? '').trim()
    const accountId = String(body.account_id ?? '').trim()
    const creds = body.credentials
    if (!pluginId) throw http400('plugin_id is required')
    if (!accountId) throw http400('account_id is required')
    if (!creds || typeof creds !== 'object' || Array.isArray(creds)) throw http400('credentials must be an object')
    const rec = store.put({ pluginId, accountId, secret: creds as Record<string, unknown> })
    sendJSON(res, 200, { ok: true, stored: true, credential: toView(rec) })
    return true
  }

  if (pathname === '/admin/credentials/export' && req.method === 'POST') {
    const body = await readJSON(req)
    const pluginId = String(body.plugin_id ?? '').trim() || undefined
    const accountId = String(body.account_id ?? '').trim() || undefined
    let recs = store.list(pluginId)
    if (accountId) recs = recs.filter((r) => r.accountId === accountId)
    if (!recs.length) throw http404('no credentials to export')
    // 只回脱敏投影：present / keys / expires_at / refreshable，永不含明文或密文
    sendJSON(res, 200, { ok: true, count: recs.length, export: 'redacted', credentials: recs.map(exportView) })
    return true
  }

  if (pathname === '/admin/credentials' && req.method === 'DELETE') {
    const url = new URL(req.url ?? '/', 'http://127.0.0.1')
    const pluginId = url.searchParams.get('plugin_id') ?? ''
    const accountId = url.searchParams.get('account_id') ?? ''
    if (!pluginId || !accountId) throw http400('plugin_id and account_id are required')
    const gone = store.delete(pluginId, accountId)
    sendJSON(res, 200, { ok: true, deleted: gone })
    return true
  }

  return false
}
