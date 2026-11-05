// 宿主环境装载：AIWORKGATE_EGRESS_FILE / AIWORKGATE_RULES_FILE / AIWORKGATE_PLUGIN_DATA_DIR。
//
// 语义对照（只读核实）：
//   - internal/plugins/env.go:28-31 — 宿主每启一个插件注入上述三个变量（+ PORT/TOKEN/ID）；
//   - internal/app/runtime_files.go:59-70 — egress.json 由宿主写成 {version, strict, default, accounts},
//     strict 就是 egress.strict 语义（docs/p8/01 §3：strict=true 时未声明 = 拒绝一切外联）；
//   - internal/rules/manager.go:28 — rules.json 是内容规则包（packs/rules/strip），与白名单无关，
//     但若未来携带 network_domains（潜在扩展）同样并入允许集合。
//
// 桥内 bridge 插件自身的声明（network_domains）以插件 SITES 表为准（/admin/manifest 导出），
// 由接线方在加载期调用 guard.declare() 注册；本文件只负责把宿主 env 指的文件读进来。

import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { EgressGuard } from "./guard.ts";
import { validateDomainEntry } from "./whitelist.ts";

export interface EgressFileShape {
  version?: number;
  strict?: boolean;
  network_domains?: string[];
  default?: unknown;
  accounts?: unknown;
  [key: string]: unknown;
}

export interface RulesFileShape {
  packs?: string[];
  rules?: unknown[];
  strip?: unknown[];
  network_domains?: string[];
  [key: string]: unknown;
}

export interface LoadedEgressConfig {
  strict: boolean;
  egressFile: string;
  rulesFile: string;
  dataDir: string;
  auditFile: string;
  /** 合并自 egress.json / rules.json 的域名声明（通常为空；SITES 表才是正主） */
  domainsFromFiles: string[];
  warnings: string[];
}

export type FileReader = (path: string) => string;

function readMaybe(path: string, read: FileReader): string {
  try {
    return read(path);
  } catch {
    return "";
  }
}

function parseTolerant<T>(text: string): Partial<T> | null {
  if (!text.trim()) return null;
  try {
    const v: unknown = JSON.parse(text);
    return typeof v === "object" && v !== null && !Array.isArray(v) ? (v as Partial<T>) : null;
  } catch {
    return null;
  }
}

const ENV = {
  egressFile: "AIWORKGATE_EGRESS_FILE",
  rulesFile: "AIWORKGATE_RULES_FILE",
  dataDir: "AIWORKGATE_PLUGIN_DATA_DIR",
} as const;

/** 从宿主 env（或测试注入的 env 对象）装载出口配置；任何文件缺失/损坏都不抛错，只记 warning。 */
export function loadEgressConfig(
  env: Record<string, string | undefined> = process.env,
  read: FileReader = (p) => readFileSync(p, "utf8"),
  dataDirOverride?: string,
): LoadedEgressConfig {
  const egressFile = env[ENV.egressFile] ?? "";
  const rulesFile = env[ENV.rulesFile] ?? "";
  const dataDir = dataDirOverride ?? env[ENV.dataDir] ?? join(process.cwd(), ".devdata");
  const warnings: string[] = [];

  let strict = false;
  const domainsFromFiles: string[] = [];

  if (egressFile) {
    const parsed = parseTolerant<EgressFileShape>(readMaybe(egressFile, read));
    if (parsed === null) {
      warnings.push(`AIWORKGATE_EGRESS_FILE=${egressFile} 缺失或不是合法 JSON；按非 strict 处理`);
    } else {
      strict = parsed.strict === true;
      for (const d of parsed.network_domains ?? []) {
        if (typeof d === "string") domainsFromFiles.push(d);
      }
    }
  } else {
    warnings.push("未注入 AIWORKGATE_EGRESS_FILE：出口白名单按非 strict（放行+记账）语义运行");
  }

  if (rulesFile) {
    const parsed = parseTolerant<RulesFileShape>(readMaybe(rulesFile, read));
    if (parsed === null) {
      warnings.push(`AIWORKGATE_RULES_FILE=${rulesFile} 缺失或不是合法 JSON（仅记录，不影响白名单）`);
    } else {
      for (const d of parsed.network_domains ?? []) {
        if (typeof d === "string") domainsFromFiles.push(d);
      }
    }
  }

  // 校验文件里带来的声明；非法条目只告警，不把坏条目写进允许集合
  const bad = domainsFromFiles.filter((d) => !validateDomainEntry(d).ok);
  for (const b of bad) warnings.push(`egress.json/rules.json 中的声明非法并已忽略：${b}`);

  return {
    strict,
    egressFile,
    rulesFile,
    dataDir,
    auditFile: join(dataDir, "egress-audit.jsonl"),
    domainsFromFiles: domainsFromFiles.filter((d) => validateDomainEntry(d).ok),
    warnings,
  };
}

/** 把装载结果落到 guard 上：strict + 文件级声明（以插件「bridge」名义）。 */
export function applyEgressConfig(
  guard: EgressGuard,
  config: LoadedEgressConfig,
): { errors: string[] } {
  guard.configureStrict(config.strict);
  if (config.domainsFromFiles.length > 0) {
    const r = guard.declare({
      plugin: "bridge",
      provider: "bridge",
      domains: config.domainsFromFiles,
    });
    if (!r.ok) return { errors: r.errors };
  }
  return { errors: [] };
}