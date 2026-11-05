// 入站请求体解析与校验：model/messages/stream/temperature/max_tokens 等。
// 失败一律抛 ChatError(400)，由 handler 统一转 OpenAI 错误体。

import { badRequest } from "./errors.ts";
import type { ChatRequest, ChatMessage } from "./types.ts";

const MAX_MESSAGES = 4096;

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** 容错取整数：true/1/"1" → 1；非法返回 undefined。 */
function intOf(v: unknown): number | undefined {
  if (v === undefined || v === null) return undefined;
  if (typeof v === "boolean") return v ? 1 : 0;
  const n = Number(v);
  return Number.isFinite(n) ? Math.trunc(n) : undefined;
}

function boolOf(v: unknown): boolean {
  if (typeof v === "boolean") return v;
  if (v === 1 || v === "1" || v === "true") return true;
  if (v === 0 || v === "0" || v === "false") return false;
  return false;
}

function numOf(v: unknown): number | undefined {
  if (v === undefined || v === null) return undefined;
  const n = Number(v);
  return Number.isFinite(n) ? n : undefined;
}

/** 校验一条消息：至少要有 role；content 可为字符串/数组/空。 */
function checkMessage(m: unknown, i: number): ChatMessage {
  if (!isPlainObject(m)) {
    throw badRequest(`messages[${i}] 必须是对象`, "bad_message");
  }
  const role = m.role;
  if (typeof role !== "string" || role.trim() === "") {
    throw badRequest(`messages[${i}].role 缺失或非法（须为字符串）`, "bad_message");
  }
  return m as ChatMessage;
}

/**
 * 把原始请求体解析成 ChatRequest。
 * @param rawBody 已 JSON.parse 的对象
 */
export function parseChatRequestBody(rawBody: unknown): ChatRequest {
  if (!isPlainObject(rawBody)) {
    throw badRequest("请求体必须是 JSON 对象（OpenAI /v1/chat/completions 形状）", "bad_request");
  }
  const { model, messages, stream } = rawBody;
  if (typeof model !== "string" || model.trim() === "") {
    throw badRequest("model 缺失或非法（须为字符串，如 trae-cn/doubao-seed-1.6）", "missing_model");
  }
  if (!Array.isArray(messages)) {
    throw badRequest("messages 缺失或不是数组", "missing_messages");
  }
  if (messages.length === 0) {
    throw badRequest("messages 不能为空", "empty_messages");
  }
  if (messages.length > MAX_MESSAGES) {
    throw badRequest(`messages 数量超过上限（${MAX_MESSAGES}）`, "too_many_messages");
  }

  const checked: ChatMessage[] = messages.map(checkMessage);
  const temperature = numOf(rawBody.temperature);
  if (temperature !== undefined && (temperature < 0 || temperature > 2)) {
    throw badRequest(`temperature 越界（${temperature}，允许 0~2）`, "bad_temperature");
  }
  const topP = numOf(rawBody.top_p);
  if (topP !== undefined && (topP < 0 || topP > 1)) {
    throw badRequest(`top_p 越界（${topP}，允许 0~1）`, "bad_top_p");
  }

  return {
    model: String(model).trim(),
    messages: checked,
    stream: boolOf(stream),
    temperature,
    top_p: topP,
    max_tokens: intOf(rawBody.max_tokens),
    max_completion_tokens: intOf(rawBody.max_completion_tokens),
    raw: rawBody,
  };
}

/** 供 handler 读请求体用的上限检查。 */
export function bodyTooLarge(size: number, max: number): boolean {
  return size > max;
}
