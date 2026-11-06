// 最小 mock 插件：形状与 @magpie-community/opencode-trae-auth 一致，供 chat 泳道自测。
//
// 只读对照（真包 index.mjs）：
//   - 插件工厂：`async ({ client }) => hooks`（L3031）
//   - hooks.config(cfg) 写 cfg.provider[id] = { name, npm, api, models }（L3581-3591）
//   - hooks.auth.loader(getAuth) → { baseURL, apiKey, headers, fetch }（L3641）
//   - 无凭据时 loader 返回 {}（L3642-3643）→ 桥把它翻译成「需登录」
//   - 聊天请求形态：POST {baseURL}/chat/completions，body 为 OpenAI chat 体，
//     stream 决定回 SSE 还是整包 JSON；错误 = { error: { message, type, code } }
//
// 本文件不联网：loader 的 fetch 自己合成响应，按 body.model 触发不同路径，
// 用来覆盖非流式 / 流式 / 上游错误 / 流内错误 / 忽略 stream 五种情形。

const MODELS = {
  "mock-chat": { name: "Mock Chat", limit: { context: 60000, output: 4096 } },
  "mock-boom": { name: "Mock Boom", limit: { context: 60000, output: 4096 } },
  "mock-http-400": { name: "Mock HTTP 400", limit: { context: 60000, output: 4096 } },
  "mock-http-500": { name: "Mock HTTP 500", limit: { context: 60000, output: 4096 } },
  "mock-sse-error": { name: "Mock SSE Error", limit: { context: 60000, output: 4096 } },
  "mock-json-only": { name: "Mock JSON Only", limit: { context: 60000, output: 4096 } },
  "mock-sse-always": { name: "Mock SSE Always", limit: { context: 60000, output: 4096 } },
  "mock-401": { name: "Mock Expired", limit: { context: 60000, output: 4096 } },
};

const enc = new TextEncoder();

const jsonError = (status, message, signIn) => {
  const headers = { "content-type": "application/json" };
  if (signIn) headers["X-Magpie-Sign-In"] = signIn;
  return new Response(
    JSON.stringify({ error: { message, type: status === 401 ? "authentication_error" : "api_error", code: null } }),
    { status, headers },
  );
};

const usage = { prompt_tokens: 11, completion_tokens: 7, total_tokens: 18 };

/** 流式：3 帧正文 + 结束帧 + usage 帧 + [DONE]，帧间有微小延迟以便验证保序/不丢帧。 */
function sseResponse(chunks, model) {
  const id = "chatcmpl-mock-" + Math.random().toString(16).slice(2, 10);
  const created = Math.floor(Date.now() / 1000);
  const stream = new ReadableStream({
    async start(ctl) {
      const frame = (obj) => enc.encode(`data: ${JSON.stringify(obj)}\n\n`);
      ctl.enqueue(frame({
        id, object: "chat.completion.chunk", created, model,
        choices: [{ index: 0, delta: { role: "assistant", content: "" }, finish_reason: null }],
      }));
      for (const text of chunks) {
        await new Promise((r) => setTimeout(r, 15));
        ctl.enqueue(frame({
          id, object: "chat.completion.chunk", created, model,
          choices: [{ index: 0, delta: { content: text }, finish_reason: null }],
        }));
      }
      ctl.enqueue(frame({
        id, object: "chat.completion.chunk", created, model,
        choices: [{ index: 0, delta: {}, finish_reason: "stop" }],
      }));
      ctl.enqueue(frame({
        id, object: "chat.completion.chunk", created, model, choices: [], usage,
      }));
      ctl.enqueue(enc.encode("data: [DONE]\n\n"));
      ctl.close();
    },
  });
  return new Response(stream, {
    status: 200,
    headers: { "content-type": "text/event-stream" },
  });
}

/** 流里吐一帧正文后吐错误帧（验证桥把错误分片原样保留并终止）。 */
function sseErrorResponse(model) {
  const id = "chatcmpl-mock-err";
  const created = Math.floor(Date.now() / 1000);
  const stream = new ReadableStream({
    async start(ctl) {
      const frame = (obj) => enc.encode(`data: ${JSON.stringify(obj)}\n\n`);
      ctl.enqueue(frame({
        id, object: "chat.completion.chunk", created, model,
        choices: [{ index: 0, delta: { content: "partial " }, finish_reason: null }],
      }));
      await new Promise((r) => setTimeout(r, 10));
      ctl.enqueue(frame({ error: { message: "mock: upstream stream failed mid-way", type: "api_error", code: "mock_mid_stream" } }));
      ctl.close();
    },
  });
  return new Response(stream, { status: 200, headers: { "content-type": "text/event-stream" } });
}

function completionResponse(model, body) {
  const asked = String(body?.model ?? model);
  return new Response(
    JSON.stringify({
      id: "chatcmpl-mock-json",
      object: "chat.completion",
      created: Math.floor(Date.now() / 1000),
      model,
      choices: [
        {
          index: 0,
          message: { role: "assistant", content: `mock reply for model=${asked}` },
          finish_reason: "stop",
        },
      ],
      usage,
    }),
    { status: 200, headers: { "content-type": "application/json" } },
  );
}

const makeMock = (siteId) => async ({ client }) => {
  // client 只在续期时被用到（save），这里保留同形接口，避免插件侧分支缺失
  const save = async (auth) => {
    try {
      await client?.auth?.set?.({ path: { id: siteId }, body: auth });
    } catch {
      // mock 不关心
    }
  };

  return {
    config: async (config) => {
      config.provider ??= {};
      config.provider[siteId] = {
        name: `${siteId} (mock)`,
        npm: "@ai-sdk/openai-compatible",
        api: "https://mock.invalid/v1",
        models: MODELS,
      };
    },
    provider: {
      id: siteId,
      async models(provider) {
        return provider.models;
      },
    },
    auth: {
      provider: siteId,
      methods: [{ type: "oauth", label: `${siteId} mock account` }],
      loader: async (getAuth) => {
        const a = await getAuth();
        // 与真包一致：没有凭据就返回空对象，让桥去翻译成「需登录」
        if (!a) return {};
        return {
          baseURL: "https://mock.invalid/v1",
          apiKey: "mock-key",
          headers: { "x-mock-account": String(a.accountId ?? "mock") },
          async fetch(input, init = {}) {
            const url = typeof input === "string" ? input : input?.url ?? String(input);
            if (!url.endsWith("/chat/completions")) {
              return jsonError(404, `mock: only /chat/completions is served (got ${url})`);
            }
            const raw = init.body;
            const body = typeof raw === "string" ? JSON.parse(raw) : JSON.parse(await new Response(raw).text());
            const model = String(body.model ?? "");
            const stream = body.stream === true;

            if (model === "mock-boom") throw new Error("mock upstream exploded (simulated network failure)");
            if (model === "mock-http-400") return jsonError(400, `mock: model ${model} rejected by upstream`);
            if (model === "mock-http-500") return jsonError(500, "mock: upstream internal error");
            if (model === "mock-401") return jsonError(401, "mock: sign-in expired", "expired");
            if (model === "mock-sse-error") return sseErrorResponse(model);
            if (model === "mock-json-only") return completionResponse(model, body);
            if (model === "mock-sse-always") return sseResponse(["Aggregated", " SSE", `(${model})`], model);

            if (stream) {
              return sseResponse(
                ["Hello", " from ", `mock(${model})`],
                model,
              );
            }
            return completionResponse(model, body);
          },
        };
      },
    },
  };
};

export const MockAuthPlugin = makeMock("mock");
export const MockNoCredPlugin = makeMock("mock-nocred");

// ---- workbuddy 形状的 mock -------------------------------------------------
//
// 只读对照真包 @magpie-community/opencode-workbuddy-auth/index.mjs：
//   L3753-3762 loader 有凭据时返回 `baseURL: ""`（注释 "no baseURL: a model's
//   own api.url says where it is served"），模型地址：登录后的动态列表带
//   per-model api.url（modelOf）；无凭据的静态表（configModels）不带，chat
//   走 provider 级 api（<endpoint>/v2）。自定义 fetch 原样请求传入的 URL。
//   无凭据时返回 {}（无 fetch）→ 桥判「需登录」。
//
// 桥侧对应行为（chat/upstream.ts + deps.getModelApiUrl）：
//   mock-wb/wb-chat    → per-model api.url + /chat/completions（fetch 回显 URL）
//   mock-wb/wb-noapi   → 模型无 api.url → provider 级 api 兜底
//   mock-wb2/wb-noapi  → provider 级也没有 → 501 no_upstream_url

const WB_MODELS = {
  "wb-chat": {
    name: "WB Chat",
    limit: { context: 60000, output: 4096 },
    api: { id: "wb-chat", url: "https://wb-mock.invalid/v2" },
  },
  // 没有 api.url：mock-wb 走 provider 级 api 兜底；mock-wb2 报 501
  "wb-noapi": { name: "WB No API", limit: { context: 60000, output: 4096 } },
};

const makeWBMock = (siteId, opts = {}) => async ({ client }) => {
  const save = async (auth) => {
    try {
      await client?.auth?.set?.({ path: { id: siteId }, body: auth });
    } catch {
      // mock 不关心
    }
  };
  return {
    config: async (config) => {
      config.provider ??= {};
      config.provider[siteId] = {
        name: `${siteId} (wb mock)`,
        npm: "@ai-sdk/openai-compatible",
        ...(opts.providerApi ? { api: opts.providerApi } : {}),
        models: WB_MODELS,
      };
    },
    provider: {
      id: siteId,
      async models(provider) {
        return provider.models;
      },
    },
    auth: {
      provider: siteId,
      methods: [{ type: "oauth", label: `${siteId} wb mock account` }],
      loader: async (getAuth) => {
        const a = await getAuth();
        // 与真包一致：没有凭据就返回空对象（无 fetch）→ 桥判「需登录」
        if (!a) return {};
        return {
          // 真包原文：no baseURL — a model's own api.url says where it is served
          baseURL: "",
          apiKey: "",
          async fetch(input, init = {}) {
            const url = typeof input === "string" ? input : input?.url ?? String(input);
            const raw = init.body;
            const body = typeof raw === "string" ? JSON.parse(raw) : JSON.parse(await new Response(raw).text());
            const asked = String(body.model ?? "");
            const stream = body.stream === true;
            // 真包实测（2026-11-07）：workbuddy 上游只收流式请求，非流式回
            // 400 {"code":11101,"msg":"Non-stream chat request is currently
            // not supported"}。mock 同形状，供验证桥的「上游强制流式 + 聚合」。
            if (!stream) {
              return new Response(
                JSON.stringify({ code: 11101, msg: "Non-stream chat request is currently not supported" }),
                { status: 400, headers: { "content-type": "application/json" } },
              );
            }
            // 把实际收到的 URL 回显进正文，verify 据此断言桥拼的地址
            return sseResponse([`wb url=${url}`, ` model=${asked}`], asked);
          },
        };
      },
    },
  };
};

export const MockWBAuthPlugin = makeWBMock("mock-wb", { providerApi: "https://wb-mock.invalid/v2" });
export const MockWBNoApiPlugin = makeWBMock("mock-wb2");
