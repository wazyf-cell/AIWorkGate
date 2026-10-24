package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Notification admin API (F-19 port). The settings page only manages the
// policy and the secret URL; delivery itself belongs to internal/notify and is
// reached through Options callbacks, keeping this package transport-only.
//
// Shapes:
//
//	GET  /api/notify       -> NotifyStatus
//	PUT  /api/notify       -> NotifyStatus
//	POST /api/notify/test  -> {"ok":true} or 502 notify_failed
//
// The URL is write-only: GET exposes url_set, never the value. Reference: A
// push_notify (src-tauri/src/commands/workbuddy/common.rs:234-250) gates by
// master switch and per-event subscription before any channel is contacted.

// notifyChannels is the accepted channel identifier set.
var notifyChannels = map[string]bool{
	"generic": true, "bark": true, "serverchan": true, "wecom": true,
}

// notifyEventKinds is the accepted subscription set. Keeping the list here
// makes a stale SPA fail loudly with Chinese text instead of silently
// subscribing to nothing.
var notifyEventKinds = map[string]bool{
	"checkin_fail": true, "checkin_done": true, "account_dead": true, "quota_low": true,
}

// notifyMinInterval/Max mirror internal/config (60s .. 24h).
const (
	notifyMinInterval = 60
	notifyMaxInterval = 86400
)

// NotifyStatus is the GET/PUT response. LastSentAt is Unix seconds, 0 when no
// delivery has succeeded in this process; LastError is redacted by the host.
type NotifyStatus struct {
	Enabled            bool     `json:"enabled"`
	Channel            string   `json:"channel"`
	URLSet             bool     `json:"url_set"`
	Events             []string `json:"events"`
	MinIntervalSeconds int      `json:"min_interval_seconds"`
	LastSentAt         int64    `json:"last_sent_at"`
	LastError          string   `json:"last_error"`
}

// NotifyUpdate is a partial update: every field is optional, so the SPA can
// change one switch without resending the others.
type NotifyUpdate struct {
	Enabled            *bool     `json:"enabled"`
	Channel            *string   `json:"channel"`
	URL                *string   `json:"url"`
	Events             *[]string `json:"events"`
	MinIntervalSeconds *int      `json:"min_interval_seconds"`
}

// validateNotifyUpdate normalizes channel and events in place and rejects
// anything outside the closed sets with a Chinese message.
func validateNotifyUpdate(update *NotifyUpdate) error {
	if update.Channel != nil {
		channel := strings.ToLower(strings.TrimSpace(*update.Channel))
		if !notifyChannels[channel] {
			return fmt.Errorf("通知渠道无效：%q（可选：generic、bark、serverchan、wecom）", *update.Channel)
		}
		update.Channel = &channel
	}
	if update.Events != nil {
		events := make([]string, 0, len(*update.Events))
		seen := map[string]bool{}
		for _, raw := range *update.Events {
			kind := strings.ToLower(strings.TrimSpace(raw))
			if !notifyEventKinds[kind] {
				return fmt.Errorf("通知事件无效：%q（可选：checkin_fail、checkin_done、account_dead、quota_low）", raw)
			}
			if seen[kind] {
				continue
			}
			seen[kind] = true
			events = append(events, kind)
		}
		update.Events = &events
	}
	if update.MinIntervalSeconds != nil {
		if *update.MinIntervalSeconds < notifyMinInterval || *update.MinIntervalSeconds > notifyMaxInterval {
			return fmt.Errorf("最小发送间隔必须是 %d–%d 秒之间的整数", notifyMinInterval, notifyMaxInterval)
		}
	}
	return nil
}

// handleNotify serves GET/PUT /api/notify.
func (s *server) handleNotify(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	if r.Method == http.MethodGet {
		if s.opts.GetNotify == nil {
			s.unavailable(w)
			return
		}
		status, err := s.opts.GetNotify()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "读取通知设置失败："+err.Error(), "internal_error")
			return
		}
		if status.Events == nil {
			status.Events = []string{}
		}
		writeJSON(w, http.StatusOK, status)
		return
	}

	if s.opts.SaveNotify == nil {
		s.unavailable(w)
		return
	}
	var update NotifyUpdate
	if !decodeJSON(w, r, &update) {
		return
	}
	if err := validateNotifyUpdate(&update); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	status, err := s.opts.SaveNotify(update)
	if err != nil {
		writeNotifySaveError(w, err)
		return
	}
	if status.Events == nil {
		status.Events = []string{}
	}
	writeJSON(w, http.StatusOK, status)
}

// handleNotifyTest sends one event through the configured channel. A send
// failure is a gateway error (502): the settings themselves were valid.
func (s *server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.TestNotify == nil {
		s.unavailable(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.opts.TestNotify(ctx); err != nil {
		writeError(w, http.StatusBadGateway, "测试通知发送失败："+err.Error(), "notify_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeNotifySaveError maps the host sentinels: a bad URL is a request error,
// a storage failure is internal.
func writeNotifySaveError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrInvalidRequest) {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	writeError(w, http.StatusInternalServerError, "保存通知设置失败："+err.Error(), "internal_error")
}
