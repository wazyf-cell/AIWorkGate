// Package schedule implements the host-side check-in scheduler and quota
// poller (M5). The plugin runtime, A and the store are reached through narrow
// interfaces so the scheduling rules stay testable without a running plugin.
package schedule

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/hosts"
	"aiworkgate/internal/model"
	"aiworkgate/internal/safego"
	"aiworkgate/internal/store"
)

const (
	// Module is the logger module name used for every scheduler entry.
	Module = "schedule"

	// Interval is how often the background loop runs a scheduling pass.
	Interval = 30 * time.Second

	dateLayout         = "2006-01-02"
	defaultCheckinTime = "09:00"

	// Notification kinds emitted through Notifier.
	KindCheckinFailed = "checkin_failed"
	KindPluginOffline = "plugin_offline"
	KindQuotaLow      = "quota_low"
)

// Plugins is the slice of the plugin runtime the scheduler needs. It is
// declared here (rather than importing internal/plugins) so tests can supply
// a fake and the scheduler cannot reach the rest of the runtime.
type Plugins interface {
	List() []model.PluginStatus
	Accounts(ctx context.Context, pluginID string) ([]model.Account, error)
	Checkin(ctx context.Context, pluginID, accountID string) (model.CheckinResult, error)
	// CheckinTime returns the host-side HH:MM override. ok=false means the
	// plugin has no override and the manifest default applies.
	CheckinTime(pluginID string) (string, bool)
}

// AHost is the A executable integration used by the manual "sign all" action
// and never by the daily schedule (D37: A keeps its own timer).
type AHost interface {
	ExePath() string
	RunTask(ctx context.Context, exePath, taskKey string) (hosts.TaskResult, error)
}

// Notifier raises user-visible notifications (tray bubbles).
type Notifier interface {
	// kind is one of checkin_failed, plugin_offline or quota_low.
	Notify(kind, title, body string)
}

// Logger is the host JSONL logger contract.
type Logger interface {
	Info(module, msg string, fields map[string]any)
	Warn(module, msg string, fields map[string]any)
	Error(module, msg string, fields map[string]any)
}

// Manager runs the daily check-in schedule and the quota poller.
//
// Now and Sleep are optional seams for tests: Now defaults to time.Now and
// Sleep defaults to a context-aware timer.
type Manager struct {
	Plugins Plugins
	Store   *store.Store
	A       AHost
	Notify  Notifier
	Log     Logger
	Now     func() time.Time
	Sleep   func(ctx context.Context, d time.Duration) error

	// RetryDelays are the waits before each automatic retry. A nil slice means
	// the contract default (5s, 20s); an empty non-nil slice disables retries.
	RetryDelays []time.Duration

	// opMu serialises check-in work so a manual CheckinAll cannot race the
	// scheduled pass for the same accounts.
	opMu sync.Mutex

	// runMu guards the background loop state (Start/Stop).
	runMu  sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	// stateMu guards the in-memory quota/notification bookkeeping.
	stateMu         sync.Mutex
	quotaLast       map[string]time.Time
	quotaNotified   map[string]string
	offlineNotified map[string]string
	// attempted records the accounts whose scheduled retries were exhausted
	// today, so a failing account is not retried on every 30s pass.
	attempted map[string]string
	// parked remembers the cooldown deadline already reported for an account,
	// so a cooled account produces one "skipped" note per window instead of one
	// every pass.
	parked map[string]int64
}

// Start launches the background loop. The first pass is a catch-up pass that
// checks in every enabled account still missing a success today, then the
// loop repeats every Interval with the time-of-day due checks. Start is
// idempotent while a loop is running.
func (m *Manager) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.runMu.Lock()
	if m.cancel != nil {
		m.runMu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.cancel = cancel
	m.done = done
	m.runMu.Unlock()

	safego.Go(m.Log, "schedulerLoop", func() {
		defer func() {
			close(done)
			// Allow Start to relaunch after a panic or normal stop.
			m.runMu.Lock()
			m.cancel = nil
			m.done = nil
			m.runMu.Unlock()
		}()
		m.loop(loopCtx)
	})
}

// Stop cancels the loop and waits for the current pass to unwind. It is safe
// to call more than once and when Start was never called.
func (m *Manager) Stop() {
	m.runMu.Lock()
	cancel, done := m.cancel, m.done
	m.runMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
	m.runMu.Lock()
	if m.done == done {
		m.cancel = nil
		m.done = nil
	}
	m.runMu.Unlock()
}

func (m *Manager) loop(ctx context.Context) {
	m.runPass(ctx, true)
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.runPass(ctx, false)
		}
	}
}

// runPass performs one scheduling pass. catchUp ignores the configured
// time-of-day and checks in every account without a success today; later
// passes only run plugins whose configured time has passed.
func (m *Manager) runPass(ctx context.Context, catchUp bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.runDueCheckins(ctx, catchUp)
	if ctx.Err() != nil {
		return
	}
	m.runQuotaPoll(ctx)
}

// runDueCheckins walks the enabled check-in plugins in List order.
func (m *Manager) runDueCheckins(ctx context.Context, catchUp bool) {
	for _, st := range m.Plugins.List() {
		if ctx.Err() != nil {
			return
		}
		pluginID := st.Manifest.ID
		if pluginID == "" || !st.Enabled || !hasCapability(st.Manifest, model.CapCheckin) {
			continue
		}
		now := m.now()
		if !catchUp {
			hh, mm := m.checkinClock(st)
			due := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
			if now.Before(due) {
				continue
			}
		}
		m.checkinPlugin(ctx, pluginID, now.Format(dateLayout))
	}
}

// checkinPlugin runs every enabled account of one plugin that has no
// ok/already record for date yet.
func (m *Manager) checkinPlugin(ctx context.Context, pluginID, date string) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	accounts, err := m.Plugins.Accounts(ctx, pluginID)
	if err != nil {
		m.warn("list accounts failed", map[string]any{"plugin": pluginID, "error": err.Error()})
		if ctx.Err() == nil {
			m.notifyPluginOffline(pluginID, err)
		}
		return
	}
	done := m.successfulAccounts(pluginID, date)
	for _, acc := range accounts {
		if ctx.Err() != nil {
			return
		}
		if !acc.Enabled || done[acc.ID] || m.attemptedToday(pluginID, acc.ID, date) {
			continue
		}
		if m.parkedUntil(pluginID, acc.ID) > m.now().Unix() {
			// Inside a cooldown that was already reported: the plugin is not
			// asked again before cooldown_until.
			continue
		}
		m.checkinAccount(ctx, pluginID, acc, date, true, true)
	}
}

// parkedUntil returns the cooldown deadline already reported for an account.
func (m *Manager) parkedUntil(pluginID, accountID string) int64 {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.parked[pluginID+"\x00"+accountID]
}

// parkUntil silences the scheduled passes for an account until deadline.
func (m *Manager) parkUntil(pluginID, accountID string, deadline int64) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.parked == nil {
		m.parked = map[string]int64{}
	}
	m.parked[pluginID+"\x00"+accountID] = deadline
}

// unpark forgets the cooldown note after a success or an explicit manual run.
func (m *Manager) unpark(pluginID, accountID string) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	delete(m.parked, pluginID+"\x00"+accountID)
}

// attemptedToday reports whether the scheduled retries for an account already
// failed today. Manual CheckinAll does not consult this guard.
func (m *Manager) attemptedToday(pluginID, accountID, date string) bool {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.attempted[pluginID+"\x00"+accountID] == date
}

func (m *Manager) markAttempted(pluginID, accountID, date string) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.attempted == nil {
		m.attempted = map[string]string{}
	}
	m.attempted[pluginID+"\x00"+accountID] = date
}

// checkinClock resolves the effective HH:MM for a plugin: host override,
// then the manifest default, then 09:00. Invalid values fall through to the
// next candidate.
func (m *Manager) checkinClock(st model.PluginStatus) (int, int) {
	var candidates []string
	if m.Plugins != nil {
		if override, ok := m.Plugins.CheckinTime(st.Manifest.ID); ok {
			candidates = append(candidates, override)
		}
	}
	candidates = append(candidates, st.Manifest.Checkin.DefaultTime, defaultCheckinTime)
	for _, value := range candidates {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if parsed, err := time.Parse("15:04", value); err == nil {
			return parsed.Hour(), parsed.Minute()
		}
	}
	parsed, _ := time.Parse("15:04", defaultCheckinTime)
	return parsed.Hour(), parsed.Minute()
}

// successfulAccounts returns the accounts with an ok/already record on date.
func (m *Manager) successfulAccounts(pluginID, date string) map[string]bool {
	out := map[string]bool{}
	if m.Store == nil {
		return out
	}
	records, err := m.Store.QueryCheckinRecords(pluginID, date, date)
	if err != nil {
		m.warn("query check-in records failed", map[string]any{
			"plugin": pluginID, "date": date, "error": err.Error(),
		})
		return out
	}
	for _, rec := range records {
		if isSuccess(rec.Result) {
			out[rec.AccountID] = true
		}
	}
	return out
}

// RunOnce performs a single pass synchronously. catchUp=true is the boot
// catch-up pass; false applies the time-of-day rules. It is exposed for the
// admin "run now" action and for tests.
func (m *Manager) RunOnce(ctx context.Context, catchUp bool) {
	m.runPass(ctx, catchUp)
}

// hasCapability reports whether the manifest lists capability cap.
func hasCapability(mf model.Manifest, capability string) bool {
	for _, c := range mf.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

func isSuccess(result string) bool {
	return result == model.CheckinOK || result == model.CheckinAlready
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// sleep waits for d, interruptible by ctx. Sleep is a test seam.
func (m *Manager) sleep(ctx context.Context, d time.Duration) error {
	if m.Sleep != nil {
		return m.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *Manager) info(msg string, fields map[string]any) {
	if m.Log != nil {
		m.Log.Info(Module, msg, fields)
	}
}

func (m *Manager) warn(msg string, fields map[string]any) {
	if m.Log != nil {
		m.Log.Warn(Module, msg, fields)
	}
}

func (m *Manager) error(msg string, fields map[string]any) {
	if m.Log != nil {
		m.Log.Error(Module, msg, fields)
	}
}

// notifyPluginOffline raises at most one plugin_offline notification per
// plugin per day so a dead plugin does not bubble every 30 seconds.
func (m *Manager) notifyPluginOffline(pluginID string, err error) {
	if m.Notify == nil {
		return
	}
	date := m.now().Format(dateLayout)
	m.stateMu.Lock()
	if m.offlineNotified == nil {
		m.offlineNotified = map[string]string{}
	}
	if m.offlineNotified[pluginID] == date {
		m.stateMu.Unlock()
		return
	}
	m.offlineNotified[pluginID] = date
	m.stateMu.Unlock()

	m.Notify.Notify(KindPluginOffline, "插件离线："+pluginID, err.Error())
}

// formatAccount renders an account for notification bodies.
func formatAccount(acc model.Account) string {
	label := strings.TrimSpace(acc.Label)
	if label == "" {
		return acc.ID
	}
	if acc.ID == "" || acc.ID == label {
		return label
	}
	return fmt.Sprintf("%s (%s)", label, acc.ID)
}
