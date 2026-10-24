/**
 * Serves the built SPA and answers every /api/* call from the synthetic fixtures.
 *
 * Why a dedicated server instead of pointing CDP at the real host:
 *
 *  - The real host's answers change minute to minute (log rows, quota, plugin
 *    pids), so a pixel diff against them is noise. Fixtures make the DOM a pure
 *    function of the CSS and the templates — which is exactly what this gate is
 *    supposed to police.
 *  - It also means the gate needs no admin launch token, no real accounts, and
 *    cannot accidentally write to a live config.
 *
 * The SPA is served from `internal/webui/dist`, the same bundle the host embeds,
 * so the gate tests what ships rather than a second copy under web/.
 */

import { createServer } from 'node:http'
import { createReadStream, existsSync, statSync } from 'node:fs'
import { extname, join, normalize, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import * as F from './fixtures.mjs'

const HERE = resolve(fileURLToPath(new URL('.', import.meta.url)))
const REPO = resolve(HERE, '..', '..', '..')
const DIST = join(REPO, 'internal', 'webui', 'dist')

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
  '.woff': 'font/woff',
  '.woff2': 'font/woff2',
}

/**
 * Route table. Order matters: the first matcher whose test() passes wins, so
 * the parameterised pool/checkin routes sit above any catch-all.
 */
const ROUTES = [
  { path: '/api/status', body: () => F.status },
  { path: '/api/plugins', body: () => F.plugins },
  { path: '/api/accounts/login-state', body: () => F.loginState },
  { path: '/api/accounts', body: () => F.accounts },
  { path: '/api/groups', body: () => F.groups },
  { path: '/api/proxies', body: () => F.proxies },
  { path: '/api/rules', body: () => F.rules },
  { path: '/api/settings', body: () => F.settings },
  { path: '/api/keys', body: () => F.keys },
  { path: '/api/notify', body: () => F.notify },
  { path: '/api/scheduler', body: () => F.scheduler },
  { path: '/api/scheduler/models', body: () => F.schedulerModels },
  { path: '/api/scheduler/sticky', body: () => F.schedulerSticky },
  { path: '/api/scheduler/explain', body: () => F.schedulerExplain },
  { path: '/api/diagnostics/probes', body: () => F.probes },
  { path: '/api/custom-connections', body: () => F.customConnections },
  { path: '/api/custom-models', body: () => F.customModels },
  { path: '/api/secrets/a-key', body: () => ({ configured: true }) },
  { test: (pathname) => pathname === '/api/logs/requests', body: (url) => F.logsRequestsPage(url) },
  { path: '/api/logs/timeline', body: () => F.logsTimeline },
  { path: '/api/logs/proxy', body: () => F.logsProxy },
  { path: '/api/logs/body', body: () => ({ req_id: 'req00000001', which: 'req', body: '{"messages":[{"role":"user","content":"示例请求正文"}]}' }) },
  { path: '/api/logs/raw', body: () => ({ line: '{"ts":"2026-10-22T12:00:00+08:00","level":"info","msg":"示例原始行"}' }) },
  { path: '/api/usage/summary', body: () => F.usageSummary },
  { path: '/api/usage/credits', body: () => F.usageCredits },
  { path: '/api/usage/expiry', body: () => F.usageExpiry },
  { path: '/api/usage/sessions', body: () => F.usageSessions },
  { path: '/api/usage/latency/models', body: () => F.usageLatencyModels },
  { path: '/api/usage/latency', body: () => F.usageLatency },
  { path: '/api/usage/keys', body: () => F.usageKeys },
  { path: '/api/usage/models', body: () => F.usageModels },
  { test: (pathname) => pathname === '/api/checkin/all', body: () => ({ results: [] }) },
  { test: (pathname) => pathname.startsWith('/api/checkin'), body: () => F.checkinRecords },
  { test: (pathname) => pathname.startsWith('/api/pools/') && pathname.endsWith('/catalog'), body: () => F.poolCatalog },
  { test: (pathname) => pathname.startsWith('/api/pools/') && pathname.endsWith('/models/overrides'), body: () => F.poolOverrides },
  { test: (pathname) => pathname.startsWith('/api/pools/'), body: () => F.poolDetail },
]

/** Mutating calls are acknowledged with a neutral ok so a view can finish. */
const MUTATING = /\/api\/(accounts\/(save|rename|checkin|bulk|patch|delete|refresh|egress|identity)|plugins\/[^/]+\/(enable|disable|start|stop)|groups|rules|proxies|keys|custom-|scheduler|pools|settings|notify|checkin\/all)/

function match(pathname, url) {
  for (const route of ROUTES) {
    if (route.path && route.path === pathname) return route.body(url)
    if (route.test && route.test(pathname)) return route.body(url)
  }
  return null
}

export function startFixtureServer({ port = 7899 } = {}) {
  if (!existsSync(join(DIST, 'index.html'))) {
    throw new Error(
      `built SPA missing at ${DIST}. Run \`npm run build\` in web/ first — the gate tests the shipped bundle.`,
    )
  }

  const requests = []

  // Settings are the one fixture the UI writes back to and re-reads: the Debug
  // toggle does GET -> flip debug_body -> PUT, and the request-log columns are
  // bound to debug_body. A stateless PUT would make that toggle a no-op under the
  // fixture and hide exactly the behaviour it is supposed to exercise, so the
  // server keeps a shallow-merged copy for the life of the run.
  let settings = structuredClone(F.settings)

  const server = createServer((req, res) => {
    const url = new URL(req.url ?? '/', 'http://127.0.0.1')
    const pathname = url.pathname

    if (pathname.startsWith('/api/')) {
      requests.push(`${req.method} ${pathname}`)
      // The logs view opens an EventSource. Answering with an empty, never-ending
      // event stream keeps that connection pending forever, which is what the
      // view expects; closing it would make the view retry in a loop and the
      // retry countdown text would differ between runs.
      if (pathname === '/api/logs/stream') {
        res.writeHead(200, {
          'Content-Type': 'text/event-stream',
          'Cache-Control': 'no-cache',
          Connection: 'keep-alive',
        })
        res.write(': fixtures do not stream\n\n')
        return
      }

      // GET/PUT /api/settings share the mutable copy above.
      if (pathname === '/api/settings' && req.method === 'PUT') {
        const chunks = []
        req.on('data', (chunk) => chunks.push(chunk))
        req.on('end', () => {
          try {
            const patch = JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}')
            settings = { ...settings, ...patch }
            res.writeHead(200, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' })
            res.end(JSON.stringify({ ok: true }))
          } catch (error) {
            res.writeHead(400, { 'Content-Type': 'application/json; charset=utf-8' })
            res.end(JSON.stringify({ error: { message: `bad settings patch: ${error.message}` } }))
          }
        })
        return
      }
      if (pathname === '/api/settings') {
        res.writeHead(200, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' })
        res.end(JSON.stringify(settings))
        return
      }

      const body = match(pathname, url)
      if (body !== null) {
        res.writeHead(200, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' })
        res.end(JSON.stringify(body))
        return
      }
      if (MUTATING.test(pathname)) {
        res.writeHead(200, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' })
        res.end(JSON.stringify({ ok: true }))
        return
      }
      // Unknown endpoint: 404 rather than a fake success, so a view that starts
      // calling something new fails visibly here instead of rendering an empty
      // state that would silently become the baseline.
      res.writeHead(404, { 'Content-Type': 'application/json; charset=utf-8' })
      res.end(JSON.stringify({ error: { message: `no fixture for ${pathname}`, type: 'fixture_missing' } }))
      return
    }

    // Static: resolve inside DIST only, then fall back to index.html for SPA routes.
    const relative = normalize(decodeURIComponent(pathname)).replace(/^([/\\])+/, '')
    const candidate = join(DIST, relative)
    if (!candidate.startsWith(DIST)) {
      res.writeHead(403)
      res.end('forbidden')
      return
    }
    let file = candidate
    if (existsSync(file) && statSync(file).isDirectory()) file = join(file, 'index.html')
    if (!existsSync(file)) file = join(DIST, 'index.html')

    const type = MIME[extname(file).toLowerCase()] ?? 'application/octet-stream'
    res.writeHead(200, { 'Content-Type': type, 'Cache-Control': 'no-store' })
    createReadStream(file).pipe(res)
  })

  return new Promise((resolvePromise, rejectPromise) => {
    server.once('error', rejectPromise)
    server.listen(port, '127.0.0.1', () => {
      resolvePromise({
        port,
        origin: `http://127.0.0.1:${port}`,
        requests,
        close: () => new Promise((done) => server.close(done)),
      })
    })
  })
}

/** Allows `node scripts/visual/serve.mjs` for manual poking. */
if (import.meta.url === `file:///${process.argv[1]?.replace(/\\/g, '/')}`) {
  const { origin } = await startFixtureServer({ port: Number(process.env.PORT ?? 7899) })
  console.log(`fixture server on ${origin}`)
}
