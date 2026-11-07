package gateway

import "testing"

// D-41 回归：CORS 只认显式白名单；通配与 "Origin: null" 一律不给读权。
func TestCorsForD41(t *testing.T) {
	g := &Gateway{}
	g.SetCorsOrigins([]string{"https://app.test", "http://localhost:3000/"})
	if allowed, echo := g.corsFor("https://APP.test/"); !allowed || echo != "https://APP.test/" {
		t.Fatalf("listed origin = %v/%q, want allowed with echoed origin", allowed, echo)
	}
	if allowed, _ := g.corsFor("https://evil.test"); allowed {
		t.Fatal("unlisted origin must not be allowed")
	}
	// D-41: "Origin: null"（沙箱 iframe / 跨源重定向）永不放行。
	for _, nullish := range []string{"null", "NULL", " null "} {
		if allowed, _ := g.corsFor(nullish); allowed {
			t.Fatalf("origin %q must not be allowed", nullish)
		}
	}
	// 空来源：无 CORS 语义（同源 /me 不受影响）。
	if allowed, _ := g.corsFor(""); allowed {
		t.Fatal("empty origin must not be allowed")
	}
	// 即使来源清单被手工塞进通配（绕过配置校验），也不得放行为通配。
	wildcard := &Gateway{}
	wildcard.SetCorsOrigins([]string{"*"})
	if allowed, echo := wildcard.corsFor("https://evil.test"); allowed {
		t.Fatalf("wildcard entry granted %q (echo %q), want nothing allowed", "https://evil.test", echo)
	}
}
