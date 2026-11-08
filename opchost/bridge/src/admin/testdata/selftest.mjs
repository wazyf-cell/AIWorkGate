// Admin-plane self-test: boots an HTTP server around the admin router with a
// magpie-shaped mock plugin, asserts every endpoint, then keeps the server up
// so the same run can be exercised with curl.
//
//   node testdata/selftest.mjs            # run assertions, stay alive for curl
//   node testdata/selftest.mjs --exit     # run assertions and exit
//
// Prints machine-readable lines: CHECK <name> PASS|FAIL <detail>, PORT <n>,
// READY_FOR_CURL. Exit code 1 if any CHECK failed.

import { createServer } from 'node:http'
import { mkdtempSync, writeFileSync, readFileSync, existsSync, unlinkSync, mkdirSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { randomBytes } from 'node:crypto'
import { createAdminRouter, createFileCredentialStore } from '../index.ts'
import { importMagpieAuth } from '../../magpie-auth.ts'
import { bridgeCredentialStore } from '../../server.ts'
import { createHookTracker, KNOWN_HOOKS, declaredHooksOf } from '../../hooks.ts'
import { loadPluginDir } from '../../loader.ts'
import { registryFromLoad } from '../wiring.ts'
import { buildIndex, DEFAULT_QUOTA_GROUPS, mergeQuotaRecords, quotaRecordFor } from '../quotagroups.ts'
import { isLoopback } from '../util.ts'
import { MockAuthPlugin, MockAIAuthPlugin, _internal, setMockState, resetMockState } from './mock-plugin.mjs'
import { IdentityAuthPlugin, getIdentityState, setIdentityState } from './mock-identity-plugin.mjs'

const dataDir = mkdtempSync(join(tmpdir(), 'bridge-admin-'))
const settingsPath = join(dataDir, 'settings.json')
// D-19④: magpie's switch says this plugin is on; the second one stays off
writeFileSync(settingsPath, JSON.stringify({ pluginCheckins: { mock: true } }))

// P9-1: magpie's multi-window quota file (shape copied from a real quotas.json).
const quotaPath = join(dataDir, 'quotas.json')
writeFileSync(
  quotaPath,
  JSON.stringify({
    'mock/mock-account': {
      at: '2026-11-06T14:31:50+08:00',
      quota: {
        provider: 'mock',
        plan: 'pro',
        readAt: '2026-11-06T14:31:50+08:00',
        windows: [
          {
            name: '总积分',
            amount: 100,
            limit: 500,
            used: 400,
            unit: 'credits',
            resetsAt: '2026-12-01T00:00:00Z',
            runsOutAt: '2026-11-20T00:00:00Z',
            runsOut: 20,
            display: '100 / 500 · 20 即将过期',
          },
          { name: '赠送积分包', amount: 20, limit: 100, used: 80, unit: 'credits', display: '20 / 100', 'x-extra': 'kept' },
        ],
      },
    },
    // task-27⑤：第二平台各两条 window——Q1-A 下 mock 与 mock-ai 不配组，
    // 两平台的 windows 必须各自独立透传（不合并、不串号）。
    // 记录键 = <pluginId>/<accountId>（桥的账号寻址）：mock-ai 是 handleOff
    // （pluginId=mock-off）名下 providerId=mock-ai 的账号。
    'mock-off/mock-ai': {
      at: '2026-11-06T15:00:00+08:00',
      quota: {
        provider: 'mock-ai',
        plan: 'team',
        readAt: '2026-11-06T15:00:00+08:00',
        windows: [
          {
            name: '总积分',
            amount: 70,
            limit: 900,
            used: 830,
            unit: 'credits',
            resetsAt: '2026-12-05T00:00:00Z',
            display: '70 / 900',
          },
          { name: '体验券', amount: 5, limit: 50, used: 45, unit: 'credits', runsOut: 5, display: '5 / 50' },
        ],
      },
    },
  }),
  'utf8',
)

const results = []
function check(name, ok, detail = '') {
  results.push({ name, ok, detail })
  console.log(`CHECK ${name} ${ok ? 'PASS' : 'FAIL'}${detail ? ` ${detail}` : ''}`)
}
const no = (s, needle) => !s.includes(needle)

/** Adapter: magpie plugin instance → the bridge's PluginHandle (loader's job). */
async function loadHandle(id, factory, providerId, opts = {}) {
  const inst = await factory({ client: {}, magpie: {} })
  return {
    id,
    name: providerId,
    version: '0.0.1',
    providerIds: [providerId],
    capabilities: () => ['checkin', 'quota', 'accounts'],
    sites: () => _internal.SITES,
    authorize: async (_providerId, o = {}) => inst.auth.methods[0].authorize(o),
    checkin: async () =>
      opts.checkinThrows
        ? Promise.reject(new Error('offline: no answer from upstream'))
        : inst.auth.checkin(async () => ({ type: 'oauth', access: 'a', source: 'desktop' })),
    accounts: async () => [{ id: 'mock-account', label: 'Mock account' }],
  }
}

const handle = await loadHandle('mock', MockAuthPlugin, 'mock')
const handleOff = await loadHandle('mock-off', MockAIAuthPlugin, 'mock-ai')
// D-20: one plugin exporting two prefixes that share a single quota record
const WB_SITES = {
  workbuddy: { id: 'workbuddy', name: 'WorkBuddy', endpoint: 'https://copilot.tencent.com', platform: 'workbuddy' },
  'workbuddy-ai': { id: 'workbuddy-ai', name: 'WorkBuddy AI', endpoint: 'https://www.workbuddy.ai', platform: 'workbuddy-ai' },
}
const handleWB = {
  ...(await loadHandle('mock-wb', MockAuthPlugin, 'workbuddy')),
  providerIds: ['workbuddy', 'workbuddy-ai'],
  sites: () => WB_SITES,
}
const api = createAdminRouter({
  dataDir,
  plugins: {
    list: () => [handle, handleOff, handleWB],
    resolve: (id) => (id === 'mock' ? handle : id === 'mock-off' ? handleOff : id === 'mock-wb' ? handleWB : undefined),
  },
  checkinSettingsPath: settingsPath,
  quotaPath,
  now: Date.now,
  logger: { info() {} },
})

const server = createServer((req, res) => {
  api.handle(req, res).then((handled) => {
    if (!handled) {
      res.writeHead(404).end('not admin')
    }
  })
})
await new Promise((r) => server.listen(0, '127.0.0.1', r))
const { port, address } = server.address()
const base = `http://127.0.0.1:${port}`
console.log(`PORT ${port}`)
console.log(`BIND ${address}`)

const post = (p, body) =>
  fetch(`${base}${p}`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body ?? {}) })
const get = (p) => fetch(`${base}${p}`)

// ---- 1. listener is loopback only -------------------------------------------------
check('bind_loopback', address === '127.0.0.1', `address=${address}`)
check('guard_remote_false', isLoopback({ socket: { remoteAddress: '10.0.0.5' } }) === false)
check('guard_loopback_true', isLoopback({ socket: { remoteAddress: '127.0.0.1' } }) === true)

// ---- 2. /admin/manifest (network_domains from SITES) ------------------------------
{
  const r = await get('/admin/manifest?plugin_id=mock')
  const body = await r.json()
  const m = body.manifest ?? {}
  check('manifest_status', r.status === 200, `status=${r.status}`)
  check('manifest_domains', (m.network_domains ?? []).includes('copilot.tencent.com') && (m.network_domains ?? []).includes('www.workbuddy.ai'), JSON.stringify(m.network_domains))
  check('manifest_providers', Array.isArray(m.providers) && m.providers[0] === 'mock', JSON.stringify(m.providers))
  check('manifest_quota_groups', (m.quota_groups ?? []).some((g) => g.id === 'codebuddy-workbuddy-credits'), JSON.stringify(m.quota_groups?.map((g) => g.id)))
  check('manifest_identity_hidden', m.identity?.exposed_in_admin === false && no(JSON.stringify(body), 'machine_id'))
  check('manifest_no_fingerprint', no(JSON.stringify(body), 'device_id') && no(JSON.stringify(body), 'machine_id'))
}

// ---- 3. Q1-A quota groups ---------------------------------------------------------
{
  const body = await (await get('/admin/quotagroups')).json()
  const g = (body.groups ?? []).find((x) => x.id === 'codebuddy-workbuddy-credits')
  check('quotagroups_status', body.ok === true && body.count >= 1, `count=${body.count}`)
  check(
    'quotagroups_prefixes',
    !!g && g.prefixes.includes('codebuddy') && g.prefixes.includes('workbuddy') && !g.prefixes.includes('workbuddy-ai'),
    JSON.stringify(g?.prefixes),
  )
}

// ---- 3b. Q1-A: codebuddy + workbuddy share; workbuddy-ai stays separate -----------
{
  const body = await (await get('/admin/manifest?plugin_id=mock-wb')).json()
  const recs = body.manifest?.quota_records ?? {}
  const idx = buildIndex(DEFAULT_QUOTA_GROUPS, 'test')
  // The manifest only lists prefixes the plugin actually exports; codebuddy is
  // a future sibling prefix, so assert the shared record through the index.
  check(
    'd20_manifest_same_record',
    recs['workbuddy/'] === 'group:workbuddy' &&
      recs['workbuddy-ai/'] !== 'group:workbuddy' &&
      quotaRecordFor(idx, 'codebuddy') === quotaRecordFor(idx, 'workbuddy') &&
      quotaRecordFor(idx, 'workbuddy-ai') !== quotaRecordFor(idx, 'workbuddy'),
    JSON.stringify({ recs, wb: quotaRecordFor(idx, 'workbuddy'), ai: quotaRecordFor(idx, 'workbuddy-ai') }),
  )
  const merged = mergeQuotaRecords(idx, [
    { prefix: 'codebuddy/', remaining: 100, total: 500, display: '100 / 500' },
    { prefix: 'workbuddy/', remaining: 120, total: 500 },
  ])
  check(
    'd20_merge_one_row',
    merged.length === 1 &&
      merged[0].remaining === 120 &&
      merged[0].group === 'codebuddy-workbuddy-credits' &&
      (merged[0].prefixes ?? []).length === 2,
    JSON.stringify(merged),
  )
  const separate = mergeQuotaRecords(idx, [
    { prefix: 'workbuddy/', remaining: 100, total: 500 },
    { prefix: 'workbuddy-ai/', remaining: 7, total: 9 },
  ])
  check('q1a_workbuddy_ai_separate', separate.length === 2, JSON.stringify(separate.map((r) => r.prefix)))
}

// ---- 4. D-19④ check-in switches inherited from magpie ----------------------------
{
  const body = await (await get('/admin/checkin/switches')).json()
  check('switches_inherited', body.switches?.mock === true && body.switches?.['mock-ai'] !== true, JSON.stringify(body.switches))
}

// ---- 5. credentials: inject / list / export never echo plaintext -----------------
const SECRET = `ak_SUPERSECRET_${randomBytes(8).toString('hex')}`
{
  const r = await post('/admin/credentials', {
    plugin_id: 'mock',
    account_id: 'mock-account',
    credentials: { access: SECRET, refresh: `rk_${SECRET}`, token_type: 'Bearer' },
  })
  const text = await r.text()
  check('cred_inject_status', r.status === 200, `status=${r.status}`)
  check('cred_inject_no_plaintext', no(text, SECRET) && no(text, 'rk_ak_'), text.slice(0, 160))
  check('cred_inject_keys_only', text.includes('"access"') && text.includes('"refresh"'))

  const listText = await (await get('/admin/credentials')).text()
  check('cred_list_no_plaintext', no(listText, SECRET))
  check('cred_list_present', listText.includes('"present":true') && listText.includes('"keys"'))

  const exportText = await (await post('/admin/credentials/export', { plugin_id: 'mock' })).text()
  check('cred_export_redacted', no(exportText, SECRET) && exportText.includes('"export":"redacted"'))

  const onDisk = existsSync(join(dataDir, 'credentials.json')) ? readFileSync(join(dataDir, 'credentials.json'), 'utf8') : ''
  check('cred_disk_encrypted', onDisk.length > 0 && no(onDisk, SECRET), `bytes=${onDisk.length}`)
}

// ---- 6. signin: plugin-owned loopback mode ---------------------------------------
let pluginModeSid = ''
{
  const r = await post('/admin/signin/start', { plugin_id: 'mock', provider: 'mock' })
  const body = await r.json()
  pluginModeSid = body.session_id ?? ''
  check('signin_plugin_status', r.status === 200 && typeof body.url === 'string', `status=${r.status}`)
  check('signin_plugin_url_https', String(body.url).startsWith('https://'), body.url)
  check('signin_plugin_no_token', no(JSON.stringify(body), 'ak_plugin_mode_token'))
  await new Promise((r2) => setTimeout(r2, 120))
  const state = await (await get(`/admin/signin/${pluginModeSid}`)).json()
  check('signin_plugin_success', state.state === 'success', `state=${state.state}`)
  check('signin_plugin_redacted', no(JSON.stringify(state), 'ak_plugin_mode_token') && no(JSON.stringify(state), 'rk_plugin_mode_token'))
}

// ---- 7. signin: bridge-owned 127.0.0.1 callback ---------------------------------
{
  const r = await post('/admin/signin/start', { plugin_id: 'mock', provider: 'mock', mode: 'bridge' })
  const body = await r.json()
  const sid = body.session_id ?? ''
  check('signin_bridge_status', r.status === 200, `status=${r.status}`)
  check('signin_bridge_redirect_loopback', typeof body.redirect_uri === 'string' && body.redirect_uri.startsWith('http://127.0.0.1:'), body.redirect_uri)
  // the browser would land here; drive it directly
  const hit = await fetch(`${body.redirect_uri}?code=bridgecode&state=mock-state`)
  check('signin_bridge_callback_hit', hit.status === 200, `status=${hit.status}`)
  await new Promise((r2) => setTimeout(r2, 120))
  const state = await (await get(`/admin/signin/${sid}`)).json()
  check('signin_bridge_success', state.state === 'success' && state.account_id === 'mock-account', `state=${state.state}`)
  check('signin_bridge_redacted', no(JSON.stringify(state), 'ak_bridgecode'))
}

// ---- 8. signin cancel closes the loopback listener -------------------------------
{
  const body = await (await post('/admin/signin/start', { plugin_id: 'mock', provider: 'mock', mode: 'bridge' })).json()
  const cancel = await post(`/admin/signin/${body.session_id}/cancel`, {})
  const c = await cancel.json()
  check('signin_cancel', cancel.status === 200 && c.state === 'cancelled', `state=${c.state}`)
  let refused = false
  try {
    await fetch(`${body.redirect_uri}?code=late`)
  } catch {
    refused = true
  }
  check('signin_cancel_closes_listener', refused)
}

// ---- 9. D-19 check-in contract ---------------------------------------------------
{
  const r = await post('/accounts/mock-account/checkin', {})
  const body = await r.json()
  check('checkin_ok_status', r.status === 200, `status=${r.status}`)
  check('checkin_ok_result', body.result === 'ok', `result=${body.result}`)
  check('checkin_ok_credits', body.credits_before === 0 && body.credits_after === 10, `before=${body.credits_before} after=${body.credits_after}`)
  check('checkin_no_day_field', !('day' in body) && typeof body.at === 'number')
  const fields = Object.keys(body).sort().join(',')
  check('checkin_fields_contract', fields === 'at,credits_after,credits_before,message,result', fields)
}
{
  const body = await (await post('/accounts/mock-account/checkin', {})).json()
  check('checkin_second_already', body.result === 'already', `result=${body.result}`)
  check('checkin_no_double_count', body.credits_after === 10, `after=${body.credits_after}`)
}
{
  // upstream keeps claiming a credit; the bridge ledger must still not double count
  setMockState({ dupeClaim: true })
  const body = await (await post('/accounts/mock-account/checkin', {})).json()
  check('checkin_ledger_dedupes_claimed', body.result === 'already' && body.credits_after === 10, `result=${body.result} after=${body.credits_after}`)
  setMockState({ dupeClaim: false })
}

// ---- 10. D-19④ switch off → skipped, never a fabricated record -------------------
{
  const r = await post('/accounts/mock-account/checkin?plugin_id=mock-off', {})
  const body = await r.json()
  check('checkin_disabled_409', r.status === 409 && body.error?.code === 'checkin_disabled', `status=${r.status} code=${body.error?.code}`)
  const agg = await (await post('/checkin', { plugin_id: 'mock-off' })).json()
  check('checkin_aggregate_skipped', agg.skipped?.length === 1 && agg.skipped[0].plugin_id === 'mock-off', JSON.stringify(agg.skipped))
  const agg2 = await (await post('/checkin', {})).json()
  check('checkin_aggregate_runs_enabled', agg2.results?.some((x) => x.plugin_id === 'mock' && x.checkin.result === 'already'), `count=${agg2.count}`)
}

// ---- 11. plugin throw → fail with classified kind, not a crash -------------------
{
  const throwing = await loadHandle('mock-throw', MockAuthPlugin, 'mock', { checkinThrows: true })
  const api2 = createAdminRouter({
    dataDir,
    plugins: { list: () => [throwing], resolve: (id) => (id === 'mock-throw' ? throwing : undefined) },
    checkinSettingsPath: settingsPath,
    logger: { info() {} },
  })
  const srv = createServer((req, res) => void api2.handle(req, res))
  await new Promise((r) => srv.listen(0, '127.0.0.1', r))
  const p = srv.address().port
  const body = await (
    await fetch(`http://127.0.0.1:${p}/checkin`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ plugin_id: 'mock-throw', account_id: 'a1' }) })
  ).json()
  const one = body.results?.[0]?.checkin ?? {}
  check('checkin_throw_is_fail', one.result === 'fail' && typeof one.message === 'string', `result=${one.result}`)
  check('checkin_fail_no_fake_kind', one.kind === undefined || ['rate_limited', 'session_dead', 'plan_limit', 'device_signed', 'quota', 'banned', 'normal'].includes(one.kind), `kind=${one.kind}`)
  srv.close()
  api2.close()
}

// ---- 12. unknown routes / plugin ---------------------------------------------------
{
  const r404 = await get('/admin/does-not-exist')
  check('admin_unknown_404', r404.status === 404)
  const rPlugin = await get('/admin/manifest?plugin_id=nope')
  check('manifest_unknown_plugin_404', rPlugin.status === 404)
}

// ---- 12b. /accounts 契约端点（P1-6：GET 列表 / PATCH enable / DELETE）-------------
{
  const jsonHeaders = { 'content-type': 'application/json' }
  // 造一个只用于删除的账号，别动主账号
  await post('/admin/credentials', { plugin_id: 'mock', account_id: 'temp-acc', credentials: { access: 'ak_temp' } })

  const r = await get('/accounts')
  const body = await r.json()
  const list = body.accounts ?? []
  const ids = list.map((a) => a.id)
  check(
    'accounts_lists_all',
    r.status === 200 && Array.isArray(list) && ids.includes('workbuddy') && ids.includes('mock-account') && ids.includes('temp-acc'),
    `status=${r.status} ids=${JSON.stringify(ids)}`,
  )

  const sample = list.find((a) => a.id === 'workbuddy')
  check(
    'accounts_contract_fields',
    !!sample &&
      typeof sample.label === 'string' &&
      typeof sample.enabled === 'boolean' &&
      typeof sample.status === 'string' &&
      typeof sample.quota === 'object' &&
      typeof sample.credential === 'object',
    JSON.stringify(sample),
  )
  check(
    'accounts_no_credentials_expired',
    sample.credential.present === false && sample.status === 'expired',
    `present=${sample.credential.present} status=${sample.status}`,
  )

  const logged = list.find((a) => a.id === 'mock-account')
  check(
    'accounts_with_credentials_ok',
    logged.credential.present === true && logged.status === 'ok' && logged.credential.refreshable === true,
    `cred=${JSON.stringify(logged.credential)} status=${logged.status}`,
  )
  // 2026-11-07 修复回归：signin settle 必须同时落 provider 键位（accountId=providerId），
  // 否则 chat 的 getAuth 与 /accounts 的 provider 行永远读不到登录凭据。
  const providerRow = list.find((a) => a.id === 'mock')
  check(
    'signin_writes_provider_level_credential',
    !!providerRow && providerRow.credential.present === true && providerRow.status === 'ok',
    `cred=${JSON.stringify(providerRow?.credential)} status=${providerRow?.status}`,
  )
  // P9-1: windows[] are passed through verbatim (unknown fields included).
  const q = logged.quota ?? {}
  check(
    'accounts_quota_windows_passthrough',
    Array.isArray(q.windows) &&
      q.windows.length === 2 &&
      q.windows[0].display === '100 / 500 · 20 即将过期' &&
      q.windows[1]['x-extra'] === 'kept',
    JSON.stringify(q.windows),
  )
  check(
    'accounts_quota_legacy_scalars',
    q.remaining === 100 &&
      q.total === 500 &&
      q.plan === 'pro' &&
      q.expires_at === Math.floor(Date.parse('2026-11-20T00:00:00Z') / 1000) &&
      q.reset_at === Math.floor(Date.parse('2026-12-01T00:00:00Z') / 1000),
    JSON.stringify({ remaining: q.remaining, total: q.total, expires_at: q.expires_at, reset_at: q.reset_at }),
  )
  // task-27⑤：两平台各两条 window 各自独立（Q1-A：mock 与 mock-ai 不配组）。
  // mock-ai = handleOff 名下 provider 账号；记录键 mock-off/mock-ai。
  const aiRow = list.find((a) => a.id === 'mock-ai')
  const aiQ = aiRow?.quota ?? {}
  check(
    'accounts_quota_second_platform_windows',
    Array.isArray(aiQ.windows) &&
      aiQ.windows.length === 2 &&
      aiQ.windows[0].display === '70 / 900' &&
      aiQ.windows[1].runsOut === 5 &&
      aiQ.remaining === 70 &&
      aiQ.total === 900 &&
      aiQ.plan === 'team',
    JSON.stringify(aiQ),
  )
  check(
    'accounts_quota_platforms_independent',
    q.remaining === 100 && aiQ.remaining === 70 && aiQ.remaining !== q.remaining,
    `mock=${q.remaining} mock-ai=${aiQ.remaining}`,
  )
  check(
    'accounts_quota_absent_omitted',
    list
      .filter((a) => a.id !== 'mock-account' && a.id !== 'mock-ai')
      .every((a) => a.quota?.windows === undefined && a.quota?.remaining === undefined),
    JSON.stringify(list.filter((a) => a.id !== 'mock-account' && a.id !== 'mock-ai').map((a) => [a.id, a.quota])),
  )
  check(
    'accounts_no_identity_fingerprint',
    list.every((a) => !('identity' in a)) && no(JSON.stringify(body), 'machine_id') && no(JSON.stringify(body), 'device_id'),
  )

  const patched = await fetch(`${base}/accounts/workbuddy`, { method: 'PATCH', headers: jsonHeaders, body: JSON.stringify({ enabled: false }) })
  const pb = await patched.json()
  check('accounts_patch_enabled', patched.status === 200 && pb.account?.enabled === false, `status=${patched.status} enabled=${pb.account?.enabled}`)
  const after = (await (await get('/accounts')).json()).accounts.find((a) => a.id === 'workbuddy')
  check('accounts_patch_reflected_in_get', after.enabled === false, `enabled=${after.enabled}`)

  const bad = await fetch(`${base}/accounts/workbuddy`, { method: 'PATCH', headers: jsonHeaders, body: JSON.stringify({ enabled: 'yes' }) })
  check('accounts_patch_rejects_bad_body', bad.status === 400, `status=${bad.status}`)
  const missing = await fetch(`${base}/accounts/nope`, { method: 'PATCH', headers: jsonHeaders, body: JSON.stringify({ enabled: true }) })
  check('accounts_patch_unknown_404', missing.status === 404, `status=${missing.status}`)

  const del = await fetch(`${base}/accounts/temp-acc`, { method: 'DELETE' })
  const db = await del.json()
  check('accounts_delete_ok', del.status === 200 && db.deleted === true, JSON.stringify(db))
  const gone = (await (await get('/accounts')).json()).accounts.map((a) => a.id)
  check('accounts_delete_gone', !gone.includes('temp-acc'), JSON.stringify(gone))
  const del2 = await (await fetch(`${base}/accounts/temp-acc`, { method: 'DELETE' })).json()
  check('accounts_delete_idempotent', del2.deleted === false, JSON.stringify(del2))

  const ledgerFile = join(dataDir, 'accounts.json')
  const raw = existsSync(ledgerFile) ? readFileSync(ledgerFile, 'utf8') : ''
  check('accounts_ledger_persisted', raw.includes('workbuddy') && raw.includes('"enabled": false'), raw.slice(0, 120).replace(/\s+/g, ' '))
  check('accounts_ledger_no_secret', no(raw, 'ak_temp') && no(raw, 'ak_plugin_mode_token'))
}

// ---- 13. wiring: bridge-core LoadResult → PluginRegistry → 契约端点 --------------
{
  const inst = await MockAuthPlugin({ client: {}, magpie: {} })
  const loaded = {
    dir: '/tmp/mock',
    providers: { mock: { id: 'mock', name: 'Mock', models: {}, plugin: { file: 'mock.mjs', exportName: 'MockAuthPlugin', providerId: 'mock', providerName: 'Mock', plugin: inst } } },
    plugins: [],
    warnings: [],
  }
  const shared = { count: () => 0, list: () => [], get: () => undefined, save: async (p) => ({ provider: p, updated_at: Date.now() }) }
  const registry = registryFromLoad(loaded, shared, { sites: () => _internal.SITES, pluginId: 'mock-wired' })
  const h = registry.list()[0]
  check('wiring_providers', h.providerIds[0] === 'mock', JSON.stringify(h.providerIds))
  // task-9 能力口径：基线精确匹配 [accounts, checkin, upstream]（mock 无 auth.usage）
  check('wiring_capabilities_baseline', JSON.stringify(h.capabilities()) === JSON.stringify(['accounts', 'checkin', 'upstream']), JSON.stringify(h.capabilities()))
  check(
    'wiring_capabilities_no_identity',
    !h.capabilities().includes('identity_request') &&
      !h.capabilities().includes('identity_os_switch') &&
      !h.capabilities().includes('refresh') &&
      !h.capabilities().some((c) => c.startsWith('login_')),
    JSON.stringify(h.capabilities()),
  )
  // quota 是条件能力：插件带 auth.usage 才声明
  inst.auth.usage = async () => ({})
  const reg2 = registryFromLoad(loaded, shared, { pluginId: 'mock-wired2' })
  check(
    'wiring_capabilities_quota_conditional',
    JSON.stringify(reg2.list()[0].capabilities()) === JSON.stringify(['accounts', 'checkin', 'quota', 'upstream']),
    JSON.stringify(reg2.list()[0].capabilities()),
  )
  check('wiring_sites', JSON.stringify(h.sites()).includes('copilot.tencent.com'))
  const started = await h.authorize('mock')
  check('wiring_authorize', typeof started.url === 'string' && started.url.startsWith('https://'), started.url)

  const api3 = createAdminRouter({ dataDir, plugins: registry, checkinSettingsPath: settingsPath })
  const srv = createServer((req, res) => void api3.handle(req, res))
  await new Promise((r) => srv.listen(0, '127.0.0.1', r))
  const p3 = srv.address().port
  resetMockState()
  const body = await (
    await fetch(`http://127.0.0.1:${p3}/accounts/mock/checkin`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: '{}' })
  ).json()
  check('wiring_checkin_ok', body.result === 'ok' && body.credits_after === 10, `result=${body.result} after=${body.credits_after}`)
  const again = await (
    await fetch(`http://127.0.0.1:${p3}/accounts/mock/checkin`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: '{}' })
  ).json()
  check('wiring_checkin_already', again.result === 'already' && again.credits_after === 10, `result=${again.result}`)
  srv.close()
  api3.close()
}

// P2: importing magpie's existing login state (never overwriting a bridge record)
{
  const authPath = join(dataDir, 'magpie-plugin-auth.json')
  writeFileSync(authPath, JSON.stringify({ mock: { access: 'a', refresh: 'r' }, off: {} }))
  process.env.MAGPIE_AUTH_FILE = authPath
  const store = createFileCredentialStore(join(dataDir, 'import-store'))
  const creds = bridgeCredentialStore(store, 'magpie-bridge')
  const imported = await importMagpieAuth(creds, ['mock', 'off', 'missing'])
  check('magpie_import_selected', imported.length === 1 && imported[0] === 'mock', JSON.stringify(imported))
  check('magpie_import_secret', store.get('magpie-bridge', 'mock')?.secret?.access === 'a')
  const again = await importMagpieAuth(creds, ['mock'])
  check('magpie_import_idempotent', again.length === 0, JSON.stringify(again))
  process.env.AIWORKGATE_MAGPIE_AUTH_IMPORT = '0'
  const disabled = await importMagpieAuth(
    bridgeCredentialStore(createFileCredentialStore(join(dataDir, 'import-store-2')), 'magpie-bridge'),
    ['mock'],
  )
  check('magpie_import_optout', disabled.length === 0, JSON.stringify(disabled))
  delete process.env.AIWORKGATE_MAGPIE_AUTH_IMPORT
  delete process.env.MAGPIE_AUTH_FILE
}

// ---- 14. D-58 hooks controlled surface --------------------------------------------
{
  // A throwaway plugin dir: mock shape with config + models fn + loader +
  // methods + checkin + usage (all the faces the bridge knows about).
  const pluginDir = join(mkdtempSync(join(tmpdir(), 'd58-hooks-')), 'plugin')
  mkdirSync(pluginDir, { recursive: true })
  writeFileSync(
    join(pluginDir, 'opencode.mjs'),
    `export async function MockHooksPlugin() {
  return {
    config: async (config) => {
      config.provider ??= {}
      config.provider.mock ??= { name: 'Mock', api: 'https://copilot.tencent.com/v2' }
    },
    provider: {
      id: 'mock',
      async models() { return { 'mock-chat': { id: 'mock-chat', name: 'Mock Chat' } } },
    },
    auth: {
      provider: 'mock',
      async loader() { return { baseURL: 'https://copilot.tencent.com/v2', apiKey: 'k' } },
      async checkin() { return { outcome: 'done', credit: 1 } },
      usage: async () => ({}),
      methods: [{ type: 'oauth', label: 'x', authorize: async () => ({ url: 'https://x.test/a' }) }],
    },
  }
}`,
    'utf8',
  )

  // 14a. loader enumerates the declared face from a real plugin instance and
  // counts the load-time calls (config + provider.models fn).
  const tracker = createHookTracker()
  const loaded = await loadPluginDir({
    dir: pluginDir,
    credentials: { count: () => 0, list: () => [], get: () => undefined, save: async (p) => ({ provider: p, updated_at: Date.now() }) },
    hooks: tracker,
  })
  const snap = tracker.snapshot()
  check(
    'hooks_declared_shape',
    loaded.providers.mock !== undefined &&
      JSON.stringify(snap.by_provider.mock) === JSON.stringify(['auth.checkin', 'auth.loader', 'auth.methods', 'auth.usage', 'config', 'provider.models']),
    JSON.stringify(snap.by_provider),
  )
  check(
    'hooks_actual_load_counts',
    snap.actual.config === 1 && snap.actual['provider.models'] === 1,
    JSON.stringify({ config: snap.actual.config, models: snap.actual['provider.models'] }),
  )
  // every known key (declared + actual-only) is present in the actual map
  const missing = snap.known.filter((k) => typeof snap.actual[k] !== 'number')
  check('hooks_actual_keys_complete', missing.length === 0 && snap.known.length === KNOWN_HOOKS.length + 1, JSON.stringify(missing))

  // 14b. the shared builder (server.ts's GET /admin/hooks) reports the same
  // surface with ok:true, and an empty context degrades to the zero shape.
  const { buildHooksSurface } = await import('../../hooks.ts')
  const surface = buildHooksSurface({ hooks: tracker })
  check(
    'hooks_surface_shape',
    surface.ok === true &&
      surface.declared.includes('auth.loader') &&
      typeof surface.by_provider.mock === 'object' &&
      typeof surface.actual['auth.checkin'] === 'number',
    JSON.stringify(surface).slice(0, 120),
  )
  const empty = buildHooksSurface({})
  check(
    'hooks_surface_empty_context',
    empty.ok === true && empty.declared.length === 0 && empty.actual['auth.loader'] === 0,
    JSON.stringify(empty).slice(0, 120),
  )
}

// ---- 15. D-57 identity reset passthrough（task-35）---------------------------------
{
  // 声明面：auth.identity 键存在即计入（不调用）
  const inst = await IdentityAuthPlugin({ client: {}, magpie: {} })
  check(
    'hooks_declare_identity',
    declaredHooksOf(inst).includes('auth.identity'),
    JSON.stringify(declaredHooksOf(inst)),
  )

  const loaded = {
    dir: '/tmp/ident',
    providers: {
      ident: {
        id: 'ident',
        name: 'Identity Provider',
        models: {},
        plugin: { file: 'mock-identity-plugin.mjs', exportName: 'IdentityAuthPlugin', providerId: 'ident', providerName: 'Identity Provider', plugin: inst },
      },
    },
    plugins: [],
    warnings: [],
  }
  const shared = { count: () => 0, list: () => [], get: () => undefined, save: async (p) => ({ provider: p, updated_at: Date.now() }) }
  // task-35②：假守卫——只记录 wrapFetch 的 (plugin, provider)，fetch 原样透传
  const wrapSeen = []
  const fakeGuard = {
    wrapFetch(params) {
      wrapSeen.push(params)
      return globalThis.fetch
    },
  }
  const registry = registryFromLoad(loaded, shared, { pluginId: 'mock-ident', egress: fakeGuard })
  const h = registry.list()[0]
  // 能力位：identity_request 恒有（钩子存在）；identity_os_switch 因 identityOS=true
  check(
    'identity_capabilities',
    h.capabilities().includes('identity_request') && h.capabilities().includes('identity_os_switch'),
    JSON.stringify(h.capabilities()),
  )

  const api5 = createAdminRouter({ dataDir, plugins: registry, checkinSettingsPath: settingsPath, egress: fakeGuard })
  const srv = createServer((req, res) => void api5.handle(req, res))
  await new Promise((r) => srv.listen(0, '127.0.0.1', r))
  const p5 = srv.address().port
  const post5 = (p, body) =>
    fetch(`http://127.0.0.1:${p5}${p}`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body ?? {}) })

  // ① 缺省 mode = request；钩子收到归一化后的 mode 与 accountId；守卫窗口生效
  setIdentityState({ identityCalls: 0, seen: [], osLocked: false })
  const r1 = await post5('/accounts/ident/identity/reset', {})
  const b1 = await r1.json()
  const st1 = getIdentityState()
  check(
    'identity_reset_request_ok',
    r1.status === 200 && b1.ok === true && st1.identityCalls === 1 && st1.seen[0]?.mode === 'request' && st1.seen[0]?.accountId === 'ident',
    `status=${r1.status} body=${JSON.stringify(b1)} seen=${JSON.stringify(st1)}`,
  )
  check(
    'identity_reset_egress_wrapped',
    wrapSeen.length === 1 && wrapSeen[0]?.plugin === 'mock-ident' && wrapSeen[0]?.provider === 'ident',
    JSON.stringify(wrapSeen),
  )

  // ② mode=os（identityOS 已声明）→ 透传到钩子并成功
  const r2 = await post5('/accounts/ident/identity/reset', { mode: '  OS  ' })
  const b2 = await r2.json()
  const st2 = getIdentityState()
  check(
    'identity_reset_os_ok',
    r2.status === 200 && b2.ok === true && st2.seen[1]?.mode === 'os',
    `status=${r2.status} body=${JSON.stringify(b2)} seen=${JSON.stringify(st2)}`,
  )

  // ③ os 被上游策略锁住：插件抛 409 → 桥原样透传状态码与消息
  setIdentityState({ osLocked: true })
  const r3 = await post5('/accounts/ident/identity/reset', { mode: 'os' })
  const b3 = await r3.json()
  check(
    'identity_reset_os_locked_passthrough',
    r3.status === 409 && b3.error?.type === 'identity_failed' && /locked by policy/.test(b3.error?.message ?? ''),
    `status=${r3.status} body=${JSON.stringify(b3)}`,
  )
  setIdentityState({ osLocked: false })

  // ④ 非法 mode → 400
  const r4 = await post5('/accounts/ident/identity/reset', { mode: 'bogus' })
  check('identity_reset_bad_mode_400', r4.status === 400, `status=${r4.status}`)

  // ⑤ 未知账号 → 404
  const r5 = await post5('/accounts/no-such-account/identity/reset', {})
  check('identity_reset_unknown_account_404', r5.status === 404, `status=${r5.status}`)

  srv.close()
  api5.close()

  // ⑥ 无 identity 钩子的插件：能力不声明，端点诚实回 501（主服务器上的 mock 即此形状）
  const r6 = await post('/accounts/mock-account/identity/reset', {})
  const b6 = await r6.json()
  check(
    'identity_reset_unsupported_501',
    r6.status === 501 && b6.error?.type === 'identity_unsupported',
    `status=${r6.status} body=${JSON.stringify(b6)}`,
  )
}

const failed = results.filter((r) => !r.ok)
console.log(`SUMMARY ${results.length - failed.length}/${results.length} passed`)
if (failed.length) {
  console.log(`FAILED ${failed.map((f) => f.name).join(', ')}`)
  process.exitCode = 1
}

if (process.argv.includes('--exit')) {
  server.close()
  api.close()
} else {
  // fresh day for the curl demo: first check-in is a claim again
  resetMockState()
  try {
    if (existsSync(join(dataDir, 'checkin-ledger.json'))) unlinkSync(join(dataDir, 'checkin-ledger.json'))
  } catch {}
  console.log('READY_FOR_CURL')
  console.log(`DATA_DIR ${dataDir}`)
  // stay up for curl; Ctrl-C or taskkill ends it
  process.on('SIGINT', () => {
    server.close()
    api.close()
    process.exit(0)
  })
}
