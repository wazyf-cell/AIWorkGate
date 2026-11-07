package app

// P14/D-59: /me is a key-scoped self-service page. It is deliberately separate
// from /api: the only credential is a client key, the session is bound to that
// key id, every request re-reads the key row (revoked/expired keys lose the
// session immediately), and responses never contain request bodies, other
// keys' rows or admin capabilities.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/gateway"
	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
	"aiworkgate/internal/store"
)

const (
	meCookieName   = "awg_me_session"
	meSessionTTL   = 12 * time.Hour
	meLoginBodyCap = 4 << 10
	meDefaultLogs  = 50
	meMaxLogs      = 200
	meModelTopN    = 12
	meWeekDays     = 7
	// meModelsBudget bounds one GET /me/api/models: the universe read walks the
	// same scheduling snapshot /v1/models is built from, whose per-plugin reads
	// already carry their own budgets.
	meModelsBudget = 30 * time.Second
	// meModelCheckBodyCap bounds the POST /me/api/models/check body: one model
	// name is all it needs (the check is a scope test, never an upstream call).
	meModelCheckBodyCap = 4 << 10
)

// meSnapshotFor is the seam the /me model selector reads the availability
// universe through; tests inject a hand-built snapshot so no plugin runs.
var meSnapshotFor = func(a *App, ctx context.Context) (*scheduler.Snapshot, error) {
	return a.schedulerSnapshot(ctx)
}

type meSession struct {
	keyID   string
	expires time.Time
}

type meHandler struct {
	app *App

	mu       sync.Mutex
	sessions map[string]meSession
}

func newMeHandler(a *App) *meHandler {
	return &meHandler{app: a, sessions: make(map[string]meSession)}
}

type meKeyView struct {
	Name             string   `json:"name"`
	Owner            bool     `json:"owner"`
	ExpiresAt        int64    `json:"expires_at"`
	DailyQuota       int      `json:"daily_quota"`
	MaxRequestsTotal int      `json:"max_requests_total"`
	ModelScope       []string `json:"model_scope"`
	UsedToday        int      `json:"used_today"`
	QuotaLeft        int      `json:"quota_left"`
}

func meViewOf(key model.ClientKey) meKeyView {
	scope := key.ModelScope
	if scope == nil {
		scope = []string{}
	}
	return meKeyView{
		Name: key.Name, Owner: key.Owner, ExpiresAt: key.ExpiresAt,
		DailyQuota: key.DailyQuota, MaxRequestsTotal: key.MaxRequestsTotal,
		ModelScope: scope, UsedToday: key.UsedToday, QuotaLeft: key.QuotaLeft,
	}
}

func (h *meHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch {
	case path == "/me" || path == "/me/":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, mePageHTML)
		}
	case path == "/me/api/session":
		switch r.Method {
		case http.MethodPost:
			h.login(w, r)
		case http.MethodDelete:
			h.logout(w, r)
		default:
			h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case path == "/me/api/me":
		if r.Method != http.MethodGet {
			h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.overview(w, r)
	case path == "/me/api/logs":
		if r.Method != http.MethodGet {
			h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.logs(w, r)
	case path == "/me/api/models":
		if r.Method != http.MethodGet {
			h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.models(w, r)
	case path == "/me/api/models/check":
		if r.Method != http.MethodPost {
			h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.modelCheck(w, r)
	default:
		h.writeError(w, http.StatusNotFound, "not found")
	}
}

func (h *meHandler) login(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, meLoginBodyCap+1))
	if err != nil || len(body) > meLoginBodyCap {
		h.writeError(w, http.StatusBadRequest, "请求体无效")
		return
	}
	var payload struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(body, &payload) != nil || strings.TrimSpace(payload.Key) == "" {
		h.writeError(w, http.StatusBadRequest, "请输入客户端 Key")
		return
	}
	app := h.app
	if app == nil || app.secrets == nil || app.store == nil {
		h.writeError(w, http.StatusServiceUnavailable, "服务尚未就绪")
		return
	}
	keyID, ok := app.secrets.VerifyClientKey(strings.TrimSpace(payload.Key))
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "Key 无效")
		return
	}
	key, found, err := app.store.GetClientKey(keyID)
	if err != nil || !found {
		h.writeError(w, http.StatusUnauthorized, "Key 无效")
		return
	}
	if reason := meKeyRejection(key); reason != "" {
		h.writeError(w, http.StatusUnauthorized, reason)
		return
	}

	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		h.writeError(w, http.StatusInternalServerError, "会话创建失败")
		return
	}
	value := hex.EncodeToString(token)
	h.mu.Lock()
	h.purgeLocked(time.Now())
	h.sessions[value] = meSession{keyID: keyID, expires: time.Now().Add(meSessionTTL)}
	h.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name: meCookieName, Value: value, Path: "/me",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: meSecureRequest(r),
		MaxAge: int(meSessionTTL / time.Second),
	})
	writeMeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": meViewOf(key)})
}

func (h *meHandler) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(meCookieName); err == nil {
		h.mu.Lock()
		delete(h.sessions, cookie.Value)
		h.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: meCookieName, Value: "", Path: "/me", MaxAge: -1, HttpOnly: true})
	writeMeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// sessionKey resolves the cookie to a live key and re-checks it on every call,
// so disable/expiry/delete take effect on the next request.
func (h *meHandler) sessionKey(w http.ResponseWriter, r *http.Request) (model.ClientKey, bool) {
	cookie, err := r.Cookie(meCookieName)
	if err != nil || cookie.Value == "" {
		h.writeError(w, http.StatusUnauthorized, "请先登录")
		return model.ClientKey{}, false
	}
	h.mu.Lock()
	session, ok := h.sessions[cookie.Value]
	if ok && time.Now().After(session.expires) {
		delete(h.sessions, cookie.Value)
		ok = false
	}
	h.mu.Unlock()
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "会话已过期，请重新登录")
		return model.ClientKey{}, false
	}
	key, found, err := h.app.store.GetClientKey(session.keyID)
	if err != nil || !found {
		h.writeError(w, http.StatusUnauthorized, "Key 已吊销")
		return model.ClientKey{}, false
	}
	if reason := meKeyRejection(key); reason != "" {
		h.writeError(w, http.StatusUnauthorized, reason)
		return model.ClientKey{}, false
	}
	return key, true
}

func meKeyRejection(key model.ClientKey) string {
	if !key.Enabled {
		return "Key 已停用"
	}
	if key.ExpiresAt > 0 && key.ExpiresAt <= time.Now().Unix() {
		return "Key 已过期（" + time.Unix(key.ExpiresAt, 0).Format("2006-01-02 15:04") + "）"
	}
	return ""
}

func (h *meHandler) overview(w http.ResponseWriter, r *http.Request) {
	key, ok := h.sessionKey(w, r)
	if !ok {
		return
	}
	now := time.Now()
	today := store.ClientKeyUsageDay(now)
	from := store.ClientKeyUsageDay(now.AddDate(0, 0, -(meWeekDays - 1)))
	todayUsage, _, _ := h.app.store.KeyUsage(key.ID, today)
	weekUsage, _ := h.app.store.KeyUsageRange(key.ID, from, today)
	models, _ := h.app.store.KeyModelUsage(key.ID, from, today, meModelTopN)
	logs, _ := h.app.store.RecentKeyLogs(key.ID, meDefaultLogs)
	writeMeJSON(w, http.StatusOK, map[string]any{
		"key":    meViewOf(key),
		"today":  todayUsageJSON(todayUsage),
		"week":   todayUsageJSON(weekUsage),
		"models": models,
		"logs":   logs,
	})
}

func (h *meHandler) logs(w http.ResponseWriter, r *http.Request) {
	key, ok := h.sessionKey(w, r)
	if !ok {
		return
	}
	limit := meDefaultLogs
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= meMaxLogs {
			limit = n
		}
	}
	logs, err := h.app.store.RecentKeyLogs(key.ID, limit)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "读取记录失败")
		return
	}
	writeMeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

// meModelInScope is the /me side of the client-key model scope admission. It
// mirrors gateway.modelInScope (internal/gateway/keys_scope.go) exactly:
//   - an empty scope allows every model;
//   - an entry is either an exact model id or a "plugin/*" prefix wildcard;
//   - ids compare canonicalised (trim + lower) through gateway.CanonicalModelID,
//     the same comparison key the catalog merge uses.
//
// The gateway keeps that matcher unexported and the /me page must not lean on
// the gateway's internal surface, so the semantics are reimplemented here;
// me_models_test.go pins the shared vectors so the two sides cannot drift.
func meModelInScope(scope []string, names ...string) bool {
	if len(scope) == 0 {
		return true
	}
	for _, raw := range scope {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if strings.HasSuffix(entry, "/*") {
			// Keeps the trailing "/" so "p1/*" never matches the bare "p1".
			prefix := strings.TrimSuffix(gateway.CanonicalModelID(entry), "*")
			for _, name := range names {
				canonical := gateway.CanonicalModelID(name)
				if strings.HasPrefix(canonical, prefix) && len(canonical) > len(prefix) {
					return true
				}
			}
			continue
		}
		for _, name := range names {
			if gateway.CanonicalModelID(entry) == gateway.CanonicalModelID(name) {
				return true
			}
		}
	}
	return false
}

// meFilterModelsForScope keeps the universe ids one key may see, mirroring the
// gateway's per-key /v1/models filter (handleModelsForKey): a key bound to one
// pool only sees that pool's ids (bare logical names are rewritten to
// "pool/<model>" at dispatch, so advertising the rest would just 404), and
// model_scope filters what remains. Sorted so the selector is stable.
func meFilterModelsForScope(key model.ClientKey, ids []string) []string {
	poolPrefix := ""
	if pool := strings.TrimSpace(key.BindPool); pool != "" {
		poolPrefix = gateway.CanonicalModelID(pool) + "/"
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if poolPrefix != "" && !strings.HasPrefix(gateway.CanonicalModelID(id), poolPrefix) {
			continue
		}
		if !meModelInScope(key.ModelScope, id) {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// meModelIDsFor computes the /me model selector list: model_scope ∩ the models
// the gateway currently publishes. Availability follows the /v1/models rules,
// read from the same sources:
//
//   - every scheduling pool with UpstreamEnabled contributes its models as
//     "<plugin>/<model>"; pool.Models already excludes the models the resource
//     page switched off, and UpstreamEnabled folds in the plugin's own enable
//     flag (snapshotFrom);
//   - a pool whose live model list is empty falls back to the persisted
//     enabled catalog rows (the gateway skips its catalog filter in that case;
//     the store query also drops models past the D-38 delisting grace window);
//   - logical bindings are published as bare names;
//   - when the snapshot cannot be built at all the persisted enabled catalog
//     is the whole (degraded) universe: plugin enablement is then unknown, so
//     the selector may over-list — the gateway's own admission check remains
//     the authoritative gate. Structured decision models stay out of the
//     selector (they are not chat models).
func meModelIDsFor(a *App, key model.ClientKey) ([]string, bool) {
	raw := make([]string, 0, 32)
	// Dedupe by canonical id keeping the first spelling, the way handleModels
	// merges the published catalog (a plugin reporting "Kimi-K2" twice stays
	// one entry).
	seen := make(map[string]bool, 32)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		canon := gateway.CanonicalModelID(id)
		if seen[canon] {
			return
		}
		seen[canon] = true
		raw = append(raw, id)
	}
	degraded := false
	ctx, cancel := context.WithTimeout(context.Background(), meModelsBudget)
	defer cancel()
	snap, err := meSnapshotFor(a, ctx)
	if err != nil || snap == nil {
		degraded = true
	} else {
		for id, pool := range snap.Pools {
			if pool == nil || !pool.UpstreamEnabled {
				continue
			}
			if len(pool.Models) == 0 {
				continue // covered by the catalog fallback below
			}
			for modelID := range pool.Models {
				add(id + "/" + modelID)
			}
		}
		for name := range snap.Bindings {
			add(name)
		}
	}
	if catalog, catErr := a.store.MeEnabledCatalogModels(); catErr == nil {
		for _, row := range catalog {
			if degraded {
				add(row.PluginID + "/" + row.ModelID)
				continue
			}
			if pool, ok := snap.Pools[row.PluginID]; ok && pool != nil &&
				pool.UpstreamEnabled && len(pool.Models) == 0 {
				add(row.PluginID + "/" + row.ModelID)
			}
		}
	}
	return meFilterModelsForScope(key, raw), degraded
}

// models serves GET /me/api/models: the model ids this key may select, for the
// self-service model picker and the copyable request example.
func (h *meHandler) models(w http.ResponseWriter, r *http.Request) {
	key, ok := h.sessionKey(w, r)
	if !ok {
		return
	}
	ids, degraded := meModelIDsFor(h.app, key)
	if ids == nil {
		ids = []string{}
	}
	payload := map[string]any{"models": ids}
	if degraded {
		payload["degraded"] = true
	}
	writeMeJSON(w, http.StatusOK, payload)
}

// meKeyModelAdmission mirrors the gateway's per-request admission
// (keyRouteModel + modelInScope) for the /me check operation: a bound pool
// rewrites bare names to "<pool>/<model>" and forbids foreign prefixes, and
// the model must sit inside the key's model_scope. The second return is the
// routed id on success or the human reason on refusal.
//
// P14-4 acceptance ("模型选择器只列 model_scope ∩ 启用模型，越权 403") needs a
// minimal really callable operation that can be refused; this check is it. It
// is a scope test only — it never touches an upstream — so a pass proves the
// key's reach, not that the model answered.
func meKeyModelAdmission(key model.ClientKey, requested string) (string, string) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", "请提供模型名"
	}
	pool := strings.TrimSpace(key.BindPool)
	routed := requested
	if pool != "" {
		id, inner, prefixed := strings.Cut(requested, "/")
		prefixed = prefixed && strings.TrimSpace(id) != "" && strings.TrimSpace(inner) != ""
		switch {
		case prefixed && id != pool:
			return "", fmt.Sprintf("该 Key 只绑定插件 %s，不能使用 %s 前缀的模型", pool, id)
		case !prefixed:
			routed = pool + "/" + requested
		}
	}
	if !meModelInScope(key.ModelScope, requested, routed) {
		return "", fmt.Sprintf("模型 %q 不在该 Key 的模型范围内", requested)
	}
	return routed, ""
}

// modelCheck serves POST /me/api/models/check {"model": "..."}: inside the
// key's scope -> 200 {"allowed":true,"model":"<routed>"}; outside -> 403 with
// code "model_not_allowed". The 403 (not the gateway's 404) is deliberate: the
// /me page is a human self-service surface, so "the key may not use this" is
// an authorisation answer, not an unknown-model answer.
func (h *meHandler) modelCheck(w http.ResponseWriter, r *http.Request) {
	key, ok := h.sessionKey(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, meModelCheckBodyCap+1))
	if err != nil || len(body) > meModelCheckBodyCap {
		h.writeError(w, http.StatusBadRequest, "请求体无效")
		return
	}
	var payload struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &payload) != nil || strings.TrimSpace(payload.Model) == "" {
		h.writeError(w, http.StatusBadRequest, "请提供要校验的模型名")
		return
	}
	routed, reason := meKeyModelAdmission(key, payload.Model)
	if reason != "" {
		writeMeJSON(w, http.StatusForbidden, map[string]any{
			"error": map[string]any{"message": reason, "code": "model_not_allowed"},
		})
		return
	}
	writeMeJSON(w, http.StatusOK, map[string]any{"allowed": true, "model": routed})
}

func todayUsageJSON(u store.ClientKeyUsage) map[string]any {
	out := map[string]any{
		"requests":          u.Requests,
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
	}
	if u.Credits != nil {
		out["credits"] = *u.Credits
	}
	return out
}

func (h *meHandler) writeError(w http.ResponseWriter, status int, message string) {
	writeMeJSON(w, status, map[string]any{"error": map[string]any{"message": message}})
}

func writeMeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func meSecureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func (h *meHandler) purgeLocked(now time.Time) {
	for token, session := range h.sessions {
		if now.After(session.expires) {
			delete(h.sessions, token)
		}
	}
}
