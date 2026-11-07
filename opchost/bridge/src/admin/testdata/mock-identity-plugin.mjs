// D-57 selftest 插件：带 auth.identity 钩子的 magpie 形状插件（task-35）。
// 与 mock-plugin.mjs 同一外形约定（node: builtins only，无第三方依赖）：
//   - auth.identity({mode, accountId}) → { ok, message? }
//   - auth.identityOS = true（决定 identity_os_switch 能力位）
// 钩子可经 setIdentityState 注入 os 锁定（抛 409，验证桥透传状态码）。

export const IDENTITY_SITE = {
  id: 'ident',
  name: 'Identity Provider',
  endpoint: 'https://ident.example.com',
  platform: 'ident',
}

const state = {
  identityCalls: 0,
  /** 钩子收到的 (mode, accountId) 序列，供断言透传正确 */
  seen: [],
  /** os 模式是否被上游策略锁住（true → 抛 409） */
  osLocked: false,
}

export function setIdentityState(next) {
  Object.assign(state, next)
}
export function getIdentityState() {
  return { identityCalls: state.identityCalls, seen: [...state.seen], osLocked: state.osLocked }
}

export function makeIdentityPlugin() {
  return async () => ({
    config: async (config) => {
      config.provider ??= {}
      config.provider[IDENTITY_SITE.id] ??= {}
      config.provider[IDENTITY_SITE.id].name ??= IDENTITY_SITE.name
      config.provider[IDENTITY_SITE.id].api ??= `${IDENTITY_SITE.endpoint}/v2`
    },
    provider: {
      id: IDENTITY_SITE.id,
      async models() {
        return { [`${IDENTITY_SITE.id}-chat`]: { id: `${IDENTITY_SITE.id}-chat`, name: 'Ident Chat' } }
      },
    },
    auth: {
      provider: IDENTITY_SITE.id,
      // 声明 OS 级支持（决定桥的 identity_os_switch 能力位）
      identityOS: true,
      // D-57：指纹重置钩子。只回 ok/message，绝不回指纹材料本身。
      async identity({ mode, accountId } = {}) {
        state.identityCalls += 1
        state.seen.push({ mode, accountId })
        if (mode === 'os') {
          if (state.osLocked) {
            const e = new Error('os identity is locked by policy')
            e.status = 409
            throw e
          }
          return { ok: true, message: 'os identity switched' }
        }
        return { ok: true, message: 'next request re-derives the fingerprint' }
      },
    },
  })
}

export const IdentityAuthPlugin = makeIdentityPlugin()
