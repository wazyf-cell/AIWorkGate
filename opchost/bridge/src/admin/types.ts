// 管理面类型。
//
// 归属：共享契约以 ../types.ts 为唯一正源（bridge-core 已落盘）。本文件只放
// **管理面独有**的类型，并显式 import 共享类型；不许再定义同名的
// CredentialRecord / CredentialStore / Capability / CHECKIN_*（会遮蔽共享定义）。
//
// 与 Go 侧契约的对应：
//   internal/plugins/client.go    —— /accounts/{id}/checkin（5 分钟超时）
//   internal/model/model.go       —— CheckinResult(228) / Account(176) / QuotaSnapshot(470)
//   plans/2026-11-05-magpie接入与公网部署计划.md —— D-19 / D-20 / D-57

import type { Capability, IdentityResetResult, LoadResult, CredentialStore as SharedCredentialStore } from '../types.ts'

export type { Capability, IdentityResetResult, LoadResult, SharedCredentialStore }

/** SITES 表里被桥读取的字段（其余字段原样保留，便于导出）。 */
export interface PluginSite {
  id: string
  name?: string
  /** 上游 base URL，例如 https://copilot.tencent.com */
  endpoint?: string
  /** 插件注册的 provider/前缀，例如 workbuddy */
  platform?: string
  /** 该构建的 API Key 页面（常是第二个域名） */
  keys?: string
  authID?: string
  [k: string]: unknown
}

/** 插件 authorize() 交回的登录起始（trae / cline / workbuddy 三家一致）。 */
export interface PluginSigninStart {
  url: string
  instructions?: string
  method?: string
  /**
   * 浏览器流程结束时 resolve。trae/cline 自己起 127.0.0.1 监听并从这里 resolve；
   * workbuddy 轮询上游。桥不自己开浏览器，只把 url 交给宿主。
   */
  callback?: () => Promise<PluginSigninResult>
  [k: string]: unknown
}

export interface PluginSigninResult {
  type: 'success' | 'failed'
  access?: string
  refresh?: string
  expires?: number
  accountId?: string
  uid?: string
  domain?: string
  error?: string
  [k: string]: unknown
}

/** 插件 auth.checkin(getAuth) 的返回（D-19 实测：claimed/done/inactive/ineligible/failed）。 */
export interface PluginCheckinResult {
  outcome: string
  /** 本次签到到账的积分，上游给出时才有 */
  credit?: number
  streak?: number
  message?: string
}

/** 管理面需要的最小插件句柄；由 admin/wiring.ts 从 LoadResult 生成。 */
export interface PluginHandle {
  id: string
  name?: string
  version?: string
  /** 该插件导出的 model 前缀，例如 ['workbuddy','workbuddy-ai'] */
  providerIds: string[]
  capabilities(): Capability[]
  /** SITES 表；用于导出 network_domains（D-57：这里不含指纹） */
  sites(): Record<string, PluginSite>
  /** 起登录；桥自持回调时带 redirectUri */
  authorize(providerId: string, opts?: { redirectUri?: string }): Promise<PluginSigninStart>
  /** D-19 签到委派 */
  checkin(providerId: string, accountId: string): Promise<PluginCheckinResult>
  /**
   * D-57 指纹重置透传：mode "request" | "os"。插件没有 auth.identity 钩子时
   * 抛错（端点据此回 501）；mode 的归一化（小写/去空白）在 handler 完成。
   */
  identity(providerId: string, accountId: string, mode: 'request' | 'os'): Promise<IdentityResetResult>
  /** 账号 id 列表，供 /checkin 批量用；插件不提供时可缺省 */
  accounts?(providerId: string): Promise<Array<{ id: string; label?: string }>>
}

export interface PluginRegistry {
  list(): PluginHandle[]
  resolve(pluginId: string): PluginHandle | undefined
}

/**
 * 管理面凭据条目。注意与 ../types.ts 的 CredentialRecord 区别：
 * 共享那份是 **provider 级**（magpie plugin-auth.json 的形状），本份是
 * **account 级**（宿主 /accounts/{id} 的粒度），并默认加密落盘。
 */
export interface AdminCredential {
  pluginId: string
  accountId: string
  /** 原始凭据材料：只交给插件或加密落盘，绝不进日志/响应 */
  secret: Record<string, unknown>
  updatedAt: number
  /** Unix 秒 */
  expiresAt?: number
  refreshable?: boolean
}

/** 脱敏投影：只有键名、存在性、生命周期。绝不含指纹与 token（D-57）。 */
export interface AdminCredentialView {
  plugin_id: string
  account_id: string
  keys: string[]
  present: true
  updated_at: number
  expires_at?: number
  refreshable?: boolean
  identity?: never
}

export interface AdminCredentialStore {
  list(pluginId?: string): AdminCredential[]
  get(pluginId: string, accountId: string): AdminCredential | undefined
  put(record: Omit<AdminCredential, 'updatedAt'> & { updatedAt?: number }): AdminCredential
  delete(pluginId: string, accountId: string): boolean
}

/** D-20：多个 model 前缀共用同一条额度记录。 */
export interface QuotaGroup {
  id: string
  prefixes: string[]
  account_ns?: string
  label?: string
  reason?: string
}

/** 签到日账行：(plugin, account, 当天) 只记一次成功。 */
export interface CheckinLedgerRow {
  plugin_id: string
  account_id: string
  date: string
  result: 'ok' | 'already'
  credit_total: number
  credit_last?: number
  at: number
}

export interface AdminDeps {
  plugins: PluginRegistry
  /** 凭据/日账/共享组的落盘目录（--data-dir） */
  dataDir: string
  /** 凭据库；缺省用 dataDir 下的加密文件库 */
  credentials?: AdminCredentialStore
  /** 与共享 CredentialStore 对接时用（provider 级 → account 级适配） */
  sharedCredentials?: SharedCredentialStore
  /** D-20 定义；缺省 builtin → <data-dir>/quota-groups.json */
  quotaGroups?: QuotaGroup[]
  /** magpie settings.json 路径（D-19④）；缺省 ~/.config/magpie/settings.json */
  checkinSettingsPath?: string
  /** magpie quotas.json 路径（P9-1 多 window 额度）；缺省 ~/.config/magpie/quotas.json */
  quotaPath?: string
  /** 桥侧开关覆盖；缺省 <data-dir>/checkins.json */
  checkinOverridePath?: string
  now?: () => number
  /** 登录超时，缺省 5 分钟（与插件的 SIGN_IN_MS 一致） */
  signinTimeoutMs?: number
  /** 桥自持回调的绑定地址；只允许回环 */
  loopbackHost?: '127.0.0.1'
  /**
   * P8 出口白名单守卫（可选）。给了它，插件 auth.checkin / auth.identity 这类
   * 自带网络调用的钩子在桥内执行时走 wrapFetch——strict 白名单对这些此前
   * 绕过守卫的调用点也出拦截/放行日志（task-35②）。
   */
  egress?: import('../egress/guard.ts').EgressGuard
  logger?: { info(msg: string, fields?: Record<string, unknown>): void }
}

/** 供 wiring 使用：SITES 解析器（插件模块级 `_internal.SITES` 由加载器取出）。 */
export type SitesResolver = (providerId: string) => Record<string, PluginSite>
