// D-58 插件 hooks 受控面（P8 附带）。
//
// 语义（缺口分析 06-承接核对表 04-P1-14 翻案）：
//   桥支持插件的 hooks/middleware 接口，但启用前人工审查。为了让「审查什么」
//   有据可查，桥把两件事分开报出来：
//     · 声明面（declared）—— 插件对象上**键存在**的钩子（typeof/数组非空判定），
//       即「这个插件声明了哪些可被调用的面」；
//     · 实际面（actual）—— 桥运行时**真正调用过**的钩子，内存计数（重启清零）。
//   消费点清单（即 actual 的计数点，逐字对应代码里的调用位置）：
//     config            loader.ts describePlugin 调 inst.config
//     provider.models   loader.ts describePlugin 调 provider.models（函数形态才算调用）
//     auth.loader       server.ts wrapLoaderFetch 包的 loader 每次 chat 取上游选项
//     fetch_override    同上：loader 返回了自定义 fetch（被桥包装/使用）
//     auth.methods      admin/wiring.ts handle.authorize 调 methods[].authorize
//     auth.checkin      admin/wiring.ts handle.checkin 调 auth.checkin
//     auth.identity     admin/wiring.ts handle.identity 调 auth.identity（D-57）
//     auth.usage        桥只做能力探测（typeof），从不调用 → actual 恒 0
//     auth.refresh      桥未接线续期调用 → actual 恒 0
//     chat.headers      chat 泳道尚未消费 → actual 恒 0
//   /admin/hooks 与 /meta 的 hooks 摘要都从这里出，两处口径一致。

import type { BridgeContext, OpenCodePlugin } from "./types.ts";

/** 桥认识的插件钩子键（声明面枚举的取值域，也决定 actual 的键形状）。 */
export const KNOWN_HOOKS = [
  "config",
  "provider.models",
  "auth.loader",
  "auth.methods",
  "auth.checkin",
  "auth.identity",
  "auth.usage",
  "auth.refresh",
  "chat.headers",
] as const;

export type KnownHook = (typeof KNOWN_HOOKS)[number];

/** 实际面独有的键：不是对象上的钩子键，而是「loader 返回了自定义 fetch」这件事。 */
export const ACTUAL_ONLY_HOOKS = ["fetch_override"] as const;

/** /admin/hooks（全量）与 /meta.hooks（摘要 declared+actual）共用的形状。 */
export interface HookSurface {
  /** 声明面：全部 provider 声明的钩子键并集（排序去重）。 */
  declared: string[];
  /** 声明面按 provider 拆开（D-58 审查档案按 provider 记录）。 */
  by_provider: Record<string, string[]>;
  /** 实际面：桥运行时真实调用计数（内存，重启清零）；已知键恒在，未调用的为 0。 */
  actual: Record<string, number>;
  /** 取值域说明：声明面可能出现的键 + 实际面独有的键。 */
  known: string[];
}

export interface HookTracker {
  /** 加载期登记一个 provider 的声明面；返回该 provider 的声明键（排序）。 */
  declare(providerId: string, plugin: OpenCodePlugin): string[];
  /** 运行期计数一次真实调用。未知键也接受（插件面演化时不丢数据）。 */
  count(hook: string): void;
  /** 当前快照；actual 覆盖全部 known 键（未调用的为 0）。 */
  snapshot(): HookSurface;
}

/** actual 全 0 基线（无 tracker / 未调用时的形状）。 */
export function zeroActual(): Record<string, number> {
  const out: Record<string, number> = {};
  for (const hook of [...KNOWN_HOOKS, ...ACTUAL_ONLY_HOOKS]) out[hook] = 0;
  return out;
}

/**
 * GET /admin/hooks 的响应体（server.ts 与 selftest 共用）。
 * 桥没接 tracker（旧调用方）时按空面处理：declared/by_provider 为空、actual 全 0。
 */
export function buildHooksSurface(ctx: BridgeContext): HookSurface & { ok: true } {
  const surface = ctx.hooks
    ? ctx.hooks.snapshot()
    : { declared: [], by_provider: {}, actual: zeroActual(), known: [...KNOWN_HOOKS, ...ACTUAL_ONLY_HOOKS] };
  return { ok: true, ...surface };
}

/**
 * 从插件实例枚举声明面：只看对象键存在性（函数 / 非空数组 / 非 null 值），
 * 不调用任何钩子。trae/workbuddy/mock 三家真包形状已核对（types.ts OpenCodePlugin）。
 */
export function declaredHooksOf(plugin: OpenCodePlugin): string[] {
  const out = new Set<string>();
  if (typeof plugin.config === "function") out.add("config");
  const models = plugin.provider?.models;
  if (models !== undefined && models !== null) out.add("provider.models");
  const auth = plugin.auth;
  if (auth && typeof auth === "object") {
    if (typeof auth.loader === "function") out.add("auth.loader");
    if (Array.isArray(auth.methods) && auth.methods.length > 0) out.add("auth.methods");
    if (typeof auth.checkin === "function") out.add("auth.checkin");
    if (typeof auth.identity === "function") out.add("auth.identity");
    if (typeof auth.usage === "function") out.add("auth.usage");
    if (typeof auth.refresh === "function") out.add("auth.refresh");
  }
  if (typeof plugin["chat.headers"] === "function") out.add("chat.headers");
  return [...out].sort();
}

export function createHookTracker(): HookTracker {
  const byProvider: Record<string, string[]> = {};
  const actual = zeroActual();

  return {
    declare(providerId: string, plugin: OpenCodePlugin): string[] {
      const declared = declaredHooksOf(plugin);
      byProvider[providerId] = declared;
      return declared;
    },
    count(hook: string): void {
      if (typeof hook !== "string" || hook.length === 0) return;
      actual[hook] = (actual[hook] ?? 0) + 1;
    },
    snapshot(): HookSurface {
      const declared = [...new Set(Object.values(byProvider).flat())].sort();
      return {
        declared,
        by_provider: { ...byProvider },
        actual: { ...actual },
        known: [...KNOWN_HOOKS, ...ACTUAL_ONLY_HOOKS],
      };
    },
  };
}
