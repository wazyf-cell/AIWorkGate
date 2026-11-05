// GET /v1/models（models.ts）
//
// 聚合全部 provider 的静态模型表为 OpenAI 兼容列表。
// 宿主 internal/plugins/client.go:213 走 model.ParseModelList 宽容解析：
// `{data:[…]}` / `{models:[…]}` / 裸数组均可，字段按 id/name/context_length/
// tools/vision/vendor 等取用，多余字段忽略。
//
// id 前缀策略：`<providerId>/<localId>`（对齐 D-20「一个插件可导出多个模型前缀」，
// 并保证跨 provider 唯一）。local_id / provider 字段保留未加前缀的信息。
// 若宿主希望无前缀，改 models.ts 的 modelId() 一处即可。

import type { BridgeContext, ModelInfo, ModelsResponse, PluginModelEntry } from "./types.ts";

export function modelId(providerId: string, localId: string): string {
  return `${providerId}/${localId}`;
}

function toModelInfo(providerId: string, localId: string, entry: PluginModelEntry): ModelInfo {
  const rawName = typeof entry.name === "string" ? entry.name.trim() : "";
  const context = typeof entry.limit?.context === "number" ? entry.limit.context : 0;
  const vision =
    Array.isArray(entry.modalities?.input) && entry.modalities.input.includes("image");
  const tools = entry.tool_call !== false;
  return {
    id: modelId(providerId, localId),
    name: rawName || localId,
    context_length: context,
    tools,
    vision,
    vendor: providerId,
    local_id: localId,
    provider: providerId,
  };
}

export function buildModels(ctx: BridgeContext): ModelsResponse {
  const data: ModelInfo[] = [];
  const seen = new Set<string>();
  const providerIds = Object.keys(ctx.registry.providers).sort();
  for (const providerId of providerIds) {
    const rec = ctx.registry.providers[providerId];
    const localIds = Object.keys(rec.models).sort();
    for (const localId of localIds) {
      const id = modelId(providerId, localId);
      if (seen.has(id)) continue;
      seen.add(id);
      data.push(toModelInfo(providerId, localId, rec.models[localId]));
    }
  }
  return { object: "list", data };
}
