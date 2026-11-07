// task-35②：插件 auth 钩子的出口守卫覆盖。
//
// 背景：插件的 auth.checkin / auth.identity（续期同理，接线后）会自己发上游
// 请求——走的是模块内全局 fetch，此前绕过了 P8 egress 白名单守卫；strict 模式
// 下这些调用既不被拦、审计里也没有拦截/放行日志。
//
// 做法：桥在调用这类钩子的窗口内，把 globalThis.fetch 换成 wrapFetch 包过的
// 版本（守卫的 audit 照常记账：allowed/denied + reason），窗口结束原样还原。
// 与 chat 泳道在 loader 边界包 options.fetch（server.ts wrapLoaderFetch）互补：
// 那条路包的是 loader 给的 fetch，这条路兜住钩子自己的直连。
//
// 并发语义（如实声明）：depth 计数保证嵌套/并发调用都处于受守卫窗口，但
// 「最外层还原」与并发调用者的执行交错时，后到的调用者可能在还原后继续直连。
// 管理面的 checkin/identity 是用户触发、准串行的，窗口重叠概率极低；若未来
// 要并发批量执行，应改为显式把守卫后的 fetch 传进插件，而不是换全局。

import type { AdminDeps } from './types.ts'

let depth = 0

export async function withEgressFetch<T>(
  deps: AdminDeps,
  pluginId: string,
  providerId: string,
  fn: () => Promise<T>,
): Promise<T> {
  const guard = deps.egress
  if (!guard || typeof globalThis.fetch !== 'function') return fn()
  if (depth > 0) {
    // 已处于某个受守卫窗口内：不重复换（避免嵌套保存/还原互相覆盖）
    return fn()
  }
  depth++
  const original = globalThis.fetch
  try {
    globalThis.fetch = guard.wrapFetch({ plugin: pluginId, provider: providerId, fetchImpl: original })
    return await fn()
  } finally {
    depth--
    if (depth === 0) globalThis.fetch = original
  }
}
