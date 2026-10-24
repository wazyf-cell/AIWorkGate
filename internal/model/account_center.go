package model

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// This file holds the S1 account-center types: the host-side overlay (alias,
// group, priority, weight, enable override), groups, the plugin account cache,
// pool membership/settings, model bindings and catalog, sticky bindings, the
// daily usage rollup and the audit trail.

// AccountMeta is the host overlay for one plugin-owned account. The overlay
// never contains credentials; it only decorates what the plugin reports.
// Pointer fields distinguish "not overridden" (NULL) from an explicit zero
// value, e.g. enabled_override NULL follows the plugin while false forces the
// account off.
type AccountMeta struct {
	PluginID        string   `json:"plugin_id"`
	AccountID       string   `json:"account_id"`
	Alias           string   `json:"alias,omitempty"`
	GroupID         string   `json:"group_id,omitempty"`
	EnabledOverride *bool    `json:"enabled_override,omitempty"`
	Priority        *int     `json:"priority,omitempty"`
	Weight          *float64 `json:"weight,omitempty"`
	Note            string   `json:"note,omitempty"`
	UpdatedAt       int64    `json:"updated_at,omitempty"`

	// Health four-tuple (plan 2026-10-21 §4), persisted in the account_meta
	// columns of the same names. They are written independently of the overlay
	// above by store.SetAccountHealth, so a failure never touches alias/group/
	// priority. StatusKind empty means healthy.
	StatusKind     string `json:"status_kind,omitempty"`
	CooldownUntil  int64  `json:"cooldown_until,omitempty"`
	CooldownReason string `json:"cooldown_reason,omitempty"`
	FailureCount   int    `json:"failure_count,omitempty"`
}

// DefaultAccountPriority and DefaultAccountWeight are the overlay defaults used
// when neither the plugin nor the user supplied a value.
const (
	DefaultAccountPriority = 100
	DefaultAccountWeight   = 1.0
)

// EffectivePriority resolves the scheduling priority: overlay first, then the
// documented default.
func (m AccountMeta) EffectivePriority() int {
	if m.Priority == nil {
		return DefaultAccountPriority
	}
	return *m.Priority
}

// EffectiveWeight resolves the scheduling weight: overlay first, then the
// documented default.
func (m AccountMeta) EffectiveWeight() float64 {
	if m.Weight == nil {
		return DefaultAccountWeight
	}
	return *m.Weight
}

// AccountGroup is a display-only account group (§D89: one group per account).
type AccountGroup struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Color        string `json:"color,omitempty"`
	Sort         int    `json:"sort"`
	CreatedAt    int64  `json:"created_at,omitempty"`
	AccountCount int    `json:"account_count"`
}

// AccountRef identifies one plugin-owned account.
type AccountRef struct {
	PluginID  string `json:"plugin_id"`
	AccountID string `json:"account_id"`
}

// PoolMember marks an account as an explicitly selected member of a plugin's
// scheduling pool. No row means the account participates by default.
type PoolMember struct {
	PluginID  string `json:"plugin_id"`
	AccountID string `json:"account_id"`
	Enabled   bool   `json:"enabled"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

// PoolSettings is the per-plugin scheduling configuration. The row with
// PluginID "__global__" holds the host-wide defaults.
const GlobalPoolID = "__global__"

type PoolSettings struct {
	PluginID            string `json:"plugin_id"`
	UpstreamEnabled     bool   `json:"upstream_enabled"`
	Strategy            string `json:"strategy,omitempty"`
	PoolPriority        int    `json:"pool_priority"`
	AccountConcurrency  int    `json:"account_concurrency"`
	PoolStickyTTL       int    `json:"pool_sticky_ttl"`
	ConvStickyTTL       int    `json:"conv_sticky_ttl"`
	HedgeMS             int    `json:"hedge_ms"`
	DefaultReasoning    string `json:"default_reasoning,omitempty"`
	BackgroundDowngrade bool   `json:"background_downgrade"`
	LongctxDowngrade    bool   `json:"longctx_downgrade"`
	ToolExec            bool   `json:"tool_exec"`
	ModelSyncTime       string `json:"model_sync_time,omitempty"`
	UpdatedAt           int64  `json:"updated_at,omitempty"`
}

// ModelBinding maps a logical (client-facing) model name to an ordered list of
// physical sources.
type ModelBinding struct {
	LogicalModel string             `json:"logical_model"`
	DisplayName  string             `json:"display_name,omitempty"`
	UpdatedAt    int64              `json:"updated_at,omitempty"`
	Items        []ModelBindingItem `json:"items"`
}

// ModelBindingItem is one physical source of a logical model. Position 0 is
// the most preferred source.
type ModelBindingItem struct {
	LogicalModel string `json:"logical_model"`
	Position     int    `json:"position"`
	SourcePlugin string `json:"source_plugin"`
	SourceModel  string `json:"source_model"`
}

// ModelCatalogEntry is one model reported by a plugin's catalog sync.
type ModelCatalogEntry struct {
	PluginID      string   `json:"plugin_id"`
	ModelID       string   `json:"model_id"`
	DisplayName   string   `json:"display_name,omitempty"`
	Vendor        string   `json:"vendor,omitempty"`
	Multiplier    float64  `json:"multiplier"`
	Thinking      []string `json:"thinking,omitempty"`
	ContextLength int      `json:"context_length,omitempty"`
	Vision        bool     `json:"vision"`
	Free          bool     `json:"free"`
	// Enabled controls whether the gateway exposes this model. A catalog row
	// missing from the table means "not synced yet" (everything is exposed).
	Enabled  bool  `json:"enabled"`
	SyncedAt int64 `json:"synced_at,omitempty"`
}

// Sticky binding scopes.
const (
	StickyScopeConversation = "conv"
	StickyScopePool         = "pool"
)

// StickyBinding pins a conversation or pool key to one account until it
// expires. It is persisted so a host restart does not reshuffle sessions.
type StickyBinding struct {
	Scope     string `json:"scope"`
	Key       string `json:"key"`
	PluginID  string `json:"plugin_id"`
	AccountID string `json:"account_id"`
	ExpiresAt int64  `json:"expires_at"`
}

// UsageDailyRow is one day's aggregate for one (plugin, account, model).
type UsageDailyRow struct {
	Day              string  `json:"day"`
	PluginID         string  `json:"plugin_id"`
	AccountID        string  `json:"account_id"`
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Credits          float64 `json:"credits"`
	LatencyMSSum     int64   `json:"latency_ms_sum"`
	TTFBMSSum        int64   `json:"ttfb_ms_sum"`
	TTFBCount        int64   `json:"ttfb_count"`
}

// AuditEntry is one management operation recorded in audit_log.
type AuditEntry struct {
	ID       int64  `json:"id"`
	TS       string `json:"ts"`
	Category string `json:"category,omitempty"`
	Action   string `json:"action,omitempty"`
	Target   string `json:"target,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Result   string `json:"result,omitempty"`
}

// Audit categories (§5.3 event taxonomy).
const (
	AuditAccount   = "account"
	AuditGroup     = "group"
	AuditPool      = "pool"
	AuditConfig    = "config"
	AuditPlugin    = "plugin"
	AuditScheduler = "scheduler"
)

// ------------------------------------------------------------------ drafts

// AccountDraft is the body of POST /api/accounts/save. A nil optional pointer
// means "reset to the default" (alias cleared, priority/weight/override NULL,
// account ungrouped), which is what a full-draft save from the UI sends.
type AccountDraft struct {
	Accounts      []AccountDraftItem `json:"accounts"`
	Groups        []GroupDraft       `json:"groups"`
	DeletedGroups []string           `json:"deleted_groups"`
	Memberships   []MembershipDraft  `json:"memberships"`
}

// AccountDraftItem is one account row of a draft. Optional fields are
// presence-tracked: an absent field leaves the stored overlay value alone,
// while an explicit JSON null (or an empty alias/group) clears it.
type AccountDraftItem struct {
	PluginID        string       `json:"plugin_id"`
	AccountID       string       `json:"account_id"`
	Alias           *string      `json:"alias"`
	GroupID         *string      `json:"group_id"`
	Priority        *int         `json:"priority"`
	Weight          *float64     `json:"weight"`
	EnabledOverride *bool        `json:"enabled_override"`
	Egress          *EgressDraft `json:"egress"`

	aliasSet, groupIDSet, prioritySet, weightSet, overrideSet, egressSet bool
}

// accountDraftWire decodes the optional fields as raw JSON so presence and an
// explicit null can be told apart.
type accountDraftWire struct {
	PluginID        string          `json:"plugin_id"`
	AccountID       string          `json:"account_id"`
	Alias           json.RawMessage `json:"alias"`
	GroupID         json.RawMessage `json:"group_id"`
	Priority        json.RawMessage `json:"priority"`
	Weight          json.RawMessage `json:"weight"`
	EnabledOverride json.RawMessage `json:"enabled_override"`
	Egress          json.RawMessage `json:"egress"`
}

// UnmarshalJSON records which optional fields the client actually sent.
func (item *AccountDraftItem) UnmarshalJSON(data []byte) error {
	var wire accountDraftWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*item = AccountDraftItem{
		PluginID:    wire.PluginID,
		AccountID:   wire.AccountID,
		aliasSet:    len(wire.Alias) > 0,
		groupIDSet:  len(wire.GroupID) > 0,
		prioritySet: len(wire.Priority) > 0,
		weightSet:   len(wire.Weight) > 0,
		overrideSet: len(wire.EnabledOverride) > 0,
		egressSet:   len(wire.Egress) > 0,
	}
	assignString := func(raw json.RawMessage, dst **string) error {
		if len(raw) == 0 || isJSONNull(raw) {
			return nil
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		*dst = &value
		return nil
	}
	if err := assignString(wire.Alias, &item.Alias); err != nil {
		return fmt.Errorf("model: alias: %w", err)
	}
	if err := assignString(wire.GroupID, &item.GroupID); err != nil {
		return fmt.Errorf("model: group_id: %w", err)
	}
	if item.prioritySet && !isJSONNull(wire.Priority) {
		var value int
		if err := json.Unmarshal(wire.Priority, &value); err != nil {
			return fmt.Errorf("model: priority: %w", err)
		}
		item.Priority = &value
	}
	if item.weightSet && !isJSONNull(wire.Weight) {
		var value float64
		if err := json.Unmarshal(wire.Weight, &value); err != nil {
			return fmt.Errorf("model: weight: %w", err)
		}
		item.Weight = &value
	}
	if item.overrideSet && !isJSONNull(wire.EnabledOverride) {
		var value bool
		if err := json.Unmarshal(wire.EnabledOverride, &value); err != nil {
			return fmt.Errorf("model: enabled_override: %w", err)
		}
		item.EnabledOverride = &value
	}
	if item.egressSet && !isJSONNull(wire.Egress) {
		var value EgressDraft
		if err := json.Unmarshal(wire.Egress, &value); err != nil {
			return fmt.Errorf("model: egress: %w", err)
		}
		item.Egress = &value
	}
	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// AliasSet reports whether the draft carried an alias (value or null).
func (item AccountDraftItem) AliasSet() bool { return item.aliasSet || item.Alias != nil }

// GroupIDSet reports whether the draft carried a group reference (value or null).
func (item AccountDraftItem) GroupIDSet() bool { return item.groupIDSet || item.GroupID != nil }

// PrioritySet reports whether the draft carried a priority (value or null).
func (item AccountDraftItem) PrioritySet() bool { return item.prioritySet || item.Priority != nil }

// WeightSet reports whether the draft carried a weight (value or null).
func (item AccountDraftItem) WeightSet() bool { return item.weightSet || item.Weight != nil }

// EnabledOverrideSet reports whether the draft carried an enable override.
func (item AccountDraftItem) EnabledOverrideSet() bool {
	return item.overrideSet || item.EnabledOverride != nil
}

// EgressSet reports whether the draft carried an egress assignment (value or
// null to clear it).
func (item AccountDraftItem) EgressSet() bool { return item.egressSet || item.Egress != nil }

// EgressDraft is the draft form of an account egress assignment. Mode
// "default" (or empty) removes the assignment.
type EgressDraft struct {
	Mode    string `json:"mode"`
	ProxyID string `json:"proxy_id"`
}

// GroupDraft is one group row of a draft. An empty ID asks the host to mint a
// new group id.
type GroupDraft struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
	Sort  *int   `json:"sort,omitempty"`
}

// MembershipDraft toggles explicit pool membership. Member=false removes the
// explicit row, returning the account to the "all credential accounts" default.
type MembershipDraft struct {
	PluginID  string `json:"plugin_id"`
	AccountID string `json:"account_id"`
	Member    bool   `json:"member"`
}

// SaveApplied reports what POST /api/accounts/save wrote.
type SaveApplied struct {
	Accounts    int `json:"accounts"`
	Groups      int `json:"groups"`
	Memberships int `json:"memberships"`
}

// ------------------------------------------------------------------ bulk

// Bulk action names accepted by POST /api/accounts/bulk.
const (
	BulkCheckin = "checkin"
	BulkRefresh = "refresh"
	BulkEnable  = "enable"
	BulkDisable = "disable"
	BulkBindIP  = "bind_ip"
	BulkGroup   = "group"
	BulkDelete  = "delete"
)

// BulkParams are the optional parameters of a bulk action.
type BulkParams struct {
	ProxyID string `json:"proxy_id,omitempty"`
	Mode    string `json:"mode,omitempty"`
	GroupID string `json:"group_id,omitempty"`
}

// BulkResult is the per-item outcome of a bulk action.
type BulkResult struct {
	PluginID  string `json:"plugin_id"`
	AccountID string `json:"account_id"`
	OK        bool   `json:"ok"`
	Message   string `json:"message,omitempty"`
}

// ------------------------------------------------------------------ login state

// LoginState is one row of GET /api/accounts/login-state.
type LoginState struct {
	PluginID        string   `json:"plugin_id"`
	Name            string   `json:"name"`
	APILevel        int      `json:"api_level"`
	Methods         []string `json:"methods"`
	ReadsLocalState bool     `json:"reads_local_state"`
	RequiresApp     bool     `json:"requires_app"`
	AutoRefresh     bool     `json:"auto_refresh"`
	Source          string   `json:"source"`
	Notes           string   `json:"notes,omitempty"`
	CheckedAt       int64    `json:"checked_at"`
}

// Login state sources.
const (
	LoginSourceManifest = "manifest"
	LoginSourceDerived  = "derived"
)

// IdentityDetails is the response of POST /api/accounts/identity/details.
// MachineID/DeviceID/SessionID are masked unless reveal was requested.
type IdentityDetails struct {
	PluginID  string    `json:"plugin_id"`
	AccountID string    `json:"account_id"`
	Label     string    `json:"label,omitempty"`
	Identity  *Identity `json:"identity,omitempty"`
	Masked    bool      `json:"masked"`
	UpdatedAt int64     `json:"updated_at,omitempty"`
}
