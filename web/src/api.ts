/**
 * Typed client for the AIWorkGate host admin API.
 *
 * All errors are normalised into ApiError so views can display
 * `{error:{message}}` bodies from the host and still survive plain-text
 * or network failures.
 */

export class ApiError extends Error {
  readonly status: number
  readonly type: string

  constructor(status: number, message: string, type = 'http_error') {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.type = type
  }
}

function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message
  if (error instanceof Error) return error.message
  return String(error)
}

export { errorMessage }

/**
 * Guard rejections (internal/webui) never come from a bad form field: the
 * browser either could not present the host-session cookie or was on another
 * origin. The cookie is minted only by POST /api/session, in exchange for the
 * one-shot launch token the host puts in the URL it opens, so a plain reload
 * cannot recover it: main.ts strips the token once spent, leaving the address
 * bar nothing to trade. Recovery means reopening the page from the tray, which
 * mints a fresh URL; a foreign origin must switch to the loopback address first.
 */
const GUARD_ERRORS: Record<string, string> = {
  cross_origin:
    '请求被拒绝：当前页面的来源与宿主管家不一致（cross_origin）。请用本机访问的地址（如 http://127.0.0.1:7865），从托盘重新打开管理页。',
  admin_session_required:
    '管理会话缺失或已过期（admin_session_required）。管理 Cookie 只能由宿主启动时生成的启动令牌换取，刷新页面不会重新获得；请从任务栏托盘的「打开管理页」重新打开 AIWorkGate。',
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  // FormData sets its own multipart boundary; overriding Content-Type would
  // make the browser send a body the server cannot parse.
  if (init.body !== undefined && !(init.body instanceof FormData)) {
    headers['Content-Type'] = 'application/json'
  }
  let response: Response
  try {
    response = await fetch(`/api${path}`, {
      ...init,
      // Same origin, so the host-session cookie earned by the launch token is
      // what authorises this call. Stated explicitly because the admin API
      // answers 403 without it.
      credentials: 'same-origin',
      headers: { ...headers, ...((init.headers as Record<string, string> | undefined) ?? {}) },
    })
  } catch {
    throw new ApiError(0, '无法连接宿主管家，请确认 AIWorkGate 正在运行。', 'network_error')
  }
  if (!response.ok) {
    let message = `请求失败（HTTP ${response.status}）`
    let type = 'http_error'
    try {
      const payload = (await response.json()) as {
        error?: { message?: string; type?: string } | string
        ok?: boolean
      }
      if (typeof payload?.error === 'string') message = payload.error
      if (payload && typeof payload.error === 'object' && payload.error.message) message = payload.error.message
      if (payload && typeof payload.error === 'object' && payload.error.type) type = payload.error.type
    } catch {
      /* non-JSON error body, keep the generic message */
    }
    if (Object.prototype.hasOwnProperty.call(GUARD_ERRORS, type)) {
      message = GUARD_ERRORS[type]
    }
    throw new ApiError(response.status, message, type)
  }
  if (response.status === 204) return undefined as T
  const text = await response.text()
  if (!text) return undefined as T
  try {
    return JSON.parse(text) as T
  } catch {
    return text as unknown as T
  }
}

/**
 * The tray opens the admin page with a one-shot launch token
 * (`http://127.0.0.1:7865/?t=<token>`). The token is the admin credential: it is
 * traded here for the host-session cookie and then stripped from the address bar
 * so a screenshot, a bookmark or a shared link cannot leak it.
 *
 * This MUST run before any other /api call: nothing else sets the cookie, so a
 * request attempted first just fails with admin_session_required.
 *
 * Returns true when a token was present and accepted.
 */
export async function exchangeLaunchToken(): Promise<boolean> {
  const params = new URLSearchParams(window.location.search)
  const token = params.get('t')
  if (!token) return false
  let response: Response
  try {
    response = await fetch('/api/session', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token }),
    })
  } catch {
    // Transport failure: keep the token so a reload can retry. Nothing was
    // granted, and the guard answers 403 on every /api call meanwhile.
    return false
  }
  if (!response.ok) {
    // The host rejected it (stale token after a restart). Strip it anyway: a
    // spent token in the address bar is exactly the leak this flow prevents.
    stripLaunchToken(params)
    return false
  }
  stripLaunchToken(params)
  return true
}

/** Removes ?t= from the address bar, preserving the rest of the URL. */
function stripLaunchToken(params: URLSearchParams): void {
  params.delete('t')
  const query = params.toString()
  window.history.replaceState(
    window.history.state,
    '',
    window.location.pathname + (query ? '?' + query : '') + window.location.hash,
  )
}

function qs(params: Record<string, string | number | boolean | undefined | null>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue
    search.set(key, String(value))
  }
  const encoded = search.toString()
  return encoded ? `?${encoded}` : ''
}

/* ------------------------------------------------------------------ */
/* Types mirrored from internal/model                                  */
/* ------------------------------------------------------------------ */

export interface HostStatus {
  version: string
  port: number
  data_dir: string
  a_configured: boolean
  a_running: boolean
  a_port?: number
  a_state?: 'ok' | 'unauthorized' | 'down'
  a_detail?: string
  plugins_running: number
  plugins_total: number
  /** Accounts with an explicit proxy assignment, and how many of those failed. */
  egress_assigned?: number
  egress_failed?: number
}

export interface Quota {
  remaining?: number
  total?: number
  reset_at?: number
  expires_at?: number
  /** Plan name reported by the plugin (S1). */
  plan?: string
  rate_limited?: boolean
  today_checked_in?: boolean
  checked_in_at?: number
}

export interface Identity {
  machine_id?: string
  device_id?: string
  session_id?: string
  source?: string
  updated_at?: number
}

export type EgressMode = 'direct' | 'proxy' | 'blocked'

export interface AccountEgress {
  mode?: EgressMode
  proxy_id?: string
  url?: string
}

export interface AccountCredential {
  present: boolean
  expires_at?: number
  refreshable?: boolean
}

export interface Account {
  id: string
  label: string
  enabled: boolean
  status: string
  added_at?: number
  quota: Quota
  identity?: Identity
  last_error?: string
  /** Optional host extensions used by the account page. */
  proxy_id?: string
  egress?: AccountEgress
  /* ---- S1 overlay (host merge view) ---- */
  plugin_id?: string
  alias?: string
  display_name?: string
  group_id?: string
  priority?: number
  weight?: number
  /** null/undefined = follow the plugin, true/false = forced by the host. */
  enabled_override?: boolean | null
  enabled_effective?: boolean
  /** Plugin offline: the row is the last known snapshot. */
  stale?: boolean
  credential?: AccountCredential
  /** Host health verdict after a failed check-in; "normal" when healthy. */
  status_kind?: string
  /** Unix seconds; 9999999999 means "until a new login". */
  cooldown_until?: number
  /** Human next-step hint (already localized by the host). */
  cooldown_reason?: string
  /**
   * Single merged health verdict (plugin status + host cooldown), resolved by
   * the host. state_source says which side decided: "plugin", "host" or "".
   */
  state: string
  state_source?: string
  state_detail?: string
  /** Verdict for an explicitly assigned proxy (absent for default/direct). */
  egress_health?: EgressHealth
}

export interface EgressHealth {
  status: 'ok' | 'failed' | 'unknown'
  proxy_id?: string
  proxy_label?: string
  /** Unix seconds when the evidence was observed. */
  at?: number
  age_seconds: number
  error?: string
}

export interface AccountsResponse {
  accounts: Account[]
}

export interface AccountGroup {
  id: string
  name: string
  color?: string
  sort?: number
  account_count?: number
}

export interface GroupsResponse {
  groups?: AccountGroup[]
}

export interface AccountRef {
  plugin_id: string
  account_id: string
}

export interface AccountRenameRequest {
  plugin_id: string
  account_id: string
  alias: string
}

export interface CheckinResultInfo {
  result?: string
  message?: string
  credits_before?: number
  credits_after?: number
  at?: number
}

export interface AccountCheckinResponse {
  ok?: boolean
  result?: CheckinResultInfo
}

export interface IdentityDetails {
  machine_id?: string
  device_id?: string
  session_id?: string
  source?: string
  updated_at?: number
  /** true when the host returned masked values. */
  masked?: boolean
}

export type IdentityDetailsData = IdentityDetails | { identity?: IdentityDetails } | null

export function normalizeIdentityDetails(data: IdentityDetailsData): IdentityDetails {
  if (!data) return {}
  if ('identity' in data && data.identity) return data.identity
  return data as IdentityDetails
}

export type BulkAction = 'checkin' | 'refresh' | 'enable' | 'disable' | 'delete' | 'group' | 'egress'

export interface BulkRequest {
  action: BulkAction
  items: AccountRef[]
  /** e.g. `{group_id}` for `group` or `{mode, proxy_id}` for `egress`. */
  params?: Record<string, string | number | boolean>
}

export interface BulkResultItem extends AccountRef {
  ok: boolean
  message?: string
}

export interface BulkResponse {
  ok?: boolean
  results?: BulkResultItem[]
}

export type AccountEgressBindingMode = 'default' | 'direct' | 'proxy'

export interface AccountEgressBinding {
  mode: AccountEgressBindingMode
  proxy_id?: string
}

export interface AccountSavePatch {
  plugin_id: string
  account_id: string
  alias?: string
  group_id?: string | null
  priority?: number
  weight?: number
  enabled_override?: boolean | null
  egress?: AccountEgressBinding
}

export interface GroupSave {
  id?: string
  name: string
  color?: string
  sort?: number
}

export interface MembershipSave {
  plugin_id: string
  account_id: string
  member: boolean
}

/** One transactional draft submitted by 保存配置. */
export interface AccountsSaveRequest {
  accounts: AccountSavePatch[]
  groups: GroupSave[]
  deleted_groups: string[]
  memberships: MembershipSave[]
}

export interface AccountsSaveResponse {
  ok?: boolean
  applied?: number
}

export interface LoginStateEntry {
  plugin_id: string
  name: string
  api_level?: number
  methods?: LoginMethod[]
  reads_local_state?: boolean
  requires_app?: boolean
  auto_refresh?: boolean
  /** manifest = declared in manifest v0.3, derived = inferred by the host. */
  source?: 'manifest' | 'derived' | string
  notes?: string
  checked_at?: number
}

export interface LoginStateResponse {
  plugins?: LoginStateEntry[]
  items?: LoginStateEntry[]
}

export function normalizeLoginState(data: LoginStateResponse | LoginStateEntry[] | null | undefined): LoginStateEntry[] {
  if (!data) return []
  if (Array.isArray(data)) return data
  if (Array.isArray(data.plugins)) return data.plugins
  if (Array.isArray(data.items)) return data.items
  return []
}

export interface ModelInfo {
  id: string
  name?: string
  context_length?: number
  tools?: boolean
  vision?: boolean
}

export interface Permissions {
  read_paths?: string[]
  launch_apps?: string[]
  network_domains?: string[]
  use_proxy?: boolean
  os_identity?: boolean
  require_admin?: boolean
  /** Declared by plugins that can write to the filesystem; absent otherwise. */
  writes_files?: boolean
}

export interface Manifest {
  id: string
  name: string
  version: string
  api_level: number
  description?: string
  author?: string
  homepage?: string
  os?: string[]
  capabilities?: string[]
  permissions?: Permissions
  models?: ModelInfo[]
  checkin?: { default_time?: string; supports_history?: boolean }
  quota?: { poll_minutes?: number }
  concurrency?: { suggested?: number }
  rules?: { packs?: string[] }
  egress?: { strict?: boolean }
}

export interface PluginStatus {
  manifest: Manifest
  dir: string
  enabled: boolean
  running: boolean
  healthy: boolean
  failed: boolean
  port?: number
  pid?: number
  started_at?: number
  installed_at?: number
  last_error?: string
  /** Host pool switch (absent when the host does not expose it yet). */
  upstream_enabled?: boolean
}

export type PluginAction = 'enable' | 'disable' | 'start' | 'stop' | 'uninstall'

/**
 * What the plugin says about itself through its own `GET /meta`. The host
 * proxies the plugin's JSON verbatim, so anything beyond these fields is
 * plugin-specific and shown as-is.
 */
export interface PluginMeta {
  id?: string
  name?: string
  version?: string
  api_level?: number
  capabilities?: string[]
  account_count?: number
  uptime_s?: number
  started_at?: number
  [key: string]: unknown
}

/**
 * Per-plugin settings (GET/PUT /api/plugins/{id}/settings). The persisted row
 * overrides the global config only where the user actually set a value:
 * concurrency 0 and strict "default" mean 跟随全局 (the host plugins.*
 * defaults win); "" for mode / checkin_time is the same "跟随全局" state.
 */
export type StrictState = 'on' | 'off' | 'default'

export interface PluginSettings {
  plugin_id: string
  enabled: boolean
  /** always | on_demand | "" ("" = 跟随全局). */
  mode: string
  /** 0 = 跟随全局, else an explicit 1..32 override. */
  concurrency: number
  /** HH:MM or "" ("" = 跟随全局). */
  checkin_time: string
  strict: StrictState
  effective?: {
    mode?: string
    concurrency?: number
    checkin_time?: string
    strict?: boolean
  }
  global?: {
    concurrency?: number
    startup_mode?: string
    strict_egress?: boolean
    checkin_default_time?: string
  }
}

export interface PluginSettingsUpdate {
  enabled?: boolean
  mode: string
  /** 0 = 跟随全局. */
  concurrency: number
  checkin_time: string
  strict: StrictState
}

export interface CheckinRecord {
  id?: number
  plugin_id?: string
  account_id: string
  account_label?: string
  date: string
  result: string
  credits_before?: number
  credits_after?: number
  message?: string
  at?: number
}

export interface CheckinAccountPayload {
  account_id?: string
  accountId?: string
  account_label?: string
  label?: string
  records?: CheckinRecord[]
  days?: Record<string, string | { result?: string; message?: string }>
}

export interface CheckinAccountRef {
  id: string
  label: string
}

export interface NormalizedCheckins {
  accounts: CheckinAccountRef[]
  records: CheckinRecord[]
}

export type CheckinData =
  | CheckinRecord[]
  | CheckinAccountPayload[]
  | {
      from?: string
      to?: string
      records?: CheckinRecord[]
      accounts?: CheckinAccountPayload[]
    }
  | null

export interface CheckinAllItem {
  plugin_id?: string
  plugin_name?: string
  account_id?: string
  account_label?: string
  result?: string
  message?: string
  error?: string
  credits_before?: number
  credits_after?: number
}

export type CheckinAllData =
  | CheckinAllItem[]
  | { results?: CheckinAllItem[]; items?: CheckinAllItem[] }
  | null

export interface Proxy {
  id: string
  name: string
  type: string
  host: string
  port: number
  username?: string
  password?: string
  enabled: boolean
  last_check_at?: number
  last_exit_ip?: string
  last_latency_ms?: number
  last_error?: string
  /** "system" for the row the host mirrors from Windows Internet Settings. */
  managed?: string
  managed_detail?: string
}

export interface ProxyCheckResult {
  ok?: boolean
  exit_ip?: string
  latency_ms?: number
  error?: string
  checked_at?: number
  [key: string]: unknown
}

export interface RulePair {
  from: string
  to: string
}

export interface StripPattern {
  pattern: string
}

/** Shared rule pack: literal replacements plus regex strips, executed by plugins. */
export interface RulePack {
  name: string
  description?: string
  rules?: RulePair[]
  strip?: StripPattern[]
}

export interface RulesResponse {
  packs: RulePack[]
  builtin: string[]
}

export interface RulesReloadResult {
  ok?: boolean
  reloaded?: string[]
  failed?: string[]
}

export interface EgressSave {
  plugin_id: string
  account_id: string
  mode: 'direct' | 'proxy' | 'default'
  proxy_id?: string
}

export interface EgressSaveResult {
  ok?: boolean
  reloaded?: boolean
  warning?: string
}

export interface IdentityReset {
  plugin_id: string
  account_id: string
  mode: 'request' | 'os'
  confirm: boolean
}

export interface OkResponse {
  ok?: boolean
}

/* ------------------------------------------------------------------ */
/* Account login (plugin contract §6)                                  */
/* ------------------------------------------------------------------ */

export type LoginMethod = 'api_key' | 'oauth' | 'ide_plugin' | 'paste_code' | 'local_state' | 'cookie'

export interface LoginField {
  name: string
  label: string
  type?: string
  required?: boolean
  placeholder?: string
  help?: string
}

export interface DetectedAccount {
  label: string
  hint?: string
}

/** What a plugin returns from POST /accounts/login. */
export interface LoginStart {
  session_id: string
  status: string
  method?: string
  url?: string
  code?: string
  instructions?: string
  fields?: LoginField[]
  account_id?: string
  error?: string
  /**
   * Plugins spell the "open this to log in" link differently: authorize_url
   * (OAuth), login_url (portal), verification_uri (device flow). The UI used to
   * read `url` only, so a plugin answering login_url produced a pending session
   * with no button — the user saw nothing to click. These are read by
   * normalizeLoginStart below, which fills `url` from whichever one is present.
   */
  authorize_url?: string
  login_url?: string
  verification_uri?: string
  verification_uri_complete?: string
}

/**
 * normalizeLoginStart fills `url` from any of the spellings a plugin may use for
 * the login link, so the "open login page" affordance appears regardless of the
 * field name the plugin picked. It never invents a URL.
 */
export function normalizeLoginStart(raw: LoginStart): LoginStart {
  if (raw.url) return raw
  const candidate =
    raw.authorize_url ||
    raw.login_url ||
    raw.verification_uri_complete ||
    raw.verification_uri ||
    ''
  if (!candidate) return raw
  return { ...raw, url: candidate }
}

/** What GET /accounts/login/{sid} and the submit endpoint return. */
export interface LoginStatus {
  status: string
  account_id?: string
  error?: string
  detected?: DetectedAccount[]
}

export interface RefreshResult {
  result: string
  expires_at?: number
  message?: string
  at?: number
}

export interface AccountRefreshResponse {
  ok?: boolean
  result: RefreshResult
}

export interface APIRequestLog {
  ts: string
  req_id?: string
  source: string
  model: string
  key_id?: string
  key_name?: string
  account?: string
  stream?: boolean
  status: number
  latency_ms: number
  ttfb_ms?: number
  prompt_tokens?: number
  completion_tokens?: number
  error?: string
  body_req?: string
  body_resp?: string
}

export type RequestLogsData =
  | APIRequestLog[]
  | {
      logs?: APIRequestLog[]
      items?: APIRequestLog[]
      records?: APIRequestLog[]
      total?: number
      error?: string
    }
  | null

/** Status buckets accepted by GET /api/logs/requests (status_class). */
export type RequestLogStatusClass = '2xx' | '4xx' | '5xx' | 'error'

/**
 * Filters of the request-log table and its CSV export. Model is a
 * case-insensitive substring; source (alias plugin_id), account_id and key_id
 * are exact. Stream is sent as a string because the HTTP query is typed.
 */
export type RequestLogFilterParams = {
  from?: string
  to?: string
  /** Keyword over req_id / model / source / account / error. */
  q?: string
  model?: string
  /** `plugin:<id>`, `custom:<id>` or `a`. */
  source?: string
  account_id?: string
  key_id?: string
  status_class?: RequestLogStatusClass
  stream?: 'true' | 'false'
  limit?: number
  offset?: number
}

export type RuntimeLogsData = string[] | { lines?: string[]; logs?: string[] } | null

/* ------------------------------------------------------------------ */
/* Usage (S2)                                                          */
/* ------------------------------------------------------------------ */

/** Shared usage filter: dimensions AND, values inside one dimension OR. */
export type UsageFilterParams = {
  /** Local date, YYYY-MM-DD. */
  from?: string
  /** Local date, YYYY-MM-DD. */
  to?: string
  /** Comma separated plugin ids. */
  plugin?: string
  /** Comma separated group ids. */
  group?: string
  /** Comma separated `plugin:account` refs. */
  account?: string
}

export interface UsageKpis {
  requests?: number
  errors?: number
  prompt_tokens?: number
  completion_tokens?: number
  credits?: number
  active_accounts?: number
}

export interface UsageSeriesPoint {
  day: string
  plugin_id: string
  requests?: number
  prompt_tokens?: number
  completion_tokens?: number
  credits?: number
}

export interface UsageSummary {
  kpis?: UsageKpis
  series?: UsageSeriesPoint[]
}

/** One account row from /usage/credits and /usage/expiry. */
export interface UsageAccountCredit {
  plugin_id?: string
  account_id?: string
  display_name?: string
  remaining?: number
  total?: number
  expires_at?: string | number
  plan?: string
  daily_credits?: number
  days_left?: number
}

export interface UsageCreditsResponse {
  accounts?: UsageAccountCredit[]
}

export interface UsageSession {
  ts?: string | number
  req_id?: string
  plugin_id?: string
  account_id?: string
  account_name?: string
  model?: string
  logical_model?: string
  conversation_id?: string
  prompt_tokens?: number
  completion_tokens?: number
  credits?: number
  ttfb_ms?: number
  latency_ms?: number
  status?: number
}

export interface UsageSessionsResponse {
  items?: UsageSession[]
  total?: number
  page?: number
  page_size?: number
}

export type UsageSessionsParams = UsageFilterParams & {
  page?: number
  page_size?: number
}

/** One day of /usage/latency. Null percentiles mean "no sample". */
export interface UsageLatencyDay {
  day: string
  requests?: number
  p50_ms?: number | null
  p95_ms?: number | null
  p50_ttfb_ms?: number | null
  p95_ttfb_ms?: number | null
}

/** One key of /usage/keys. Credits stay null when never reported. */
export interface UsageKeyRow {
  key_id: string
  name?: string
  requests?: number
  prompt_tokens?: number
  completion_tokens?: number
  cached_tokens?: number
  credits?: number | null
}

/** One model of /usage/models. Credits stay null when never reported. */
export interface UsageModelRow {
  model: string
  requests?: number
  prompt_tokens?: number
  completion_tokens?: number
  credits?: number | null
}

/** One model of /usage/latency/models. Null percentiles mean "no sample". */
export interface UsageModelLatencyRow {
  model: string
  requests?: number
  p50_ms?: number | null
  p95_ms?: number | null
  p50_ttfb_ms?: number | null
  p95_ttfb_ms?: number | null
}

/* ------------------------------------------------------------------ */
/* Unified log timeline (S2)                                           */
/* ------------------------------------------------------------------ */

export type LogTimelineType = 'all' | 'api' | 'runtime' | 'audit' | 'proxy' | 'connectivity'

export type LogBodyWhich = 'req' | 'resp'

export type LogTimelineParams = {
  from?: string
  to?: string
  type?: LogTimelineType
  /** Comma separated categories. */
  category?: string
  level?: string
  q?: string
  /** Opaque cursor from the previous page's `next_cursor`; omit for the newest page. */
  cursor?: string
  page_size?: number
}

export interface LogEvent {
  ts?: string | number
  type?: string
  category?: string
  level?: string
  source?: string
  summary?: string
  req_id?: string
  /** Row id of an audit/proxy/connectivity event; the key of GET /api/logs/raw. */
  row_id?: number
  account?: string
  model?: string
  status?: number
  latency_ms?: number
}

/** One tunnelled CONNECT through a local egress listener. */
export interface ProxyLog {
  id?: number
  ts?: string
  at?: number
  plugin_id?: string
  account_id?: string
  proxy_id?: string
  proxy_label?: string
  target_host?: string
  target_port?: number
  result?: 'ok' | 'denied' | 'failed' | string
  status?: number
  duration_ms?: number
  bytes_up?: number
  bytes_down?: number
  error?: string
}

export type LogProxyParams = {
  from?: string
  to?: string
  result?: string
  plugin?: string
  account?: string
  q?: string
  page?: number
  page_size?: number
}

export interface LogProxyResponse {
  items?: ProxyLog[]
  total?: number
  page?: number
  page_size?: number
}

export interface LogTimelineResponse {
  items?: LogEvent[]
  page_size?: number
  /** True when a `next_cursor` page exists behind the rows just returned. */
  has_more?: boolean
  next_cursor?: string
  /** Some source stopped reading early because its page budget filled up. */
  truncated?: boolean
  source_counts?: Record<string, number>
}

export interface LogBodyResponse {
  body?: string
  truncated?: boolean
  debug?: boolean
}

export type LogRawFile = 'host' | 'plugin' | 'api' | 'audit' | 'proxy' | 'connectivity'

/**
 * Query of GET /api/logs/raw: the single source line behind a timeline event.
 * A file source (`host`, `plugin`) is located by day + timestamp; a database
 * source (`api`, `audit`, `proxy`, `connectivity`) by its row id.
 */
export type LogRawParams = {
  file: LogRawFile
  /** Plugin id, log module, req_id or row id, depending on `file`. */
  id?: string
  /** RFC3339 timestamp of the event, used to locate the day file. */
  ts?: string
  /** Narrows a file match when several events share one second. */
  summary?: string
}

/**
 * The payload is one raw line. `line` is the primary field; `text`/`body` are
 * accepted because the endpoint is new and hosts may answer either shape.
 */
export interface LogRawResponse {
  line?: string
  text?: string
  body?: string
  truncated?: boolean
}

export type LogStreamParams = {
  type?: LogTimelineType
  category?: string
  level?: string
  q?: string
}

/* ------------------------------------------------------------------ */
/* Scheduler centre + per-plugin resource pool (S4)                    */
/* ------------------------------------------------------------------ */

export type PoolStrategy =
  | 'credit_first'
  | 'remaining_first'
  | 'random'
  | 'weighted3'
  | 'p2c'
  | 'fixed'

export type InterPoolMode = 'smart' | 'fixed'

export type SchedulerPreset = 'balanced' | 'freshness' | 'amortized' | 'latency' | 'fixed'

export const POOL_STRATEGY_LABELS: Record<PoolStrategy, string> = {
  credit_first: '积分先过期优先',
  remaining_first: '剩余积分多优先',
  random: '随机',
  weighted3: '三因子加权随机',
  p2c: 'P2C 随机二选一',
  fixed: '固定优先',
}

export const POOL_STRATEGY_KEYS = Object.keys(POOL_STRATEGY_LABELS) as PoolStrategy[]

export const INTER_POOL_LABELS: Record<InterPoolMode, string> = {
  smart: '智能调度',
  fixed: '固定优先',
}

/** Per-pool settings shared by the scheduler and the pool page. */
export interface PoolSettings {
  strategy: PoolStrategy
  /** 0 = unlimited. */
  account_concurrency: number
  /** Seconds, 0 = disabled. */
  pool_sticky_ttl: number
  /** Seconds, 0 = disabled. */
  conv_sticky_ttl: number
  /** Milliseconds, 0 = disabled. */
  hedge_ms: number
  default_reasoning: boolean
  background_downgrade: boolean
  longctx_downgrade: boolean
  /** "HH:MM" local time for the daily catalog sync; "" disables it. */
  model_sync_time: string
}

export function emptyPoolSettings(): PoolSettings {
  return {
    strategy: 'credit_first',
    // Mirrors the backend baselines in internal/app/scheduler_snapshot.go
    // (poolDefaults). It is only the fallback for a field the host did not
    // send, so a mismatch here would show one value in the form while another
    // one is actually in effect.
    // account_concurrency stays 0: unlimited is the safe default; a limit here
    // would throttle pools that work today.
    account_concurrency: 0,
    pool_sticky_ttl: 300,
    conv_sticky_ttl: 1800,
    hedge_ms: 8000,
    default_reasoning: false,
    background_downgrade: false,
    longctx_downgrade: false,
    model_sync_time: '',
  }
}

function numberOr(value: unknown, fallback: number): number {
  const numeric = typeof value === 'number' ? value : Number(value)
  return Number.isFinite(numeric) ? numeric : fallback
}

export function normalizePoolSettings(raw: Partial<PoolSettings> | null | undefined): PoolSettings {
  const base = emptyPoolSettings()
  if (!raw) return base
  const strategy =
    typeof raw.strategy === 'string' && POOL_STRATEGY_KEYS.includes(raw.strategy as PoolStrategy)
      ? (raw.strategy as PoolStrategy)
      : base.strategy
  return {
    strategy,
    account_concurrency: numberOr(raw.account_concurrency, base.account_concurrency),
    pool_sticky_ttl: numberOr(raw.pool_sticky_ttl, base.pool_sticky_ttl),
    conv_sticky_ttl: numberOr(raw.conv_sticky_ttl, base.conv_sticky_ttl),
    hedge_ms: numberOr(raw.hedge_ms, base.hedge_ms),
    default_reasoning: Boolean(raw.default_reasoning),
    background_downgrade: Boolean(raw.background_downgrade),
    longctx_downgrade: Boolean(raw.longctx_downgrade),
    model_sync_time: typeof raw.model_sync_time === 'string' ? raw.model_sync_time.trim() : '',
  }
}

export function samePoolSettings(a: PoolSettings, b: PoolSettings): boolean {
  return (
    a.strategy === b.strategy &&
    a.account_concurrency === b.account_concurrency &&
    a.pool_sticky_ttl === b.pool_sticky_ttl &&
    a.conv_sticky_ttl === b.conv_sticky_ttl &&
    a.hedge_ms === b.hedge_ms &&
    a.default_reasoning === b.default_reasoning &&
    a.background_downgrade === b.background_downgrade &&
    a.longctx_downgrade === b.longctx_downgrade &&
    a.model_sync_time === b.model_sync_time
  )
}

export interface SchedulerGlobalSettings {
  inter_pool: InterPoolMode
  defaults: PoolSettings
}

export interface SchedulerPoolSettings extends PoolSettings {
  plugin_id: string
  name?: string
  upstream_enabled: boolean
  pool_priority: number
}

export interface ModelBindingSource {
  plugin_id: string
  model: string
}

export interface ModelBinding {
  logical_model: string
  display_name?: string
  items: ModelBindingSource[]
}

export interface ModelBindingSave {
  display_name: string
  items: ModelBindingSource[]
}

export interface SchedulerHealth {
  plugin_id: string
  circuit_open: boolean
}

export interface SchedulerState {
  version: number
  global: SchedulerGlobalSettings
  pools: SchedulerPoolSettings[]
  models: ModelBinding[]
  sticky_count: number
  health: SchedulerHealth[]
}

export interface SchedulerPresetResult {
  ok?: boolean
  version?: number
  message?: string
  applied?: string[]
  changes?: string[] | string
}

export function normalizeSchedulerState(
  raw: Partial<SchedulerState> | null | undefined,
): SchedulerState {
  const pools = Array.isArray(raw?.pools) ? raw.pools : []
  const models = Array.isArray(raw?.models) ? raw.models : []
  const health = Array.isArray(raw?.health) ? raw.health : []
  return {
    version: numberOr(raw?.version, 0),
    global: {
      inter_pool: raw?.global?.inter_pool === 'fixed' ? 'fixed' : 'smart',
      defaults: normalizePoolSettings(raw?.global?.defaults),
    },
    pools: pools.map((pool) => ({
      ...normalizePoolSettings(pool),
      plugin_id: pool.plugin_id,
      name: pool.name || pool.plugin_id,
      upstream_enabled: pool.upstream_enabled !== false,
      pool_priority: numberOr(pool.pool_priority, 10),
    })),
    models: models.map((model) => ({
      logical_model: model.logical_model,
      display_name: model.display_name || model.logical_model,
      items: Array.isArray(model.items) ? model.items : [],
    })),
    sticky_count: numberOr(raw?.sticky_count, 0),
    health: health.map((item) => ({
      plugin_id: item.plugin_id,
      circuit_open: Boolean(item.circuit_open),
    })),
  }
}

export function normalizeModelBindings(
  data: ModelBinding[] | { models?: ModelBinding[]; items?: ModelBinding[] } | null | undefined,
): ModelBinding[] {
  if (!data) return []
  const list = Array.isArray(data) ? data : (data.models ?? data.items ?? [])
  return list.map((model) => ({
    logical_model: model.logical_model,
    display_name: model.display_name || model.logical_model,
    items: Array.isArray(model.items) ? model.items : [],
  }))
}

export interface SchedulerExplainRequest {
  model: string
  conversation_id?: string
}

export interface SchedulerDecision {
  plugin_id?: string
  account_id?: string
  model?: string
  logical_model?: string
  sticky?: boolean
  reasons?: string[]
}

export interface SchedulerCandidate {
  plugin_id?: string
  account_id?: string
  score?: number
  chosen?: boolean
  rejected_reason?: string
}

export interface SchedulerExplainResponse {
  decision?: SchedulerDecision
  candidates?: SchedulerCandidate[]
}

export interface StickyBinding {
  scope: string
  key: string
  plugin_id: string
  account_id: string
  expires_at?: number | string
}

export interface StickyResponse {
  items?: StickyBinding[]
}

export interface PoolCredits {
  total: number
  healthy: number
  today_used: number
  pool_size: number
}

export interface PoolMember {
  account_id: string
  display_name?: string
  enabled?: boolean
  status?: string
  remaining?: number
  member?: boolean
}

export interface PoolCatalogModel {
  model_id: string
  display_name?: string
  vendor?: string
  multiplier?: number
  context_length?: number
  vision?: boolean
  thinking?: string[]
  free?: boolean
  enabled?: boolean
  synced_at?: number
}

/**
 * Model-catalog sync lifecycle of a pool (A: ApiService.tsx:855-916,
 * models_sync.rs:331-359). `next_retry_at` is 0 when nothing is queued.
 */
export interface ModelSyncStatus {
  /** Unix seconds; 0 = never synced. */
  last_success_at?: number
  /** Unix seconds of the newest attempt, successful or not. */
  last_attempt_at?: number
  /** Short, credential-free error of the newest failed attempt. */
  last_error?: string
  failure_count?: number
  /** Unix seconds; 0 = no pending retry. */
  next_retry_at?: number
  running?: boolean
}

export interface PoolDetail {
  plugin_id: string
  name: string
  api_level: number
  upstream_enabled: boolean
  credits: PoolCredits
  settings: PoolSettings
  members: PoolMember[]
  catalog: PoolCatalogModel[]
  /** Absent on hosts that have not wired the lifecycle summary yet. */
  model_sync?: ModelSyncStatus | null
}

export type PoolCatalogData =
  | PoolCatalogModel[]
  | { catalog?: PoolCatalogModel[]; items?: PoolCatalogModel[] }
  | null

export interface PoolSyncResult {
  ok?: boolean
  at?: number
  synced_at?: number
  model_count?: number
  count?: number
  message?: string
  error?: string
}

export function normalizePoolDetail(
  raw: Partial<PoolDetail> | null | undefined,
  fallbackId = '',
): PoolDetail {
  const credits = raw?.credits
  return {
    plugin_id: raw?.plugin_id || fallbackId,
    name: raw?.name || raw?.plugin_id || fallbackId,
    api_level: numberOr(raw?.api_level, 0),
    upstream_enabled: raw?.upstream_enabled !== false,
    credits: {
      total: numberOr(credits?.total, 0),
      healthy: numberOr(credits?.healthy, 0),
      today_used: numberOr(credits?.today_used, 0),
      pool_size: numberOr(credits?.pool_size, 0),
    },
    settings: normalizePoolSettings(raw?.settings),
    members: Array.isArray(raw?.members)
      ? raw.members.map((member) => ({
          account_id: member.account_id,
          display_name: member.display_name || member.account_id,
          enabled: member.enabled !== false,
          status: member.status || '',
          remaining: numberOr(member.remaining, 0),
          member: Boolean(member.member),
        }))
      : [],
    catalog: normalizeCatalog(raw?.catalog ?? null),
    model_sync: normalizeModelSyncStatus(raw?.model_sync),
  }
}

/** Missing/partial lifecycle payloads become 0/"", never NaN. */
export function normalizeModelSyncStatus(
  raw: Partial<ModelSyncStatus> | null | undefined,
): ModelSyncStatus | null {
  if (!raw || typeof raw !== 'object') return null
  return {
    last_success_at: numberOr(raw.last_success_at, 0),
    last_attempt_at: numberOr(raw.last_attempt_at, 0),
    last_error: raw.last_error || '',
    failure_count: numberOr(raw.failure_count, 0),
    next_retry_at: numberOr(raw.next_retry_at, 0),
    running: Boolean(raw.running),
  }
}

export function normalizeCatalog(data: PoolCatalogData): PoolCatalogModel[] {
  if (!data) return []
  const list = Array.isArray(data) ? data : (data.catalog ?? data.items ?? [])
  return list.map((model) => ({
    model_id: model.model_id,
    display_name: model.display_name || model.model_id,
    vendor: model.vendor || '',
    multiplier: numberOr(model.multiplier, 0),
    context_length: numberOr(model.context_length, 0),
    vision: Boolean(model.vision),
    thinking: Array.isArray(model.thinking) ? model.thinking : [],
    free: Boolean(model.free),
    enabled: model.enabled !== false,
    synced_at: numberOr(model.synced_at, 0),
  }))
}

/* ------------------------------------------------------------------ */
/* Model catalog human overrides (plan 2026-10-21 §3)                  */
/* ------------------------------------------------------------------ */

/**
 * One model's hand-maintained metadata. A `null` field means "not overridden":
 * the layer stored by the host only carries the fields the user pinned, and
 * `overridden` names them so the table can badge "人工".
 */
export interface ModelOverrideRecord {
  display_name?: string | null
  vendor?: string | null
  context_length?: number | null
  vision?: boolean | null
  thinking?: string[] | null
  multiplier?: number | null
  free?: boolean | null
  updated_at?: number
  /** Pinned field names, e.g. `["vendor", "thinking"]`. */
  overridden?: string[]
}

/**
 * PUT body for one model. An absent key keeps the stored value; an explicit
 * `null` clears that field (and clearing every field removes the override).
 */
export type ModelOverridePatch = Partial<{
  display_name: string | null
  vendor: string | null
  context_length: number | null
  vision: boolean | null
  thinking: string[] | null
  multiplier: number | null
  free: boolean | null
}>

export interface ModelOverridesResponse {
  plugin_id?: string
  overrides?: Record<string, ModelOverrideRecord>
}

export interface ModelOverrideSaveResponse {
  ok?: boolean
  plugin_id?: string
  model?: string
  override?: ModelOverrideRecord | null
}

export interface ModelOverrideDeleteResponse {
  ok?: boolean
  plugin_id?: string
  model?: string
  removed?: boolean
}

/**
 * Normalises the `overrides` map: every field becomes `null` when the host (or
 * an older API) omits it, so the view can tell "follow the plugin" apart from
 * "pinned to false/0".
 */
export function normalizeModelOverrides(
  raw: ModelOverridesResponse | null | undefined,
): Record<string, ModelOverrideRecord> {
  const out: Record<string, ModelOverrideRecord> = {}
  for (const [model, entry] of Object.entries(raw?.overrides ?? {})) {
    if (!entry) continue
    out[model] = {
      display_name: entry.display_name ?? null,
      vendor: entry.vendor ?? null,
      context_length:
        typeof entry.context_length === 'number' && entry.context_length > 0
          ? entry.context_length
          : null,
      vision: typeof entry.vision === 'boolean' ? entry.vision : null,
      thinking: Array.isArray(entry.thinking) ? entry.thinking : null,
      multiplier: typeof entry.multiplier === 'number' ? entry.multiplier : null,
      free: typeof entry.free === 'boolean' ? entry.free : null,
      updated_at: numberOr(entry.updated_at, 0),
      overridden: Array.isArray(entry.overridden) ? entry.overridden : [],
    }
  }
  return out
}

export interface ClientKey {
  id: string
  name: string
  prefix: string
  enabled: boolean
  created_at: number
  last_used_at?: number
  /** Model allow-list; empty/null means every model. Entries are exact ids or `plugin/*`. */
  model_scope?: string[] | null
  /** Plugin id the key is bound to; '' means every pool. */
  bind_pool?: string
  /** Daily request quota; 0 means unlimited. */
  daily_quota?: number
  /** Requests counted for the current local day. */
  used_today?: number
  /** Remaining requests today; -1 when unlimited. */
  quota_left?: number
}

/** Body of POST /keys (name required) and PUT /keys/{id} (any subset). */
export interface KeyScopeInput {
  name?: string
  enabled?: boolean
  model_scope?: string[]
  bind_pool?: string
  daily_quota?: number
}

export interface CreateKeyResponse {
  secret: string
  key: ClientKey
}

/**
 * User-defined OpenAI-compatible upstream model (Settings → 自定义模型).
 * `name` is the model id clients request; the API key is write-only and only
 * its presence (`key_set`) is returned.
 */
export type CustomProtocol = 'openai' | 'anthropic' | 'gemini'
export type CustomInputType = 'text' | 'image'
export type CustomReasoningTier = 'off' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh' | 'max' | 'auto'

/** One (endpoint, API key, protocol) connection owning many models. */
export interface CustomConnection {
  id: string
  label: string
  protocol: CustomProtocol
  base_url: string
  enabled: boolean
  key_set: boolean
  model_count?: number
  created_at: number
  updated_at: number
}

export interface CustomConnectionInput {
  label?: string
  protocol: CustomProtocol
  base_url: string
  api_key: string
  enabled?: boolean
}

export interface CustomConnectionPatch {
  label?: string
  protocol?: CustomProtocol
  base_url?: string
  api_key?: string
  enabled?: boolean
}

export interface RemoteModel {
  id: string
  name: string
  context_window?: number
  max_output_tokens?: number
  owned_by?: string
}

export interface ImportModelInput {
  upstream_model: string
  name?: string
  context_window?: number
  max_output_tokens?: number
  input_types?: CustomInputType[]
  reasoning_tiers?: CustomReasoningTier[]
  default_reasoning?: string
}

export interface ImportSkip {
  upstream_model: string
  reason: string
}

export interface CustomModel {
  id: string
  connection_id: string
  name: string
  base_url: string
  protocol?: CustomProtocol
  upstream_model: string
  enabled: boolean
  key_set: boolean
  created_at: number
  updated_at: number
  context_window: number
  max_output_tokens: number
  input_types: CustomInputType[]
  reasoning_tiers: CustomReasoningTier[]
  default_reasoning: string
  /** True when a plugin model or logical binding already owns this id. */
  conflict?: boolean
}

/** Body of POST /custom-models. connection_id is the normal path; base_url and
 * api_key remain for clients that predate connections. */
export interface CustomModelInput {
  connection_id?: string
  name: string
  base_url?: string
  upstream_model: string
  api_key?: string
  enabled?: boolean
  context_window?: number
  max_output_tokens?: number
  input_types?: CustomInputType[]
  reasoning_tiers?: CustomReasoningTier[]
  default_reasoning?: string
}

/**
 * Body of PUT /custom-models/{id}: any subset. An absent api_key keeps the
 * stored key, "" clears it. connection_id and upstream_model are immutable.
 */
export interface CustomModelPatch {
  name?: string
  enabled?: boolean
  api_key?: string
  context_window?: number
  max_output_tokens?: number
  input_types?: CustomInputType[]
  reasoning_tiers?: CustomReasoningTier[]
  default_reasoning?: string
}

export interface CustomModelTestResult {
  ok: boolean
  latency_ms: number
  error?: string
}

/* ------------------------------------------------------------------ */
/* Connectivity diagnostics                                            */
/* ------------------------------------------------------------------ */

export type DiagnosticsKind = 'a' | 'plugin' | 'custom_model'

/**
 * One row of GET /api/diagnostics/probes (newest first). `target` is the
 * human label (`a`, a plugin name or a custom-model name); `target_id` is the
 * machine id (plugin id for plugin probes, custom-model id for custom models).
 */
export interface DiagnosticsProbe {
  id: string
  kind: DiagnosticsKind
  target: string
  target_id: string
  ok: boolean
  /** Upstream HTTP status; 0 when no response was received. */
  status: number
  latency_ms: number
  /** Redacted, credential-free explanation; "" on success. */
  detail: string
  /** Unix seconds. */
  at: number
}

/**
 * POST /api/diagnostics/probe. A failed probe is still HTTP 200 with
 * ok:false, so a rejection here means the host itself could not be reached.
 */
export interface ProbeResult {
  ok: boolean
  kind: DiagnosticsKind
  target: string
  target_id: string
  /** Upstream HTTP status; 0 when no response was received. */
  status: number
  latency_ms: number
  detail: string
  /** Stable failure code (for example `timeout`); absent on success. */
  error_code?: string
  /** Unix seconds. */
  at: number
  probe_id: string
}

/** Target fields of one probe; `kind` selects which ones are sent. */
export interface ProbeTarget {
  plugin_id?: string
  model?: string
  custom_model_id?: string
}

/** Query of GET /api/diagnostics/probes. */
export interface ProbeQuery {
  /** Newest-first row count; the host defaults to 20. */
  limit?: number
  kind?: DiagnosticsKind | ''
  target_id?: string
}

/**
 * Host settings mirror internal/config exactly (nested groups, snake_case JSON
 * tags). The UI edits the known fields and deep-merges them over the last raw
 * payload so unknown keys are round-tripped untouched.
 */
export interface AConfig {
  base_url: string
  exe_path: string
  auto_start: boolean
  model_cache_minutes: number
}

export interface ProxyConfig {
  enabled: boolean
  http: string
  https: string
  no_proxy: string[]
}

/**
 * Host egress accounting switches (config `egress`), separate from ProxyConfig
 * which is the upstream proxy plugins are told to use. `direct_logging` puts a
 * loopback CONNECT listener in front of direct-mode egress so it also lands in
 * the proxy traffic log; it is off by default because it adds one hop.
 */
export interface EgressConfig {
  direct_logging: boolean
}

export interface PluginsConfig {
  concurrency: number
  /** "always" or "on_demand". */
  startup_mode: string
  strict_egress: boolean
}

export interface CheckinConfig {
  default_time: string
  retry_count: number
  retry_backoff_seconds: number[]
}

export interface RetentionConfig {
  api_request_days: number
  body_preview_days: number
  checkin_days: number
  host_log_days: number
  /** Separate window for the proxy traffic log: one row per CONNECT. */
  proxy_log_days: number
}

export interface LoggingConfig {
  debug_body: boolean
}

/**
 * Public /v1 surface switches. CorsAllowOrigins lists browser origins allowed
 * to read gateway responses; each entry is "*" or an http(s) origin, and an
 * empty list means no browser cross-origin access (preflight is rejected).
 */
export interface GatewayConfig {
  cors_allow_origins?: string[]
}

/**
 * Notification channels / events (ported from A, MIT:
 * src-tauri/src/notify.rs:11-137). The webhook URL is a secret and never
 * appears in any of these payloads.
 */
export type NotifyChannel = 'generic' | 'bark' | 'serverchan' | 'wecom'

export type NotifyEventKind = 'checkin_fail' | 'checkin_done' | 'account_dead' | 'quota_low'

/** Non-secret half of the notification policy (config.json `notify`). */
export interface NotifyConfig {
  enabled: boolean
  channel: NotifyChannel
  events: NotifyEventKind[]
  min_interval_seconds: number
}

/** GET/PUT /api/notify. The webhook URL is write-only: only `url_set` is read. */
export interface NotifyStatus {
  enabled: boolean
  channel: NotifyChannel
  url_set: boolean
  events: NotifyEventKind[]
  min_interval_seconds: number
  /** Unix seconds; 0 when nothing has been delivered in this process. */
  last_sent_at: number
  /** Redacted host-side error, empty after a success. */
  last_error: string
}

/** Partial update: an absent field keeps the stored value; url "" clears it. */
export interface NotifyUpdate {
  enabled?: boolean
  channel?: NotifyChannel
  url?: string
  events?: NotifyEventKind[]
  min_interval_seconds?: number
}

export interface Settings {
  version: number
  port: number
  listen_lan: boolean
  /** Self-registration under HKCU Run on the next host start. */
  autostart: boolean
  a: AConfig
  proxy: ProxyConfig
  egress: EgressConfig
  plugins: PluginsConfig
  checkin: CheckinConfig
  retention: RetentionConfig
  logging: LoggingConfig
  gateway: GatewayConfig
  notify: NotifyConfig
  [key: string]: unknown
}

export const DEFAULT_SETTINGS: Settings = {
  version: 1,
  port: 7865,
  listen_lan: false,
  autostart: true,
  a: {
    base_url: 'http://127.0.0.1:7864',
    exe_path: '',
    auto_start: false,
    model_cache_minutes: 10,
  },
  proxy: {
    enabled: false,
    http: '',
    https: '',
    no_proxy: ['127.0.0.1', 'localhost'],
  },
  egress: {
    direct_logging: false,
  },
  plugins: {
    concurrency: 2,
    startup_mode: 'always',
    strict_egress: false,
  },
  checkin: {
    default_time: '09:00',
    retry_count: 2,
    retry_backoff_seconds: [5, 20],
  },
  retention: {
    api_request_days: 30,
    body_preview_days: 3,
    checkin_days: 180,
    host_log_days: 30,
    proxy_log_days: 7,
  },
  logging: {
    debug_body: false,
  },
  gateway: {
    cors_allow_origins: [],
  },
  notify: {
    enabled: false,
    channel: 'generic',
    events: ['checkin_fail', 'checkin_done', 'account_dead', 'quota_low'],
    min_interval_seconds: 600,
  },
}

/* ------------------------------------------------------------------ */
/* Normalisers: the host funcs return `any` for list endpoints         */
/* ------------------------------------------------------------------ */

export function normalizeAccounts(data: AccountsResponse | Account[] | null | undefined): Account[] {
  if (!data) return []
  if (Array.isArray(data)) return data
  return Array.isArray(data.accounts) ? data.accounts : []
}

export function normalizeCheckins(data: CheckinData): NormalizedCheckins {
  const accounts = new Map<string, CheckinAccountRef>()
  const records: CheckinRecord[] = []

  const ensure = (id: string, label?: string) => {
    const key = id || 'unknown'
    const existing = accounts.get(key)
    if (!existing) {
      accounts.set(key, { id: key, label: label || key })
      return
    }
    if (label && existing.label === existing.id) existing.label = label
  }

  const pushAccount = (raw: CheckinAccountPayload) => {
    const id = raw.account_id || raw.accountId || 'unknown'
    const label = raw.account_label || raw.label || id
    ensure(id, label)
    if (Array.isArray(raw.records)) {
      for (const record of raw.records) {
        const accountId = record.account_id || id
        records.push({
          ...record,
          account_id: accountId,
          account_label: record.account_label || label,
        })
        ensure(accountId, record.account_label || label)
      }
    }
    if (raw.days && typeof raw.days === 'object') {
      for (const [date, value] of Object.entries(raw.days)) {
        if (typeof value === 'string') {
          records.push({ account_id: id, account_label: label, date, result: value })
        } else if (value) {
          records.push({ account_id: id, account_label: label, date, result: value.result || '', message: value.message })
        }
      }
    }
  }

  if (Array.isArray(data)) {
    for (const item of data) {
      const candidate = item as CheckinRecord & CheckinAccountPayload
      if (typeof candidate.date === 'string' && !candidate.records && !candidate.days) {
        records.push(candidate)
        ensure(candidate.account_id || 'unknown', candidate.account_label)
      } else {
        pushAccount(candidate)
      }
    }
  } else if (data) {
    for (const record of data.records ?? []) {
      records.push(record)
      ensure(record.account_id || 'unknown', record.account_label)
    }
    for (const account of data.accounts ?? []) pushAccount(account)
  }

  return {
    accounts: Array.from(accounts.values()).sort((a, b) => a.label.localeCompare(b.label, 'zh-CN')),
    records,
  }
}

export function normalizeCheckinAll(data: CheckinAllData): CheckinAllItem[] {
  if (!data) return []
  if (Array.isArray(data)) return data
  if (Array.isArray(data.results)) return data.results
  if (Array.isArray(data.items)) return data.items
  return []
}

export function normalizeRequestLogs(data: RequestLogsData): APIRequestLog[] {
  if (!data) return []
  if (Array.isArray(data)) return data
  if (Array.isArray(data.logs)) return data.logs
  if (Array.isArray(data.items)) return data.items
  if (Array.isArray(data.records)) return data.records
  return []
}

export function normalizeRuntimeLogs(data: RuntimeLogsData): string[] {
  if (!data) return []
  if (Array.isArray(data)) {
    return data.flatMap((line) => String(line).split(/\r?\n/)).filter((line) => line.length > 0)
  }
  if (Array.isArray(data.lines)) return normalizeRuntimeLogs(data.lines)
  if (Array.isArray(data.logs)) return normalizeRuntimeLogs(data.logs)
  return []
}

/* ------------------------------------------------------------------ */
/* Endpoints                                                           */
/* ------------------------------------------------------------------ */

/**
 * Accounts stay a callable (`api.accounts(plugin)`) so existing views keep
 * working, with the account-scoped mutations attached as methods.
 */
const accountsApi = Object.assign(
  (plugin: string): Promise<AccountsResponse | Account[]> =>
    request<AccountsResponse | Account[]>(`/accounts${qs({ plugin })}`),
  {
    /** Filtered merge view used by the account centre (`plugin`/`group`/`q`). */
    query(params: { plugin?: string; group?: string; q?: string } = {}): Promise<AccountsResponse | Account[]> {
      return request<AccountsResponse | Account[]>(`/accounts${qs(params)}`)
    },
    rename(plugin: string, account: string, alias: string): Promise<OkResponse> {
      const body: AccountRenameRequest = { plugin_id: plugin, account_id: account, alias }
      return request<OkResponse>('/accounts/rename', {
        method: 'POST',
        body: JSON.stringify(body),
      })
    },
    checkin(plugin: string, account: string): Promise<AccountCheckinResponse> {
      return request<AccountCheckinResponse>('/accounts/checkin', {
        method: 'POST',
        body: JSON.stringify({ plugin_id: plugin, account_id: account }),
      })
    },
    bulk(payload: BulkRequest): Promise<BulkResponse> {
      return request<BulkResponse>('/accounts/bulk', {
        method: 'POST',
        body: JSON.stringify(payload),
      })
    },
    save(payload: AccountsSaveRequest): Promise<AccountsSaveResponse> {
      return request<AccountsSaveResponse>('/accounts/save', {
        method: 'POST',
        body: JSON.stringify(payload),
      })
    },
    identityDetails(
      plugin: string,
      account: string,
      reveal: boolean,
    ): Promise<IdentityDetailsData> {
      return request<IdentityDetailsData>('/accounts/identity/details', {
        method: 'POST',
        body: JSON.stringify({ plugin_id: plugin, account_id: account, reveal }),
      })
    },
    loginState(): Promise<LoginStateResponse> {
      return request<LoginStateResponse>('/accounts/login-state')
    },
    setEgress(
      plugin: string,
      account: string,
      mode: EgressSave['mode'],
      proxyId?: string,
    ): Promise<EgressSaveResult> {
      const body: EgressSave = { plugin_id: plugin, account_id: account, mode }
      if (mode === 'proxy' && proxyId) body.proxy_id = proxyId
      return request<EgressSaveResult>('/accounts/egress', {
        method: 'PUT',
        body: JSON.stringify(body),
      })
    },
    resetIdentity(
      plugin: string,
      account: string,
      mode: IdentityReset['mode'],
      confirm: boolean,
    ): Promise<OkResponse> {
      const body: IdentityReset = { plugin_id: plugin, account_id: account, mode, confirm }
      return request<OkResponse>('/accounts/identity/reset', {
        method: 'POST',
        body: JSON.stringify(body),
      })
    },
    async loginStart(plugin: string, method: LoginMethod | ''): Promise<LoginStart> {
      const raw = await request<LoginStart>('/accounts/login', {
        method: 'POST',
        body: JSON.stringify({ plugin_id: plugin, method }),
      })
      // Normalize here so every caller sees `url` filled regardless of the field
      // name the plugin chose (authorize_url / login_url / verification_uri).
      return normalizeLoginStart(raw)
    },
    loginPoll(plugin: string, sessionId: string): Promise<LoginStatus> {
      return request<LoginStatus>(`/accounts/login/${encodeURIComponent(sessionId)}${qs({ plugin })}`)
    },
    loginSubmit(
      plugin: string,
      sessionId: string,
      values: Record<string, string>,
    ): Promise<LoginStatus> {
      return request<LoginStatus>(`/accounts/login/${encodeURIComponent(sessionId)}/submit`, {
        method: 'POST',
        body: JSON.stringify({ plugin_id: plugin, values }),
      })
    },
    loginCancel(plugin: string, sessionId: string): Promise<OkResponse> {
      return request<OkResponse>(`/accounts/login/${encodeURIComponent(sessionId)}${qs({ plugin })}`, {
        method: 'DELETE',
      })
    },
    patchAccount(plugin: string, account: string, patch: Record<string, unknown>): Promise<OkResponse> {
      return request<OkResponse>('/accounts/patch', {
        method: 'POST',
        body: JSON.stringify({ plugin_id: plugin, account_id: account, patch }),
      })
    },
    deleteAccount(plugin: string, account: string): Promise<OkResponse> {
      return request<OkResponse>('/accounts/delete', {
        method: 'POST',
        body: JSON.stringify({ plugin_id: plugin, account_id: account }),
      })
    },
    refreshAccount(plugin: string, account: string): Promise<AccountRefreshResponse> {
      return request<AccountRefreshResponse>('/accounts/refresh', {
        method: 'POST',
        body: JSON.stringify({ plugin_id: plugin, account_id: account }),
      })
    },
  },
)

export const api = {
  status(): Promise<HostStatus> {
    return request<HostStatus>('/status')
  },

  getSettings(): Promise<Record<string, unknown>> {
    return request<Record<string, unknown>>('/settings')
  },

  saveSettings(settings: Record<string, unknown>): Promise<void> {
    return request<void>('/settings', { method: 'PUT', body: JSON.stringify(settings) })
  },

  aKey: {
    status(): Promise<{ configured: boolean }> {
      return request<{ configured: boolean }>('/secrets/a-key')
    },
    set(key: string): Promise<void> {
      return request<void>('/secrets/a-key', { method: 'PUT', body: JSON.stringify({ key }) })
    },
  },

  setLANPassword(password: string): Promise<void> {
    return request<void>('/secrets/lan-password', { method: 'PUT', body: JSON.stringify({ password }) })
  },

  plugins: {
    list(): Promise<PluginStatus[]> {
      return request<PluginStatus[]>('/plugins')
    },
    /**
     * Upload a plugin archive. The host only accepts a multipart `file` field
     * (a server-side path is rejected: it let any caller install anything the
     * gateway user could read, including over UNC). `enabled` defaults to false,
     * so a fresh install lands stopped and the user starts it explicitly.
     */
    install(file: File, enabled = false): Promise<void> {
      const body = new FormData()
      body.append('file', file)
      body.append('enabled', enabled ? 'true' : 'false')
      return request<void>('/plugins/install', { method: 'POST', body })
    },
    action(id: string, action: PluginAction): Promise<void> {
      return request<void>(`/plugins/${encodeURIComponent(id)}/${action}`, { method: 'POST', body: '{}' })
    },
    settings(id: string): Promise<PluginSettings> {
      return request<PluginSettings>(`/plugins/${encodeURIComponent(id)}/settings`)
    },
    /** The plugin's own self-report (`GET /meta`), proxied by the host. */
    meta(id: string): Promise<PluginMeta> {
      return request<PluginMeta>(`/plugins/${encodeURIComponent(id)}/meta`)
    },
    updateSettings(id: string, payload: PluginSettingsUpdate): Promise<OkResponse> {
      return request<OkResponse>(`/plugins/${encodeURIComponent(id)}/settings`, {
        method: 'PUT',
        body: JSON.stringify(payload),
      })
    },
  },

  accounts: accountsApi,

  groups: {
    list(): Promise<GroupsResponse> {
      return request<GroupsResponse>('/groups')
    },
    create(payload: GroupSave): Promise<{ id?: string }> {
      return request<{ id?: string }>('/groups', { method: 'POST', body: JSON.stringify(payload) })
    },
  },

  rules: {
    list(): Promise<RulesResponse> {
      return request<RulesResponse>('/rules')
    },
    save(pack: RulePack): Promise<OkResponse> {
      return request<OkResponse>(`/rules/${encodeURIComponent(pack.name)}`, {
        method: 'PUT',
        body: JSON.stringify(pack),
      })
    },
    remove(name: string): Promise<OkResponse> {
      return request<OkResponse>(`/rules/${encodeURIComponent(name)}`, { method: 'DELETE' })
    },
    reset(name: string): Promise<OkResponse> {
      return request<OkResponse>(`/rules/${encodeURIComponent(name)}/reset`, {
        method: 'POST',
        body: '{}',
      })
    },
    reload(): Promise<RulesReloadResult> {
      return request<RulesReloadResult>('/rules/reload', { method: 'POST', body: '{}' })
    },
  },

  checkins(plugin: string, from: string, to: string): Promise<CheckinData> {
    return request<CheckinData>(`/checkin${qs({ plugin, from, to })}`)
  },

  checkinAll(): Promise<CheckinAllData> {
    return request<CheckinAllData>('/checkin/all', { method: 'POST', body: '{}' })
  },

  proxies: {
    list(): Promise<Proxy[]> {
      return request<Proxy[]>('/proxies')
    },
    save(proxy: Omit<Proxy, 'id'> & { id?: string }): Promise<void> {
      if (proxy.id) {
        return request<void>(`/proxies/${encodeURIComponent(proxy.id)}`, {
          method: 'PUT',
          body: JSON.stringify(proxy),
        })
      }
      return request<void>('/proxies', { method: 'POST', body: JSON.stringify(proxy) })
    },
    remove(id: string): Promise<void> {
      return request<void>(`/proxies/${encodeURIComponent(id)}`, { method: 'DELETE' })
    },
    check(id: string): Promise<ProxyCheckResult> {
      return request<ProxyCheckResult>(`/proxies/${encodeURIComponent(id)}/check`, { method: 'POST', body: '{}' })
    },
  },

  logs: {
    requests(filters: RequestLogFilterParams): Promise<RequestLogsData> {
      return request<RequestLogsData>(`/logs/requests${qs({ ...filters })}`)
    },
    /** Absolute CSV URL, for an `<a download>` click. Same filters as `requests`. */
    requestsExportUrl(filters: RequestLogFilterParams): string {
      return `/api/logs/requests/export${qs({ ...filters })}`
    },
    runtime(source: string, n: number): Promise<RuntimeLogsData> {
      return request<RuntimeLogsData>(`/logs/runtime${qs({ source, n })}`)
    },
    /** Unified API + runtime timeline (S2). */
    timeline(params: LogTimelineParams): Promise<LogTimelineResponse> {
      return request<LogTimelineResponse>(`/logs/timeline${qs({ ...params })}`)
    },
    /** Proxy traffic log: one row per tunnelled CONNECT. */
    proxy(params: LogProxyParams): Promise<LogProxyResponse> {
      return request<LogProxyResponse>(`/logs/proxy${qs({ ...params })}`)
    },
    /** Full request/response body; rejects with type `debug_disabled` when debug is off. */
    body(reqId: string, which: LogBodyWhich): Promise<LogBodyResponse> {
      return request<LogBodyResponse>(`/logs/body${qs({ req_id: reqId, which })}`)
    },
    /**
     * One raw source line behind a timeline event. The endpoint may not exist
     * yet on older hosts; callers must treat 404/501 as "source unsupported".
     */
    raw(params: LogRawParams): Promise<LogRawResponse> {
      return request<LogRawResponse>(`/logs/raw${qs({ ...params })}`)
    },
    /** SSE endpoint shared by EventSource; the caller owns the connection. */
    streamUrl(params: LogStreamParams = {}): string {
      return `/api/logs/stream${qs({ ...params })}`
    },
  },

  usage: {
    summary(filters: UsageFilterParams): Promise<UsageSummary> {
      return request<UsageSummary>(`/usage/summary${qs({ ...filters })}`)
    },
    credits(filters: UsageFilterParams): Promise<UsageCreditsResponse> {
      return request<UsageCreditsResponse>(`/usage/credits${qs({ ...filters })}`)
    },
    expiry(filters: UsageFilterParams): Promise<UsageCreditsResponse> {
      return request<UsageCreditsResponse>(`/usage/expiry${qs({ ...filters })}`)
    },
    sessions(params: UsageSessionsParams): Promise<UsageSessionsResponse> {
      return request<UsageSessionsResponse>(`/usage/sessions${qs({ ...params })}`)
    },
    /** Per-day latency/TTFB percentiles over successful requests (1–90 days). */
    latency(days: number): Promise<UsageLatencyDay[]> {
      return request<UsageLatencyDay[]>(`/usage/latency${qs({ days })}`)
    },
    /** Per-key request/token/credit totals for the last N days (1–90). */
    keys(days: number): Promise<UsageKeyRow[]> {
      return request<UsageKeyRow[]>(`/usage/keys${qs({ days })}`)
    },
    /** Per-model consumption ranking for the last N days (1–90). */
    models(days: number): Promise<UsageModelRow[]> {
      return request<UsageModelRow[]>(`/usage/models${qs({ days })}`)
    },
    /** Per-model latency/TTFB percentiles over successful requests (1–90). */
    modelLatency(days: number): Promise<UsageModelLatencyRow[]> {
      return request<UsageModelLatencyRow[]>(`/usage/latency/models${qs({ days })}`)
    },
    /** Absolute CSV URL, for an `<a download>` click. */
    exportUrl(filters: UsageFilterParams): string {
      return `/api/usage/export${qs({ ...filters })}`
    },
  },

  /** Out-of-app notifications. The URL is write-only; `url: ""` clears it. */
  notify: {
    get(): Promise<NotifyStatus> {
      return request<NotifyStatus>('/notify')
    },
    update(update: NotifyUpdate): Promise<NotifyStatus> {
      return request<NotifyStatus>('/notify', { method: 'PUT', body: JSON.stringify(update) })
    },
    test(): Promise<{ ok: boolean }> {
      return request<{ ok: boolean }>('/notify/test', { method: 'POST', body: '{}' })
    },
  },

  keys: {
    list(): Promise<ClientKey[]> {
      return request<ClientKey[]>('/keys')
    },    create(input: KeyScopeInput): Promise<CreateKeyResponse> {
      return request<CreateKeyResponse>('/keys', { method: 'POST', body: JSON.stringify(input) })
    },
    /** Partial edit: an absent field keeps the stored value. */
    update(id: string, input: KeyScopeInput): Promise<{ ok: boolean; key: ClientKey }> {
      return request<{ ok: boolean; key: ClientKey }>(`/keys/${encodeURIComponent(id)}`, {
        method: 'PUT',
        body: JSON.stringify(input),
      })
    },
    remove(id: string): Promise<void> {
      return request<void>(`/keys/${encodeURIComponent(id)}`, { method: 'DELETE' })
    },
  },

  /** 自定义接入：一个端点 + Key + 协议，下面挂多个模型。 */
  customConnections: {
    list(): Promise<{ connections: CustomConnection[] }> {
      return request<{ connections: CustomConnection[] }>('/custom-connections')
    },
    create(input: CustomConnectionInput): Promise<{ ok: boolean; created: boolean; connection: CustomConnection }> {
      return request<{ ok: boolean; created: boolean; connection: CustomConnection }>('/custom-connections', {
        method: 'POST',
        body: JSON.stringify(input),
      })
    },
    update(id: string, patch: CustomConnectionPatch): Promise<{ ok: boolean; connection: CustomConnection }> {
      return request<{ ok: boolean; connection: CustomConnection }>(`/custom-connections/${encodeURIComponent(id)}`, {
        method: 'PUT',
        body: JSON.stringify(patch),
      })
    },
    remove(id: string): Promise<{ ok: boolean; id: string }> {
      return request<{ ok: boolean; id: string }>(`/custom-connections/${encodeURIComponent(id)}`, {
        method: 'DELETE',
      })
    },
    /** Calls the protocol's model-list endpoint and reports its count. */
    test(id: string): Promise<{ ok: boolean; latency_ms: number; model_count: number; error?: string }> {
      return request<{ ok: boolean; latency_ms: number; model_count: number; error?: string }>(
        `/custom-connections/${encodeURIComponent(id)}/test`,
        { method: 'POST', body: '{}' },
      )
    },
    remoteModels(id: string): Promise<{ models: RemoteModel[] }> {
      return request<{ models: RemoteModel[] }>(`/custom-connections/${encodeURIComponent(id)}/remote-models`)
    },
    importModels(id: string, models: ImportModelInput[]): Promise<{ ok: boolean; created: CustomModel[]; skipped: ImportSkip[] }> {
      return request<{ ok: boolean; created: CustomModel[]; skipped: ImportSkip[] }>(
        `/custom-connections/${encodeURIComponent(id)}/models/import`,
        { method: 'POST', body: JSON.stringify({ models }) },
      )
    },
  },

  /** 用户自定义上游模型（设置 → 自定义模型），归属于某个接入。 */
  customModels: {
    list(): Promise<{ models: CustomModel[] }> {
      return request<{ models: CustomModel[] }>('/custom-models')
    },
    create(input: CustomModelInput): Promise<{ ok: boolean; model: CustomModel }> {
      return request<{ ok: boolean; model: CustomModel }>('/custom-models', {
        method: 'POST',
        body: JSON.stringify(input),
      })
    },
    update(id: string, patch: CustomModelPatch): Promise<{ ok: boolean; model: CustomModel }> {
      return request<{ ok: boolean; model: CustomModel }>(`/custom-models/${encodeURIComponent(id)}`, {
        method: 'PUT',
        body: JSON.stringify(patch),
      })
    },
    remove(id: string): Promise<{ ok: boolean; id: string }> {
      return request<{ ok: boolean; id: string }>(`/custom-models/${encodeURIComponent(id)}`, {
        method: 'DELETE',
      })
    },
    /** Explicit connectivity probe (5s, one "ping" request). */
    test(id: string): Promise<CustomModelTestResult> {
      return request<CustomModelTestResult>(`/custom-models/${encodeURIComponent(id)}/test`, {
        method: 'POST',
        body: '{}',
      })
    },
  },

  /** 连通性诊断：显式探测 A / 插件模型 / 自定义模型，并读取探测历史。 */
  diagnostics: {
    /** One explicit probe; failures come back as ok:false, not as a rejection. */
    probe(kind: DiagnosticsKind, target: ProbeTarget = {}): Promise<ProbeResult> {
      const body: Record<string, string> = { kind }
      if (kind === 'plugin') {
        if (target.plugin_id) body.plugin_id = target.plugin_id
        if (target.model) body.model = target.model
      } else if (kind === 'custom_model' && target.custom_model_id) {
        body.custom_model_id = target.custom_model_id
      }
      return request<ProbeResult>('/diagnostics/probe', {
        method: 'POST',
        body: JSON.stringify(body),
      })
    },
    /** Newest-first history; a missing `probes` field (old host) becomes []. */
    async probes(query: ProbeQuery = {}): Promise<DiagnosticsProbe[]> {
      const data = await request<{ probes?: DiagnosticsProbe[] }>(
        `/diagnostics/probes${qs({ limit: query.limit, kind: query.kind, target_id: query.target_id })}`,
      )
      return Array.isArray(data?.probes) ? data.probes : []
    },
  },

  scheduler: {
    get(): Promise<SchedulerState> {
      return request<SchedulerState>('/scheduler')
    },
    saveGlobal(payload: SchedulerGlobalSettings): Promise<OkResponse> {
      // model_sync_time is per-pool; the global defaults endpoint rejects it.
      const d = payload.defaults
      const defaults = {
        strategy: d.strategy,
        account_concurrency: d.account_concurrency,
        pool_sticky_ttl: d.pool_sticky_ttl,
        conv_sticky_ttl: d.conv_sticky_ttl,
        hedge_ms: d.hedge_ms,
        default_reasoning: d.default_reasoning,
        background_downgrade: d.background_downgrade,
        longctx_downgrade: d.longctx_downgrade,
      }
      return request<OkResponse>('/scheduler/global', {
        method: 'PUT',
        body: JSON.stringify({ inter_pool: payload.inter_pool, defaults }),
      })
    },
    savePool(plugin: string, payload: SchedulerPoolSettings): Promise<OkResponse> {
      return request<OkResponse>(`/scheduler/pools/${encodeURIComponent(plugin)}`, {
        method: 'PUT',
        body: JSON.stringify(payload),
      })
    },
    preset(name: SchedulerPreset): Promise<SchedulerPresetResult> {
      return request<SchedulerPresetResult>(`/scheduler/presets/${encodeURIComponent(name)}`, {
        method: 'POST',
        body: '{}',
      })
    },
    models(): Promise<ModelBinding[] | { models?: ModelBinding[]; items?: ModelBinding[] }> {
      return request<ModelBinding[] | { models?: ModelBinding[]; items?: ModelBinding[] }>(
        '/scheduler/models',
      )
    },
    saveModel(logical: string, payload: ModelBindingSave): Promise<OkResponse> {
      return request<OkResponse>(`/scheduler/models/${encodeURIComponent(logical)}`, {
        method: 'PUT',
        body: JSON.stringify(payload),
      })
    },
    removeModel(logical: string): Promise<OkResponse> {
      return request<OkResponse>(`/scheduler/models/${encodeURIComponent(logical)}`, {
        method: 'DELETE',
      })
    },
    explain(payload: SchedulerExplainRequest): Promise<SchedulerExplainResponse> {
      return request<SchedulerExplainResponse>('/scheduler/explain', {
        method: 'POST',
        body: JSON.stringify(payload),
      })
    },
    sticky(): Promise<StickyResponse> {
      return request<StickyResponse>('/scheduler/sticky')
    },
    clearSticky(): Promise<OkResponse> {
      return request<OkResponse>('/scheduler/sticky', { method: 'DELETE' })
    },
  },

  pools: {
    get(plugin: string): Promise<PoolDetail> {
      return request<PoolDetail>(`/pools/${encodeURIComponent(plugin)}`)
    },
    save(plugin: string, payload: Partial<PoolSettings> & { upstream_enabled?: boolean }): Promise<OkResponse> {
      return request<OkResponse>(`/pools/${encodeURIComponent(plugin)}`, {
        method: 'PUT',
        body: JSON.stringify(payload),
      })
    },
    setMembers(plugin: string, items: string[]): Promise<OkResponse> {
      return request<OkResponse>(`/pools/${encodeURIComponent(plugin)}/members`, {
        method: 'PUT',
        body: JSON.stringify({ items }),
      })
    },
    sync(plugin: string): Promise<PoolSyncResult> {
      return request<PoolSyncResult>(`/pools/${encodeURIComponent(plugin)}/sync`, {
        method: 'POST',
        body: '{}',
      })
    },
    catalog(plugin: string): Promise<PoolCatalogData> {
      return request<PoolCatalogData>(`/pools/${encodeURIComponent(plugin)}/catalog`)
    },
    setModelEnabled(plugin: string, model: string, enabled: boolean): Promise<PoolCatalogData> {
      return request<PoolCatalogData>(
        `/pools/${encodeURIComponent(plugin)}/catalog/${encodeURIComponent(model)}`,
        { method: 'PUT', body: JSON.stringify({ enabled }) },
      )
    },
    overrides(plugin: string): Promise<ModelOverridesResponse> {
      return request<ModelOverridesResponse>(
        `/pools/${encodeURIComponent(plugin)}/models/overrides`,
      )
    },
    saveOverride(
      plugin: string,
      model: string,
      patch: ModelOverridePatch,
    ): Promise<ModelOverrideSaveResponse> {
      return request<ModelOverrideSaveResponse>(
        `/pools/${encodeURIComponent(plugin)}/models/overrides/${encodeURIComponent(model)}`,
        { method: 'PUT', body: JSON.stringify(patch) },
      )
    },
    clearOverride(plugin: string, model: string): Promise<ModelOverrideDeleteResponse> {
      return request<ModelOverrideDeleteResponse>(
        `/pools/${encodeURIComponent(plugin)}/models/overrides/${encodeURIComponent(model)}`,
        { method: 'DELETE' },
      )
    },
  },
}
