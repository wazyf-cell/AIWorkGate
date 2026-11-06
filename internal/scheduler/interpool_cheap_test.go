package scheduler

import (
	"context"

	"aiworkgate/internal/model"
	"testing"
	"time"
)

func TestInterPoolCheapOrdersByPrice(t *testing.T) {
	now := testBase
	cheap := pool("pa", account("a1"))
	cheap.Models = map[string]model.ModelInfo{"m-cheap": {ID: "m-cheap"}}
	pricey := pool("pb", account("b1"))
	pricey.Models = map[string]model.ModelInfo{"m-rich": {ID: "m-rich"}}
	snap := snapOf(cheap, pricey)
	snap.InterPool = InterPoolCheap
	snap.Bindings["grp"] = Binding{Logical: "grp", Items: []Source{
		{PluginID: "pb", Model: "m-rich"},
		{PluginID: "pa", Model: "m-cheap"},
	}}
	snap.Prices = map[string]float64{"m-cheap": 1.5, "m-rich": 12}
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})

	if d, err := s.Pick(context.Background(), snap, Request{Model: "grp"}); err != nil || d.PluginID != "pa" {
		t.Fatalf("cheap pick = %+v err=%v, want pa", d, err)
	}

	// Unknown price yields to a priced source; both unknown keeps smart order.
	snap.Prices = map[string]float64{"m-rich": 12}
	if d, err := s.Pick(context.Background(), snap, Request{Model: "grp"}); err != nil || d.PluginID != "pb" {
		t.Fatalf("unknown price pick = %+v err=%v, want pb", d, err)
	}
}
