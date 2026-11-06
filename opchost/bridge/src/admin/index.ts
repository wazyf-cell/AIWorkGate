// 管理面出口：桥的 server.ts 只需把请求交给 createAdminRouter().handle(req,res)。
//
// 集成契约（给 bridge-core / 宿主侧）：
//   const admin = createAdminRouter({
//     plugins,            // PluginRegistry：list() + resolve(id)
//     dataDir,            // --data-dir 参数
//     logger,             // 可选；只记 id 与计数，凭据永不进日志
//   })
//   // 在 server 里： if (await admin.handle(req, res)) return
//   // 关闭时：      admin.close()
//
// PluginHandle 的最小实现（插件加载器负责把 magpie AuthPlugin 适配成这样）：
//   authorize(providerId, opts?) → 把 auth.methods[].authorize() 的结果原样返回
//   checkin(providerId, accountId) → 调插件的 auth.checkin(getAuth)，返回其 outcome 对象
//   sites() → 插件导出的 SITES（workbuddy/trae 都在 _internal.SITES）
//   accounts(providerId) → 可选，列账号 id，供 /checkin 批量用
//
// 本模块不含任何凭据明文输出路径：/admin/** 的响应只有键名、存在性、生命周期。

export { createAdminRouter } from './router.ts'
export type { AdminRouter } from './router.ts'

export type {
  AdminCredential,
  AdminCredentialStore,
  AdminCredentialView,
  AdminDeps,
  Capability,
  CheckinLedgerRow,
  LoadResult,
  PluginCheckinResult,
  PluginHandle,
  PluginRegistry,
  PluginSigninResult,
  PluginSigninStart,
  PluginSite,
  QuotaGroup,
  SharedCredentialStore,
  SitesResolver,
} from './types.ts'

export { createFileCredentialStore, toView, exportView } from './credentials.ts'
export { createSigninManager } from './signin.ts'
export { deriveManifest, hostOf } from './manifest.ts'
export {
  DEFAULT_QUOTA_GROUPS,
  buildIndex,
  loadQuotaGroups,
  mergeQuotaRecords,
  normalizePrefix,
  quotaRecordFor,
  validateGroups,
} from './quotagroups.ts'
export { createCheckinLedger } from './ledger.ts'
export type { CheckinLedger } from './ledger.ts'
export {
  defaultQuotaPath,
  loadQuotaWindows,
  quotaRecordForAccount,
  quotaWindowsView,
} from './quotas.ts'
export type { QuotaRecord, QuotaWindow, QuotaWindowsView } from './quotas.ts'
export { mapOutcome, normalizeCheckin, runCheckin } from './checkin.ts'
export { createAccountLedger, listAccounts, buildAccount, statusOf, accountRefsFor } from './accounts.ts'
export type { AccountPayload, AccountQuota, AccountCredential, AccountLedger, AccountRef } from './accounts.ts'
export { loadCheckinSwitches } from './switches.ts'
export { isLoopback, redactKeys, sanitizeForLog, secretFingerprint } from './util.ts'
export { registryFromLoad, adminDepsFromBridge, chatAuthStore } from './wiring.ts'
export type { RegistryOptions, ChatAuthStore } from './wiring.ts'
