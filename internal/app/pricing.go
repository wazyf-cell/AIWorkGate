package app

// D-31③: look up the configured price of one model. Exact ids win; a
// case-insensitive match keeps hand-written configs usable.

import (
	"strings"

	"aiworkgate/internal/config"
	"aiworkgate/internal/gateway"
)

func (a *App) priceFor(model string) (gateway.Price, string, bool) {
	if a == nil {
		return gateway.Price{}, "", false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return gateway.Price{}, "", false
	}
	models := a.configSnapshot().Pricing.Models
	if len(models) == 0 {
		return gateway.Price{}, "", false
	}
	if price, ok := models[model]; ok {
		return toGatewayPrice(price), model, true
	}
	for id, price := range models {
		if strings.EqualFold(strings.TrimSpace(id), model) {
			return toGatewayPrice(price), id, true
		}
	}
	return gateway.Price{}, "", false
}

func toGatewayPrice(p config.Price) gateway.Price {
	return gateway.Price{
		Input: p.Input, Output: p.Output, CacheRead: p.CacheRead,
		CacheWrite: p.CacheWrite, Multiplier: p.Multiplier,
	}
}
