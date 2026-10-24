package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/hosts"
	"aiworkgate/internal/logging"
	"aiworkgate/internal/model"
	"aiworkgate/internal/notify"
	"aiworkgate/internal/schedule"
	"aiworkgate/internal/winutil"
)

// schedPlugins adapts the plugin manager to the scheduler's narrow interface.
type schedPlugins struct{ app *App }

func (p schedPlugins) List() []model.PluginStatus {
	if p.app == nil || p.app.pluginMgr == nil {
		return nil
	}
	return p.app.pluginMgr.List()
}

func (p schedPlugins) Accounts(ctx context.Context, pluginID string) ([]model.Account, error) {
	accounts, err := p.app.pluginMgr.Accounts(ctx, pluginID)
	if err == nil {
		// Remember the labels so a notification can name an account the user
		// recognises without another plugin round trip at flush time.
		p.app.rememberCheckinAccounts(pluginID, accounts)
	}
	return accounts, err
}

func (p schedPlugins) Checkin(ctx context.Context, pluginID, accountID string) (model.CheckinResult, error) {
	result, err := p.app.pluginMgr.Checkin(ctx, pluginID, accountID)
	// The scheduler calls this once per attempt, retries included; observing
	// here is the only place the app sees the full run without owning it.
	p.app.observeCheckin(pluginID, accountID, result, err)
	return result, err
}

func (p schedPlugins) CheckinTime(pluginID string) (string, bool) {
	if p.app.store != nil {
		if _, _, _, at, _, ok, err := p.app.store.GetPluginSettings(pluginID); err == nil && ok {
			if at = strings.TrimSpace(at); at != "" {
				return at, true
			}
		}
	}
	if p.app.pluginMgr != nil {
		if st, ok := p.app.pluginMgr.Get(pluginID); ok {
			if at := strings.TrimSpace(st.Manifest.Checkin.DefaultTime); at != "" {
				return at, true
			}
		}
	}
	return "", false
}

// schedA adapts the AI Work 助手 executable for the manual "sign all" action.
type schedA struct{ app *App }

func (a schedA) ExePath() string {
	if a.app == nil {
		return ""
	}
	return a.app.aExePath()
}

func (a schedA) RunTask(ctx context.Context, exePath, taskKey string) (hosts.TaskResult, error) {
	return hosts.RunTask(ctx, exePath, taskKey)
}

// logNotifier keeps the historical host-log line for every event and forwards
// the events internal/notify owns. The scheduler's per-account checkin_failed
// is intentionally not forwarded: the run aggregator below folds a whole run
// into one message (A notifies once per event, not once per account —
// src-tauri/src/tasks/scheduler.rs:344-366).
type logNotifier struct {
	log  *logging.Logger
	emit func(kind, title, body string)
}

func (n logNotifier) Notify(kind, title, body string) {
	if n.log != nil {
		n.log.Warn("notify", title, map[string]any{"kind": kind, "body": body})
	}
	if n.emit != nil {
		n.emit(kind, title, body)
	}
}

func (a *App) initSchedule() {
	if a.pluginMgr == nil || a.store == nil {
		return
	}
	cfg := a.configSnapshot()
	a.sched = &schedule.Manager{
		Plugins:     schedPlugins{app: a},
		Store:       a.store,
		A:           schedA{app: a},
		Notify:      logNotifier{log: a.logger, emit: a.onScheduledNotify},
		Log:         a.logger,
		RetryDelays: retryDelaysFromConfig(cfg.Checkin.RetryCount, cfg.Checkin.RetryBackoffSeconds),
	}
}

// retryDelaysFromConfig turns the settings into the scheduler's per-attempt
// waits. A count beyond the configured backoff list repeats its last value.
func retryDelaysFromConfig(count int, backoffSeconds []int) []time.Duration {
	delays := make([]time.Duration, 0, count)
	for i := 0; i < count; i++ {
		seconds := 20
		if n := len(backoffSeconds); n > 0 {
			if i < n {
				seconds = backoffSeconds[i]
			} else {
				seconds = backoffSeconds[n-1]
			}
		}
		delays = append(delays, time.Duration(seconds)*time.Second)
	}
	return delays
}

func (a *App) checkinAll() any {
	if a.sched == nil {
		return map[string]any{"results": []schedule.Result{}, "error": "scheduler is not available"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	return map[string]any{"results": a.sched.CheckinAll(ctx)}
}

// startAIfConfigured launches A at host startup when the user enabled
// a.auto_start. A already running (port open) is left untouched.
func (a *App) startAIfConfigured() {
	if a == nil || a.logger == nil {
		return
	}
	cfg := a.configSnapshot()
	if !cfg.A.AutoStart {
		return
	}
	if hosts.PortOpen(a.aPort) {
		a.logger.Info("app", "A is already running", map[string]any{"port": a.aPort})
		return
	}
	exe := a.aExePath()
	if exe == "" {
		a.logger.Warn("app", "a.auto_start is on but A's executable was not found", nil)
		return
	}
	if err := hosts.StartApp(exe); err != nil {
		a.logger.Warn("app", "cannot start A", map[string]any{"exe": exe, "error": err.Error()})
		return
	}
	a.logger.Info("app", "A started", map[string]any{"exe": exe})
}

// aExePath resolves A's executable: the configured path wins, otherwise scan
// the HKCU Run entries for the AI Work 助手 autostart value.
func (a *App) aExePath() string {
	if a == nil {
		return ""
	}
	if p := strings.TrimSpace(a.configSnapshot().A.ExePath); p != "" {
		return p
	}
	entries, err := winutil.ListRunEntries()
	if err != nil {
		return ""
	}
	for _, raw := range entries {
		exe := exeFromRunValue(raw)
		low := strings.ToLower(exe)
		if strings.Contains(low, "ai-work-assistant.exe") || strings.Contains(low, "ai work") {
			return exe
		}
	}
	return ""
}

// exeFromRunValue extracts the executable from a Run value such as
// `"C:\path\app.exe" --flag`.
func exeFromRunValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if raw[0] == '"' {
		if end := strings.IndexByte(raw[1:], '"'); end >= 0 {
			return raw[1 : 1+end]
		}
		return ""
	}
	if end := strings.IndexAny(raw, " \t"); end >= 0 {
		return raw[:end]
	}
	return raw
}

// ---------------------------------------------------------------- notifications

const (
	// checkinRunQuiet ends a check-in run that produced no new observation for
	// this long. The scheduler's default retry backoff is 5s+20s, so 30s never
	// splits one run at the default settings; a custom long backoff simply
	// flushes the run early, which the per-(kind, plugin) throttle absorbs.
	checkinRunQuiet = 30 * time.Second

	// Bounds keep a verbose upstream message out of a push (A's scheduler
	// summary is one short line as well, scheduler.rs:344-366).
	notifyReasonMaxRunes = 120
	notifyTextMaxRunes   = 200
)

// checkinRun accumulates the attempts of one plugin's check-in pass. The map
// key is the account; the last attempt wins, mirroring "one outcome per
// account" rather than "one message per attempt".
type checkinRun struct {
	outcome map[string]string
	pending map[string]bool
	detail  map[string]checkinFailure
	dead    map[string]bool
	order   []string
	seen    map[string]bool
	timer   *time.Timer
}

type checkinFailure struct {
	reason string
	kind   string
}

func (r *checkinRun) allFinal(expected []string) bool {
	if len(expected) == 0 {
		return false
	}
	for _, id := range expected {
		if _, ok := r.outcome[id]; !ok || r.pending[id] {
			return false
		}
	}
	return true
}

// checkinNotifier folds check-in attempts into events. It is the host-side
// equivalent of A's per-run summary notification (tasks/trae_checkin.rs:750-756,
// commands/checkin.rs:154-165): one message per run, never one per account.
type checkinNotifier struct {
	app *App

	mu        sync.Mutex
	runs      map[string]*checkinRun
	accounts  map[string][]string
	labels    map[string]string
	doneDay   map[string]string
	failedDay map[string]string
	deadDay   map[string]string
	now       func() time.Time
}

func newCheckinNotifier(a *App) *checkinNotifier {
	return &checkinNotifier{
		app:       a,
		runs:      map[string]*checkinRun{},
		accounts:  map[string][]string{},
		labels:    map[string]string{},
		doneDay:   map[string]string{},
		failedDay: map[string]string{},
		deadDay:   map[string]string{},
		now:       time.Now,
	}
}

// notifySettingsFromConfig is the single conversion point; the notify package
// deliberately does not import config.
func notifySettingsFromConfig(cfg config.NotifyConfig) notify.Settings {
	return notify.Settings{
		Enabled:     cfg.Enabled,
		Channel:     cfg.Channel,
		Events:      append([]string(nil), cfg.Events...),
		MinInterval: time.Duration(cfg.MinIntervalSeconds) * time.Second,
	}
}

func (a *App) rememberCheckinAccounts(pluginID string, accounts []model.Account) {
	if a == nil || a.checkinNotify == nil || pluginID == "" {
		return
	}
	a.checkinNotify.remember(pluginID, accounts)
}

func (a *App) observeCheckin(pluginID, accountID string, result model.CheckinResult, err error) {
	if a == nil || a.checkinNotify == nil {
		return
	}
	a.checkinNotify.observe(pluginID, accountID, result, err)
}

// emitNotify is the only path to the dispatcher, so a test App without a
// dispatcher can still drive the scheduling adapters.
func (a *App) emitNotify(pluginID string, event notify.Event) {
	if a == nil || a.notify == nil {
		return
	}
	a.notify.Emit(pluginID, event)
}

// onScheduledNotify forwards the events the schedule package raises through
// its Notifier seam. quota_low already had the D60 threshold and the per-account
// per-day dedupe (internal/schedule/quota.go:112-151), so it is reused as-is.
// checkin_failed is dropped here: the run aggregator sends the single
// aggregated message; plugin_offline is not a subscribed event kind.
func (a *App) onScheduledNotify(kind, title, body string) {
	switch kind {
	case schedule.KindQuotaLow:
		a.emitNotify(notifyPluginFromTitle(title), notify.Event{
			Kind:  notify.KindQuotaLow,
			Title: title,
			Body:  body,
			At:    time.Now(),
		})
	}
}

// notifyPluginFromTitle recovers the plugin id from the scheduler's
// "<事件>：<插件>" titles without widening the schedule.Notifier contract.
func notifyPluginFromTitle(title string) string {
	for _, sep := range []string{"：", ":"} {
		if idx := strings.Index(title, sep); idx >= 0 {
			return strings.TrimSpace(title[idx+len(sep):])
		}
	}
	return ""
}

func (n *checkinNotifier) remember(pluginID string, accounts []model.Account) {
	ids := make([]string, 0, len(accounts))
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, acc := range accounts {
		if !acc.Enabled || acc.ID == "" {
			continue
		}
		ids = append(ids, acc.ID)
		label := strings.TrimSpace(acc.Label)
		if label == "" {
			label = acc.ID
		}
		n.labels[pluginID+"\x00"+acc.ID] = label
	}
	n.accounts[pluginID] = ids
}

// observe records one attempt. It never blocks: the actual push is queued by
// the dispatcher.
func (n *checkinNotifier) observe(pluginID, accountID string, result model.CheckinResult, err error) {
	if n == nil || pluginID == "" || accountID == "" {
		return
	}
	ready := false
	n.mu.Lock()
	run := n.runs[pluginID]
	if run == nil {
		run = &checkinRun{
			outcome: map[string]string{},
			pending: map[string]bool{},
			detail:  map[string]checkinFailure{},
			dead:    map[string]bool{},
			seen:    map[string]bool{},
		}
		n.runs[pluginID] = run
	}
	if !run.seen[accountID] {
		run.seen[accountID] = true
		run.order = append(run.order, accountID)
	}
	success := err == nil && (result.Result == model.CheckinOK || result.Result == model.CheckinAlready)
	switch {
	case success:
		run.outcome[accountID] = "ok"
		delete(run.pending, accountID)
		delete(run.detail, accountID)
	case err == nil && result.Result == model.CheckinSkipped:
		// A cooldown/device skip is not a run failure; it only says the
		// account was not called this time. A skip that still carries a
		// session_dead/banned kind means the account is parked on a permanent
		// state, which is exactly what account_dead reports.
		run.outcome[accountID] = "skipped"
		delete(run.pending, accountID)
		if result.Kind == model.StatusKindSessionDead || result.Kind == model.StatusKindBanned {
			run.detail[accountID] = checkinFailure{reason: result.Message, kind: result.Kind}
			run.dead[accountID] = true
		}
	default:
		reason := result.Message
		if err != nil {
			reason = err.Error()
		}
		run.outcome[accountID] = "fail"
		run.detail[accountID] = checkinFailure{reason: reason, kind: result.Kind}
		// An unattributed failure is retried by the scheduler, so the account
		// is not final yet. A classified failure is terminal for this run
		// (internal/schedule/checkin.go:84-94).
		run.pending[accountID] = result.Kind == ""
		if result.Kind == model.StatusKindSessionDead || result.Kind == model.StatusKindBanned {
			run.dead[accountID] = true
		}
	}
	ready = run.allFinal(n.accounts[pluginID])
	if run.timer != nil {
		run.timer.Stop()
	}
	run.timer = time.AfterFunc(checkinRunQuiet, func() { n.flush(pluginID, run) })
	n.mu.Unlock()

	if ready {
		// Flush off the check-in call path: flush reads account health from
		// the store and must not add latency to the plugin round trip.
		go n.flush(pluginID, run)
	}
}

// flush turns the completed run into at most three events and forgets it. A
// stale timer (the run already flushed) is a no-op.
func (n *checkinNotifier) flush(pluginID string, run *checkinRun) {
	if n == nil {
		return
	}
	now := n.now()
	day := now.Format("2006-01-02")

	n.mu.Lock()
	if n.runs[pluginID] != run {
		n.mu.Unlock()
		return
	}
	delete(n.runs, pluginID)
	if run.timer != nil {
		run.timer.Stop()
	}

	var okCount, failCount int
	firstReason := ""
	var newFailures []string
	type deadAccount struct {
		id    string
		label string
		kind  string
	}
	var deadAccounts []deadAccount

	for _, id := range run.order {
		key := pluginID + "\x00" + id
		switch run.outcome[id] {
		case "ok":
			okCount++
			continue
		case "fail":
			failCount++
			failure := run.detail[id]
			if firstReason == "" {
				firstReason = failure.reason
			}
			if n.failedDay[key] != day {
				// One checkin_fail message per failing account per day; the
				// message itself covers the whole run (A, scheduler.rs:344-366).
				n.failedDay[key] = day
				newFailures = append(newFailures, id)
			}
		}
		if run.dead[id] && n.deadDay[key] != day {
			n.deadDay[key] = day
			label := n.labels[key]
			if label == "" {
				label = id
			}
			deadAccounts = append(deadAccounts, deadAccount{id: id, label: label, kind: run.detail[id].kind})
		}
	}

	sendDone := failCount == 0 && okCount > 0 && n.doneDay[pluginID] != day
	if sendDone {
		n.doneDay[pluginID] = day
	}
	n.mu.Unlock()

	// Account health is read outside the lock; a missing store or row only
	// means the fallback wording is used.
	for _, dead := range deadAccounts {
		next := deadNextStep(n.app, pluginID, dead.id, dead.kind)
		n.emit(pluginID, notify.Event{
			Kind:  notify.KindAccountDead,
			Title: "账号失效：" + pluginID,
			Body:  boundedNotice(dead.label+"："+next, notifyTextMaxRunes),
			At:    now,
		})
	}

	if len(newFailures) > 0 {
		body := fmt.Sprintf("插件 %s 签到完成：成功 %d，失败 %d", pluginID, okCount, failCount)
		if reason := boundedNotice(firstReason, notifyReasonMaxRunes); reason != "" {
			body += "；首个失败：" + reason
		}
		n.emit(pluginID, notify.Event{
			Kind:  notify.KindCheckinFail,
			Title: "签到失败：" + pluginID,
			Body:  boundedNotice(body, notifyTextMaxRunes),
			At:    now,
		})
	}
	if sendDone {
		n.emit(pluginID, notify.Event{
			Kind:  notify.KindCheckinDone,
			Title: "签到完成：" + pluginID,
			Body:  boundedNotice(fmt.Sprintf("插件 %s 签到完成：成功 %d，失败 0", pluginID, okCount), notifyTextMaxRunes),
			At:    now,
		})
	}
}

func (n *checkinNotifier) emit(pluginID string, event notify.Event) {
	if n.app != nil {
		n.app.emitNotify(pluginID, event)
	}
}

// deadNextStep prefers the cooldown reason the schedule package stored (it
// already carries the human next step, account_health.go:168-180) and falls
// back to the same wording when only the kind is known.
func deadNextStep(a *App, pluginID, accountID, kind string) string {
	if a != nil && a.store != nil {
		if health, ok, err := a.store.GetAccountHealth(pluginID, accountID); err == nil && ok {
			if reason := strings.TrimSpace(health.CooldownReason); reason != "" {
				return reason
			}
		}
	}
	switch kind {
	case model.StatusKindSessionDead:
		return "登录态已失效，请重新登录"
	case model.StatusKindBanned:
		return "账号被封禁，请更换账号"
	}
	return "账号已失效，请重新登录"
}

// boundedNotice redacts credentials, collapses whitespace and caps the rune
// count so a plugin message can never dominate a push notification.
func boundedNotice(text string, maxRunes int) string {
	text = logging.RedactBody(strings.TrimSpace(text))
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return text
}
