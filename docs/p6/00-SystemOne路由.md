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
# 实测形状（2026-11-08，TokenRhythm 真实 JEV）：state 是对象 {"message":...}，
# questions 是对象（键=题id），题目文本字段是 instructions。
curl -sS http://127.0.0.1:7865/v1/systemone \
  -H "Authorization: Bearer <客户端Key>" \
  -H "Content-Type: application/json" \
  -d '{"state":{"message":"选一个"},"questions":{"q1":{"type":"noul","instructions":"对这条陈述打分"}}}'
```

实测 200 响应形状（TokenRhythm `NeoHorse-Jev-4B`）：

```json
{
  "model": "NeoHorse-Jev-4B",
  "answers": {"q1": {"type": "noul", "noul": 0.972}},
  "usage": {"input_tokens": 18, "output_tokens": 34},
  "extensions": {"…"}
}
```

- `/v1/decision` 与 `/v1/systemone` 是同一实现：请求体一样，返回形状一样（`answers` 不包装成 chat 消息）。
- 响应原样透传：上游的 `code/message/traceId` 不改写。
- **不重试**：超时/429/5xx 直接返回给调用方（D-18⑤）。
- 体积限制：纯文本 1 MiB、带图（顶层 `image` 非空）8 MiB；超限本地 413，不发上游。题数上限（纯文本 1..16、带图恰好 1 题）对 questions 的**对象**与**数组**两种形状都生效。
- `usage.input_tokens/output_tokens` 若上报会进入请求日志；未上报记未知而不是 0。

### 3.1 实测协议与 F-12 设计的差异（2026-11-08 真实 JEV 实测 + 官方文档）

- **`questions` 必须是对象**（键=题id）：数组形状实测 422 `JEV_INVALID_REQUEST`。网关的 intent/decision 本地校验兼容两种形状，但转发给 JEV 的 P11 intent payload 已改为对象。
- **题目文本字段是 `instructions`**：F-12 草案写的 `question` 字段实测不存在。
- **`answers` 响应是对象**（键=题id）：网关的意图分类解析兼容对象与数组两种拼写（`parseIntentLabel`）。
- **choice 选项字段已实测确认（官方文档 + 真 key 双 200）**：`criteria` = 选项名→描述的**对象**（1~255 项，值必须非空字符串，null 被拒）。响应含 `choice`（选中的选项名）、`probabilities`（选项→概率）、`confidence`。网关 intent payload 已带 criteria（值先用 label 名，intent 配置将来扩展描述）。
- **score 分档字段已实测确认**：`criteria` = 等级描述**数组**（2~10 档）；响应含 `score`（**从 0 编号**的等级期望，如 3.585）、`legend`（`{"0":"日常",...}` 编号→描述）、`probabilities`、`confidence`。
- **noul**：`criteria` 可省略（实测 200），也可给 false/true 两描述。
- **官方口径**：推荐端点 `/v1/decision`（`/v1/systemone` 同请求体）；`state` 建议字符串（也可结构化）；纯文本 1~16 题 ≤1MiB、图片顶层 `image` 单题 ≤8MiB；上游**编码后**输入 token 超限同样 422 不静默截断；choice/score 的概率**不保证校准**，`confidence` 仅参考。
- **`model_id` 实测为 `NeoHorse-Jev-4B`**：不在 `/models` 列表，是决策模型专属名。

## 4. 互斥保护

`/v1/models` 会把决策模型标记为 `structured: true`。把同一个模型名发给 `/v1/chat/completions` 会在本地返回：

```
400 structured_model
该模型是结构化决策模型，请用 /v1/systemone 或 /v1/decision
```

不会落到上游。

## 5. 验收状态

- 本地可验证（已做，`internal/gateway/decision_test.go`）：原样透传、别名/协议选择、chat 互斥、`structured:true`、体积上限、错误透传且只调用一次、questions 对象/数组双形状的题数上限与带图单题保护（task-70）。
- **真实 JEV key 直连已实测（2026-11-08，见 §3.1）**：`noul` 返回 0~1 概率（实测 0.972）；`choice` 的选项字段实测未确认（`options`/`criteria` 全 422），需 JEV 协议文档后补一例；P11 intent payload 已按实测形状修正。

## 6. Gemini 入站（含工具调用）

`POST /v1beta/models/{model}:generateContent` 与 `:streamGenerateContent?alt=sse` 会把 Gemini 请求翻译成 chat 请求，走与 OpenAI 入站相同的调度/插件路径，再把结果翻译回 Gemini 形状。

- 文本、`inlineData` 图片、`systemInstruction`、`generationConfig`（temperature/topP/maxOutputTokens）双向映射。
- **工具**：`tools[].functionDeclarations` → OpenAI `tools[].function`；模型轮的 `functionCall` → assistant `tool_calls`（自动生成稳定 `call_<name>_<n>` id）；用户轮的 `functionResponse` → OpenAI `role:"tool"` 消息，按函数名与最近的调用配对。
- **回译**：非流式 `message.tool_calls` 与流式 `delta.tool_calls`（分片 arguments 会拼装）→ Gemini `parts[].functionCall`，`finishReason` 为 `STOP`；同时有文本时先输出 text part。
- 验证：`TestGeminiRequestTranslatesToolsAndCalls`、`TestGeminiNonStreamToolCallBackTranslated`、`TestGeminiStreamToolCallBackTranslated`。
