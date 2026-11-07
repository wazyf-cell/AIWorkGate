package scheduler

// D-29 账号权重与 D-25② 用量半衰期的选取因子。
//
// D-29：Account.Weight 字段与 UI/快照链路早已全通，但选取比较器不消费它。
// 这里把权重作为选取因子并入 compositeScores（weighted3/p2c 的加权随机的
// 权重）。语义约束（计划原文）：**确定性优先级 Priority 仍先于 Weight**——
// Priority 不同必须严格按 Priority；Priority 相同才按 Weight 加权/排序。
// 因此权重因子只在"组内权重不全相等"时启用（见 weightFactors）：全部账号
// 权重相等（含 Weight=0 未配置）时因子全 1，分数与旧实现逐字节相同，
// "Weight=0 维持现状"由测试锁定。
//
// 确定性比较器（fixed / credit_first）在同优先级内把 Weight 作为紧随
// Priority 的排序键（降序），见 selectAccountLocked。
//
// D-25②：magpie routing.go 的 usageHalfLife 让"刚被重度使用"的账号短暂降
// 权。这里的窗口计数只记内存（Acquire 时间戳），按半衰期衰减后以
// usageFactor 并入 compositeScores：账号最近使用量越大分数越低，随时间自然
// 恢复。计划原文默认 1 小时，本仓库按任务定稿取 10 分钟。

import "math"

// usageHalfLifeSeconds is the D-25② 半衰期：一个使用事件在 10 分钟后权重减半。
const usageHalfLifeSeconds = 600

// usageForgetWindow bounds the retained usage window: after 8 half-lives a
// hit's weight is below 0.4%, so older hits are dropped to bound memory.
const usageForgetWindow = 8 * usageHalfLifeSeconds

// effectiveWeight normalizes an account weight: 0/负值是"未配置"，取中性 1
// （快照桥接层已经用 DefaultAccountWeight 兜底，这里是调度器侧的兜底）。
func effectiveWeight(weight float64) float64 {
	if weight <= 0 {
		return 1
	}
	return weight
}

// weightFactors returns one composite-score multiplier per account folding the
// D-29 weight in. All-1 output means "weights do not participate" and keeps
// the legacy scores bit-identical.
//
// 语义约束（计划原文）：Priority 不同必须严格按 Priority，Priority 相同才按
// Weight 加权。因此当且仅当组内权重不全相等时因子才生效：
//
//   - Priority 高于组内最低的账号：因子 0 —— 加权随机（weightedIndex 会跳过
//     非正分数）与 p2c 都不会再选它们，权重无论多大都不能翻转优先级；
//   - Priority 相同的账号：因子 = weight / maxWeight，比例仍为权重比（缩放
//     对 weightedIndex/p2c 无影响，归一只让 explain 分数不超过基础分）。
//
// 确定性比较器（fixed/credit_first）在同优先级内用 Weight 降序作为紧随
// Priority 的排序键，见 selectAccountLocked。
func weightFactors(accounts []Account) []float64 {
	factors := make([]float64, len(accounts))
	if len(accounts) == 0 {
		return factors
	}
	first := effectiveWeight(accounts[0].Weight)
	uniform := true
	minPriority := 0
	maxWeight := 0.0
	for i, acc := range accounts {
		if i == 0 || acc.Priority < minPriority {
			minPriority = acc.Priority
		}
		weight := effectiveWeight(acc.Weight)
		if weight > maxWeight {
			maxWeight = weight
		}
		if weight != first {
			uniform = false
		}
	}
	if uniform {
		// 全等权重（含全部 Weight=0）维持现状：Priority/其余因子不受影响。
		for i := range factors {
			factors[i] = 1
		}
		return factors
	}
	for i, acc := range accounts {
		if acc.Priority > minPriority {
			// 严格按 Priority：权重不得把低优先级账号顶上去。
			continue
		}
		factors[i] = effectiveWeight(acc.Weight) / maxWeight
	}
	return factors
}

// usagePrune drops usage hits older than the forget window, reusing the slice.
func usagePrune(hits []int64, now int64) []int64 {
	cutoff := now - usageForgetWindow
	keep := hits[:0]
	for _, at := range hits {
		if at > cutoff {
			keep = append(keep, at)
		}
	}
	return keep
}

// recordUsageHitLocked appends one usage timestamp per upstream attempt
// (Acquire), pruning the forget window. Caller holds s.mu.
func (s *Scheduler) recordUsageHitLocked(key AccountKey, now int64) {
	s.usageHits[key] = append(usagePrune(s.usageHits[key], now), now)
}

// decayedUsageLocked sums every in-window hit's half-life weight:
// Σ 0.5^(age/halfLife). Caller holds s.mu.
func (s *Scheduler) decayedUsageLocked(key AccountKey, now int64) float64 {
	hits := usagePrune(s.usageHits[key], now)
	s.usageHits[key] = hits
	var total float64
	for _, at := range hits {
		age := float64(now-at) / usageHalfLifeSeconds
		if age < 0 {
			age = 0
		}
		total += math.Pow(0.5, age)
	}
	return total
}

// usageFactor turns the decayed usage into a score multiplier: one fresh
// request halves the account's score (1/(1+1)), five concurrent ones press it
// to ~0.17, and the factor recovers toward 1 as the hits decay — "刚被重度
// 使用"的账号短暂降权而不被拉黑。
func usageFactor(usage float64) float64 {
	if usage <= 0 {
		return 1
	}
	return 1 / (1 + usage)
}
