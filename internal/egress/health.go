package egress

// Short-window egress health: is the proxy an account is assigned to actually
// passing traffic right now? The evidence is deliberately recent-only (a tunnel
// row or a manual exit-IP check inside the window), so a proxy that was fixed
// stops counting as failed without any "clear" action, and a dead one is not
// blamed forever. The scheduler uses this as a soft filter and /healthz reports
// it; neither may block on the store, so lookups are cached for a few seconds.

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

const (
	// EvidenceWindow bounds how old a tunnel row may be to describe "now".
	EvidenceWindow = 10 * time.Minute
	// ManualCheckWindow bounds a proxies-page check the same way. It is wider
	// because checks are manual and the exit IP rarely changes.
	ManualCheckWindow = 30 * time.Minute
	// CacheTTL keeps a scheduling or health poll from re-reading the store on
	// every account of every request.
	CacheTTL = 10 * time.Second
)

const (
	HealthOK      = "ok"
	HealthFailed  = "failed"
	HealthUnknown = "unknown"
	HealthDirect  = "direct"
)

// Evidence is the cached verdict for one account's egress.
type Evidence struct {
	// Mode is the assignment mode: "proxy", "direct", "blocked" or "" (follow
	// the plugin default). Only "proxy" is evidenced below.
	Mode       string
	Status     string
	ProxyID    string
	ProxyLabel string
	// At is when the evidence was observed (unix seconds), 0 when unknown.
	At    int64
	Error string
}

// Dead reports whether the assigned proxy is known to be failing.
func (e Evidence) Dead() bool { return e.Status == HealthFailed }

// HealthSource is the store read side. *store.Store satisfies it.
type HealthSource interface {
	ListAccountEgress(pluginID string) ([]model.AccountEgress, error)
	GetProxy(id string) (model.Proxy, string, error)
	LatestProxyLog(pluginID, accountID string, since int64) (*model.ProxyLog, error)
}

type healthKey struct {
	pluginID  string
	accountID string
}

type healthEntry struct {
	evidence Evidence
	at       time.Time
}

// Health caches egress evidence per (plugin, account).
type Health struct {
	src HealthSource
	now func() time.Time
	ttl time.Duration

	mu    sync.Mutex
	cache map[healthKey]healthEntry
}

// NewHealth wraps a store. A nil source yields unknown evidence everywhere.
func NewHealth(src HealthSource) *Health {
	return &Health{src: src, now: time.Now, ttl: CacheTTL, cache: make(map[healthKey]healthEntry)}
}

// SetNow injects the clock for tests.
func (h *Health) SetNow(now func() time.Time) {
	if h == nil || now == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = now
}

// Lookup returns the cached evidence, reading the store on a miss. It never
// panics and never returns an error: an unreadable store is unknown, which is
// the fail-open side of the filter.
func (h *Health) Lookup(pluginID, accountID string) Evidence {
	if h == nil || h.src == nil {
		return Evidence{Status: HealthUnknown}
	}
	pluginID = strings.TrimSpace(pluginID)
	accountID = strings.TrimSpace(accountID)
	if pluginID == "" || accountID == "" {
		return Evidence{Status: HealthUnknown}
	}
	key := healthKey{pluginID: pluginID, accountID: accountID}
	now := h.now()

	h.mu.Lock()
	entry, ok := h.cache[key]
	h.mu.Unlock()
	if ok && now.Sub(entry.at) < h.ttl {
		return entry.evidence
	}
	ev := h.read(pluginID, accountID, now)
	h.mu.Lock()
	h.cache[key] = healthEntry{evidence: ev, at: now}
	h.mu.Unlock()
	return ev
}

// Invalidate drops one pair (a new tunnel row just landed).
func (h *Health) Invalidate(pluginID, accountID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.cache, healthKey{pluginID: strings.TrimSpace(pluginID), accountID: strings.TrimSpace(accountID)})
}

// InvalidateAll drops every verdict (proxy rows or assignments changed).
func (h *Health) InvalidateAll() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cache = make(map[healthKey]healthEntry)
}

// read builds fresh evidence. Rules, in order:
//
//  1. no assignment / direct / blocked -> that mode, no fault;
//  2. assigned proxy missing -> failed (the assignment cannot resolve);
//  3. newest tunnel row / manual check inside the window wins, whichever is
//     newer: failed -> failed, ok -> ok, denied -> unknown (policy, not a tunnel
//     fault);
//  4. nothing recent -> unknown.
func (h *Health) read(pluginID, accountID string, now time.Time) Evidence {
	assignments, err := h.src.ListAccountEgress(pluginID)
	if err != nil {
		return Evidence{Status: HealthUnknown}
	}
	mode, proxyID := "", ""
	for _, a := range assignments {
		if strings.TrimSpace(a.AccountID) != accountID {
			continue
		}
		mode = strings.ToLower(strings.TrimSpace(a.Mode))
		if mode == "" {
			if strings.TrimSpace(a.ProxyID) != "" {
				mode = model.EgressProxy
			} else {
				mode = model.EgressDirect
			}
		}
		proxyID = strings.TrimSpace(a.ProxyID)
		break
	}
	if mode != model.EgressProxy || proxyID == "" || IsDirectProxyID(proxyID) {
		if mode == "" {
			return Evidence{Status: HealthDirect}
		}
		return Evidence{Mode: mode, Status: HealthDirect}
	}

	ev := Evidence{Mode: mode, ProxyID: proxyID}
	proxy, _, err := h.src.GetProxy(proxyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			ev.Status = HealthFailed
			ev.Error = "代理记录不存在或已删除"
		} else {
			ev.Status = HealthUnknown
		}
		return ev
	}
	ev.ProxyLabel = firstNonEmpty(proxy.Name, proxy.ID)
	if !proxy.Enabled {
		ev.Status = HealthFailed
		ev.Error = "代理已停用"
		return ev
	}

	// The newest of the two evidence streams decides; an older stream cannot
	// overrule a newer one.
	var (
		rowAt, checkAt int64
		rowStatus      = ""
		rowError       string
	)
	if row, err := h.src.LatestProxyLog(pluginID, accountID, now.Add(-EvidenceWindow).Unix()); err == nil && row != nil {
		rowAt = row.At
		rowError = sanitizeEvidenceError(row.Error)
		switch row.Result {
		case model.ProxyResultFailed:
			rowStatus = HealthFailed
		case model.ProxyResultOK:
			rowStatus = HealthOK
		default:
			rowStatus = HealthUnknown
		}
		if row.ProxyLabel != "" {
			ev.ProxyLabel = row.ProxyLabel
		}
	}
	if proxy.LastCheckAt > 0 && now.Sub(time.Unix(proxy.LastCheckAt, 0)) <= ManualCheckWindow {
		checkAt = proxy.LastCheckAt
	}

	switch {
	case rowStatus != "" && rowAt >= checkAt:
		ev.Status, ev.At, ev.Error = rowStatus, rowAt, rowError
		if ev.Status == HealthOK {
			ev.Error = ""
		}
	case checkAt > 0:
		ev.At = checkAt
		if strings.TrimSpace(proxy.LastError) != "" {
			ev.Status = HealthFailed
			ev.Error = sanitizeEvidenceError(proxy.LastError)
		} else {
			ev.Status = HealthOK
		}
	default:
		ev.Status = HealthUnknown
	}
	return ev
}

// sanitizeEvidenceError flattens one line, strips proxy userinfo and caps the
// length so a tunnel error cannot carry credentials into the admin API.
func sanitizeEvidenceError(raw string) string {
	s := strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
	if s == "" {
		return ""
	}
	s = evidenceUserinfoRE.ReplaceAllString(s, "$1***@")
	const max = 200
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

var evidenceUserinfoRE = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]+@`)

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// String renders evidence for logs and error messages without credentials.
func (e Evidence) String() string {
	if e.Status == "" {
		return "unknown"
	}
	label := e.ProxyLabel
	if label == "" {
		label = e.ProxyID
	}
	if label == "" {
		return e.Status
	}
	return fmt.Sprintf("%s (%s)", e.Status, label)
}
