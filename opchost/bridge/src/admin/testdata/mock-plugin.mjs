// Minimal magpie-shaped plugin used to exercise the bridge admin plane.
// Mirrors the real packages' outward shape (verified against
// @magpie-community/opencode-workbuddy-auth and opencode-trae-auth):
//   - default surface: async ({client, magpie}) => ({ provider, auth: { checkin, methods } })
//   - SITES table reachable through _internal.SITES
//   - authorize() returns { url, instructions, method, callback }
//   - auth.checkin(getAuth) returns { outcome, credit?, message? }
// No third-party imports; node: builtins only (same as the real packages).

export const SITES = {
  mock: {
    id: 'mock',
    name: 'Mock Provider',
    endpoint: 'https://copilot.tencent.com',
    platform: 'mock',
    authID: 'mock-desktop',
    keys: 'https://www.workbuddy.ai/profile/keys',
    app: '5.9.3',
  },
  'mock-ai': {
    id: 'mock-ai',
    name: 'Mock Provider AI',
    endpoint: 'https://www.workbuddy.ai',
    platform: 'mock-ai',
    authID: 'mock-desktop-ai',
    app: '5.9.3',
  },
}

const state = {
  checkinCalls: 0,
  /** when true the upstream claims a credit on every call (tests the bridge ledger) */
  dupeClaim: false,
  /** when true the upstream says the day is already in */
  alreadyToday: false,
}

export function setMockState(next) {
  Object.assign(state, next)
}
export function getMockState() {
  return { ...state }
}
/** Back to a fresh day: first check-in is a claim again. */
export function resetMockState() {
  state.checkinCalls = 0
  state.dupeClaim = false
  state.alreadyToday = false
}

/** Magpie-style factory: one plugin object per site. */
export function makeMockPlugin(siteId) {
  const site = SITES[siteId]
  if (!site) throw new Error(`no such mock site: ${siteId}`)
  return async ({ client, magpie } = {}) => ({
    config: async (config) => {
      config.provider ??= {}
      config.provider[site.id] ??= {}
      config.provider[site.id].name ??= site.name
      config.provider[site.id].api ??= `${site.endpoint}/v2`
    },
    provider: {
      id: site.id,
      async models() {
        return { [`${site.id}-chat`]: { id: `${site.id}-chat`, name: 'Mock Chat' } }
      },
    },
    auth: {
      provider: site.id,
      refresh: async () => undefined,
      // (D-19) 真包的签到入口：返回 { outcome, credit?, streak?, message? }
      async checkin(getAuth) {
        state.checkinCalls += 1
        const auth = await getAuth()
        if (auth?.type === 'api') return { outcome: 'ineligible', message: 'check-in needs a signed-in account, not an API key' }
        if (state.alreadyToday) return { outcome: 'done', credit: 10 }
        if (state.dupeClaim) return { outcome: 'claimed', credit: 10, message: 'claimed again by upstream' }
        // 第一次 claimed，之后上游说今天已签
        return state.checkinCalls === 1
          ? { outcome: 'claimed', credit: 10, streak: 1, message: 'credit claimed' }
          : { outcome: 'done', credit: 10 }
      },
      options: [],
      methods: [
        {
          type: 'oauth',
          label: `${site.name} account (browser)`,
          // opts.redirectUri is set when the bridge owns the 127.0.0.1 callback;
          // without it the plugin owns the loopback listener (real behaviour).
          authorize: async (opts = {}) => {
            const redirectUri = opts.redirectUri
            const url = new URL(`https://${new URL(site.endpoint).hostname}/authorization`)
            url.searchParams.set('client_id', 'mock-client')
            url.searchParams.set('state', 'mock-state')
            if (redirectUri) url.searchParams.set('redirect_uri', redirectUri)
            return {
              url: url.toString(),
              instructions: `Sign in to ${site.name} in the browser; this finishes by itself.`,
              method: 'auto',
              async callback(params) {
                // bridge mode: the bridge hands the captured query back in
                if (params && Object.keys(params).length) {
                  if (params.error) return { type: 'failed', error: String(params.error) }
                  if (!params.code && !params.userJwt) return { type: 'failed', error: 'no code in the callback' }
                  return {
                    type: 'success',
                    access: `ak_${params.code ?? params.userJwt}`,
                    refresh: `rk_${params.code ?? params.userJwt}`,
                    expires: Date.now() + 3600_000,
                    accountId: 'mock-account',
                    uid: 'uid-mock',
                  }
                }
                // plugin mode: resolve as the real loopback flow would
                return {
                  type: 'success',
                  access: 'ak_plugin_mode_token',
                  refresh: 'rk_plugin_mode_token',
                  expires: Date.now() + 3600_000,
                  accountId: 'mock-account',
                  uid: 'uid-mock',
                }
              },
            }
          },
        },
      ],
    },
  })
}

export const MockAuthPlugin = makeMockPlugin('mock')
export const MockAIAuthPlugin = makeMockPlugin('mock-ai')

// for tests, as the real packages do
export const _internal = { SITES, state }
