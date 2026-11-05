// SSE 工具：帧编码（写回客户端）+ 上游流解析（字节 → 事件）。
//
// 客户端侧帧格式必须严格是 `data: <payload>\n\n`；结尾 `data: [DONE]\n\n`。
// 上游侧解析对换行/空行做容错：按行累积 `data:` 字段，空行即一帧。

const enc = new TextEncoder();

export function encodeFrame(payload: string): Uint8Array {
  return enc.encode(`data: ${payload}\n\n`);
}

export const DONE_FRAME = "data: [DONE]\n\n";
export const DONE_BYTES = enc.encode(DONE_FRAME);

/** 解析出的上游 SSE 事件。event 可为空；data 是去掉 "data:" 前缀的原文。 */
export interface SSEEvent {
  event: string;
  data: string;
}

/**
 * 把上游 body 逐帧读成 SSEEvent。
 * 空行分隔帧；帧内多个 `data:` 行按 SSE 规范以 \n 合并。
 * 流结束时若没有 [DONE]，由调用方负责补发。
 */
export async function* parseSSEStream(
  body: ReadableStream<Uint8Array>,
): AsyncGenerator<SSEEvent> {
  const reader = body.getReader();
  const decoder = new TextDecoder("utf-8");
  const events: SSEEvent[] = [];
  let buffer = "";
  let curEvent = "";
  let dataLines: string[] = [];

  const takeField = (line: string): boolean => {
    if (line.startsWith(":")) return true; // 注释行
    if (line.startsWith("event:")) {
      curEvent = line.slice("event:".length).trim();
      return true;
    }
    if (line.startsWith("data:")) {
      const raw = line.slice("data:".length);
      // 规范：data 后的单个前导空格去掉；"data:xxx" 也合法
      dataLines.push(raw.startsWith(" ") ? raw.slice(1) : raw);
      return true;
    }
    return false; // 未知字段忽略
  };

  const endFrame = (): void => {
    if (dataLines.length === 0 && curEvent === "") return;
    events.push({ event: curEvent, data: dataLines.join("\n") });
    curEvent = "";
    dataLines = [];
  };

  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let idx: number;
      while ((idx = buffer.indexOf("\n")) !== -1) {
        const line = buffer.slice(0, idx);
        buffer = buffer.slice(idx + 1);
        if (line === "") endFrame();
        else takeField(line);
      }
      while (events.length > 0) yield events.shift() as SSEEvent;
    }
    // 收尾：没有换行结尾的最后一帧 + 未闭合的帧
    if (buffer.length > 0) takeField(buffer);
    endFrame();
    while (events.length > 0) yield events.shift() as SSEEvent;
  } finally {
    try {
      await reader.cancel();
    } catch {
      // 取消失败无妨：调用方可能已断开
    }
  }
}

/** 单帧快捷判读：data 是否为 [DONE]。 */
export function isDone(data: string): boolean {
  return data.trim() === "[DONE]";
}
