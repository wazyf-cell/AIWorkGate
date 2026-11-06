package app

// P14/D-59: /me is a key-scoped self-service page. It is deliberately separate
// from /api: the only credential is a client key, the session is bound to that
// key id, every request re-reads the key row (revoked/expired keys lose the
// session immediately), and responses never contain request bodies, other
// keys' rows or admin capabilities.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/model"
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
)

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
