// 越界拒绝错误：调用方（chat 上游等）把它映射成可读错误；HTTP 语义 403。
// 独立成文件避免 guard ↔ wrap 循环依赖。

import type { EgressReason } from "./whitelist.ts";

export class EgressError extends Error {
  reason: EgressReason;
  status = 403;
  host: string;
  port: number;
  plugin: string;
  provider: string;

  constructor(init: {
    reason: EgressReason;
    host: string;
    port: number;
    plugin: string;
    provider: string;
    message: string;
  }) {
    super(init.message);
    this.name = "EgressError";
    this.reason = init.reason;
    this.host = init.host;
    this.port = init.port;
    this.plugin = init.plugin;
    this.provider = init.provider;
  }
}

/** 拒绝原因 → 可读文案（进日志/错误体）。 */
export function describeDeny(
  reason: EgressReason,
  host: string,
  port: number,
  plugin: string,
  strict: boolean,
): string {
  switch (reason) {
    case "EGRESS_NO_DECLARATION":
      return `出口白名单：插件「${plugin}」strict=${strict} 且未声明任何 network_domains，按策略拒绝一切外联（${host}:${port}）`;
    case "EGRESS_DOMAIN_NOT_DECLARED":
      return `出口白名单：插件「${plugin}」请求 ${host}:${port} 未在声明域内，已拒绝`;
    case "EGRESS_PORT_FORBIDDEN":
      return `出口白名单：插件「${plugin}」请求 ${host}:${port}，端口不在允许列表（80/443），已拒绝`;
    case "EGRESS_PRIVATE_ADDRESS_BLOCKED":
      return `出口白名单：插件「${plugin}」请求 ${host}:${port} 命中私网/保留地址（SSRF 防护），已拒绝`;
    default:
      return `出口白名单：插件「${plugin}」请求 ${host}:${port} 被拒绝（${reason}）`;
  }
}