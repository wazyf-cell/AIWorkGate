// 出口白名单核心引擎：加载期语法校验（docs/p8/01 §4.1）+ 运行期匹配（§4.2）。
//
// 语义来源：docs/p8/01-出口白名单设计与测试矩阵.md（唯一正本），这里只实现不发明：
//   - 声明项必须是 ASCII 主机名；禁止 scheme/path/端口/用户信息；
//   - 通配符只允许最左且带点（*.trae.ai），永不匹配 IP；
//   - IP 字面量允许但必须精确匹配；
//   - 匹配：normalize(host)=小写去尾点；entry 无 '*' → 恒等；"*.S" → host 以 ".S" 结尾
//     且 host != S（至少多一级）；
//   - strict=false 且未声明 → 放行 + WARN（矩阵 #10）；strict=true 且未声明 → 拒绝。

import * as dns from "node:dns";

// ---- 拒绝原因码（固定枚举，写日志/审计用；与 01 号 §6.1 对齐） -------------

export type EgressReason =
  | "EGRESS_NO_DECLARATION"
  | "EGRESS_DOMAIN_NOT_DECLARED"
  | "EGRESS_PORT_FORBIDDEN"
  | "EGRESS_PRIVATE_ADDRESS_BLOCKED"
  | "EGRESS_BAD_TARGET";

// ---- 语法校验（§4.1） ------------------------------------------------------

const LABEL_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
const HOSTNAME_RE = /^[a-z0-9.-]+$/;
const WILDCARD_PREFIX = "*.";

export interface DomainEntry {
  /** 归一后的声明原文（小写，无尾点） */
  value: string;
  wildcard: boolean;
}

/** 默认拒绝的「公共后缀通配」：父域只有一级即视为公共后缀（*.ai、*.com…）。 */
function isPublicSuffixLike(parent: string): boolean {
  return !parent.includes(".");
}

export function validateDomainEntry(raw: string): { ok: true; entry: DomainEntry } | { ok: false; error: string } {
  const input = String(raw ?? "");
  if (input.trim() === "") return { ok: false, error: "声明为空" };
  const trimmed = input.trim();
  if (trimmed !== input) return { ok: false, error: `声明含首尾空白：${JSON.stringify(input)}` };

  const lower = trimmed.toLowerCase();
  if (/[^\x00-\x7f]/.test(lower)) {
    return { ok: false, error: `声明含非 ASCII 字符（IDN 必须写成 punycode）：${input}` };
  }

  let wildcard = false;
  let host = lower;
  if (host.startsWith(WILDCARD_PREFIX)) {
    wildcard = true;
    host = host.slice(2);
    if (host === "") return { ok: false, error: `裸通配符：${input}` };
  }
  if (host.includes("*")) {
    return { ok: false, error: `通配符只允许最左且必须带点：${input}` };
  }
  if (!HOSTNAME_RE.test(host)) {
    return { ok: false, error: `只允许 [a-z0-9.-]，禁止 scheme/路径/端口/用户信息：${input}` };
  }
  if (wildcard && isPublicSuffixLike(host)) {
    return { ok: false, error: `禁止公共后缀通配：${input}（父域 ${host} 只有一级标签）` };
  }

  if (host.endsWith(".")) {
    return { ok: false, error: `声明不得以点结尾（尾点只在请求侧归一）：${input}` };
  }
  if (host.length < 1 || host.length > 253) {
    return { ok: false, error: `总长必须 1~253：${input}` };
  }

  const labels = host.split(".");
  if (labels.length < 2) {
    return { ok: false, error: `至少需要 2 个标签（服务器桥只允许回环到宿主监听器，不经白名单）：${input}` };
  }
  for (const label of labels) {
    if (label === "") return { ok: false, error: `空标签（连续点）：${input}` };
    if (!LABEL_RE.test(label)) {
      return { ok: false, error: `标签 ${label} 非 [a-z0-9-]、或以 - 开头/结尾、或超长：${input}` };
    }
  }
  // IP 字面量：允许精确匹配（IPv4 点分十进制）；通配符永不匹配 IP
  if (isIPv4Literal(host) && wildcard) {
    return { ok: false, error: `通配符永不匹配 IP：${input}` };
  }

  return { ok: true, entry: { value: host, wildcard } };
}

export interface DeclarationResult {
  ok: boolean;
  entries: DomainEntry[];
  errors: string[];
}

/** 整组校验：任一项非法 → 整组拒绝（矩阵 #19/#20/#21 = LOADFAIL）。 */
export function validateDeclarations(list: string[], max = 64): DeclarationResult {
  const seen = new Set<string>();
  const entries: DomainEntry[] = [];
  const errors: string[] = [];
  for (const raw of list) {
    const r = validateDomainEntry(raw);
    if (!r.ok) {
      errors.push(r.error);
      continue;
    }
    // 通配与精确共用一个 value，去重键必须区分（*.trae.ai ≠ trae.ai）
    const key = (r.entry.wildcard ? "*." : "") + r.entry.value;
    if (seen.has(key)) continue;
    seen.add(key);
    entries.push(r.entry);
  }
  if (seen.size > max) {
    errors.push(`声明数量 ${seen.size} 超过上限 ${max}（防白名单退化成通配）`);
  }
  return { ok: errors.length === 0, entries, errors };
}

// ---- 运行期匹配（§4.2） ----------------------------------------------------

export function normalizeHost(raw: string): string {
  const trimmed = String(raw ?? "").trim().toLowerCase();
  return trimmed.endsWith(".") ? trimmed.slice(0, -1) : trimmed;
}

/** host 是否命中某条声明。 */
export function hostMatches(normalizedHost: string, entry: DomainEntry): boolean {
  if (!entry.wildcard) return normalizedHost === entry.value;
  const parent = entry.value;
  return (
    normalizedHost.length > parent.length &&
    normalizedHost.endsWith("." + parent)
  );
}

/** 任意一条声明命中即放行（host 已 normalize）。 */
export function anyMatch(normalizedHost: string, entries: DomainEntry[]): boolean {
  return entries.some((e) => hostMatches(normalizedHost, e));
}

// ---- 私网/保留地址判定（§7-④ SSRF 防护） ------------------------------------

export function isIPv4Literal(host: string): boolean {
  const parts = host.split(".");
  if (parts.length !== 4) return false;
  return parts.every((p) => {
    if (!/^\d{1,3}$/.test(p)) return false;
    const n = Number(p);
    return n >= 0 && n <= 255;
  });
}

function ipv4ToInt(host: string): number {
  return host.split(".").reduce((acc, p) => acc * 256 + Number(p), 0) >>> 0;
}

function ipv6InRange(addr: string): boolean {
  // 只判最常见的保留块；完整列表见文档，这里覆盖矩阵 #14 与常见 SSRF 目标
  const a = addr.toLowerCase();
  if (a === "::1") return true; // loopback
  if (a.startsWith("fc") || a.startsWith("fd")) return true; // fc00::/7 唯一本地
  if (a.startsWith("fe8") || a.startsWith("fe9") || a.startsWith("fea") || a.startsWith("feb")) return true; // fe80::/10
  if (a.startsWith("::ffff:")) {
    // IPv4 映射地址：按 IPv4 判定
    const mapped = a.slice("::ffff:".length);
    if (isIPv4Literal(mapped)) return isPrivateOrReservedIPv4(mapped);
  }
  return false;
}

export function isPrivateOrReservedIPv4(host: string): boolean {
  const n = ipv4ToInt(host);
  const inRange = (start: number, end: number): boolean => n >= start && n <= end;
  if (inRange(0x00000000, 0x00ffffff)) return true; // 0.0.0.0/8
  if (inRange(0x0a000000, 0x0affffff)) return true; // 10.0.0.0/8
  if (inRange(0x7f000000, 0x7fffffff)) return true; // 127.0.0.0/8
  if (inRange(0xa9fe0000, 0xa9feffff)) return true; // 169.254.0.0/16（云元数据）
  if (inRange(0xac100000, 0xac1fffff)) return true; // 172.16.0.0/12
  if (inRange(0xc0a80000, 0xc0a8ffff)) return true; // 192.168.0.0/16
  if (inRange(0x64400000, 0x647fffff)) return true; // 100.64.0.0/10 CGNAT
  if (inRange(0xc0000000, 0xc00000ff)) return true; // 192.0.0.0/24
  if (inRange(0xc6120000, 0xc613ffff)) return true; // 198.18.0.0/15 基准测试
  if (inRange(0xe0000000, 0xffffffff)) return true; // 组播/保留
  return false;
}

/** host 可能是 IP 字面量且属于私网/保留段。 */
export function isPrivateLiteral(host: string): boolean {
  if (isIPv4Literal(host)) return isPrivateOrReservedIPv4(host);
  if (host.includes(":")) return ipv6InRange(host);
  return false;
}

export type Resolver = (host: string) => Promise<string[] | null>;

/** 默认解析器：dns.promises.lookup all;失败返回 null（此时不做私网判定）。 */
export const defaultResolver: Resolver = async (host: string): Promise<string[] | null> => {
  try {
    const result = await dns.promises.lookup(host, { all: true });
    return result.map((r) => r.address);
  } catch {
    return null;
  }
};

/** 域名解析后是否命中私网/保留地址。resolve 返回 null（解析失败）→ 返回 false，允许继续（连接本来就会失败，L3 兜底）。 */
export async function resolvesToPrivate(
  host: string,
  resolve: Resolver = defaultResolver,
): Promise<boolean> {
  const ips = await resolve(host);
  if (!ips || ips.length === 0) return false;
  return ips.some((ip) => isPrivateLiteral(ip));
}