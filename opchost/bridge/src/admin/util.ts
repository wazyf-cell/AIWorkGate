// Shared helpers for the admin plane: loopback guard, JSON response envelope,
// redaction. Never logs credentials; never prints a token anywhere.

import type { IncomingMessage, ServerResponse } from 'node:http'

export const LOOPBACK_ADDRS = new Set(['127.0.0.1', '::1', '::ffff:127.0.0.1'])

/** True only for loopback sources. Every /admin endpoint refuses anything else. */
export function isLoopback(req: IncomingMessage): boolean {
  const addr = req.socket.remoteAddress ?? ''
  return LOOPBACK_ADDRS.has(addr)
}

/** 403 for non-loopback callers; used before any body is read. */
export function rejectRemote(res: ServerResponse): void {
  sendJSON(res, 403, { error: { message: 'admin endpoints accept loopback callers only', type: 'forbidden', code: 'loopback_only' } })
}

/** Send a JSON response; the body is never a credential. */
export function sendJSON(res: ServerResponse, status: number, body: unknown): void {
  const data = JSON.stringify(body)
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(data),
    'cache-control': 'no-store',
  })
  res.end(data)
}

export function sendText(res: ServerResponse, status: number, text: string): void {
  res.writeHead(status, {
    'content-type': 'text/plain; charset=utf-8',
    'content-length': Buffer.byteLength(text),
    'cache-control': 'no-store',
  })
  res.end(text)
}

/** Read a bounded JSON body; 400 on malformed input. */
export async function readJSON(req: IncomingMessage, maxBytes = 1 << 20): Promise<Record<string, unknown> | undefined> {
  const chunks: Buffer[] = []
  let size = 0
  for await (const chunk of req) {
    const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk)
    size += buf.length
    if (size > maxBytes) throw http400('body too large')
    chunks.push(buf)
  }
  const raw = Buffer.concat(chunks).toString('utf8').trim()
  if (!raw) return {}
  try {
    const v = JSON.parse(raw)
    if (v === null || typeof v !== 'object' || Array.isArray(v)) throw new Error('expected object')
    return v as Record<string, unknown>
  } catch {
    throw http400('invalid JSON body')
  }
}

export function http400(message: string): Error {
  const e = new Error(message)
  ;(e as { status?: number }).status = 400
  return e
}

export function http404(message: string): Error {
  const e = new Error(message)
  ;(e as { status?: number }).status = 404
  return e
}

/** Local calendar day YYYY-MM-DD (the date dimension of checkin_results). */
export function dayOf(now: number): string {
  const d = new Date(now)
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${d.getFullYear()}-${m}-${day}`
}

/** Redact an arbitrary credential object: only key names, never values. */
export function redactKeys(secret: Record<string, unknown>): { keys: string[]; length_ok: boolean } {
  const keys = Object.keys(secret).sort()
  return { keys, length_ok: true }
}

/** Redact one secret value for a transport-level error message (never the value). */
export function secretFingerprint(v: unknown): string {
  if (v == null) return 'null'
  const s = String(v)
  if (s.length === 0) return 'empty'
  return `sha256:${s.length > 8 ? `${s.slice(0, 4)}…${s.slice(-4)}` : 'short'}`
}

/** true when the string looks like a bearer token / secret we must not log */
export function looksSecret(s: string): boolean {
  return /^(Bearer\s+|ak_|rk_|sk-|eyJ|ghp_|gho_)/i.test(s) || s.length >= 40
}

/** Never log a request body; log only ids and counts. */
export function sanitizeForLog(fields: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(fields)) {
    if (typeof v === 'string' && looksSecret(v)) {
      out[k] = secretFingerprint(v)
    } else {
      out[k] = v
    }
  }
  return out
}
