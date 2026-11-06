# `cheap` 池间策略（P9/D-44）

价格引擎（D-31③）落地后，`cheap` 按**配置价格**选择候选来源池。

## 配置

1. `config.json` 的 `pricing.models` 填每 1M tokens 单价（见 `docs/p13/00-三栏与导出.md`）。
2. 管理页「调度」把**池间策略**切到「便宜优先」（`inter_pool: "cheap"`）。

## 排序规则

- 对同一请求解析出的候选池来源，按 **`input + output` 单价 × 倍率** 升序。
- 单价相同或未知时，回退到「智能调度」的排序（到期、倍率、剩余积分、池优先级）。
- 只有一个物理模型时没有可比项，效果等同原策略。
- 组权重（P11）若设置，仍优先按权重轮转胜出的成员排在队首。

## 验证

- `TestInterPoolCheapOrdersByPrice`：低价池先选；无价来源让位于有价来源。
- `TestSnapshotExposesPricingForCheapMode`：快照携带 `(input+output)×multiplier`。
- 管理端接受 `inter_pool=cheap`（`TestSchedulerAcceptsCheapInterPool`）。

## 限制

- 只比较**配置价**，不读取 models.dev 或上游账单。
- 未配置价格的模型不参与“便宜”判断（排在有价模型之后）。
- 与 `credit_first` 等账号内策略可叠加：先选池，再在池内按原策略选账号。
