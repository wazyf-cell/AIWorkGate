package gateway

// Per-client-key model scope, pool binding and daily quota (plan 2026-10-21 §1).
//
// Ported from A (TraeWorkAssistant, MIT, 朱天伟): the resolved-key snapshot and
// per-day accounting live in src-tauri/src/api_server/api_keys.rs:55-78,
// 254-293, 327-358, the model admission rejection in routes.rs:390-424. A keeps
// everything in one JSON file; this host reads the snapshot through
// Options.KeyRecord and accumulates through Options.KeyUsage, so the gateway
// never imports the store.
//
// Rules implemented here:
//   - model_scope (empty = every model): an entry is an exact model id or a
//     "plugin/*" wildcard over the plugin's models.
//   - bind_pool (empty = every pool): a bare model name is routed to that
//     plugin as "<pool>/<model>"; a different plugin prefix is a 404.
//   - daily_quota (0 = unlimited): the request counter of the local calendar
//     day reached the limit -> 429 key_quota_exceeded with Retry-After until
//     the next local midnight. When the host installs a KeyUsageReserver the
//     slot is reserved atomically at admission (A auth.rs:74-91: one lock
//     around verify + increment), so concurrent bursts cannot overshoot; the
//     completion path then only adds tokens/credits.
//   - every finished inference request is accumulated once; successes and
//     failures both count as one request, tokens only when usage was parsed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// KeyRecord is the scope view of one verified client key.
type KeyRecord struct {
	ID         string
	Name       string
	Enabled    bool
	ModelScope []string
	BindPool   string
	DailyQuota int
}

// keyRecordContextKey is the request-context slot holding the snapshot.
type keyRecordContextKey struct{}

// withKeyRecord attaches the authenticated key's snapshot to a request.
func withKeyRecord(ctx context.Context, kr KeyRecord) context.Context {
	return context.WithValue(ctx, keyRecordContextKey{}, kr)
}

// keyRecordFrom reads the snapshot attached by ServeHTTP.
func keyRecordFrom(ctx context.Context) (KeyRecord, bool) {
	if ctx == nil {
		return KeyRecord{}, false
	}
	kr, ok := ctx.Value(keyRecordContextKey{}).(KeyRecord)
	return kr, ok
}

// keyRecordFor loads the snapshot. Errors and a missing row degrade to
// "unrestricted": authentication already succeeded against the secret store, so
// a database hiccup must not reject an otherwise valid key.
func (g *Gateway) keyRecordFor(keyID string) (KeyRecord, bool) {
	if g == nil || g.keyRecord == nil || keyID == "" {
		return KeyRecord{}, false
	}
	kr, ok, err := g.keyRecord(keyID)
	if err != nil || !ok {
		return KeyRecord{}, false
	}
	return kr, true
}

// isInferencePath lists the endpoints that consume a key's daily quota. The
// passthrough endpoints (embeddings/images) are billable inference too: they
// reach A with the operator's credential, so they must be admitted like chat.
// /v1/models stays outside it (it is a catalogue read, not inference).
func isInferencePath(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/messages":
		return true
	}
	return isPassThroughPath(path)
}

// keyUsageDay is the local calendar date every counter and the Retry-After
// moment agree on.
func keyUsageDay(t time.Time) string {
	return t.In(time.Local).Format("2006-01-02")
}

// secondsUntilNextLocalDay is the Retry-After of a quota rejection: the number
// of seconds until the counter resets at local 00:00 (at least 1).
func secondsUntilNextLocalDay(now time.Time) int {
	local := now.In(time.Local)
	next := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, 1)
	secs := int((next.Sub(local) + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// keyQuotaAllows enforces daily_quota before a request is routed. It returns
// false after writing the 429.
//
// Ported from A (TraeWorkAssistant, MIT, 朱天伟; api_server/auth.rs:74-91,
// api_keys.rs:327-390): A verifies the key and increments used_today under one
// lock inside authentication, so a burst of N requests cannot overshoot. When a
// KeyUsageReserver is installed this gate reserves the slot atomically before
// routing; without one it keeps the older read-then-record behaviour.
func (g *Gateway) keyQuotaAllows(w http.ResponseWriter, r *http.Request, rec *reqRecord) bool {
	if g == nil || rec == nil || !rec.billable {
		return true
	}
	kr, ok := keyRecordFrom(r.Context())
	if !ok || kr.DailyQuota <= 0 {
		return true
	}
	if g.keyUsageToday == nil && currentKeyUsageReserver() == nil {
		return true
	}
	day := keyUsageDay(g.now())
	if reserver := currentKeyUsageReserver(); reserver != nil {
		reserved, used, err := reserver.ReserveKeyUsage(kr.ID, day, kr.DailyQuota)
		if err != nil {
			// Fail open: accounting trouble must not take the gateway down.
			return true
		}
		if reserved {
			keyQuotaReservations.Store(rec, keyQuotaReservation{keyID: kr.ID, day: day})
			return true
		}
		return g.rejectKeyQuota(w, rec, used, kr.DailyQuota)
	}
	used, err := g.keyUsageToday(kr.ID, day)
	if err != nil || used < kr.DailyQuota {
		// Fail open: accounting trouble must not take the gateway down.
		return true
	}
	return g.rejectKeyQuota(w, rec, used, kr.DailyQuota)
}

// rejectKeyQuota writes the 429 of an exhausted daily quota.
func (g *Gateway) rejectKeyQuota(w http.ResponseWriter, rec *reqRecord, used, quota int) bool {
	w.Header().Set("Retry-After", strconv.Itoa(secondsUntilNextLocalDay(g.now())))
	rec.quotaRejected = true
	g.fail(w, rec, http.StatusTooManyRequests, "key_quota_exceeded", "key_quota_exceeded",
		fmt.Sprintf("该客户端 Key 今日配额已用尽（已用 %d 次 / 上限 %d 次），将于次日 00:00 重置", used, quota))
	return false
}

// KeyUsageReserver is the host store's atomic day counter
// (store.Store.ReserveKeyUsage / ReleaseKeyUsageReservation ported from A
// api_keys.rs:327-390, MIT, 朱天伟). The gateway never imports the store, so the
// host installs it once at startup.
type KeyUsageReserver interface {
	// ReserveKeyUsage consumes one slot of keyID's day bucket when the counter
	// is still below quota (0 = unlimited) and reports the counter on refusal.
	ReserveKeyUsage(keyID, day string, quota int) (reserved bool, used int, err error)
	// ReleaseKeyUsageReservation gives one reserved slot back (a request the
	// key's model scope rejected never consumed quota).
	ReleaseKeyUsageReservation(keyID, day string) error
}

var (
	keyUsageReserverMu sync.RWMutex
	keyUsageReserver   KeyUsageReserver
)

// InstallKeyUsageReserver wires the store's atomic counter into the quota gate.
// Nil restores the legacy read-then-record behaviour (tests, standalone hosts).
func InstallKeyUsageReserver(reserver KeyUsageReserver) {
	keyUsageReserverMu.Lock()
	defer keyUsageReserverMu.Unlock()
	keyUsageReserver = reserver
}

func currentKeyUsageReserver() KeyUsageReserver {
	keyUsageReserverMu.RLock()
	defer keyUsageReserverMu.RUnlock()
	return keyUsageReserver
}

// keyQuotaReservation remembers the slot taken at admission. reqRecord lives in
// gateway.go (outside this port's edit surface), so the bookkeeping rides in a
// side table keyed by the record pointer; ServeHTTP's deferred recordKeyUsage
// always consumes the entry.
type keyQuotaReservation struct {
	keyID string
	day   string
}

var keyQuotaReservations sync.Map // *reqRecord -> keyQuotaReservation

// recordKeyUsage accumulates one finished request. It never fails: the client
// already received its response.
func (g *Gateway) recordKeyUsage(rec *reqRecord) {
	if rec == nil {
		return
	}
	// A reservation already counted the request at admission, so completion must
	// only add tokens/credits; without one the legacy request count applies.
	reserved := false
	var reservation keyQuotaReservation
	if value, ok := keyQuotaReservations.LoadAndDelete(rec); ok {
		reservation, _ = value.(keyQuotaReservation)
		reserved = true
	}
	if reserved && rec.scopeRejected {
		// The key's model scope refused the request before any model ran: give
		// the reserved slot back (A only counts requests that reach a model).
		if reserver := currentKeyUsageReserver(); reserver != nil {
			_ = reserver.ReleaseKeyUsageReservation(reservation.keyID, reservation.day)
		}
	}
	if g == nil || g.keyUsage == nil || rec.keyID == "" {
		return
	}
	if !rec.billable || rec.quotaRejected || rec.scopeRejected {
		return
	}
	var credits *float64
	if rec.credits != nil {
		value := *rec.credits
		credits = &value
	}
	prompt, completion := int(rec.prompt), int(rec.completion)
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	cached := int(rec.cached)
	if cached < 0 {
		cached = 0
	}
	requests := 1
	if reserved {
		requests = 0
	}
	day := keyUsageDay(g.now())
	if reserved && reservation.day != "" {
		// Keep the tokens in the same bucket as the reserved request, even when
		// the response crosses local midnight.
		day = reservation.day
	}
	// Tokens are only known when the upstream reported usage (bodyTap leaves
	// them zero otherwise); requests always count, success or failure.
	g.keyUsage(rec.keyID, day, requests, prompt, completion, cached, credits)
}

// modelInScope ports A's whitelist admission (routes.rs:390-424, MIT, 朱天伟):
// an empty scope allows everything, an entry is either an exact model id or a
// "plugin/*" prefix wildcard. names carries every spelling worth checking
// (the requested name and, for a bound pool, the routed "<pool>/<model>").
func modelInScope(scope []string, names ...string) bool {
	if len(scope) == 0 {
		return true
	}
	for _, raw := range scope {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if strings.HasSuffix(entry, "/*") {
			// Keeps the trailing "/"; ids compare case-insensitively like the
			// catalog merge (A unified_catalog.rs:37 canonical_id).
			prefix := strings.TrimSuffix(CanonicalModelID(entry), "*")
			for _, name := range names {
				canonical := CanonicalModelID(name)
				if strings.HasPrefix(canonical, prefix) && len(canonical) > len(prefix) {
					return true
				}
			}
			continue
		}
		for _, name := range names {
			if CanonicalModelID(entry) == CanonicalModelID(name) {
				return true
			}
		}
	}
	return false
}

// keyRouteModel applies the key's model_scope and bind_pool to one buffered
// request and returns the name to route. On rejection it writes the 404 and
// returns ok=false.
func (g *Gateway) keyRouteModel(w http.ResponseWriter, r *http.Request, rec *reqRecord, model string) (string, bool) {
	kr, ok := keyRecordFrom(r.Context())
	if !ok {
		return model, true
	}
	requested := strings.TrimSpace(model)
	routed := requested
	pool := strings.TrimSpace(kr.BindPool)
	if pool != "" {
		id, inner, prefixed := strings.Cut(requested, "/")
		prefixed = prefixed && strings.TrimSpace(id) != "" && strings.TrimSpace(inner) != ""
		switch {
		case prefixed:
			if id != pool {
				g.failKeyScope(w, rec, requested,
					fmt.Sprintf("该 Key 只绑定插件 %s，不能使用 %s 前缀的模型", pool, id))
				return "", false
			}
		case requested == "":
			g.failKeyScope(w, rec, requested, fmt.Sprintf("该 Key 只绑定插件 %s，请求里却没有模型名", pool))
			return "", false
		default:
			// Bare names are only routable once we know the bound plugin is
			// actually installed; otherwise the 404 would name a dead pool.
			if _, found := g.pluginStatus(pool); !found {
				g.failKeyScope(w, rec, requested,
					fmt.Sprintf("该 Key 绑定的插件 %s 未安装或不可用", pool))
				return "", false
			}
			routed = pool + "/" + requested
		}
	}
	if !modelInScope(kr.ModelScope, requested, routed) {
		g.failKeyScope(w, rec, requested, "该模型不在该 Key 的模型范围内")
		return "", false
	}
	return routed, true
}

// failKeyScope writes the 404 that tells the user the key — not the gateway —
// rejected the model.
func (g *Gateway) failKeyScope(w http.ResponseWriter, rec *reqRecord, model, detail string) {
	rec.scopeRejected = true
	msg := fmt.Sprintf("模型 %q 不可用：%s。请检查宿主「设置 → 客户端 Key」的模型范围与绑定插件。", model, detail)
	g.fail(w, rec, http.StatusNotFound, "invalid_request_error", "model_not_found", msg)
}

// handleModelsForKey serves GET /v1/models, filtering by the key's scope. The
// merged list is small, so it is buffered once and filtered; an unrestricted
// key takes the unchanged path.
func (g *Gateway) handleModelsForKey(w http.ResponseWriter, r *http.Request, rec *reqRecord) {
	kr, ok := keyRecordFrom(r.Context())
	if !ok || (len(kr.ModelScope) == 0 && strings.TrimSpace(kr.BindPool) == "") {
		g.handleModels(w, r, rec)
		return
	}
	buf := &bufferedResponse{header: make(http.Header)}
	g.handleModels(buf, r, rec)

	var list modelsList
	if err := json.Unmarshal(buf.body.Bytes(), &list); err != nil {
		buf.respond(w, buf.body.Bytes())
		return
	}
	filtered := make([]modelEntry, 0, len(list.Data))
	pool := strings.TrimSpace(kr.BindPool)
	poolPrefix := ""
	if pool != "" {
		poolPrefix = CanonicalModelID(pool) + "/"
	}
	for _, entry := range list.Data {
		if !modelInScope(kr.ModelScope, entry.ID) {
			continue
		}
		// A key bound to one pool may only be shown that pool's models: bare
		// names are rewritten to pool/<name> at dispatch, so advertising the
		// rest of the catalog would just produce 404s.
		if poolPrefix != "" && !strings.HasPrefix(CanonicalModelID(entry.ID), poolPrefix) {
			continue
		}
		filtered = append(filtered, entry)
	}
	payload, err := json.Marshal(modelsList{Object: list.Object, Data: filtered})
	if err != nil {
		buf.respond(w, buf.body.Bytes())
		return
	}
	if g.debugBody.Load() {
		rec.bodyResp = string(payload)
	}
	buf.respond(w, payload)
}

// bufferedResponse captures one handler's response so it can be filtered.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

// respond replays the captured headers and payload (or a replacement payload).
func (b *bufferedResponse) respond(w http.ResponseWriter, payload []byte) {
	for key, values := range b.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// prepareMessagesForKey applies scope and pool binding to /v1/messages before
// handleMessages parses the body. Only restricted keys pay the one body buffer;
// the body is handed back unchanged except a rewritten model.
func (g *Gateway) prepareMessagesForKey(w http.ResponseWriter, r *http.Request, rec *reqRecord) bool {
	kr, ok := keyRecordFrom(r.Context())
	if !ok || (len(kr.ModelScope) == 0 && strings.TrimSpace(kr.BindPool) == "") {
		return true
	}
	body, failStatus := readAnthropicBody(w, r)
	if failStatus != 0 {
		rec.status = failStatus
		return false
	}
	meta := parseRequestMeta(body)
	routed, allowed := g.keyRouteModel(w, r, rec, meta.model)
	if !allowed {
		return false
	}
	if routed != meta.model {
		rewritten, err := rewriteModel(body, routed)
		if err != nil {
			g.fail(w, rec, http.StatusBadRequest, "invalid_request_error", "invalid_json",
				"request body must be a JSON object")
			return false
		}
		body = rewritten
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return true
}
