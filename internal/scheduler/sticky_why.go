package scheduler

// D-43 StickyWhy 七态（plans/2026-11-05-magpie接入与公网部署计划.md）：
// 调度结果必须能回答"为什么 sticky / 为什么换号"。七个状态就是本实现里真实
// 可区分的全部 sticky 分支，与计划建议的状态集一一对应：
//
//	no_session      无会话亲和可用（请求没有 ConversationID / ClientKey，或该
//	                scope 的亲和已被 TTL=0 关闭）。
//	first_pick      有会话键但还没有 pin：本次选择建立第一个 pin。
//	session_pinned  pin 命中且可用：同会话 Turn 递增仍选中同一账号（计划
//	                「Turn 递增不换号」）。
//	window_expired  pin 存在但已过窗口（Within/TTL 到期，或 scope 亲和被关
//	                闭）：丢弃旧 pin 重新选择。
//	account_down    pin 的账号当前不可用：冷却/额度/出口/插件 status/并发让
//	                位/本次重试 Exclude——非 strict 时丢弃 pin 换号。
//	strict_hold     strict=true：窗口过期或账号临时不可用仍维持亲和，只有
//	                账号永久不可用（禁用/无凭证/非成员/已移出池）才换号。
//	pool_changed    pin 的插件池不在本轮候选里（上游关闭/熔断/整池无可用账
//	                号）：亲和不跨池，丢弃 pin 换号。
//
// 粒度约定：StickyWhy 只给粗分类，具体细节（哪条冷却、让位原因）仍走
// Candidate.Reason 与 Decision.Reasons 的自由文本。
const (
	StickyNoSession     = "no_session"
	StickyFirstPick     = "first_pick"
	StickySessionPinned = "session_pinned"
	StickyWindowExpired = "window_expired"
	StickyAccountDown   = "account_down"
	StickyStrictHold    = "strict_hold"
	StickyPoolChanged   = "pool_changed"
)

// stickyWhyRank orders the states by specificity so a request with both a
// conversation and a client key keeps the most informative verdict: the
// conversation scope is evaluated first and a generic verdict from the pool
// scope never overwrites a more specific one (and vice versa).
func stickyWhyRank(why string) int {
	switch why {
	case StickySessionPinned, StickyStrictHold:
		return 3
	case StickyWindowExpired, StickyAccountDown, StickyPoolChanged:
		return 2
	case StickyFirstPick:
		return 1
	default: // StickyNoSession / unknown
		return 0
	}
}

// stickyWhyMoreSpecific reports whether candidate should replace current.
func stickyWhyMoreSpecific(current, candidate string) bool {
	return stickyWhyRank(candidate) > stickyWhyRank(current)
}
