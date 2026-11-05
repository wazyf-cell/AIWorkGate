// 出口审计：data-dir 的 JSONL 追加 + 防日志洪泛 + 告警事件。
//
// 口径（docs/p8/01 §6.2/§6.3）：
//   - 只记 plugin/provider/host/port/ts/reason；**不记** URL path/query/请求头/正文/凭据；
//   - 同一 (plugin, host, reason) 每分钟最多 1 条明细，其余计数累加（矩阵 #22/#23）；
//   - EGRESS_PRIVATE_ADDRESS_BLOCKED 出现即告警；5 分钟内未声明拒绝 >10 告警（#24）。

import { appendFileSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";
import type { EgressReason } from "./whitelist.ts";

export type AuditAction = "denied" | "allowed";

export interface AuditFields {
  plugin: string;
  provider: string;
  host: string;
  port?: number;
  action: AuditAction;
  reason?: string;
}

/** 审计 JSONL 行（顺序固定，便于人读与聚合；禁止塞入任何凭据/请求内容）。 */
export interface AuditLine extends AuditFields {
  /** ISO8601 UTC */
  ts: string;
  /** 同 key 本分钟内第几次（第 1 次恒为 1） */
  count: number;
}

export interface AuditStats {
  denied: number;
  allowedWarn: number;
  detailLines: number;
  suppressed: number;
  alarms: string[];
}

export interface AuditOptions {
  /** JSONL 输出路径；空 = 只写日志不落盘 */
  file?: string;
  now?: () => number;
  log?: (level: "info" | "warn" | "error", message: string) => void;
}

const MINUTE_MS = 60_000;
const WINDOW_MS = 5 * MINUTE_MS;
const ALARM_THRESHOLD = 10;

export class AuditLog {
  private opts: Required<Pick<AuditOptions, "now" | "log">> & { file: string };
  private lastDetail = new Map<string, number>();
  private counts = new Map<string, number>();
  private deniedWindow: number[] = [];
  lastAlarmAt = 0;
  stats: AuditStats = { denied: 0, allowedWarn: 0, detailLines: 0, suppressed: 0, alarms: [] };

  constructor(opts: AuditOptions = {}) {
    this.opts = {
      file: opts.file ?? "",
      now: opts.now ?? (() => Date.now()),
      log: opts.log ?? (() => undefined),
    };
  }

  private key(f: AuditFields): string {
    return [f.plugin, f.provider, f.host, f.reason ?? ""].join("\u0001");
  }

  private writeLine(line: AuditLine): void {
    const raw = JSON.stringify(line);
    const file = this.opts.file;
    if (file) {
      try {
        mkdirSync(dirname(file), { recursive: true });
        appendFileSync(file, raw + "\n", "utf8");
        this.stats.detailLines += 1;
      } catch (err) {
        this.opts.log("error", `egress audit 落盘失败：${String(err)}`);
      }
    }
    this.opts.log("info", `egress audit: ${raw}`);
  }

  /** 记录一次判定。洪泛抑制：同 key 一分钟一条明细，其余累加 count。 */
  record(f: AuditFields): void {
    const now = this.opts.now();
    const reason = f.reason as EgressReason | undefined;
    if (f.action === "denied") {
      this.stats.denied += 1;
      this.trackDeniedWindow(now, reason);
    } else {
      this.stats.allowedWarn += 1;
    }
    const key = this.key(f);
    const last = this.lastDetail.get(key) ?? -Infinity;
    if (now - last < MINUTE_MS) {
      this.stats.suppressed += 1;
      this.counts.set(key, (this.counts.get(key) ?? 1) + 1);
      return;
    }
    this.counts.set(key, (this.counts.get(key) ?? 0) + 1);
    this.lastDetail.set(key, now);
    this.writeLine({
      ts: new Date(now).toISOString(),
      plugin: f.plugin,
      provider: f.provider,
      host: f.host,
      ...(f.port !== undefined ? { port: f.port } : {}),
      action: f.action,
      ...(reason ? { reason } : {}),
      count: this.counts.get(key) ?? 1,
    });
  }

  private trackDeniedWindow(now: number, reason?: string): void {
    if (reason === "EGRESS_PRIVATE_ADDRESS_BLOCKED") {
      const line = `插件访问私网/保留地址被拒（EGRESS_PRIVATE_ADDRESS_BLOCKED）@ ${new Date(now).toISOString()}`;
      this.stats.alarms.push(line);
      this.opts.log("warn", `egress alarm: ${line}`);
    }
    // 未声明/越界拒绝的 5 分钟滚动计数
    if (reason === "EGRESS_DOMAIN_NOT_DECLARED" || reason === "EGRESS_NO_DECLARATION") {
      this.deniedWindow.push(now);
      while (this.deniedWindow.length > 0 && this.deniedWindow[0] <= now - WINDOW_MS) {
        this.deniedWindow.shift();
      }
      if (
        this.deniedWindow.length > ALARM_THRESHOLD &&
        now - this.lastAlarmAt >= MINUTE_MS
      ) {
        this.lastAlarmAt = now;
        const line = `5 分钟内未声明域名拒绝 ${this.deniedWindow.length} 次（>${ALARM_THRESHOLD}），插件可能被投毒或有隐藏 telemetry @ ${new Date(now).toISOString()}`;
        this.stats.alarms.push(line);
        this.opts.log("warn", `egress alarm: ${line}`);
      }
    }
  }
}