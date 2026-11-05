// model 前缀路由：`trae-cn/doubao-seed-1.6` → providerId=trae-cn, modelId=doubao-seed-1.6。
// 规则：仅当「/」前的段恰好命中某个已知 provider id 时才拆前缀；
// 否则整串视为 modelId，交给唯一 provider（多 provider 且无前缀时判歧义）。

import { notFound } from "./errors.ts";
import type { ResolvedRoute } from "./types.ts";

export function resolveModelRoute(
  requested: string,
  providerIds: readonly string[],
  preferredProviderId?: string,
): ResolvedRoute {
  const trimmed = requested.trim();
  const firstSlash = trimmed.indexOf("/");
  if (firstSlash > 0) {
    const maybe = trimmed.slice(0, firstSlash);
    // 前缀必须精确命中 provider id；命中才拆（避免把 "meta-llama/Llama-3"
    // 这类 model 自身带斜杠的误拆）。
    if (providerIds.includes(maybe)) {
      return {
        providerId: maybe,
        modelId: trimmed.slice(firstSlash + 1) || maybe,
        requested: trimmed,
      };
    }
  }

  // 无前缀：唯一 provider 直接收；多 provider 时若给出了偏好（如 /v1/models
  // 端点的默认）用它，否则判歧义。
  const uniques = [...new Set(providerIds)];
  if (uniques.length === 1) {
    return { providerId: uniques[0], modelId: trimmed, requested: trimmed };
  }
  if (preferredProviderId && uniques.includes(preferredProviderId)) {
    return { providerId: preferredProviderId, modelId: trimmed, requested: trimmed };
  }
  throw notFound(
    `model「${trimmed}」没有携带 provider 前缀，而桥上有多个插件（${uniques.join(", ")}）。` +
      `请使用 ` +
      uniques.map((p) => `${p}/${trimmed}`).join(" 或 "),
    "ambiguous_model_route",
  );
}
