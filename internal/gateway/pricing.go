package gateway

// D-31③ pricing: the host supplies a configured price per model (per 1M
// tokens). The gateway turns the recorded tokens into cost_usd at log time;
// no price means "unknown" and the column stays NULL.

import "strings"

// Price is the configured per-million-token rate plus a multiplier.
type Price struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
	Multiplier float64
}

// cost computes one request's cost. cfg-cached prompt tokens are billed at the
// cache-read rate and excluded from the full input rate; cache writes are not
// captured yet (D-31 remainder).
func (p Price) cost(promptTokens, cachedTokens, completionTokens int64) float64 {
	multiplier := p.Multiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	cached := float64(cachedTokens)
	if cached < 0 {
		cached = 0
	}
	input := float64(promptTokens) - cached
	if input < 0 {
		input = 0
	}
	output := float64(completionTokens)
	if output < 0 {
		output = 0
	}
	return (input*p.Input + cached*p.CacheRead + output*p.Output) / 1_000_000 * multiplier
}

// priceForModel tries the served name first, then the upstream/logical/routed
// names, and finally a case-insensitive match.
func (g *Gateway) priceForModel(names ...string) (Price, string, bool) {
	if g.priceFor == nil {
		return Price{}, "", false
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if price, key, ok := g.priceFor(name); ok {
			return price, key, true
		}
	}
	return Price{}, "", false
}
