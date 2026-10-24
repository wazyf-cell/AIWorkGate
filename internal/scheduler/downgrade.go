package scheduler

import (
	"strings"

	"aiworkgate/internal/model"
)

// BodyMeta is the request metadata the downgrade rules need. The gateway
// supplies it before forwarding (§6.5).
type BodyMeta struct {
	MaxTokens       int
	PromptChars     int
	EstimatedTokens int
}

// Downgrade reasons returned by ApplyDowngrades.
const (
	DowngradeBackground = "background"
	DowngradeLongCtx    = "long_context"
)

// ApplyDowngrades implements §6.5 background-task and long-context downgrades.
// It returns the possibly rewritten decision (Model points at the cheaper
// flash tier, LogicalModel and account stay) plus a non-empty reason when a
// downgrade happened. The caller is responsible for logging "downgraded" and
// returning X-Aiworkgate-Downgraded.
func ApplyDowngrades(snap *Snapshot, decision Decision, meta BodyMeta) (Decision, string) {
	if snap == nil {
		return decision, ""
	}
	pool := snap.Pools[decision.PluginID]
	if pool == nil {
		return decision, ""
	}
	background := pool.BackgroundDowngrade &&
		meta.MaxTokens > 0 && meta.MaxTokens <= 128 &&
		meta.PromptChars >= 0 && meta.PromptChars <= 512
	estimated := meta.EstimatedTokens
	if estimated <= 0 {
		estimated = meta.PromptChars / 4
	}
	longContext := pool.LongContextDowngrade && estimated >= 100000
	if !background && !longContext {
		return decision, ""
	}
	alternative, ok := flashAlternative(pool, decision.Model)
	if !ok {
		return decision, ""
	}
	downgraded := decision
	downgraded.Model = alternative
	reason := DowngradeLongCtx
	if background {
		reason = DowngradeBackground
	}
	downgraded.Reasons = append(downgraded.Reasons,
		"downgraded:"+reason+" from "+decision.Model+" to "+alternative)
	return downgraded, reason
}

// DefaultReasoning resolves the reasoning effort a pool injects when the
// request does not carry reasoning_effort. The second result is false when the
// pool has no configured default or the request already set one.
func DefaultReasoning(snap *Snapshot, decision Decision, requestHasReasoning bool) (string, bool) {
	if requestHasReasoning || snap == nil {
		return "", false
	}
	pool := snap.Pools[decision.PluginID]
	if pool == nil {
		return "", false
	}
	reasoning := strings.TrimSpace(pool.DefaultReasoning)
	if reasoning == "" {
		return "", false
	}
	return reasoning, true
}

// flashAlternative finds the cheapest flash-tier model of the pool that costs
// less than current. Explicit "flash" names win; a model whose multiplier is at
// most half of the current one is accepted as a fallback tier.
func flashAlternative(pool *Pool, current string) (string, bool) {
	if pool == nil || len(pool.Models) == 0 {
		return "", false
	}
	currentMultiplier := ModelMultiplier(pool.Models, current)
	best := ""
	bestMultiplier := 0.0
	for id, info := range pool.Models {
		if id == current || strings.TrimSpace(id) == "" {
			continue
		}
		multiplier := ModelMultiplier(pool.Models, id)
		if multiplier >= currentMultiplier {
			continue
		}
		name := strings.ToLower(id + " " + info.Name)
		flash := strings.Contains(name, "flash")
		if !flash && !(multiplier > 0 && multiplier*2 <= currentMultiplier) {
			continue
		}
		if best == "" || multiplier < bestMultiplier ||
			(multiplier == bestMultiplier && id < best) {
			best, bestMultiplier = id, multiplier
		}
	}
	return best, best != ""
}

// ModelIsFree reports whether the catalog marks a model as free.
func ModelIsFree(models map[string]model.ModelInfo, id string) bool {
	info, ok := models[id]
	return ok && info.Free
}
