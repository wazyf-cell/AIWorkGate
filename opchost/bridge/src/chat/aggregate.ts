// 非流式聚合：把上游结果（整包 JSON / 或误返回的 SSE）归一成标准 OpenAI 响应。
// 目标形状：{ id, object:"chat.completion", created, model, choices, usage }

import { randomBytes } from "node:crypto";
import type {
  ChatMessage,
  OpenAIChatChoice,
  OpenAIChatCompletion,
  OpenAIUsage,
} from "./types.ts";

export function newCompletionId(): string {
  return `chatcmpl-${randomBytes(12).toString("hex")}`;
}

function asRecord(v: unknown): Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v)
    ? (v as Record<string, unknown>)
    : {};
}

/** 粗略 token 估算（仅在上下游都没给 usage 时兜底，并打 estimated 标记）。 */
export function estimateTokens(text: string): number {
  if (!text) return 0;
  // 中英混排经验值：约 1 token / 3 字符（CJK 偏 1/1.5，英文偏 1/4）
  return Math.max(1, Math.ceil([...text].length / 3));
}

function messagesText(messages: ChatMessage[]): string {
  let out = "";
  for (const m of messages) {
    if (typeof m.content === "string") out += m.content;
    else if (Array.isArray(m.content)) {
      for (const part of m.content) {
        const p = asRecord(part);
        if (typeof p.text === "string") out += p.text;
      }
    }
  }
  return out;
}

function normalizeUsage(raw: unknown, fallback: OpenAIUsage): OpenAIUsage {
  const u = asRecord(raw);
  const pick = (k: string): number | undefined => {
    const n = Number(u[k]);
    return Number.isFinite(n) ? n : undefined;
  };
  const prompt = pick("prompt_tokens");
  const completion = pick("completion_tokens");
  const total = pick("total_tokens");
  if (prompt === undefined && completion === undefined && total === undefined) {
    return fallback;
  }
  const p = prompt ?? 0;
  const c = completion ?? 0;
  const t = total ?? p + c;
  const out: OpenAIUsage = { prompt_tokens: p, completion_tokens: c, total_tokens: t };
  // 上游若有额外 usage 细节（如 cached_tokens 或 trae 的 credits），保留
  for (const [k, v] of Object.entries(u)) {
    if (!(k in out)) out[k] = v;
  }
  return out;
}

function normalizeChoice(raw: unknown, index: number): OpenAIChatChoice {
  const c = asRecord(raw);
  const message = asRecord(c.message ?? c.delta);
  const choice: OpenAIChatChoice = {
    index: Number.isFinite(Number(c.index)) ? Number(c.index) : index,
    message: {
      role: typeof message.role === "string" && message.role ? message.role : "assistant",
      content:
        typeof message.content === "string" || message.content === null
          ? (message.content as string | null)
          : typeof message.content === "undefined"
            ? ""
            : JSON.stringify(message.content),
    },
    finish_reason:
      typeof c.finish_reason === "string" && c.finish_reason ? c.finish_reason : "stop",
  };
  if (typeof message.reasoning_content === "string" && message.reasoning_content) {
    choice.message!.reasoning_content = message.reasoning_content;
  }
  if (Array.isArray(message.tool_calls) && message.tool_calls.length > 0) {
    choice.message!.tool_calls = message.tool_calls.map((rawCall, i) => {
      const call = asRecord(rawCall);
      const fn = asRecord(call.function);
      return {
        index: i,
        id: typeof call.id === "string" ? call.id : undefined,
        type: typeof call.type === "string" && call.type ? call.type : "function",
        function: {
          name: typeof fn.name === "string" ? fn.name : "",
          arguments: typeof fn.arguments === "string" ? fn.arguments : "",
        },
      };
    });
  }
  return choice;
}

/** 归一化上游整包 JSON。 */
export function normalizeCompletion(
  raw: unknown,
  ctx: { model: string; messages: ChatMessage[] },
): OpenAIChatCompletion {
  const r = asRecord(raw);
  const rawChoices = Array.isArray(r.choices) ? r.choices : [];
  const choices = rawChoices.length > 0
    ? rawChoices.map((c, i) => normalizeChoice(c, i))
    : [normalizeChoice({}, 0)];

  const completionText = choices
    .map((c) => (typeof c.message?.content === "string" ? c.message.content : ""))
    .join("");
  const fallbackUsage: OpenAIUsage = {
    prompt_tokens: estimateTokens(messagesText(ctx.messages)),
    completion_tokens: estimateTokens(completionText),
    total_tokens:
      estimateTokens(messagesText(ctx.messages)) + estimateTokens(completionText),
    estimated: true,
  };

  const out: OpenAIChatCompletion = {
    id: typeof r.id === "string" && r.id ? r.id : newCompletionId(),
    object: "chat.completion",
    created: Number.isFinite(Number(r.created))
      ? Number(r.created)
      : Math.floor(Date.now() / 1000),
    model: ctx.model,
    choices,
    usage: normalizeUsage(r.usage, fallbackUsage),
  };
  if (typeof r.system_fingerprint === "string") out.system_fingerprint = r.system_fingerprint;
  return out;
}

interface AggState {
  id: string;
  created: number;
  model: string;
  text: string;
  reasoning: string;
  finish: string;
  usage: unknown;
  toolCalls: Map<number, { id?: string; name?: string; arguments: string }>;
}

function newState(model: string): AggState {
  return {
    id: "",
    created: 0,
    model,
    text: "",
    reasoning: "",
    finish: "",
    usage: null,
    toolCalls: new Map(),
  };
}

/** 把一条 chunk 的 delta 合并进聚合状态（保序：tool_calls 按 index 累积 arguments）。 */
function absorb(state: AggState, payload: Record<string, unknown>, chunkModel: string): void {
  if (typeof payload.id === "string" && payload.id) state.id = payload.id;
  if (Number.isFinite(Number(payload.created)) && !state.created) {
    state.created = Number(payload.created);
  }
  if (chunkModel) state.model = chunkModel;
  const choices = Array.isArray(payload.choices) ? payload.choices : [];
  for (const rawChoice of choices) {
    const c = asRecord(rawChoice);
    const delta = asRecord(c.delta ?? c.message);
    if (typeof delta.content === "string") state.text += delta.content;
    if (typeof delta.reasoning_content === "string") state.reasoning += delta.reasoning_content;
    if (Array.isArray(delta.tool_calls)) {
      for (const rawCall of delta.tool_calls) {
        const call = asRecord(rawCall);
        const idx = Number.isFinite(Number(call.index)) ? Number(call.index) : state.toolCalls.size;
        const cur = state.toolCalls.get(idx) ?? { arguments: "" };
        if (typeof call.id === "string" && call.id) cur.id = call.id;
        const fn = asRecord(call.function);
        if (typeof fn.name === "string" && fn.name) cur.name = fn.name;
        if (typeof fn.arguments === "string") cur.arguments += fn.arguments;
        state.toolCalls.set(idx, cur);
      }
    }
    if (typeof c.finish_reason === "string" && c.finish_reason) state.finish = c.finish_reason;
  }
  if (payload.usage) state.usage = payload.usage;
}

/** 把流式帧聚合回一个非流式 completion（上游忽略 stream=false 时的兜底）。 */
export function aggregateChunks(
  payloads: string[],
  ctx: { model: string; messages: ChatMessage[] },
): OpenAIChatCompletion {
  const state = newState(ctx.model);
  for (const raw of payloads) {
    if (raw.trim() === "[DONE]") continue;
    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      continue; // 非 JSON 帧无法聚合，跳过（原帧由流式路径负责透传）
    }
    absorb(state, asRecord(parsed), ctx.model);
  }
  const message: Record<string, unknown> = {
    role: "assistant",
    content: state.text || (state.toolCalls.size > 0 ? null : ""),
  };
  if (state.reasoning) message.reasoning_content = state.reasoning;
  if (state.toolCalls.size > 0) {
    message.tool_calls = [...state.toolCalls.entries()]
      .sort((a, b) => a[0] - b[0])
      .map(([idx, c]) => ({
        index: idx,
        id: c.id,
        type: "function",
        function: { name: c.name ?? "", arguments: c.arguments },
      }));
  }
  const completionText = state.text;
  const promptTokens = estimateTokens(messagesText(ctx.messages));
  const completionTokens = estimateTokens(completionText);
  const fallbackUsage: OpenAIUsage = {
    prompt_tokens: promptTokens,
    completion_tokens: completionTokens,
    total_tokens: promptTokens + completionTokens,
    estimated: true,
  };
  return {
    id: state.id || newCompletionId(),
    object: "chat.completion",
    created: state.created || Math.floor(Date.now() / 1000),
    model: state.model || ctx.model,
    choices: [
      {
        index: 0,
        message,
        finish_reason: state.toolCalls.size > 0 ? "tool_calls" : state.finish || "stop",
      },
    ],
    usage: normalizeUsage(state.usage, fallbackUsage),
  };
}
