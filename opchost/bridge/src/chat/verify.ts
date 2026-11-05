// chat 泳道自检脚本（测试基建，随 dev-server 一起跑）。
//
//   node opchost/bridge/src/chat/verify.ts --base http://127.0.0.1:18080 --suite mock
//   node opchost/bridge/src/chat/verify.ts --base http://127.0.0.1:18081 --suite trae
//
// 断言面：非流式标准 OpenAI JSON、SSE 帧字节边界与 [DONE]、错误帧终止、
// 无凭据 401「需登录」、上游 4xx/5xx/抛错的映射、入参校验。
// 任何一条失败 → 退出码 1，并打印断言原文。

interface Case {
  name: string;
  run: () => Promise<void>;
}

const argv = process.argv.slice(2);
const argOf = (flag: string, fallback: string): string => {
  const i = argv.indexOf(flag);
  return i >= 0 && argv[i + 1] ? (argv[i + 1] as string) : fallback;
};
const BASE = argOf("--base", "http://127.0.0.1:18080").replace(/\/+$/, "");
const SUITE = argOf("--suite", "mock");

let passed = 0;
const failures: string[] = [];

function check(cond: boolean, message: string): void {
  if (!cond) throw new Error(`断言失败：${message}`);
}

function checkEq(actual: unknown, expected: unknown, what: string): void {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  check(a === e, `${what} 期望 ${e}，实际 ${a}`);
}

async function post(body: unknown, raw?: string): Promise<{ status: number; ct: string; text: string }> {
  const res = await fetch(`${BASE}/v1/chat/completions`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: raw ?? JSON.stringify(body),
  });
  return { status: res.status, ct: res.headers.get("content-type") ?? "", text: await res.text() };
}

/** 严格切帧：必须每个非空帧都是 `data: <payload>`，且以空行分隔。 */
function splitFrames(text: string): string[] {
  check(!text.includes("\r\n"), "不应出现 CRLF（SSE 行结束必须是 \\n）");
  const parts = text.split("\n\n");
  // 末尾通常留一个空串（最后帧也以 \n\n 结尾）
  if (parts.length > 0 && parts[parts.length - 1] === "") parts.pop();
  return parts;
}

function assertFrameShape(frames: string[], label: string): void {
  check(frames.length > 0, `${label}: 没有任何帧`);
  for (const [i, frame] of frames.entries()) {
    check(frame.startsWith("data: "), `${label}: 第 ${i} 帧不是以 "data: " 开头 → ${frame.slice(0, 40)}`);
    check(!frame.includes("\n"), `${label}: 第 ${i} 帧内部出现换行（帧边界被破坏）`);
    const payload = frame.slice("data: ".length);
    check(payload.length > 0, `${label}: 第 ${i} 帧 payload 为空`);
    if (payload !== "[DONE]") JSON.parse(payload); // 必须可解析
  }
  checkEq(frames[frames.length - 1], "data: [DONE]", `${label}: 最后一帧`);
  const doneCount = frames.filter((f) => f === "data: [DONE]").length;
  checkEq(doneCount, 1, `${label}: [DONE] 出现次数`);
}

function payloads(frames: string[]): Record<string, unknown>[] {
  return frames
    .filter((f) => f !== "data: [DONE]")
    .map((f) => JSON.parse(f.slice("data: ".length)) as Record<string, unknown>);
}

// ---- mock suite ------------------------------------------------------------

const mockCases: Case[] = [
  {
    name: "非流式：标准 OpenAI JSON（id/object/created/model/choices/usage）",
    run: async () => {
      const r = await post({
        model: "mock/mock-chat",
        messages: [{ role: "user", content: "hi" }],
        temperature: 0.3,
        max_tokens: 64,
      });
      checkEq(r.status, 200, "HTTP 状态");
      check(r.ct.includes("application/json"), `content-type 应为 JSON，实际 ${r.ct}`);
      const body = JSON.parse(r.text) as Record<string, unknown>;
      check(typeof body.id === "string" && (body.id as string).length > 0, "id 缺失");
      checkEq(body.object, "chat.completion", "object");
      check(typeof body.created === "number", "created 应为数字");
      checkEq(body.model, "mock/mock-chat", "model 回显为请求串");
      check(Array.isArray(body.choices) && body.choices.length === 1, "choices 应只有一条");
      const choice = (body.choices as Record<string, unknown>[])[0] as Record<string, unknown>;
      const message = choice.message as Record<string, unknown>;
      checkEq(message.role, "assistant", "choices[0].message.role");
      // 上游收到的是去前缀的 model，说明前缀路由/改写生效
      checkEq(message.content, "mock reply for model=mock-chat", "上游实际 model 名");
      checkEq(choice.finish_reason, "stop", "finish_reason");
      const usage = body.usage as Record<string, unknown>;
      check(typeof usage.prompt_tokens === "number", "usage.prompt_tokens");
      check(typeof usage.completion_tokens === "number", "usage.completion_tokens");
      check(typeof usage.total_tokens === "number", "usage.total_tokens");
    },
  },
  {
    name: "流式：连续 data: 帧 + 结尾 [DONE] + 保序不丢帧",
    run: async () => {
      const r = await post({
        model: "mock/mock-chat",
        messages: [{ role: "user", content: "hi" }],
        stream: true,
      });
      checkEq(r.status, 200, "HTTP 状态");
      check(r.ct.includes("text/event-stream"), `content-type 应为 SSE，实际 ${r.ct}`);
      const frames = splitFrames(r.text);
      assertFrameShape(frames, "流式");
      check(frames.length >= 5, `帧数应 >=5（role+3正文+finish+usage+[DONE]），实际 ${frames.length}`);
      const objs = payloads(frames);
      for (const o of objs) checkEq(o.object, "chat.completion.chunk", "分片 object");
      const text = objs
        .map((o) => {
          const ch = (o.choices as Record<string, unknown>[] | undefined)?.[0];
          const delta = ch?.delta as Record<string, unknown> | undefined;
          return typeof delta?.content === "string" ? (delta.content as string) : "";
        })
        .join("");
      checkEq(text, "Hello from mock(mock-chat)", "分片正文按序拼接");
      const usageChunk = objs.find((o) => o.usage);
      check(!!usageChunk, "缺少 usage 分片（choices 为空的收尾帧）");
      const models = new Set(objs.map((o) => o.model));
      checkEq([...models], ["mock/mock-chat"], "分片 model 回显");
    },
  },
  {
    name: "流式：上游忽略 stream 回 JSON → 桥转成单帧 + [DONE]",
    run: async () => {
      const r = await post({
        model: "mock/mock-json-only",
        messages: [{ role: "user", content: "hi" }],
        stream: true,
      });
      checkEq(r.status, 200, "HTTP 状态");
      const frames = splitFrames(r.text);
      assertFrameShape(frames, "json-only");
      const objs = payloads(frames);
      checkEq(objs.length, 1, "应为 1 个内容帧");
      const ch = (objs[0].choices as Record<string, unknown>[])[0];
      const delta = ch.delta as Record<string, unknown>;
      checkEq(delta.content, "mock reply for model=mock-json-only", "转换后的 delta.content");
    },
  },
  {
    name: "流式：上游流内错误 → 错误分片后立即 [DONE] 终止",
    run: async () => {
      const r = await post({
        model: "mock/mock-sse-error",
        messages: [{ role: "user", content: "hi" }],
        stream: true,
      });
      checkEq(r.status, 200, "HTTP 状态（流已开始，错误在流内表达）");
      const frames = splitFrames(r.text);
      assertFrameShape(frames, "sse-error");
      const objs = payloads(frames);
      const errIdx = objs.findIndex((o) => o.error);
      check(errIdx >= 0, "缺少 error 分片");
      checkEq(errIdx, objs.length - 1, "error 分片必须是最后一帧内容（之后只有 [DONE]）");
      const err = objs[errIdx].error as Record<string, unknown>;
      checkEq(err.message, "mock: upstream stream failed mid-way", "error.message 原文保留");
    },
  },
  {
    name: "无凭据 → 401「需登录」语义错误（不是 500）",
    run: async () => {
      const r = await post({
        model: "mock-nocred/mock-chat",
        messages: [{ role: "user", content: "hi" }],
      });
      checkEq(r.status, 401, "HTTP 状态必须是 401");
      const body = JSON.parse(r.text) as { error: Record<string, unknown> };
      checkEq(body.error.code, "needs_sign_in", "error.code");
      checkEq(body.error.type, "authentication_error", "error.type");
      check((body.error.message as string).includes("尚未登录"), "文案必须可读且指明需登录");
      check((body.error.message as string).includes("登录"), "文案必须含登录指引");
    },
  },
  {
    name: "流式 + 无凭据 → 同样 401 JSON（不是半截流）",
    run: async () => {
      const r = await post({
        model: "mock-nocred/mock-chat",
        messages: [{ role: "user", content: "hi" }],
        stream: true,
      });
      checkEq(r.status, 401, "HTTP 状态");
      check(r.ct.includes("application/json"), `必须回 JSON 而不是 SSE，实际 ${r.ct}`);
      check(!r.text.includes("data:"), "不得出现 SSE 帧");
    },
  },
  {
    name: "非流式：上游无视 stream=false 仍回流 → 桥聚合回非流式 JSON",
    run: async () => {
      const r = await post({
        model: "mock/mock-sse-always",
        messages: [{ role: "user", content: "hi" }],
        stream: false,
      });
      checkEq(r.status, 200, "HTTP 状态");
      check(r.ct.includes("application/json"), `必须回 JSON，实际 ${r.ct}`);
      const body = JSON.parse(r.text) as Record<string, unknown>;
      checkEq(body.object, "chat.completion", "object");
      const choice = (body.choices as Record<string, unknown>[])[0] as Record<string, unknown>;
      const message = choice.message as Record<string, unknown>;
      checkEq(message.content, "Aggregated SSE(mock-sse-always)", "聚合后的完整正文（保序）");
      checkEq(choice.finish_reason, "stop", "finish_reason");
      const usage = body.usage as Record<string, unknown>;
      check(typeof usage.prompt_tokens === "number", "usage.prompt_tokens");
    },
  },
  {
    name: "上游 400 → 原状态透出 + 可读 message",
    run: async () => {
      const r = await post({ model: "mock/mock-http-400", messages: [{ role: "user", content: "hi" }] });
      checkEq(r.status, 400, "HTTP 状态应保留 400");
      const body = JSON.parse(r.text) as { error: Record<string, unknown> };
      check((body.error.message as string).length > 0, "error.message 不得为空");
    },
  },
  {
    name: "上游 500 → 502 + 上游文案不丢",
    run: async () => {
      const r = await post({ model: "mock/mock-http-500", messages: [{ role: "user", content: "hi" }] });
      checkEq(r.status, 502, "HTTP 状态");
      const body = JSON.parse(r.text) as { error: Record<string, unknown> };
      checkEq(body.error.message, "mock: upstream internal error", "上游原文");
    },
  },
  {
    name: "上游抛错 → 502 可读错误（不吞）",
    run: async () => {
      const r = await post({ model: "mock/mock-boom", messages: [{ role: "user", content: "hi" }] });
      checkEq(r.status, 502, "HTTP 状态");
      const body = JSON.parse(r.text) as { error: Record<string, unknown> };
      check((body.error.message as string).includes("mock upstream exploded"), "必须带出原始错误");
    },
  },
  {
    name: "入参校验：缺 model / 空 messages / 坏 JSON / GET 方法",
    run: async () => {
      const a = await post({ messages: [{ role: "user", content: "hi" }] });
      checkEq(a.status, 400, "缺 model");
      const b = await post({ model: "mock/mock-chat", messages: [] });
      checkEq(b.status, 400, "空 messages");
      const c = await post(null, "{not json");
      checkEq(c.status, 400, "坏 JSON");
      const d = await fetch(`${BASE}/v1/chat/completions`, { method: "GET" });
      checkEq(d.status, 405, "GET 方法");
    },
  },
];

// ---- trae suite（真实包、无凭据） ------------------------------------------

const traeCases: Case[] = [
  {
    name: "真实 trae 包 + 无凭据（trae-cn）→ 401 需登录",
    run: async () => {
      const r = await post({
        model: "trae-cn/doubao-seed-1.6",
        messages: [{ role: "user", content: "hi" }],
      });
      checkEq(r.status, 401, "HTTP 状态必须不是 500");
      const body = JSON.parse(r.text) as { error: Record<string, unknown> };
      checkEq(body.error.code, "needs_sign_in", "error.code");
      check((body.error.message as string).includes("trae-cn"), "文案需点明插件");
    },
  },
  {
    name: "真实 trae 包 + 无凭据（trae-global，流式）→ 401 JSON 不是半截流",
    run: async () => {
      const r = await post({
        model: "trae-global/gemini-2.5-pro",
        messages: [{ role: "user", content: "hi" }],
        stream: true,
      });
      checkEq(r.status, 401, "HTTP 状态");
      check(!r.text.includes("data:"), "不得出现 SSE 帧");
    },
  },
  {
    name: "无前缀 model + 多 provider → 歧义错误（可读）",
    run: async () => {
      const r = await post({ model: "doubao-seed-1.6", messages: [{ role: "user", content: "hi" }] });
      checkEq(r.status, 404, "HTTP 状态");
      const body = JSON.parse(r.text) as { error: Record<string, unknown> };
      checkEq(body.error.code, "ambiguous_model_route", "error.code");
      check((body.error.message as string).includes("trae-cn/"), "文案需给出可用前缀");
    },
  },
];

async function main(): Promise<void> {
  const cases = SUITE === "trae" ? traeCases : mockCases;
  console.log(`verify: base=${BASE} suite=${SUITE} cases=${cases.length}`);
  for (const c of cases) {
    try {
      await c.run();
      passed += 1;
      console.log(`PASS  ${c.name}`);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      failures.push(`${c.name}: ${msg}`);
      console.log(`FAIL  ${c.name}`);
      console.log(`      ${msg}`);
    }
  }
  console.log(`\n结果: ${passed}/${cases.length} 通过`);
  if (failures.length > 0) {
    console.log("失败清单:");
    for (const f of failures) console.log(` - ${f}`);
    process.exit(1);
  }
}

main().catch((err: unknown) => {
  console.error(`verify 崩溃: ${String(err)}`);
  process.exit(1);
});
