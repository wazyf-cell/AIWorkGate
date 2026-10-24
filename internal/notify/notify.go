// Package notify delivers operational events to the user's desktop (a Windows
// toast) and, optionally, to their phone or chat through one of four channels.
// It is a port of the notification channel layer of the MIT-licensed reference
// product A:
//
//   - src-tauri/src/notify.rs:11-13   (channel failures are logged, never fatal)
//   - src-tauri/src/notify.rs:115-137 (Server酱 form shape)
//   - src-tauri/src/notify.rs:93-112  (企业微信 group robot text shape)
//   - src-tauri/src/notify.rs:57-90   (Bark title/body push)
//
// Unlike A this host never blocks a check-in run on a notification: Emit only
// enqueues, a single worker owns the HTTP calls, and every call is bounded by
// a five-second context.
package notify

import (
	"context"
	"net/url"
	"strings"
	"time"
)

// Event is one user-visible notification. At is the moment the host observed
// the event, not the moment it was delivered.
type Event struct {
	Title string    `json:"title"`
	Body  string    `json:"body"`
	Kind  string    `json:"kind"`
	At    time.Time `json:"at"`
}

// Sender delivers one event through one configured channel.
type Sender interface {
	Send(ctx context.Context, event Event) error
}

// Channels accepted by the settings page and the config file.
const (
	ChannelGeneric    = "generic"
	ChannelBark       = "bark"
	ChannelServerChan = "serverchan"
	ChannelWeCom      = "wecom"
)

// Event kinds. The set is deliberately small: every kind maps to one message
// the user can act on (A keeps the same event gate in push_notify, see
// src-tauri/src/commands/workbuddy/common.rs:234-250).
const (
	KindCheckinFail = "checkin_fail"
	KindCheckinDone = "checkin_done"
	KindAccountDead = "account_dead"
	KindQuotaLow    = "quota_low"
	KindTest        = "test"
)

// Channels lists the accepted channel identifiers in UI order.
var Channels = []string{ChannelGeneric, ChannelBark, ChannelServerChan, ChannelWeCom}

// Kinds lists the event kinds a user may subscribe to.
var Kinds = []string{KindCheckinFail, KindCheckinDone, KindAccountDead, KindQuotaLow}

// URLSecretKey is the secrets.Store extra key that holds the webhook URL. The
// URL is a credential: it never appears in config.json, logs or API responses.
const URLSecretKey = "notify.url"

// Timeout bounds one delivery attempt. A uses a 10s ureq timeout
// (src-tauri/src/notify.rs:75-77); this host keeps the shorter 5s budget so a
// slow endpoint cannot pin the worker.
const Timeout = 5 * time.Second

// DefaultMinInterval is the default per-(kind, plugin) throttle.
const DefaultMinInterval = 10 * time.Minute

// ValidChannel reports whether name is an accepted channel identifier.
func ValidChannel(name string) bool {
	switch name {
	case ChannelGeneric, ChannelBark, ChannelServerChan, ChannelWeCom:
		return true
	}
	return false
}

// ValidKind reports whether kind is an accepted subscription.
func ValidKind(kind string) bool {
	switch kind {
	case KindCheckinFail, KindCheckinDone, KindAccountDead, KindQuotaLow:
		return true
	}
	return false
}

// NormalizeChannel folds unknown or empty identifiers to generic, matching the
// config normalization rule.
func NormalizeChannel(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if !ValidChannel(name) {
		return ChannelGeneric
	}
	return name
}

// Settings is the runtime view of config.NotifyConfig. The URL is deliberately
// absent: it is read from the DPAPI secret store on every delivery.
type Settings struct {
	Enabled     bool
	Channel     string
	Events      []string
	MinInterval time.Duration
}

// Allows reports whether kind is subscribed.
func (s Settings) Allows(kind string) bool {
	for _, event := range s.Events {
		if event == kind {
			return true
		}
	}
	return false
}

// RedactURL masks everything after the host so a webhook path, device key or
// query token can never reach a log line or an API response.
func RedactURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "（通知地址无效）"
	}
	return parsed.Scheme + "://" + parsed.Host + "/***"
}
