package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aiworkgate/internal/safego"
)

// Logger is the host log contract (internal/logging.Logger satisfies it).
type Logger interface {
	Info(module, msg string, fields map[string]any)
	Warn(module, msg string, fields map[string]any)
}

// DispatcherOptions wires a Dispatcher.
type DispatcherOptions struct {
	// Endpoint returns the stored webhook URL (secrets.Store extra). It is
	// called on every delivery so a settings change needs no restart.
	Endpoint func() string
	// Log receives delivery outcomes. Error text is already redacted.
	Log Logger
	// Settings is the initial configuration.
	Settings Settings
	// Now is a test seam; nil selects time.Now.
	Now func() time.Time
	// QueueSize bounds the pending deliveries; 0 selects 64.
	QueueSize int
}

// Dispatcher applies the user's notification policy and owns the delivery
// worker. Emit is non-blocking: a check-in run must never wait for a webhook or
// for a desktop toast.
//
// Policy order (A push_notify, src-tauri/src/commands/workbuddy/common.rs:234-250):
// master switch, per-event subscription, then a per-(kind, plugin) interval
// throttle so a flapping plugin cannot drain the user's daily push quota.
//
// A subscribed event is always shown as a desktop toast. When a webhook URL is
// configured the same event is also pushed to it; when it is not, the toast is
// the delivery and its absence is not an error.
type Dispatcher struct {
	endpoint func() string
	log      Logger
	now      func() time.Time

	mu       sync.Mutex
	settings Settings
	throttle map[string]time.Time
	doneDay  map[string]string

	lastSent  time.Time
	lastError string

	queue chan delivery
	stop  chan struct{}
	done  chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once
	doneOnce  sync.Once
	started   atomic.Bool
}

type delivery struct {
	channel  string
	endpoint string
	event    Event
}

// NewDispatcher returns a dispatcher that has not started delivering yet.
func NewDispatcher(opts DispatcherOptions) *Dispatcher {
	size := opts.QueueSize
	if size <= 0 {
		size = 64
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	d := &Dispatcher{
		endpoint: opts.Endpoint,
		log:      opts.Log,
		now:      now,
		throttle: map[string]time.Time{},
		doneDay:  map[string]string{},
		queue:    make(chan delivery, size),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	d.Update(opts.Settings)
	return d
}

// Start launches the delivery worker. It is idempotent.
func (d *Dispatcher) Start() {
	if d == nil {
		return
	}
	d.startOnce.Do(func() {
		d.started.Store(true)
		safego.Go(d.log, "notifyWorker", func() { d.worker() })
	})
}

// Stop ends the worker. Pending deliveries are dropped: the host is going
// away and a notification is not worth delaying shutdown.
func (d *Dispatcher) Stop() {
	if d == nil {
		return
	}
	d.stopOnce.Do(func() {
		close(d.stop)
		// A dispatcher that was never started has no worker to close done.
		if !d.started.Load() {
			d.doneOnce.Do(func() { close(d.done) })
		}
	})
	<-d.done
}

// Update replaces the policy. The zero interval keeps the default throttle,
// matching config normalization.
func (d *Dispatcher) Update(settings Settings) {
	if d == nil {
		return
	}
	settings.Channel = NormalizeChannel(settings.Channel)
	if settings.MinInterval <= 0 {
		settings.MinInterval = DefaultMinInterval
	}
	settings.Events = append([]string(nil), settings.Events...)

	d.mu.Lock()
	d.settings = settings
	d.mu.Unlock()
}

// Settings returns a copy of the current policy.
func (d *Dispatcher) Settings() Settings {
	if d == nil {
		return Settings{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.settings
	out.Events = append([]string(nil), d.settings.Events...)
	return out
}

// State reports the last successful delivery and the last (redacted) failure.
func (d *Dispatcher) State() (lastSent time.Time, lastError string) {
	if d == nil {
		return time.Time{}, ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastSent, d.lastError
}

// Emit applies the policy and enqueues one event. pluginID scopes the throttle
// (and the once-per-day checkin_done dedupe); pass "" for host-wide events.
func (d *Dispatcher) Emit(pluginID string, event Event) {
	if d == nil {
		return
	}
	settings := d.Settings()
	if !settings.Enabled {
		return
	}
	if event.Kind != KindTest && !settings.Allows(event.Kind) {
		return
	}

	endpoint := ""
	if d.endpoint != nil {
		endpoint = strings.TrimSpace(d.endpoint())
	}

	now := d.now()
	if event.At.IsZero() {
		event.At = now
	}

	d.mu.Lock()
	throttleKey := event.Kind + "\x00" + pluginID
	if settings.MinInterval > 0 {
		if last, ok := d.throttle[throttleKey]; ok && now.Sub(last) < settings.MinInterval {
			d.mu.Unlock()
			return
		}
		d.throttle[throttleKey] = now
	}
	if event.Kind == KindCheckinDone {
		// One "check-in done" per plugin per local day. A restart may re-send
		// once; the alternative (persisting the marker) is not worth a store
		// write for a courtesy message.
		day := now.Format("2006-01-02")
		if d.doneDay[pluginID] == day {
			d.mu.Unlock()
			return
		}
		d.doneDay[pluginID] = day
	}
	d.mu.Unlock()

	select {
	case d.queue <- delivery{channel: settings.Channel, endpoint: endpoint, event: event}:
	default:
		// Bounded queue: dropping is preferable to blocking the check-in run.
		d.warn("notification dropped: queue full", map[string]any{"kind": event.Kind})
	}
}

// Test sends one event through the current channel, bypassing the master
// switch and the subscription filter. A desktop toast is always raised, so the
// action gives feedback even before a webhook URL is configured; a missing URL
// is therefore not an error. Errors are already redacted.
func (d *Dispatcher) Test(ctx context.Context) error {
	if d == nil {
		return errors.New("notify: 通知服务未初始化")
	}
	settings := d.Settings()
	endpoint := ""
	if d.endpoint != nil {
		endpoint = strings.TrimSpace(d.endpoint())
	}
	event := Event{
		Title: "AIWorkGate 测试通知",
		Body:  "这是一条测试通知，收到说明通知渠道配置可用。",
		Kind:  KindTest,
		At:    d.now(),
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var webhookErr error
	if endpoint != "" {
		sender, err := NewSender(settings.Channel, endpoint)
		if err != nil {
			d.setError(err)
			return err
		}
		webhookErr = sender.Send(ctx, event)
		if webhookErr != nil {
			d.setError(webhookErr)
			d.warn("test notification failed", map[string]any{"channel": settings.Channel, "error": webhookErr.Error()})
		}
	}

	toastErr := d.sendToast(event)
	if toastErr != nil {
		d.warn("desktop toast failed", map[string]any{"kind": event.Kind, "error": toastErr.Error()})
	}
	if webhookErr != nil {
		return webhookErr
	}
	if toastErr != nil && endpoint == "" {
		// Nothing was delivered: the toast is the whole test.
		d.setError(toastErr)
		return toastErr
	}
	d.setSent(d.now())
	if toastErr != nil {
		// The webhook delivery succeeded; record the toast failure but do not
		// turn a successful test into an error.
		d.setError(toastErr)
	}
	d.info("test notification sent", map[string]any{"channel": settings.Channel, "desktop_toast": true})
	return nil
}

func (d *Dispatcher) worker() {
	defer d.doneOnce.Do(func() { close(d.done) })
	for {
		select {
		case <-d.stop:
			return
		case job := <-d.queue:
			d.deliver(job)
		}
	}
}

func (d *Dispatcher) deliver(job delivery) {
	webhookFailed := false
	if job.endpoint != "" {
		sender, err := NewSender(job.channel, job.endpoint)
		if err != nil {
			webhookFailed = true
			d.setError(err)
			d.warn("notification send failed", map[string]any{
				"kind": job.event.Kind, "channel": job.channel, "error": err.Error(),
			})
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), Timeout)
			err = sender.Send(ctx, job.event)
			cancel()
			if err != nil {
				webhookFailed = true
				d.setError(err)
				d.warn("notification send failed", map[string]any{
					"kind": job.event.Kind, "channel": job.channel, "error": err.Error(),
				})
			}
		}
	}

	if job.endpoint != "" && !webhookFailed {
		d.setSent(d.now())
		d.info("notification sent", map[string]any{"kind": job.event.Kind, "channel": job.channel})
	}

	// The desktop toast runs alongside the webhook and is not retried: a toast
	// backend failure is reported through State but never fails the delivery.
	if toastErr := d.sendToast(job.event); toastErr != nil {
		if !webhookFailed {
			// Keep the webhook error when there is one: it is the actionable one.
			d.setError(toastErr)
		}
		d.warn("desktop toast failed", map[string]any{
			"kind": job.event.Kind, "error": toastErr.Error(),
		})
		return
	}

	if job.endpoint == "" {
		// No webhook configured: the toast is the delivery.
		d.setSent(d.now())
		d.info("desktop toast shown", map[string]any{"kind": job.event.Kind})
	}
}

func (d *Dispatcher) setSent(at time.Time) {
	d.mu.Lock()
	d.lastSent = at
	d.lastError = ""
	d.mu.Unlock()
}

func (d *Dispatcher) setError(err error) {
	if err == nil {
		return
	}
	d.mu.Lock()
	d.lastError = err.Error()
	d.mu.Unlock()
}

func (d *Dispatcher) info(msg string, fields map[string]any) {
	if d.log != nil {
		d.log.Info("notify", msg, fields)
	}
}

func (d *Dispatcher) warn(msg string, fields map[string]any) {
	if d.log != nil {
		d.log.Warn("notify", msg, fields)
	}
}
