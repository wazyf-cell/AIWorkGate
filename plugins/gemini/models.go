// Package main — the model catalog and effort mapping for Gemini Cloud Code.
//
// The whole channel exposes exactly one upstream model, and the effort tier is
// carried as a suffix on the model id (gemini-3.8-flash-low, ...-medium,
// ...-high, ...-tiered). Everything below is verbatim from the reference's
// vendor/channel-pack/src/gemini.ts.
package main

import (
	"strings"
	"time"
)

const (
	// upstreamFlash is GEMINI_UPSTREAM_FLASH: the only model this channel serves.
	upstreamFlash = "gemini-3.8-flash"

	// Thinking budgets per tier. tiered is -1, which means "let the upstream
	// decide" and is sent by omitting thinkingBudget entirely.
	budgetLow    = 1000
	budgetMedium = 4000
	budgetHigh   = 10000
	budgetTiered = -1

	contextWindow = 1000000

	// defaultMaxOutputTokens is the definition's maxOutputTokens.
	defaultMaxOutputTokens = 64000
	defaultEffort          = "medium"

	// defaultProject is GEMINI_DEFAULT_PROJECT, used when loadCodeAssist returns
	// no cloudaicompanionProject.
	defaultProject = "aicode-consumers"

	// sessionLaneInfer is the lane the inference path uses in the session id.
	sessionLaneInfer = "infer"

	// imagePixelBudget is DEFAULT_IMAGE_PIXEL_BUDGET (640,000 px).
	imagePixelBudget = 640000

	// Inline limits declared by the reference implementation.
	maxImageBytesInline = 10 * 1024 * 1024
	maxRequestBodyBytes = 64 * 1024 * 1024

	idleTimeout     = 120 * time.Second
	creditsTimeout  = 30 * time.Second
	quotaCacheTTL   = 60 * time.Second
	requestTimeout  = 30 * time.Second
	expiryLeadTime  = 60 * time.Second
	authFlowTimeout = 360 * time.Second
)

// effortIDs is GEMINI_EFFORT_IDS, in the reference's order.
var effortIDs = []string{"low", "medium", "high", "tiered"}

// effortLabel is geminiEffortLabel.
func effortLabel(id string) string {
	switch id {
	case "low":
		return "低"
	case "medium":
		return "中"
	case "high":
		return "高"
	case "tiered":
		return "自适应"
	}
	return id
}

// effortToTier clamps an unknown effort to "medium", matching geminiEffortToTier.
func effortToTier(effort string) string {
	for _, id := range effortIDs {
		if effort == id {
			return effort
		}
	}
	return defaultEffort
}

// thinkingBudget is geminiThinkingBudget.
func thinkingBudget(tier string) int {
	switch tier {
	case "low":
		return budgetLow
	case "high":
		return budgetHigh
	case "tiered":
		return budgetTiered
	}
	return budgetMedium
}

// canonicalModelID strips a trailing -<effort> suffix, so a host that lists the
// tier variants still resolves to the one upstream model.
func canonicalModelID(modelID string) string {
	for _, tier := range effortIDs {
		if strings.HasSuffix(modelID, "-"+tier) {
			return strings.TrimSuffix(modelID, "-"+tier)
		}
	}
	return modelID
}

// modelSpecFor is geminiModelSpec. An unknown model is an invalid request: the
// reference answers 404 rather than silently substituting.
func modelSpecFor(modelID, effort string) (modelSpec, error) {
	if canonicalModelID(modelID) != upstreamFlash {
		return modelSpec{}, errUnknownModel
	}
	tier := effortToTier(effort)
	return modelSpec{
		Upstream:        upstreamFlash + "-" + tier,
		Tier:            tier,
		ThinkingBudget:  thinkingBudget(tier),
		IncludeThoughts: true,
	}, nil
}

// catalogModel is one entry of the manifest catalog.
type catalogModel struct {
	ID            string
	Name          string
	ContextWindow int
	MaxTokens     int
	SupportsImage bool
	Efforts       []string
	DefaultEffort string
}

// catalog is GEMINI_FALLBACK_MODELS, expanded so the host can address an effort
// tier directly as a model id (the reference accepts either form).
func catalog() []catalogModel {
	out := make([]catalogModel, 0, 1+len(effortIDs))
	base := catalogModel{
		ID:            upstreamFlash,
		Name:          "Gemini 3.8 Flash",
		ContextWindow: contextWindow,
		MaxTokens:     defaultMaxOutputTokens,
		SupportsImage: true,
		Efforts:       append([]string(nil), effortIDs...),
		DefaultEffort: defaultEffort,
	}
	out = append(out, base)
	// The reference lets a caller address "<model>-<tier>"; each such id resolves
	// to the same upstream model with that tier pinned.
	for _, tier := range effortIDs {
		out = append(out, catalogModel{
			ID:            upstreamFlash + "-" + tier,
			Name:          "Gemini 3.8 Flash (" + effortLabel(tier) + ")",
			ContextWindow: contextWindow,
			MaxTokens:     defaultMaxOutputTokens,
			SupportsImage: true,
			Efforts:       []string{tier},
			DefaultEffort: tier,
		})
	}
	return out
}

// effortFromModelID returns the tier a "-<tier>" model id pins, or "" when the id
// carries no suffix.
func effortFromModelID(modelID string) string {
	for _, tier := range effortIDs {
		if strings.HasSuffix(modelID, "-"+tier) {
			return tier
		}
	}
	return ""
}
