package app

import (
	"context"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/egress"
	"aiworkgate/internal/hosts"
	"aiworkgate/internal/model"
	"aiworkgate/internal/plugins"
	"aiworkgate/internal/scheduler"
	"aiworkgate/internal/store"
)

// aAdapter exposes the AI Work 助手 (A) gateway to internal/gateway. The API
// key and the base URL are resolved on every call: A re-picks its loopback
// port on each launch, so the adapter follows the live discovered port
// instead of the startup snapshot held in baseURL.
type aAdapter struct {
	app     *App
	baseURL string
}

// BaseURL implements gateway.AUpstream.
func (ad *aAdapter) BaseURL() string {
	if ad == nil {
		return ""
	}
	if ad.app != nil {
		if _, _, url := ad.app.aEnsure(false); url != "" {
			return url
		}
	}
	return ad.baseURL
}

// APIKey implements gateway.AUpstream.
func (ad *aAdapter) APIKey() string {
	if ad == nil || ad.app == nil || ad.app.secrets == nil {
		return ""
	}
	key, _ := ad.app.secrets.GetAAPIKey()
	return key
}

// Models implements gateway.AUpstream.
func (ad *aAdapter) Models(ctx context.Context) ([]model.ModelInfo, error) {
	return hosts.NewClient(ad.BaseURL(), ad.APIKey()).Models(ctx)
}

// keyAdapter verifies client API keys against the DPAPI secret store and
// resolves their display names from SQLite.
type keyAdapter struct {
	app *App
	// orphanLogged makes the "hash without a store row" warning one-shot so a
	// looping client cannot flood the host log.
	orphanLogged sync.Once
}

// Verify implements gateway.KeyVerifier.
//
// Fail closed: a hash that has no enabled row in client_keys cannot have its
// scope/quota enforced, so it must not authenticate. That covers both a deleted
// row and a ListClientKeys error; the gateway would otherwise skip the
// disabled/scope/quota gates entirely.
func (k *keyAdapter) Verify(plain string) (id, name string, ok bool) {
	if k == nil || k.app == nil || k.app.secrets == nil {
		return "", "", false
	}
	id, ok = k.app.secrets.VerifyClientKey(plain)
	if !ok {
		return "", "", false
	}
	if k.app.store == nil {
		k.logOrphanedHash()
		return "", "", false
	}
	keys, err := k.app.store.ListClientKeys()
	if err != nil {
		k.logOrphanedHash()
		return "", "", false
	}
	key, found := findClientKey(keys, id)
	if !found || !key.Enabled {
		k.logOrphanedHash()
		return "", "", false
	}
	return id, key.Name, true
}

// logOrphanedHash records a rejected orphaned hash once. Neither the key id nor
// the hash is logged.
func (k *keyAdapter) logOrphanedHash() {
	k.orphanLogged.Do(func() {
		if k.app == nil || k.app.logger == nil {
			return
		}
		k.app.logger.Warn("gateway", "client key rejected: no enabled store row for its hash", map[string]any{})
	})
}

// Touch implements gateway.KeyVerifier.
func (k *keyAdapter) Touch(id string) {
	if k == nil || k.app == nil || k.app.store == nil || id == "" {
		return
	}
	_ = k.app.store.TouchClientKey(id, time.Now().Unix())
}

// apiLogSink adapts *logging.APILogger (Write returns an error) to the
// fire-and-forget gateway.APILogSink interface.
type apiLogSink struct {
	app *App
}

func (s apiLogSink) Write(rec model.APIRequestLog) {
	if s.app == nil || s.app.apiLog == nil {
		return
	}
	if err := s.app.apiLog.Write(rec); err != nil && s.app.logger != nil {
		s.app.logger.Warn("app", "api log write failed", map[string]any{"error": err.Error()})
	}
}

// pluginReloader notifies only running plugins after a rules file changed; a
// stopped plugin reads the file when it is next launched.
type pluginReloader struct {
	mgr *plugins.Manager
}

func (r pluginReloader) Reload(ctx context.Context, pluginID string) error {
	if r.mgr == nil {
		return nil
	}
	st, ok := r.mgr.Get(pluginID)
	if !ok || !st.Running {
		return nil
	}
	return r.mgr.Reload(ctx, pluginID)
}

// accountLister is the slice of the plugin runtime the merged account view
// needs. *plugins.Manager satisfies it; tests use a fake source.
type accountLister interface {
	List() []model.PluginStatus
	Accounts(ctx context.Context, pluginID string) ([]model.Account, error)
}

// schedulerSource is the slice of the plugin runtime the scheduling snapshot
// needs. *plugins.Manager satisfies it; tests use a fake.
type schedulerSource interface {
	List() []model.PluginStatus
	Accounts(ctx context.Context, pluginID string) ([]model.Account, error)
	Models(ctx context.Context, pluginID string) ([]model.ModelInfo, error)
}

// stickyStoreAdapter persists scheduler sticky bindings in sticky_bindings.
type stickyStoreAdapter struct{ store *store.Store }

func (s stickyStoreAdapter) ListSticky(scope string) ([]scheduler.StickyBinding, error) {
	if s.store == nil {
		return nil, nil
	}
	rows, err := s.store.ListStickyBindings(scope)
	if err != nil {
		return nil, err
	}
	out := make([]scheduler.StickyBinding, 0, len(rows))
	for _, row := range rows {
		out = append(out, scheduler.StickyBinding{
			Scope: row.Scope, Key: row.Key, PluginID: row.PluginID,
			AccountID: row.AccountID, ExpiresAt: row.ExpiresAt,
		})
	}
	return out, nil
}

func (s stickyStoreAdapter) SaveSticky(binding scheduler.StickyBinding) error {
	if s.store == nil {
		return nil
	}
	return s.store.UpsertStickyBinding(model.StickyBinding{
		Scope: binding.Scope, Key: binding.Key, PluginID: binding.PluginID,
		AccountID: binding.AccountID, ExpiresAt: binding.ExpiresAt,
	})
}

func (s stickyStoreAdapter) DeleteSticky(scope, key string) error {
	if s.store == nil {
		return nil
	}
	return s.store.DeleteStickyBinding(scope, key)
}

func (s stickyStoreAdapter) DeleteExpiredSticky(now int64) (int64, error) {
	if s.store == nil {
		return 0, nil
	}
	return s.store.DeleteExpiredStickyBindings(now)
}

// accountEntry is an Account plus its host overlay, its owning plugin id and,
// when assigned, the persisted account-level egress rule. It is the JSON shape
// of GET /api/accounts (design §8.1).
//
// StatusKind/CooldownUntil/CooldownReason are the host health state of plan
// 2026-10-21 §4. They shadow the plugin-reported scheduling hints of the same
// name on the embedded Account: the account page must show the host's verdict,
// not an optimistic plugin hint.
//
// State is the single health verdict the UI renders. The two sources used to
// disagree (plugin "expired" next to host "normal"), so the host now resolves
// one value and names where it came from:
//
//	invalid > expired            plugin credential verdict, decisive
//	host status_kind             an active host cooldown (rate limit, 5xx, ...)
//	rate_limited                 plugin hint, no host cooldown yet
//	normal / unknown             plugin status
//
// StateSource is "plugin", "host" or "" for normal.
type accountEntry struct {
	model.Account
	PluginID         string               `json:"plugin_id"`
	Alias            string               `json:"alias"`
	DisplayName      string               `json:"display_name"`
	GroupID          string               `json:"group_id"`
	Priority         int                  `json:"priority"`
	Weight           float64              `json:"weight"`
	EnabledOverride  *bool                `json:"enabled_override"`
	EnabledEffective bool                 `json:"enabled_effective"`
	Stale            bool                 `json:"stale"`
	Note             string               `json:"note,omitempty"`
	Egress           *model.AccountEgress `json:"egress,omitempty"`
	StatusKind       string               `json:"status_kind"`
	CooldownUntil    int64                `json:"cooldown_until"`
	CooldownReason   string               `json:"cooldown_reason"`
	State            string               `json:"state"`
	StateSource      string               `json:"state_source"`
	StateDetail      string               `json:"state_detail,omitempty"`
	EgressHealth     *egressHealthView    `json:"egress_health,omitempty"`
}

// egressHealthView is the account page's view of the assigned proxy. Status is
// ok / failed / unknown; At is when the evidence was observed, so the UI can say
// "3 分钟前" instead of presenting a stale exit IP as current.
type egressHealthView struct {
	Status     string `json:"status"`
	ProxyID    string `json:"proxy_id,omitempty"`
	ProxyLabel string `json:"proxy_label,omitempty"`
	At         int64  `json:"at,omitempty"`
	AgeSeconds int64  `json:"age_seconds"`
	Error      string `json:"error,omitempty"`
}

// accountMergeOptions carries the per-listing inputs of mergeAccountEntries.
type accountMergeOptions struct {
	Meta   map[string]model.AccountMeta
	Egress map[string]model.AccountEgress
	// Health returns egress evidence and whether the account has an explicit
	// proxy assignment (only those rows show an exit verdict).
	Health func(pluginID, accountID string) (egress.Evidence, bool)
	Stale  bool
	Now    int64
}

// accountState resolves the single health verdict of one account.
func accountState(acc model.Account, hostKind string, cooldownUntil int64, cooldownReason string, now int64) (state, source, detail string) {
	switch strings.TrimSpace(acc.Status) {
	case model.AccountInvalid:
		return model.AccountInvalid, "plugin", "凭据失效，需要重新登录"
	case model.AccountExpired:
		return model.AccountExpired, "plugin", "凭据已过期，需要重新登录"
	}
	kind := strings.TrimSpace(hostKind)
	if kind != "" && kind != model.StatusKindNormal && cooldownUntil > now {
		return kind, "host", strings.TrimSpace(cooldownReason)
	}
	switch strings.TrimSpace(acc.Status) {
	case model.AccountRateLimited:
		return model.AccountRateLimited, "plugin", "上游限流，稍后自动重试"
	case model.AccountOK:
		return model.StatusKindNormal, "", ""
	}
	return model.AccountUnknown, "plugin", "插件未报告可用状态"
}

// accountKey builds the (plugin, account) lookup key of an overlay row.
func accountKey(pluginID, accountID string) string { return pluginID + "\x00" + accountID }

// accountMetaMap indexes overlay rows by (plugin, account).
func accountMetaMap(rows []model.AccountMeta) map[string]model.AccountMeta {
	if len(rows) == 0 {
		return nil
	}
	out := make(map[string]model.AccountMeta, len(rows))
	for _, meta := range rows {
		out[accountKey(meta.PluginID, meta.AccountID)] = meta
	}
	return out
}

// mergeAccountEntries decorates plugin-reported accounts with the host
// overlay. DisplayName follows alias || label || id; enabled_effective is the
// plugin flag unless the user forced the account off.
func mergeAccountEntries(pluginID string, accounts []model.Account, opt accountMergeOptions) []accountEntry {
	now := opt.Now
	if now <= 0 {
		now = time.Now().Unix()
	}
	out := make([]accountEntry, 0, len(accounts))
	for _, acc := range accounts {
		entry := accountEntry{
			Account:          acc,
			PluginID:         pluginID,
			DisplayName:      acc.ID,
			Priority:         model.DefaultAccountPriority,
			Weight:           model.DefaultAccountWeight,
			EnabledEffective: acc.Enabled,
			Stale:            opt.Stale,
			// Healthy accounts report the explicit "normal" state; the stored
			// column stays NULL (plan 2026-10-21 §4 lists normal as a state).
			StatusKind: model.StatusKindNormal,
			// Keep the plugin-reported scheduling hint unless the host health
			// row below (the outer field shadows the embedded Account field)
			// has its own, later cooldown.
			CooldownUntil: acc.CooldownUntil,
		}
		if overlay, ok := opt.Meta[accountKey(pluginID, acc.ID)]; ok {
			entry.Alias = overlay.Alias
			entry.GroupID = overlay.GroupID
			entry.Note = overlay.Note
			entry.Priority = overlay.EffectivePriority()
			entry.Weight = overlay.EffectiveWeight()
			if overlay.StatusKind != "" {
				entry.StatusKind = overlay.StatusKind
			}
			if overlay.CooldownUntil > entry.CooldownUntil {
				entry.CooldownUntil = overlay.CooldownUntil
			}
			entry.CooldownReason = overlay.CooldownReason
			if overlay.EnabledOverride != nil {
				override := *overlay.EnabledOverride
				entry.EnabledOverride = &override
				entry.EnabledEffective = acc.Enabled && override
			}
		}
		entry.DisplayName = firstNonEmpty(entry.Alias, acc.Label, acc.ID)
		if assignment, ok := opt.Egress[acc.ID]; ok {
			assigned := assignment
			entry.Egress = &assigned
		}
		entry.State, entry.StateSource, entry.StateDetail = accountState(
			acc, entry.StatusKind, entry.CooldownUntil, entry.CooldownReason, now)
		if opt.Health != nil {
			if evidence, ok := opt.Health(pluginID, acc.ID); ok {
				view := egressHealthView{
					Status:     evidence.Status,
					ProxyID:    evidence.ProxyID,
					ProxyLabel: evidence.ProxyLabel,
					At:         evidence.At,
					Error:      evidence.Error,
				}
				if evidence.At > 0 && now > evidence.At {
					view.AgeSeconds = now - evidence.At
				}
				entry.EgressHealth = &view
			}
		}
		out = append(out, entry)
	}
	return out
}
