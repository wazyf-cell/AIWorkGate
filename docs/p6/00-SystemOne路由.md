# SystemOne / decision 结构化决策路由（P6，D-12/D-18）

宿主持有一个**通用决策上游**：配置里写 `base_url + protocol + model_id`，不写死厂商。首个（也是目前唯一实测目标）是 TokenRhythm 的 JEV。

## 1. config.json

在宿主配置（Windows `%APPDATA%\AIWorkGate\config.json`，Linux `~/.config/AIWorkGate/config.json`）加：

```jsonc
{
  "decision": {
    "upstreams": [
      {
        "id": "jev",
        "base_url": "https://tokenrhythm.studio/v1",
        "protocol": "systemone",     // 或 "decision"
        "model_id": "NeoHorse-Jev-4B"
        // "disabled": true        // 需要临时下线时加这一行
      }
    ]
  }
}
```

- `protocol` 决定转发路径：`systemone` → `{base_url}/systemone`，`decision` → `{base_url}/decision`。
- 只配一个上游时，`/v1/systemone` 与 `/v1/decision` 都落到它（别名同实现）。
- 配多个时按 `protocol` 精确选择。

## 2. API Key（不进 config.json）

优先级：环境变量 > 加密密钥库。

```bash
# 无头部署推荐：systemd EnvironmentFile（0600）
AIWORKGATE_DECISION_JEV_API_KEY=sk-...
```

变量名规则：`AIWORKGATE_DECISION_<ID>_API_KEY`，ID 转大写、非字母数字换 `_`（`my-provider.1` → `AIWORKGATE_DECISION_MY_PROVIDER_1_API_KEY`）。桌面版可用密钥库键 `decision.<id>.api_key`（当前没有专用 UI，后续补）。

## 3. 调用

```bash
# state 可以是字符串或结构化 JSON；questions 支持 choice|noul|score
curl -sS http://127.0.0.1:7865/v1/systemone \
  -H "Authorization: Bearer <客户端Key>" \
  -H "Content-Type: application/json" \
  -d '{"state":"选一个","questions":[{"type":"noul"}]}'
```

- `/v1/decision` 与 `/v1/systemone` 是同一实现：请求体一样，返回形状一样（`answers` 不包装成 chat 消息）。
- 响应原样透传：上游的 `code/message/traceId` 不改写。
- **不重试**：超时/429/5xx 直接返回给调用方（D-18⑤）。
- 体积限制：纯文本 1 MiB、带图（顶层 `image` 非空）8 MiB；超限本地 413，不发上游。
- `usage.input_tokens/output_tokens` 若上报会进入请求日志；未上报记未知而不是 0。

## 4. 互斥保护

`/v1/models` 会把决策模型标记为 `structured: true`。把同一个模型名发给 `/v1/chat/completions` 会在本地返回：

```
400 structured_model
该模型是结构化决策模型，请用 /v1/systemone 或 /v1/decision
```

不会落到上游。

## 5. 验收状态

- 本地可验证（已做，`internal/gateway/decision_test.go`）：原样透传、别名/协议选择、chat 互斥、`structured:true`、体积上限、错误透传且只调用一次。
- **真实 JEV key 的最小请求未跑**：需要你提供 key；`noul` 应返回 0~1 概率，`choice`/`score` 各需一例。

## 6. Gemini 入站（含工具调用）

`POST /v1beta/models/{model}:generateContent` 与 `:streamGenerateContent?alt=sse` 会把 Gemini 请求翻译成 chat 请求，走与 OpenAI 入站相同的调度/插件路径，再把结果翻译回 Gemini 形状。

- 文本、`inlineData` 图片、`systemInstruction`、`generationConfig`（temperature/topP/maxOutputTokens）双向映射。
- **工具**：`tools[].functionDeclarations` → OpenAI `tools[].function`；模型轮的 `functionCall` → assistant `tool_calls`（自动生成稳定 `call_<name>_<n>` id）；用户轮的 `functionResponse` → OpenAI `role:"tool"` 消息，按函数名与最近的调用配对。
- **回译**：非流式 `message.tool_calls` 与流式 `delta.tool_calls`（分片 arguments 会拼装）→ Gemini `parts[].functionCall`，`finishReason` 为 `STOP`；同时有文本时先输出 text part。
- 验证：`TestGeminiRequestTranslatesToolsAndCalls`、`TestGeminiNonStreamToolCallBackTranslated`、`TestGeminiStreamToolCallBackTranslated`。
