package app

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"aiworkgate/internal/model"
)

// modelSyncKeyPrefix marks the last day a pool's catalog was auto-synced.
const modelSyncKeyPrefix = "model_sync_day:"

// modelSyncRetryKeyPrefix holds the failure backoff: "day|attempts|nextUnix".
// A failed sync must not be retried every minute for the rest of the day.
const modelSyncRetryKeyPrefix = "model_sync_retry:"

// modelSyncStateKeyPrefix holds the last attempt outcome per pool as JSON
// {"last_success_at":unix,"last_attempt_at":unix,"last_error":"..."}. A shows
// the same lifecycle on its API service page (ApiService.tsx:855-916,
// models_sync.rs:331-359, MIT, 朱天伟); without it a failure stayed invisible
// because only the backoff was persisted. The error is a short, credential-free
// one-liner, never a URL with a token.
const modelSyncStateKeyPrefix = "model_sync_state:"

// modelSyncRunning is true while the scheduled scan is talking to plugins, so
// the resource page can render "同步中".
var modelSyncRunning atomic.Bool

// modelSyncState is the persisted lifecycle of one pool's catalog sync.
type modelSyncState struct {
	LastSuccessAt int64  `json:"last_success_at"`
	LastAttemptAt int64  `json:"last_attempt_at"`
	LastError     string `json:"last_error"`
}

// syncRetryBackoff is the wait after the 1st, 2nd, 3rd... consecutive failure.
var syncRetryBackoff = []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour}

// syncRetry is the parsed failure state for one pool.
type syncRetry struct {
	day      string
	attempts int
	next     int64
}

func parseSyncRetry(raw string) syncRetry {
	var r syncRetry
	parts := strings.Split(raw, "|")
	if len(parts) != 3 {
		return r
	}
	attempts, err := strconv.Atoi(parts[1])
	if err != nil || attempts < 1 {
		return r
	}
	next, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || next < 0 {
		return r
	}
	return syncRetry{day: parts[0], attempts: attempts, next: next}
}

// retryDue reports whether a failed sync may be attempted again now. A state
// from an earlier day never blocks today's scheduled run.
func retryDue(raw, today string, now time.Time) bool {
	r := parseSyncRetry(raw)
	if r.day != today {
		return true
	}
	return now.Unix() >= r.next
}

// nextRetry renders the state after a failure that happened today.
func nextRetry(raw, today string, now time.Time) string {
	r := parseSyncRetry(raw)
	attempts := 1
	if r.day == today {
		attempts = r.attempts + 1
	}
	delay := syncRetryBackoff[len(syncRetryBackoff)-1]
	if attempts-1 < len(syncRetryBackoff) {
		delay = syncRetryBackoff[attempts-1]
	}
	return fmt.Sprintf("%s|%d|%d", today, attempts, now.Add(delay).Unix())
}

// modelSyncLoop turns the per-pool model_sync_time setting into an actual
// scheduled job. Until now the field was stored, validated and displayed, but
// nothing ever read it, so the resource page could only sync on demand.
func (a *App) modelSyncLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-a.shutdownCh:
			return
		case now := <-ticker.C:
			a.runDueModelSyncs(now)
		}
	}
}

// runDueModelSyncs syncs every pool whose configured HH:MM has passed today and
// that has not already been auto-synced today. An empty time means the pool
// opted out.
func (a *App) runDueModelSyncs(now time.Time) {
	if a.store == nil {
		return
	}
	modelSyncRunning.Store(true)
	defer modelSyncRunning.Store(false)
	rows, err := a.store.ListPoolSettings()
	if err != nil {
		a.logger.Warn("app", "model sync scan failed", map[string]any{"error": err.Error()})
		return
	}
	today := now.Format("2006-01-02")
	for _, row := range rows {
		id := strings.TrimSpace(row.PluginID)
		if id == "" || id == model.GlobalPoolID {
			continue
		}
		at := strings.TrimSpace(row.ModelSyncTime)
		if at == "" || !syncTimeReached(at, now) {
			continue
		}
		key := modelSyncKeyPrefix + id
		if last, ok, err := a.store.GetKV(key); err == nil && ok && last == today {
			continue
		}
		retryKey := modelSyncRetryKeyPrefix + id
		retryRaw, retryOK, retryErr := a.store.GetKV(retryKey)
		if retryErr == nil && retryOK && !retryDue(retryRaw, today, now) {
			continue
		}
		st, perr := a.installedPlugin(id)
		if perr != nil || !st.Running {
			// Not installed or stopped: the schedule stays pending, so it runs
			// as soon as the plugin is up later the same day.
			continue
		}
		res, serr := a.syncPoolCatalog(id)
		if serr != nil {
			a.logger.Warn("app", "model catalog auto-sync failed", map[string]any{
				"plugin": id, "error": serr.Error(),
			})
			a.recordModelSyncAttempt(id, now, serr.Error())
			a.backOffModelSync(retryKey, id, retryRaw, today, now)
			continue
		}
		if skipped, ok := res.(map[string]any); ok && skipped["skipped"] == true {
			reason, _ := skipped["reason"].(string)
			a.logger.Info("app", "model catalog auto-sync skipped", map[string]any{
				"plugin": id, "reason": skipped["reason"],
			})
			a.recordModelSyncAttempt(id, now, "skipped: "+reason)
			a.backOffModelSync(retryKey, id, retryRaw, today, now)
			continue
		}
		a.recordModelSyncAttempt(id, now, "")
		if err := a.store.SetKV(key, today); err != nil {
			a.logger.Warn("app", "model sync marker write failed", map[string]any{
				"plugin": id, "error": err.Error(),
			})
			continue
		}
		if err := a.store.DeleteKV(retryKey); err != nil {
			a.logger.Warn("app", "model sync retry state cleanup failed", map[string]any{
				"plugin": id, "error": err.Error(),
			})
		}
		a.logger.Info("app", "model catalog auto-synced", map[string]any{
			"plugin": id, "scheduled_at": at,
		})
	}
}

// backOffModelSync records the next attempt after a failure or a skip, so the
// one-minute tick does not hammer the upstream until midnight.
func (a *App) backOffModelSync(key, id, raw, today string, now time.Time) {
	next := nextRetry(raw, today, now)
	if err := a.store.SetKV(key, next); err != nil {
		a.logger.Warn("app", "model sync retry state write failed", map[string]any{
			"plugin": id, "error": err.Error(),
		})
	}
}

// recordModelSyncAttempt persists the outcome of one catalog sync so the
// resource page can show "never synced / last synced at / last error". An empty
// errMsg means success and clears the stored error.
func (a *App) recordModelSyncAttempt(id string, now time.Time, errMsg string) {
	if a.store == nil || id == "" {
		return
	}
	state := modelSyncState{}
	if raw, ok, err := a.store.GetKV(modelSyncStateKeyPrefix + id); err == nil && ok {
		_ = json.Unmarshal([]byte(raw), &state)
	}
	state.LastAttemptAt = now.Unix()
	if errMsg == "" {
		state.LastSuccessAt = now.Unix()
		state.LastError = ""
	} else {
		state.LastError = sanitizeSyncError(errMsg)
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return
	}
	if err := a.store.SetKV(modelSyncStateKeyPrefix+id, string(payload)); err != nil {
		a.logger.Warn("app", "model sync state write failed", map[string]any{
			"plugin": id, "error": err.Error(),
		})
	}
}

// syncSecretValueRe matches the value of a credential-looking "key=" segment.
var syncSecretValueRe = regexp.MustCompile(`(?i)(bearer\s+|token=|key=|api[-_]?key=|password=)[^\s&,;]+`)

// syncUserInfoRe matches the userinfo part of a URL ("user:pass@").
var syncUserInfoRe = regexp.MustCompile(`://[^/@\s]+@`)

// sanitizeSyncError turns an upstream failure into a short, log-safe one-liner:
// URLs lose their query string (where plugins put tokens), credential-looking
// values are masked, and the result is capped so the status payload stays small.
func sanitizeSyncError(msg string) string {
	fields := strings.Fields(msg)
	for i, field := range fields {
		if !strings.Contains(field, "://") {
			continue
		}
		if q := strings.IndexByte(field, '?'); q >= 0 {
			field = field[:q]
		}
		fields[i] = field
	}
	out := syncUserInfoRe.ReplaceAllString(strings.Join(fields, " "), "://***@")
	out = syncSecretValueRe.ReplaceAllString(out, "${1}***")
	if runes := []rune(out); len(runes) > 160 {
		out = string(runes[:160]) + "…"
	}
	return out
}

// modelSyncSummary aggregates the per-pool lifecycle into one object for the
// resource page: the newest success/attempt, the latest error, the highest
// consecutive failure count and the soonest pending retry. A: ApiService.tsx
// :855-916 shows the same fields for its single scheduler.
func (a *App) modelSyncSummary() map[string]any {
	out := map[string]any{
		"last_success_at": int64(0),
		"last_attempt_at": int64(0),
		"last_error":      "",
		"failure_count":   0,
		"next_retry_at":   int64(0),
		"running":         modelSyncRunning.Load(),
	}
	if a.store == nil {
		return out
	}
	rows, err := a.store.ListPoolSettings()
	if err != nil {
		return out
	}
	var lastSuccess, lastAttempt, nextRetry int64
	failures := 0
	lastError := ""
	now := time.Now().Unix()
	for _, row := range rows {
		id := strings.TrimSpace(row.PluginID)
		if id == "" || id == model.GlobalPoolID {
			continue
		}
		if raw, ok, err := a.store.GetKV(modelSyncStateKeyPrefix + id); err == nil && ok {
			var state modelSyncState
			if json.Unmarshal([]byte(raw), &state) == nil {
				if state.LastSuccessAt > lastSuccess {
					lastSuccess = state.LastSuccessAt
				}
				if state.LastAttemptAt > lastAttempt {
					lastAttempt = state.LastAttemptAt
					lastError = state.LastError
				}
			}
		}
		if raw, ok, err := a.store.GetKV(modelSyncRetryKeyPrefix + id); err == nil && ok {
			retry := parseSyncRetry(raw)
			if retry.attempts > failures {
				failures = retry.attempts
			}
			if retry.next > now && (nextRetry == 0 || retry.next < nextRetry) {
				nextRetry = retry.next
			}
		}
	}
	out["last_success_at"] = lastSuccess
	out["last_attempt_at"] = lastAttempt
	out["last_error"] = lastError
	out["failure_count"] = failures
	out["next_retry_at"] = nextRetry
	return out
}

// syncTimeReached reports whether the local wall clock has passed HH:MM today.
// An unparsable value never fires.
func syncTimeReached(hhmm string, now time.Time) bool {
	at, err := time.Parse("15:04", hhmm)
	if err != nil {
		return false
	}
	return now.Hour()*60+now.Minute() >= at.Hour()*60+at.Minute()
}

// modelSyncStatus describes one pool's auto-sync schedule for the resource page.
// The legacy keys stay untouched; "model_sync" carries the lifecycle summary
// (last success/attempt/error, pending retry) the UI renders next to the manual
// sync button.
func (a *App) modelSyncStatus(pluginID string) map[string]any {
	out := map[string]any{"model_sync_time": "", "auto_synced_on": ""}
	if a.store == nil {
		return out
	}
	if rows, err := a.store.ListPoolSettings(); err == nil {
		for _, row := range rows {
			if row.PluginID == pluginID {
				out["model_sync_time"] = row.ModelSyncTime
			}
		}
	}
	if last, ok, err := a.store.GetKV(modelSyncKeyPrefix + pluginID); err == nil && ok && last != "" {
		out["auto_synced_on"] = last
	}
	out["model_sync"] = a.modelSyncSummary()
	return out
}
