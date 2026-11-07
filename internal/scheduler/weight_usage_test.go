package scheduler

// D-29 账号权重与 D-25② 用量半衰期的测试锁定。全部使用注入时钟；加权随机的
// 随机源用确定性的 [0,1) 序列，让"分配比例"断言稳定而不靠真实随机。

import (
	"testing"
)

// deterministicRand returns a deterministic uniform [0,1) sequence, so
// weighted-distribution assertions stay stable across runs. The 919 step (coprime
// with 1000) spreads early values across the whole unit interval instead of
// crawling away from 0.
func deterministicRand() func() float64 {
	seq := 0
	return func() float64 {
		seq++
		return (float64((seq*919)%1000) + 0.5) / 1000
	}
}

// D-29: 同 Priority 不同 Weight 的分配比例接近权重比（3:1）。
func TestAccountWeightSplitsEqualPriorityTraffic(t *testing.T) {
	clk := &clock{at: testBase}
	heavy := account("heavy")
	heavy.Weight = 3
	light := account("light")
	light.Weight = 1

	p := pool("p", heavy, light)
	p.Strategy = StrategyWeighted3
	s := New(Options{Now: clk.now, Rand: deterministicRand()})
	snap := snapOf(p)

	counts := map[string]int{}
	const picks = 600
	for i := 0; i < picks; i++ {
		counts[pick(t, s, snap, "p/m1").AccountID]++
	}
	ratio := float64(counts["heavy"]) / float64(float64(counts["heavy"])+float64(counts["light"]))
	if ratio < 0.70 || ratio > 0.80 {
		t.Fatalf("weighted3 split = %v, heavy share %.2f not near 3:1 (0.75)", counts, ratio)
	}
}

// D-29: Weight=0（未配置）维持现状——等权因子全 1，fixed 仍按 ID、weighted3
// 仍均匀分布。
func TestAccountWeightZeroKeepsLegacyBehavior(t *testing.T) {
	clk := &clock{at: testBase}
	a, b := account("a"), account("b")
	a.Weight, b.Weight = 0, 0

	fixed := pool("pf", a, b)
	fixed.Strategy = StrategyFixed
	s := New(Options{Now: clk.now})
	if got := pick(t, s, snapOf(fixed), "pf/m1").AccountID; got != "a" {
		t.Fatalf("fixed with unset weights = %s, want a (ID order preserved)", got)
	}

	uniform := pool("pu", a, b)
	uniform.Strategy = StrategyWeighted3
	s2 := New(Options{Now: clk.now, Rand: deterministicRand()})
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		counts[pick(t, s2, snapOf(uniform), "pu/m1").AccountID]++
	}
	if counts["a"] < 80 || counts["a"] > 120 {
		t.Fatalf("weighted3 with unset weights = %v, want a uniform split", counts)
	}
}

// D-29: 确定性策略在同 Priority 内按 Weight 降序、不同 Priority 严格按
// Priority（权重不得翻转优先级）。
func TestAccountWeightOrdersDeterministicStrategies(t *testing.T) {
	clk := &clock{at: testBase}
	a := account("a")
	a.Priority, a.Weight = 100, 1
	b := account("b")
	b.Priority, b.Weight = 100, 9
	c := account("c")
	c.Priority, c.Weight = 2, 1

	// Priority strictly first: c (priority 2) wins every pick no matter how
	// small its weight is compared with b's 9. fixed is a stateless comparator,
	// so the pick repeats until c leaves the candidate set.
	fixed := pool("p", a, b, c)
	fixed.Strategy = StrategyFixed
	s := New(Options{Now: clk.now})
	for i := 0; i < 3; i++ {
		if got := pick(t, s, snapOf(fixed), "p/m1").AccountID; got != "c" {
			t.Fatalf("fixed pick %d = %s, want c (priority strictly precedes weight)", i+1, got)
		}
	}

	// Once the higher-priority account is out of the candidate set, weight
	// decides inside the priority-100 tie: b (weight 9) before a (weight 1).
	cooling := account("c")
	cooling.Priority, cooling.Weight = 2, 1
	cooling.CooldownUntil = clk.at + 60
	without := pool("p2", a, b, cooling)
	if got := pick(t, s, snapOf(without), "p2/m1").AccountID; got != "b" {
		t.Fatalf("fixed without c = %s, want b (weight desc within priority)", got)
	}

	// credit_first keeps its quota ordering but also lets weight speak inside
	// one priority: b (weight 9) is tried before a (weight 1) when both are
	// otherwise identical.
	credit := pool("pc", a, b)
	credit.Strategy = StrategyCreditFirst
	s2 := New(Options{Now: clk.now})
	if got := pick(t, s2, snapOf(credit), "pc/m1").AccountID; got != "b" {
		t.Fatalf("credit_first = %s, want b (weight desc within priority)", got)
	}
}

// D-29: 权重不同且优先级不同时，加权随机也严格按 Priority——低优先级账号
// 即使权重 1000 倍也不会被选中。
func TestAccountWeightCannotOverridePriority(t *testing.T) {
	clk := &clock{at: testBase}
	first := account("first")
	first.Priority, first.Weight = 10, 1
	second := account("second")
	second.Priority, second.Weight = 20, 1000

	p := pool("p", first, second)
	p.Strategy = StrategyWeighted3
	s := New(Options{Now: clk.now, Rand: deterministicRand()})
	snap := snapOf(p)
	for i := 0; i < 100; i++ {
		if got := pick(t, s, snap, "p/m1").AccountID; got != "first" {
			t.Fatalf("weighted3 pick %d = %s, want first (priority strictly precedes weight)", i+1, got)
		}
	}

	// 等权时维持旧行为：优先级不参与分数，权重不参与分层（回归保护）。
	equal := []Account{first, second}
	equal[1].Weight = 1
	p2 := pool("p2", equal...)
	p2.Strategy = StrategyWeighted3
	s2 := New(Options{Now: clk.now, Rand: deterministicRand()})
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		counts[pick(t, s2, snapOf(p2), "p2/m1").AccountID]++
	}
	if counts["first"] == 0 || counts["second"] == 0 {
		t.Fatalf("weighted3 with equal weights = %v, want both accounts reachable", counts)
	}
}

// D-25②: 刚被使用的账号分数低于闲置账号；衰减随时间恢复（注入时钟）。
func TestUsageHalfLifeDemotesRecentlyUsedAccount(t *testing.T) {
	clk := &clock{at: testBase}
	a, b := account("a"), account("b")
	p := pool("p", a, b)
	s := New(Options{Now: clk.now})

	// 两个账号都闲置：分数相等。
	idle := s.compositeScores(p, p.Accounts, clk.at)
	if idle[0] != idle[1] {
		t.Fatalf("idle scores = %v/%v, want equal", idle[0], idle[1])
	}

	// a 刚被使用：分数必须低于 b（两次相同请求间隔内，重用账号降权）。
	s.Acquire(accKey("p", "a"))
	afterUse := s.compositeScores(p, p.Accounts, clk.at)
	if afterUse[0] >= afterUse[1] {
		t.Fatalf("used account score %v not below idle %v", afterUse[0], afterUse[1])
	}
	if want := idle[1] / 2; afterUse[0] > want {
		t.Fatalf("one fresh request should halve the score: %v > %v", afterUse[0], want)
	}

	// 两个半衰期后恢复大半：0.25 的残余使用量 → 因子 0.8。
	clk.advance(2 * usageHalfLifeSeconds)
	recovered := s.compositeScores(p, p.Accounts, clk.at)
	if recovered[0] <= recovered[1]*0.75 {
		t.Fatalf("after 2 half-lives score = %v, want close to idle %v", recovered[0], recovered[1])
	}

	// 遗忘窗口滑出后：严格回到闲置分数。
	clk.advance(usageForgetWindow - 2*usageHalfLifeSeconds + 1)
	restored := s.compositeScores(p, p.Accounts, clk.at)
	if restored[0] != restored[1] {
		t.Fatalf("after the forget window scores = %v/%v, want equal", restored[0], restored[1])
	}
}

// D-25②: 重度使用（多次 Acquire）比单次降得更狠，且加权随机会避开刚用满的
// 账号。
func TestUsageHalfLifePressureRisesWithUsage(t *testing.T) {
	clk := &clock{at: testBase}
	a, b := account("a"), account("b")
	p := pool("p", a, b)
	s := New(Options{Now: clk.now})

	s.Acquire(accKey("p", "a"))
	one := s.compositeScores(p, p.Accounts, clk.at)
	s.Acquire(accKey("p", "a"))
	s.Acquire(accKey("p", "a"))
	s.Acquire(accKey("p", "a"))
	three := s.compositeScores(p, p.Accounts, clk.at)
	if three[0] >= one[0] {
		t.Fatalf("heavier usage did not demote further: %v >= %v", three[0], one[0])
	}

	// weighted3 端到端：a 刚被打满，确定性随机序列下 b 应拿到绝大多数请求。
	p.Strategy = StrategyWeighted3
	s2 := New(Options{Now: clk.now, Rand: deterministicRand()})
	snap := snapOf(p)
	s2.Acquire(accKey("p", "a"))
	s2.Acquire(accKey("p", "a"))
	s2.Acquire(accKey("p", "a"))
	s2.Acquire(accKey("p", "a"))
	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		counts[pick(t, s2, snap, "p/m1").AccountID]++
	}
	if counts["b"] < 80 {
		t.Fatalf("weighted3 with a freshly saturated = %v, want b to dominate", counts)
	}
}

// 确认 usage 窗口不会把时间戳剪进未来（防御性）：负 age 按 0 处理。
func TestUsageHitClampsFutureTimestamps(t *testing.T) {
	clk := &clock{at: testBase}
	s := New(Options{Now: clk.now})
	s.Acquire(accKey("p", "a"))
	clk.advance(-10) // 回拨注入时钟：不应 panic，也不应放大使用量
	scores := s.compositeScores(pool("p", account("a")), []Account{account("a")}, clk.at)
	if len(scores) != 1 || scores[0] <= 0 {
		t.Fatalf("score after clock rewind = %v", scores)
	}
}
