package app

import (
	"testing"

	"aiworkgate/internal/config"
)

func TestPriceForLooksUpCaseInsensitively(t *testing.T) {
	app, _ := openAppWithStore(t)
	app.cfg = config.Default()
	app.cfg.Pricing = config.PricingConfig{Currency: "USD", Models: map[string]config.Price{
		"GPT-4o": {Input: 2, Output: 8, Multiplier: 0.5},
	}}
	price, key, ok := app.priceFor("gpt-4o")
	if !ok || key != "GPT-4o" || price.Input != 2 || price.Multiplier != 0.5 {
		t.Fatalf("price = %+v key=%q ok=%v", price, key, ok)
	}
	if _, _, ok := app.priceFor("unknown"); ok {
		t.Fatalf("unknown model must not have a price")
	}
}
