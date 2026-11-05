// SSE 回写：上游流 → 客户端。负责保序、不丢帧、错误分片、[DONE] 收尾。
//
// 客户端帧契约（本卡验收 ④）：
//   每帧 `data: {json}\n\n`；正常/错误都以 `data: [DONE]\n\n` 结尾；
//   错误帧是 `data: {"error":{"message":...}}\n\n` 后立刻终止。
// 响应头采用「惰性写出」：上游若在写第一帧前就给出错误，我们还能干净地回一个
// 非 2xx JSON 错误，而不是半个流。

import type { ServerResponse } from "node:http";
import { ChatError, errorFrameFor } from "./errors.ts";
import { encodeFrame, isDone, parseSSEStream } from "./sse.ts";
import type { OpenAIChatChoice, OpenAIChatCompletion } from "./types.ts";

const SSE_HEADERS: Record<string, string> = {
  "content-type": "text/event-stream; charset=utf-8",
  "cache-control": "no-cache, no-transform",
  connection: "keep-alive",
  "x-accel-buffering": "no",
};

/** 惰性 SSE 写出器：第一次写才发响应头。 */
export class SSEWriter {
  private res: ServerResponse;
  private started = false;
  private closed = false;
  /** 已写出的帧数（供「上游一帧没给就断了」的判定用） */
  frames = 0;

  constructor(res: ServerResponse) {
    this.res = res;
    res.on("close", () => {
      this.closed = true;
    });
  }

  get isClosed(): boolean {
    return this.closed || this.res.writableEnded;
  }

  /** 是否已发出响应头（决定还能不能改状态码）。 */
  get headersSent(): boolean {
    return this.started;
  }

  private ensureHeaders(): void {
    if (this.started) return;
    this.started = true;
    this.res.writeHead(200, SSE_HEADERS);
    this.res.flushHeaders?.();
  }

  /** 写一帧 `data: <payload>\n\n`。 */
  write(payload: string): void {
    if (this.isClosed) return;
    this.ensureHeaders();
    this.res.write(Buffer.from(encodeFrame(payload)));
    this.frames += 1;
  }

  /** 写 `data: [DONE]\n\n` 并结束响应。 */
  done(): void {
    if (this.isClosed) return;
    this.write("[DONE]");
    this.end();
  }

  end(): void {
    if (this.isClosed) return;
    this.ensureHeaders();
    this.res.end();
  }
}

export interface PipeContext {
  model: string;
  chunkId: string;
  created: number;
}

/** 把上游 chunk 补成合法 chat.completion.chunk。
 * id/object/created 缺才补；model 统一回显为客户端请求的原始串（与非流式一致）。 */
function normalizeChunk(
  payload: Record<string, unknown>,
  ctx: PipeContext,
): Record<string, unknown> {
  const out: Record<string, unknown> = { ...payload };
  if (typeof out.id !== "string" || !out.id) out.id = ctx.chunkId;
  if (typeof out.object !== "string" || !out.object) out.object = "chat.completion.chunk";
  if (!Number.isFinite(Number(out.created))) out.created = ctx.created;
  out.model = ctx.model;
  return out;
}

function errorBodyFrame(message: string): string {
  return JSON.stringify({ error: { message, type: "api_error", code: null } });
}

/**
 * 流式主循环：上游 SSE → 客户端 SSE。
 * 客户端断开时取消上游 body，避免空转读。
 */
export async function pipeUpstreamStream(
  upstream: Response,
  writer: SSEWriter,
  ctx: PipeContext,
): Promise<void> {
  const contentType = (upstream.headers.get("content-type") ?? "").toLowerCase();

  // 上游没按流返回（忽略 stream:true）：把 JSON 变成「一帧 + 结束帧 + [DONE]」
  if (!contentType.includes("text/event-stream")) {
    await pipeNonStreamUpstream(upstream, writer, ctx);
    return;
  }
  if (!upstream.body) {
    writer.write(errorBodyFrame("上游返回 event-stream 但没有响应体"));
    writer.done();
    return;
  }

  let finished = false;
  try {
    for await (const event of parseSSEStream(upstream.body)) {
      if (writer.isClosed) {
        await cancelQuietly(upstream);
        return;
      }
      const data = event.data;
      if (isDone(data)) {
        writer.done();
        finished = true;
        return;
      }
      if (data.trim() === "") continue;

      let parsed: unknown;
      try {
        parsed = JSON.parse(data);
      } catch {
        writer.write(data); // 非 JSON 帧原样透传，绝不丢帧
        continue;
      }
      const obj = (typeof parsed === "object" && parsed !== null ? parsed : {}) as Record<
        string,
        unknown
      >;
      if (obj.error) {
        writer.write(JSON.stringify({ error: obj.error }));
        writer.done();
        finished = true;
        return;
      }
      writer.write(JSON.stringify(normalizeChunk(obj, ctx)));
    }
  } catch (err) {
    if (!writer.isClosed) {
      writer.write(errorFrameFor(err));
      writer.done();
      finished = true;
    }
  }
  if (finished || writer.isClosed) return;
  // 上游没给 [DONE]（异常截断）：一帧都没给时补一帧可读错误，然后收尾
  if (writer.frames === 0) {
    writer.write(
      errorFrameFor(
        new ChatError({
          status: 502,
          message: "上游流在给出任何内容前就结束了（可能是上游限流或连接被中断）",
          code: "empty_upstream_stream",
        }),
      ),
    );
  }
  writer.done();
}

/** 上游返回 JSON（忽略 stream:true）：整包 → 一帧 delta + 结束帧 + [DONE]。 */
async function pipeNonStreamUpstream(
  upstream: Response,
  writer: SSEWriter,
  ctx: PipeContext,
): Promise<void> {
  const text = await upstream.text();
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    writer.write(
      errorFrameFor(
        new ChatError({
          status: 502,
          message: `上游返回了无法解析的响应（content-type=${
            upstream.headers.get("content-type") ?? "?"
          }）：${text.slice(0, 200)}`,
          code: "bad_upstream_body",
        }),
      ),
    );
    writer.done();
    return;
  }
  const obj = (typeof parsed === "object" && parsed !== null ? parsed : {}) as Record<
    string,
    unknown
  >;
  if (obj.error) {
    writer.write(JSON.stringify({ error: obj.error }));
    writer.done();
    return;
  }
  const completion = obj as unknown as OpenAIChatCompletion;
  const choice: OpenAIChatChoice =
    Array.isArray(completion.choices) && completion.choices.length > 0
      ? completion.choices[0]
      : {};
  const message = (choice.message ?? {}) as NonNullable<OpenAIChatChoice["message"]>;
  writer.write(
    JSON.stringify({
      id: completion.id || ctx.chunkId,
      object: "chat.completion.chunk",
      created: Number(completion.created) || ctx.created,
      model: completion.model || ctx.model,
      choices: [
        {
          index: 0,
          delta: {
            role: message.role ?? "assistant",
            content: message.content ?? "",
            ...(message.reasoning_content
              ? { reasoning_content: message.reasoning_content }
              : {}),
            ...(message.tool_calls ? { tool_calls: message.tool_calls } : {}),
          },
          finish_reason: choice.finish_reason ?? "stop",
        },
      ],
      ...(completion.usage ? { usage: completion.usage } : {}),
    }),
  );
  writer.done();
}

async function cancelQuietly(upstream: Response): Promise<void> {
  try {
    await upstream.body?.cancel();
  } catch {
    // 已断开则无所谓
  }
}
