/**
 * Synthetic API fixtures for the visual regression gate.
 *
 * These are hand-written fakes, NOT recordings of the running host. Two reasons
 * it must stay that way:
 *
 *  1. A recording would carry real account ids, real client-key prefixes and
 *     real upstream error text into the repository. The project's standing rule
 *     (handoff 20261022 §6) is that keys and account secrets never land on disk.
 *  2. A recording changes every time somebody uses the app, so the gate would
 *     go red for data drift instead of for style regressions. Fixed row counts
 *     and fixed timestamps keep the pixels meaningful.
 *
 * The shapes below were read off the real host once (field names, nesting, which
 * fields are optional) so the views take their normal rendering paths rather
 * than their empty/error branches. PLAUSIBLE-but-fake values everywhere else.
 *
 * Row counts are load-bearing: a table that renders 5 rows in the baseline and
 * 6 rows tonight would diff. Keep them stable and change them deliberately.
 */

/** A fixed instant so no "3 分钟前" relative label can drift. 2026-10-22 12:00:00 +08:00 */
export const FIXTURE_NOW_MS = 1792651200000
export const FIXTURE_NOW_ISO = '2026-10-22T12:00:00+08:00'

const iso = (offsetSeconds) => new Date(FIXTURE_NOW_MS + offsetSeconds * 1000).toISOString()

/** 12 plugins, in the manifest order the host reports. */
const PLUGIN_SEEDS = [
  { id: 'aichat', name: '示例插件 A（转发）', port: 1544 },
  { id: 'autoglm', name: '示例插件 B', port: 1554 },
  { id: 'cline', name: '示例插件 C', port: 1564 },
  { id: 'codearts', name: '示例插件 D', port: 1574 },
  { id: 'gemini', name: '示例插件 E', port: 1584 },
  { id: 'lobsterai', name: '示例插件 F', port: 1594 },
  { id: 'loomy', name: '示例插件 G', port: 1604 },
  { id: 'minimax', name: '示例插件 H', port: 1614 },
  { id: 'officeace', name: '示例插件 I', port: 1624 },
  { id: 'opencode', name: '示例插件 J', port: 1634 },
  { id: 'raccoon', name: '示例插件 K', port: 1644 },
  { id: 'zcode', name: '示例插件 L', port: 1654 },
]

function manifest(seed, index) {
  return {
    id: seed.id,
    name: seed.name,
    version: '0.1.0',
    api_level: 2,
    description: `示例插件 ${index + 1}：用于视觉回归的固定假描述，说明该插件把宿主的 OpenAI 兼容请求转发到上游服务。`,
    os: ['windows'],
    capabilities:
      index % 3 === 0
        ? ['upstream', 'accounts']
        : index % 3 === 1
          ? ['upstream', 'accounts', 'checkin', 'quota', 'refresh', 'egress', 'login_oauth']
          : ['upstream', 'accounts', 'checkin'],
    permissions: {
      network_domains: index % 2 === 0 ? 'api.example.test' : 'api.example.test api.other.test',
      use_proxy: index % 2 === 1,
    },
    checkin: index % 3 === 2 ? {} : { default_time: '09:00' },
    quota: index % 3 === 2 ? {} : { poll_minutes: 30 },
    concurrency: { suggested: 2 },
    rules: {},
    egress: {},
    models: [
      { id: `${seed.id}/model-one`, name: 'Model One', context_length: 200000, tools: true, vision: false },
      { id: `${seed.id}/model-two`, name: 'Model Two', context_length: 1000000, tools: true, vision: true },
    ],
    startup: { mode: 'always', idle_timeout_s: 600 },
    entry: { exe: `${seed.id}-plugin.exe` },
  }
}

export const plugins = PLUGIN_SEEDS.map((seed, index) => ({
  manifest: manifest(seed, index),
  dir: `C:\\Users\\example\\AppData\\Roaming\\AIWorkGate\\plugins\\${seed.id}`,
  enabled: index !== 4 && index !== 9,
  running: index !== 4 && index !== 9,
  healthy: index !== 2 && index !== 4 && index !== 9,
  failed: index === 4,
  port: seed.port,
  pid: 40000 + index,
  started_at: Math.floor(FIXTURE_NOW_MS / 1000) - 3600,
  last_error: index === 4 ? 'upstream handshake failed: connection refused' : undefined,
  upstream_enabled: index % 5 !== 0,
}))

/** Five accounts across four plugins: healthy, expiring, warned, stale, disabled. */
export const accounts = {
  accounts: [
    {
      id: 'acct_alpha',
      label: '示例账号 Alpha',
      enabled: true,
      status: 'ok',
      added_at: 1792480000,
      quota: { remaining: 820, total: 1000, plan: '示例套餐', rate_limited: false, today_checked_in: true, checked_in_at: 1792640000 },
      credential: { present: true, expires_at: 1792700000, refreshable: true },
      plugin_id: 'aichat',
      alias: '',
      display_name: '示例账号 Alpha',
      group_id: 'grp_primary',
      priority: 100,
      weight: 1,
      enabled_override: null,
      enabled_effective: true,
      stale: false,
      status_kind: 'normal',
      cooldown_until: 0,
      cooldown_reason: '',
      state: 'normal',
      state_source: '',
      egress: { mode: 'direct' },
    },
    {
      id: 'acct_bravo',
      label: '示例账号 Bravo',
      enabled: true,
      status: 'ok',
      added_at: 1792490000,
      quota: { remaining: 140, total: 500, plan: '示例套餐·小', rate_limited: false },
      credential: { present: true, expires_at: 1792652000, refreshable: true },
      identity: { machine_id: 'machine-0001', device_id: 'device-0001', source: 'derived' },
      plugin_id: 'officeace',
      alias: '',
      display_name: '示例账号 Bravo',
      group_id: 'grp_primary',
      priority: 100,
      weight: 1,
      enabled_override: null,
      enabled_effective: true,
      stale: false,
      egress: { mode: 'proxy', proxy_id: 'px_broken', url: 'http://127.0.0.1:1081' },
      status_kind: 'normal',
      cooldown_until: 0,
      cooldown_reason: '',
      state: 'normal',
      state_source: '',
      egress_health: {
        status: 'failed',
        proxy_id: 'px_broken',
        proxy_label: '示例代理·失败',
        at: Math.floor(FIXTURE_NOW_MS / 1000) - 180,
        age_seconds: 180,
        error: 'egress: upstream CONNECT api.example.test:443: read status line: EOF',
      },
    },
    {
      id: 'acct_charlie',
      label: '示例账号 Charlie',
      enabled: true,
      status: 'error',
      added_at: 1792500000,
      quota: {},
      credential: { present: true, refreshable: true },
      plugin_id: 'opencode',
      alias: '匿名通道',
      display_name: '匿名通道',
      group_id: '',
      priority: 90,
      weight: 2,
      enabled_override: null,
      enabled_effective: true,
      stale: false,
      last_error: '示例警告：上游返回 403，该模型在当前地区不可用',
      status_kind: 'rate_limited',
      cooldown_until: 1792660000,
      cooldown_reason: '冷却中，等待限额窗口重置',
      state: 'rate_limited',
      state_source: 'host',
      state_detail: '冷却中，等待限额窗口重置',
    },
    {
      id: 'acct_delta',
      label: '示例账号 Delta',
      enabled: false,
      status: 'disabled',
      added_at: 1792510000,
      quota: { remaining: 0, total: 0, rate_limited: true },
      credential: { present: true, refreshable: false },
      plugin_id: 'zcode',
      alias: '',
      display_name: '示例账号 Delta',
      group_id: 'grp_secondary',
      priority: 100,
      weight: 1,
      enabled_override: false,
      enabled_effective: false,
      stale: true,
      status_kind: 'normal',
      cooldown_until: 0,
      cooldown_reason: '',
      state: 'unknown',
      state_source: 'plugin',
      state_detail: '插件未报告可用状态',
    },
    {
      id: 'acct_echo',
      label: '示例账号 Echo',
      enabled: true,
      status: 'ok',
      added_at: 1792520000,
      quota: { remaining: 4200, total: 5000, plan: '示例套餐·大', daily_credits: 200 },
      credential: { present: true, expires_at: 1793200000, refreshable: true },
      identity: { machine_id: 'machine-0002', session_id: 'session-0002', source: 'request' },
      plugin_id: 'aichat',
      alias: '',
      display_name: '示例账号 Echo',
      group_id: 'grp_secondary',
      priority: 80,
      weight: 1,
      enabled_override: true,
      enabled_effective: true,
      stale: false,
      status_kind: 'normal',
      cooldown_until: 0,
      cooldown_reason: '',
      state: 'normal',
      state_source: '',
    },
  ],
}

export const groups = {
  groups: [
    { id: 'grp_primary', name: '主力池', color: '#5b8def', sort: 1, account_count: 2 },
    { id: 'grp_secondary', name: '备用池', color: '#e2994a', sort: 2, account_count: 2 },
  ],
}

/** Two proxies, one healthy and one failing, so both badge styles render. */
export const proxies = [
  {
    id: 'px_example',
    name: '示例代理·可用',
    type: 'socks5',
    host: '127.0.0.1',
    port: 1080,
    enabled: true,
    last_check_at: Math.floor(FIXTURE_NOW_MS / 1000) - 120,
    last_exit_ip: '203.0.113.10',
    last_latency_ms: 82,
  },
  {
    id: 'px_broken',
    name: '示例代理·失败',
    type: 'http',
    host: '127.0.0.1',
    port: 1081,
    enabled: false,
    last_check_at: Math.floor(FIXTURE_NOW_MS / 1000) - 600,
    last_error: 'connect timeout after 5000ms',
  },
  {
    id: 'system',
    name: '系统代理（跟随 Windows）',
    type: 'http',
    host: '',
    port: 0,
    enabled: false,
    managed: 'system',
    managed_detail: 'Windows 未开启系统代理，开启后自动同步。',
  },
]

export const rules = {
  builtin: ['wb-moderation'],
  packs: [
    {
      name: 'wb-moderation',
      description: '示例规则包：字面替换 + 令牌前缀剥离（内置，可重置）',
      rules: [
        { from: 'You are an example assistant.', to: 'You are an example assistant.' },
        { from: 'Default branch (for PRs)', to: 'Default branch (for PRs)' },
      ],
      strip: [{ pattern: 'cc_[A-Za-z0-9]{8,}' }, { pattern: 'x-anthropic-[a-z-]+' }],
    },
    {
      name: 'custom-header',
      description: '示例自定义规则包：给上游请求补一个固定头',
      rules: [{ from: 'ORIGIN_HEADER', to: 'sanitized-header' }],
      strip: [],
    },
  ],
}

export const settings = {
  version: 1,
  port: 7865,
  listen_lan: false,
  autostart: false,
  a: {
    base_url: 'http://127.0.0.1:17864',
    exe_path: 'C:\\Program Files\\Example\\Example App\\example.exe',
    auto_start: true,
    model_cache_minutes: 10,
  },
  proxy: { enabled: true, http: '', https: '', no_proxy: ['127.0.0.1', 'localhost'] },
  plugins: { concurrency: 2, startup_mode: 'always', strict_egress: false },
  egress: { direct_logging: false },
  checkin: { default_time: '09:00', retry_count: 2, retry_backoff_seconds: [5, 20] },
  retention: { api_request_days: 30, body_preview_days: 3, checkin_days: 180, host_log_days: 30, proxy_log_days: 7 },
  logging: { debug_body: false },
  gateway: { cors_allow_origins: null, default_plugin: '', legacy_a_fallback: true },
  notify: {
    enabled: true,
    channel: 'generic',
    events: ['checkin_fail', 'account_dead', 'checkin_done'],
    min_interval_seconds: 600,
  },
}

export const keys = [
  {
    id: 'ck_example01',
    name: '示例密钥·一',
    prefix: 'agk_EXMP',
    enabled: true,
    created_at: 1792600000,
    last_used_at: 1792649999,
    model_scope: [],
    bind_pool: '',
    daily_quota: 0,
    used_today: 42,
    quota_left: -1,
  },
  {
    id: 'ck_example02',
    name: '示例密钥·二',
    prefix: 'agk_DEMO',
    enabled: false,
    created_at: 1792610000,
    model_scope: ['example/model-one'],
    bind_pool: 'aichat',
    daily_quota: 500,
    used_today: 120,
    quota_left: 380,
  },
]

export const notify = {
  enabled: true,
  channel: 'generic',
  url_set: true,
  events: ['checkin_fail', 'account_dead', 'checkin_done'],
  min_interval_seconds: 600,
  last_sent_at: Math.floor(FIXTURE_NOW_MS / 1000) - 1800,
  last_error: '',
}

export const customConnections = {
  connections: [
    {
      id: 'cc_example01',
      name: '示例接入·一',
      base_url: 'https://api.example.test/v1',
      protocol: 'openai',
      enabled: true,
      key_set: true,
      created_at: 1792648000,
      updated_at: 1792648000,
    },
  ],
}

export const customModels = {
  models: [
    {
      id: 'cm_example01',
      connection_id: 'cc_example01',
      name: 'example-model-one',
      base_url: 'https://api.example.test/v1',
      protocol: 'openai',
      connection_enabled: true,
      upstream_model: 'example-model-one',
      enabled: true,
      created_at: 1792648485,
      updated_at: 1792648485,
      context_window: 200000,
      max_output_tokens: 8192,
      input_types: ['text', 'image'],
      reasoning_tiers: ['low', 'high'],
      default_reasoning: '',
      key_set: true,
    },
    {
      id: 'cm_example02',
      connection_id: 'cc_example01',
      name: 'example-model-two',
      base_url: 'https://api.example.test/v1',
      protocol: 'openai',
      connection_enabled: true,
      upstream_model: 'example-model-two',
      enabled: false,
      created_at: 1792648500,
      updated_at: 1792648500,
      context_window: 1000000,
      max_output_tokens: 16384,
      input_types: ['text'],
      reasoning_tiers: [],
      default_reasoning: '',
      key_set: true,
    },
  ],
}

export const scheduler = {
  global: {
    inter_pool: 'smart',
    defaults: {
      account_concurrency: 2,
      background_downgrade: false,
      conv_sticky_ttl: 600,
      default_reasoning: '',
      hedge_ms: 0,
      longctx_downgrade: false,
      pool_sticky_ttl: 600,
      strategy: 'credit_first',
    },
  },
  health: PLUGIN_SEEDS.map((seed, index) => ({
    plugin_id: seed.id,
    circuit_open: index === 4,
  })),
  models: [
    {
      logical: 'example/logical-one',
      note: '示例绑定：跨两个插件回落',
      targets: [
        { plugin_id: 'aichat', model: 'example/model-one', priority: 1 },
        { plugin_id: 'opencode', model: 'example/model-two', priority: 2 },
      ],
    },
  ],
  pools: PLUGIN_SEEDS.map((seed, index) => ({
    plugin_id: seed.id,
    name: seed.name,
    api_level: 2,
    account_concurrency: 2,
    background_downgrade: false,
    conv_sticky_ttl: 600,
    default_reasoning: '',
    hedge_ms: 0,
    longctx_downgrade: false,
    pool_priority: index % 2 === 0 ? 100 : 50,
    pool_sticky_ttl: 600,
    strategy: 'credit_first',
    upstream_enabled: index % 5 !== 0,
  })),
  sticky_count: 1,
  version: 1,
}

export const schedulerModels = { models: scheduler.models }
export const schedulerSticky = {
  bindings: [
    {
      conversation_id: 'conv-0001',
      plugin_id: 'aichat',
      account_id: 'acct_alpha',
      model: 'example/model-one',
      expires_at: Math.floor(FIXTURE_NOW_MS / 1000) + 600,
    },
  ],
  count: 1,
}

export const loginState = {
  plugins: PLUGIN_SEEDS.map((seed, index) => ({
    plugin_id: seed.id,
    name: seed.name,
    api_level: 2,
    methods: index % 3 === 0 ? [] : index % 3 === 1 ? ['oauth'] : ['oauth', 'paste_code'],
    reads_local_state: false,
    requires_app: index % 3 === 1,
    auto_refresh: index % 3 === 1,
    source: index % 5 === 0 ? 'manifest' : 'derived',
    checked_at: Math.floor(FIXTURE_NOW_MS / 1000),
  })),
}

export const probes = {
  probes: [
    { id: 1, kind: 'custom_model', target: 'example-model-one', target_id: 'cm_example01', ok: true, status: 200, latency_ms: 412, detail: '模型连通正常', at: Math.floor(FIXTURE_NOW_MS / 1000) - 300 },
    { id: 2, kind: 'plugin', target: 'aichat/example/model-one', target_id: 'aichat', ok: true, status: 200, latency_ms: 902, detail: '模型连通正常', at: Math.floor(FIXTURE_NOW_MS / 1000) - 600 },
    { id: 3, kind: 'plugin', target: 'opencode/example/model-two', target_id: 'opencode', ok: false, status: 403, latency_ms: 168, detail: '上游鉴权失败：示例地区限制', at: Math.floor(FIXTURE_NOW_MS / 1000) - 900 },
    { id: 4, kind: 'a', target: 'A 上游', target_id: 'a', ok: true, status: 200, latency_ms: 61, detail: '模型连通正常', at: Math.floor(FIXTURE_NOW_MS / 1000) - 1200 },
  ],
}

export const status = {
  version: '0.1.0',
  port: 7865,
  data_dir: 'C:\\Users\\example\\AppData\\Roaming\\AIWorkGate',
  a_configured: true,
  a_running: true,
  a_port: 17864,
  a_state: 'ok',
  a_detail: '',
  plugins_running: 10,
  plugins_total: 12,
  egress_assigned: 1,
  egress_failed: 1,
}

/* ------------------------------------------------------------------ */
/* Logs: fixed row counts, fixed ids, fixed latencies                 */
/* ------------------------------------------------------------------ */

/**
 * Request rows covering every status style the table renders: 2xx, 4xx, 5xx and
 * the "error" bucket (which the view colours differently). The first twelve are
 * hand-written; the rest are generated so a 50-row page has a real second page
 * to load. Counts are pinned so heights stay fixed.
 */
const craftedRequestLogs = [
    { ts: iso(-60), req_id: 'req00000001', source: 'plugin:aichat', model: 'aichat/example/model-one', key_id: 'ck_example01', key_name: '示例密钥·一', account: 'acct_alpha', stream: false, status: 200, latency_ms: 412, ttfb_ms: 400, first_token_at: iso(-60), prompt_tokens: 128, completion_tokens: 64 },
    { ts: iso(-150), req_id: 'req00000002', source: 'plugin:officeace', model: 'officeace/example/model-one', key_id: 'ck_example01', key_name: '示例密钥·一', account: 'acct_bravo', stream: true, status: 200, latency_ms: 966, ttfb_ms: 820, first_token_at: iso(-150), prompt_tokens: 35, completion_tokens: 33 },
    { ts: iso(-240), req_id: 'req00000003', source: 'plugin:opencode', model: 'opencode/example/model-two', key_id: 'ck_example01', key_name: '示例密钥·一', account: 'acct_charlie', stream: false, status: 403, latency_ms: 1798, error: '{"error":{"code":"auth","message":"示例：该模型在当前地区不可用","type":"auth"}}' },
    { ts: iso(-330), req_id: 'req00000004', source: 'plugin:zcode', model: 'zcode/example/model-one', key_id: 'ck_example02', key_name: '示例密钥·二', account: 'acct_delta', stream: false, status: 429, latency_ms: 8459, error: '{"error":{"code":429,"message":"示例：额度不足","type":"upstream_error"}}' },
    { ts: iso(-420), req_id: 'req00000005', source: 'gateway', model: '', key_id: 'ck_example01', key_name: '示例密钥·一', stream: false, status: 200, latency_ms: 9 },
    { ts: iso(-510), req_id: 'req00000006', source: 'plugin:cline', model: 'cline/example/model-one', key_id: 'ck_example02', key_name: '示例密钥·二', account: 'acct_echo', stream: false, status: 401, latency_ms: 247, error: '{"error":{"code":"auth","message":"示例：令牌已失效，请重新登录","type":"auth"}}' },
    { ts: iso(-600), req_id: 'req00000007', source: 'plugin:opencode', model: 'opencode/example/model-one', key_id: 'ck_example01', key_name: '示例密钥·一', account: 'acct_charlie', stream: false, status: 499, latency_ms: 90012, error: 'client canceled during retry backoff' },
    { ts: iso(-690), req_id: 'req00000008', source: 'plugin:aichat', model: 'aichat/example/model-two', key_id: 'ck_example01', key_name: '示例密钥·一', account: 'acct_alpha', stream: true, status: 200, latency_ms: 1404, ttfb_ms: 1300, first_token_at: iso(-690), prompt_tokens: 35, completion_tokens: 14 },
    { ts: iso(-780), req_id: 'req00000009', source: 'plugin:zcode', model: 'zcode/example/model-two', key_id: 'ck_example02', key_name: '示例密钥·二', account: 'acct_delta', stream: false, status: 503, latency_ms: 8, error: '示例：暂无可用的模型账号（account cooldown）' },
    { ts: iso(-870), req_id: 'req00000010', source: 'plugin:officeace', model: 'officeace/example/model-two', key_id: 'ck_example02', key_name: '示例密钥·二', account: 'acct_bravo', stream: false, status: 400, latency_ms: 387, error: '{"error":{"code":"upstream_error","message":"示例：上游返回 404 Invalid model.","type":"upstream_error"}}' },
    { ts: iso(-960), req_id: 'req00000011', source: 'gateway', model: '', key_id: 'ck_example01', key_name: '示例密钥·一', stream: false, status: 200, latency_ms: 6 },
    { ts: iso(-1050), req_id: 'req00000012', source: 'plugin:aichat', model: 'aichat/example/model-one', key_id: 'ck_example01', key_name: '示例密钥·一', account: 'acct_echo', stream: false, status: 200, latency_ms: 826, ttfb_ms: 800, first_token_at: iso(-1050), prompt_tokens: 35, completion_tokens: 38 },
]

const syntheticRequestLogs = Array.from({ length: 108 }, (_, index) => {
  const n = 13 + index
  const status = [200, 200, 200, 429, 503][index % 5]
  const stream = index % 3 === 0
  const failed = status !== 200
  return {
    ts: iso(-(1200 + index * 120)),
    req_id: `req${String(n).padStart(8, '0')}`,
    source: ['plugin:aichat', 'plugin:officeace', 'plugin:zcode'][index % 3],
    model: ['aichat/example/model-one', 'officeace/example/model-two', 'zcode/example/model-two'][index % 3],
    key_id: index % 2 === 0 ? 'ck_example01' : 'ck_example02',
    key_name: index % 2 === 0 ? '示例密钥·一' : '示例密钥·二',
    account: ['acct_alpha', 'acct_bravo', 'acct_delta'][index % 3],
    stream,
    status,
    latency_ms: 120 + (index % 17) * 61,
    ...(failed ? {} : {
      ttfb_ms: 110 + (index % 13) * 50,
      first_token_at: iso(-(1200 + index * 120)),
      prompt_tokens: 24 + (index % 9) * 8,
      completion_tokens: 12 + (index % 7) * 6,
    }),
    ...(failed ? { error: status === 429 ? '示例：额度不足' : '示例：暂无可用的模型账号（account cooldown）' } : {}),
  }
})

export const logsRequests = {
  logs: [...craftedRequestLogs, ...syntheticRequestLogs],
  total: 120,
}

/** limit/offset slice so the view can actually reach a second page. */
export function logsRequestsPage(url) {
  const limit = Math.max(1, Number(url.searchParams.get('limit') ?? 50))
  const offset = Math.max(0, Number(url.searchParams.get('offset') ?? 0))
  return {
    logs: logsRequests.logs.slice(offset, offset + limit),
    total: logsRequests.total,
  }
}

/** Unified timeline: every source type present so all filter chips have data. */
export const logsTimeline = {
  items: [
    { ts: iso(-30), type: 'audit', category: 'action', level: 'info', source: 'action', summary: 'PUT /api/settings /api/settings: 2xx 109ms [2xx]', row_id: 101 },
    { ts: iso(-45), type: 'audit', category: 'audit', level: 'info', source: 'config', summary: 'settings config.json [ok]', row_id: 102 },
    { ts: iso(-60), type: 'api', category: 'request', level: 'info', source: 'aichat', summary: '200 aichat/example/model-one', req_id: 'req00000001', account: 'acct_alpha', model: 'aichat/example/model-one', status: 200, latency_ms: 412 },
    { ts: iso(-75), type: 'runtime', category: 'config', level: 'info', source: 'app', summary: 'scheduling snapshot uses cached accounts' },
    { ts: iso(-90), type: 'proxy', category: 'egress', level: 'info', source: 'egress', summary: 'CONNECT 203.0.113.10:443 ok', row_id: 103 },
    { ts: iso(-105), type: 'runtime', category: 'plugin', level: 'info', source: 'plugins', summary: 'plugin process started' },
    { ts: iso(-120), type: 'connectivity', category: 'probe', level: 'warn', source: 'probe', summary: 'probe opencode/example/model-two 403 上游鉴权失败', row_id: 104 },
    { ts: iso(-135), type: 'api', category: 'request', level: 'warn', source: 'opencode', summary: '403 opencode/example/model-two', req_id: 'req00000003', account: 'acct_charlie', model: 'opencode/example/model-two', status: 403, latency_ms: 1798 },
    { ts: iso(-150), type: 'audit', category: 'action', level: 'info', source: 'action', summary: 'POST /api/accounts/checkin /api/accounts/checkin: 2xx 240ms [2xx]', row_id: 105 },
    { ts: iso(-165), type: 'runtime', category: 'checkin', level: 'error', source: 'notify', summary: '示例告警：额度不足 zcode' },
  ],
  total: 10,
  has_more: false,
  page_size: 100,
  truncated: false,
  source_counts: { api: 2, audit: 3, connectivity: 1, proxy: 1, runtime: 3 },
}

/** Proxy tunnel log: one ok, one denied, one failed — all three badge styles. */
export const logsProxy = {
  items: [
    { id: 1, ts: iso(-60), at: Math.floor(FIXTURE_NOW_MS / 1000) - 60, plugin_id: 'aichat', account_id: 'acct_alpha', proxy_id: 'px_example', proxy_label: '示例代理·可用', target_host: 'api.example.test', target_port: 443, result: 'ok', status: 200, duration_ms: 45, bytes_up: 1024, bytes_down: 8192, error: '' },
    { id: 2, ts: iso(-180), at: Math.floor(FIXTURE_NOW_MS / 1000) - 180, plugin_id: 'officeace', account_id: 'acct_bravo', proxy_id: 'px_broken', proxy_label: '示例代理·失败', target_host: 'api.example.test', target_port: 443, result: 'failed', status: 502, duration_ms: 2039, bytes_up: 0, bytes_down: 0, error: 'egress: upstream CONNECT api.example.test:443: read status line: EOF' },
    { id: 3, ts: iso(-300), at: Math.floor(FIXTURE_NOW_MS / 1000) - 300, plugin_id: 'aichat', account_id: 'acct_alpha', proxy_id: 'px_example', proxy_label: '示例代理·可用', target_host: '', target_port: 0, result: 'denied', status: 405, duration_ms: 12, bytes_up: 0, bytes_down: 0, error: 'not a CONNECT request' },
  ],
  page: 1,
  page_size: 50,
  total: 3,
}

/* ------------------------------------------------------------------ */
/* Usage                                                              */
/* ------------------------------------------------------------------ */

const USAGE_DAYS = ['2026-10-20', '2026-10-21', '2026-10-22']

export const usageSummary = {
  kpis: { requests: 286, errors: 130, prompt_tokens: 55391, completion_tokens: 2547, credits: 0, active_accounts: 5 },
  // Three days × three sources: a fixed, non-degenerate series for the chart.
  series: USAGE_DAYS.flatMap((day, dayIndex) => [
    { day, plugin_id: 'plugin:aichat', requests: 12 + dayIndex * 4, prompt_tokens: 1200 + dayIndex * 300, completion_tokens: 320 + dayIndex * 40, credits: 0 },
    { day, plugin_id: 'plugin:officeace', requests: 8 + dayIndex * 2, prompt_tokens: 900 + dayIndex * 120, completion_tokens: 210 + dayIndex * 30, credits: 0 },
    { day, plugin_id: 'plugin:opencode', requests: 20 + dayIndex * 6, prompt_tokens: 2400 + dayIndex * 400, completion_tokens: 610 + dayIndex * 90, credits: 0 },
  ]),
}

export const usageCredits = {
  accounts: [
    { plugin_id: 'aichat', account_id: 'acct_alpha', display_name: '示例账号 Alpha', remaining: 820, total: 1000, plan: '示例套餐', daily_credits: 200, days_left: 21 },
    { plugin_id: 'officeace', account_id: 'acct_bravo', display_name: '示例账号 Bravo', remaining: 140, total: 500, plan: '示例套餐·小', daily_credits: 30, days_left: 4 },
    { plugin_id: 'aichat', account_id: 'acct_echo', display_name: '示例账号 Echo', remaining: 4200, total: 5000, plan: '示例套餐·大', daily_credits: 200, days_left: 40 },
  ],
}

export const usageExpiry = {
  accounts: [
    { plugin_id: 'aichat', account_id: 'acct_alpha', display_name: '示例账号 Alpha', expires_at: '2026-11-12T00:00:00+08:00', days_left: 21 },
    { plugin_id: 'officeace', account_id: 'acct_bravo', display_name: '示例账号 Bravo', expires_at: '2026-10-26T00:00:00+08:00', days_left: 4 },
    { plugin_id: 'aichat', account_id: 'acct_echo', display_name: '示例账号 Echo', expires_at: '2026-12-01T00:00:00+08:00', days_left: 40 },
  ],
}

export const usageSessions = {
  items: [
    { ts: iso(-60), req_id: 'req00000001', plugin_id: 'aichat', account_id: 'acct_alpha', account_name: '示例账号 Alpha', model: 'example/model-one', logical_model: 'example/logical-one', conversation_id: 'conv-0001', prompt_tokens: 128, completion_tokens: 64, credits: 0, ttfb_ms: 400, latency_ms: 412, status: 200 },
    { ts: iso(-150), req_id: 'req00000002', plugin_id: 'officeace', account_id: 'acct_bravo', account_name: '示例账号 Bravo', model: 'example/model-one', logical_model: '', conversation_id: 'conv-0002', prompt_tokens: 35, completion_tokens: 33, credits: 0, ttfb_ms: 820, latency_ms: 966, status: 200 },
    { ts: iso(-240), req_id: 'req00000003', plugin_id: 'opencode', account_id: 'acct_charlie', account_name: '匿名通道', model: 'example/model-two', logical_model: '', conversation_id: 'conv-0003', prompt_tokens: 0, completion_tokens: 0, credits: 0, ttfb_ms: 0, latency_ms: 1798, status: 403 },
  ],
  total: 3,
  page: 1,
  page_size: 20,
}

export const usageLatency = [
  { day: '2026-10-21', requests: 18, p50_ms: 809, p95_ms: 4755, p50_ttfb_ms: 1863, p95_ttfb_ms: 4755 },
  { day: '2026-10-22', requests: 99, p50_ms: 1312, p95_ms: 7088, p50_ttfb_ms: 1340, p95_ttfb_ms: 7811 },
  { day: '2026-10-23', requests: 41, p50_ms: 1100, p95_ms: 5200, p50_ttfb_ms: 1200, p95_ttfb_ms: 6000 },
]

export const usageKeys = [
  { key_id: 'ck_example01', name: '示例密钥·一', requests: 148, prompt_tokens: 13118, completion_tokens: 890, cached_tokens: 0, credits: null },
  { key_id: 'ck_example02', name: '示例密钥·二', requests: 3, prompt_tokens: 23, completion_tokens: 93, cached_tokens: 0, credits: null },
  { key_id: 'ck_example03', name: '示例密钥·三', requests: 2, prompt_tokens: 0, completion_tokens: 0, cached_tokens: 0, credits: null },
]

export const usageModels = [
  { model: 'aichat/example/model-one', requests: 90, prompt_tokens: 12000, completion_tokens: 900, credits: null },
  { model: 'officeace/example/model-one', requests: 46, prompt_tokens: 4200, completion_tokens: 380, credits: null },
  { model: 'opencode/example/model-two', requests: 31, prompt_tokens: 8100, completion_tokens: 210, credits: null },
]

export const usageLatencyModels = [
  { model: 'aichat/example/model-one', requests: 90, p50_ms: 820, p95_ms: 2400, p50_ttfb_ms: 700, p95_ttfb_ms: 2100 },
  { model: 'officeace/example/model-one', requests: 46, p50_ms: 1100, p95_ms: 3100, p50_ttfb_ms: 980, p95_ttfb_ms: 2900 },
  { model: 'opencode/example/model-two', requests: 31, p50_ms: 640, p95_ms: 1800, p50_ttfb_ms: 520, p95_ttfb_ms: 1600 },
]

/* ------------------------------------------------------------------ */
/* Checkin: a full month grid so the calendar shape is deterministic   */
/* ------------------------------------------------------------------ */

/** 31 days × one account, results cycling ok/failed/skipped at fixed positions. */
export const checkinRecords = (() => {
  const records = []
  for (let day = 1; day <= 31; day++) {
    const date = `2026-10-${String(day).padStart(2, '0')}`
    const result = day % 7 === 0 ? 'failed' : day % 5 === 0 ? 'skipped' : 'ok'
    records.push({
      id: day,
      plugin_id: 'aichat',
      account_id: 'acct_alpha',
      account_label: '示例账号 Alpha',
      date,
      result,
      credits_before: 800 + day,
      credits_after: result === 'ok' ? 800 + day + 20 : 800 + day,
      message: result === 'failed' ? '示例：上游服务不可用' : '',
      at: Math.floor(new Date(`${date}T09:00:00+08:00`).getTime() / 1000),
    })
  }
  return { records }
})()

/* ------------------------------------------------------------------ */
/* Pool detail                                                        */
/* ------------------------------------------------------------------ */

export const poolDetail = {
  plugin_id: 'opencode',
  name: '示例插件 J',
  api_level: 2,
  upstream_enabled: true,
  catalog: [
    { model_id: 'example/model-one', display_name: 'Example Model One', enabled: true, free: true, multiplier: 0, context_length: 200000, vision: false, vendor: '', thinking: ['low', 'high'], synced_at: Math.floor(FIXTURE_NOW_MS / 1000) },
    { model_id: 'example/model-two', display_name: 'Example Model Two', enabled: true, free: false, multiplier: 1, context_length: 1000000, vision: true, vendor: '', thinking: ['low', 'medium', 'high'], synced_at: Math.floor(FIXTURE_NOW_MS / 1000) },
    { model_id: 'example/model-three', display_name: 'Example Model Three', enabled: false, free: false, multiplier: 2, context_length: 262144, vision: false, vendor: '示例厂商', thinking: null, synced_at: Math.floor(FIXTURE_NOW_MS / 1000) },
  ],
  credits: { healthy: 1, pool_size: 2, today_requests: 18, today_used: 12, total: 400 },
  members: [
    { account_id: 'acct_charlie', display_name: '匿名通道', enabled: true, member: true, remaining: 400, status: 'ok' },
    { account_id: 'acct_alpha', display_name: '示例账号 Alpha', enabled: true, member: false, remaining: 820, status: 'ok' },
  ],
  model_sync: { failure_count: 0, last_attempt_at: Math.floor(FIXTURE_NOW_MS / 1000) - 3600, last_error: '', last_success_at: Math.floor(FIXTURE_NOW_MS / 1000) - 3600, next_retry_at: 0, running: false },
  settings: {
    account_concurrency: 2,
    background_downgrade: false,
    conv_sticky_ttl: 600,
    default_reasoning: '',
    hedge_ms: 0,
    longctx_downgrade: false,
    model_sync_time: '05:45',
    pool_priority: 100,
    pool_sticky_ttl: 600,
    strategy: 'credit_first',
    tool_exec: false,
    upstream_enabled: true,
  },
}

export const poolCatalog = { plugin_id: 'opencode', catalog: poolDetail.catalog }
export const poolOverrides = { plugin_id: 'opencode', overrides: {} }

export { PLUGIN_SEEDS }

/**
 * GET /api/scheduler/explain answer: one chosen account plus two rejected
 * candidates, so the explanation panel shows both branches of its UI.
 */
export const schedulerExplain = {
  decision: {
    plugin_id: 'aichat',
    account_id: 'acct_alpha',
    model: 'example/model-one',
    logical_model: 'example/logical-one',
    sticky: false,
    reasons: [
      '策略 credit_first：按剩余积分排序',
      '账号 acct_alpha 积分 82%，池内最高',
      '会话未命中粘性绑定，允许重新选择',
    ],
  },
  candidates: [
    { plugin_id: 'aichat', account_id: 'acct_alpha', score: 0.92, chosen: true },
    { plugin_id: 'aichat', account_id: 'acct_bravo', score: 0.61, rejected_reason: '积分较低' },
    { plugin_id: 'opencode', account_id: 'acct_charlie', score: 0.4, rejected_reason: '上游已关闭' },
  ],
}
