package webui

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"aiworkgate/internal/model"
)

// S1 account-center limits from design §5.1.
const (
	maxAliasRunes      = 64
	minAccountPriority = 0
	maxAccountPriority = 999
	minAccountWeight   = 0.1
	maxAccountWeight   = 100.0
	maxBulkItems       = 1000
	maxDraftItems      = 5000
)

// bulkActions is the accepted POST /api/accounts/bulk action set. "egress" is
// accepted as an alias of "bind_ip" for the account center's bulk bar.
var bulkActions = map[string]bool{
	model.BulkCheckin: true,
	model.BulkRefresh: true,
	model.BulkEnable:  true,
	model.BulkDisable: true,
	model.BulkBindIP:  true,
	model.BulkGroup:   true,
	model.BulkDelete:  true,
	"egress":          true,
}

// normalizeBulkAction folds the UI alias onto the canonical action name.
func normalizeBulkAction(action string) string {
	if action == "egress" {
		return model.BulkBindIP
	}
	return action
}

// writeAccountError maps account-center adapter errors onto the admin API.
func writeAccountError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
	case errors.Is(err, ErrAccountNotFound):
		writeError(w, http.StatusNotFound, err.Error(), "not_found")
	default:
		writePluginCallError(w, err)
	}
}

// validAlias reports whether an alias fits the 64-rune overlay limit.
func validAlias(alias string) bool {
	return utf8.RuneCountInString(alias) <= maxAliasRunes
}

// normalizeEgress validates one draft egress assignment the same way
// PUT /api/accounts/egress does: empty/default drops it, direct ignores the
// proxy, proxy needs a known enabled proxy.
func (s *server) normalizeEgress(mode, proxyID string) (string, string, bool) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	proxyID = strings.TrimSpace(proxyID)
	switch mode {
	case "", "default":
		return "default", "", true
	case model.EgressDirect:
		return model.EgressDirect, "", true
	case model.EgressProxy:
		if proxyID == "" {
			return "", "", false
		}
		proxy, ok := s.findProxy(proxyID)
		if !ok || !proxy.Enabled {
			return "", "", false
		}
		return model.EgressProxy, proxyID, true
	default:
		return "", "", false
	}
}

// groupIDSet loads every stored group id together with the ids created by the
// same draft, so an account may reference a group that is created in the call.
func (s *server) groupIDSet(extra []model.GroupDraft) (map[string]bool, error) {
	groups, err := s.opts.Groups()
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(groups)+len(extra))
	for _, group := range groups {
		if id := strings.TrimSpace(group.ID); id != "" {
			ids[id] = true
		}
	}
	for _, group := range extra {
		if id := strings.TrimSpace(group.ID); id != "" {
			ids[id] = true
		}
	}
	return ids, nil
}

// handleAccountCheckin runs one immediate check-in and records it.
func (s *server) handleAccountCheckin(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.AccountCheckin == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		PluginID  string `json:"plugin_id"`
		AccountID string `json:"account_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	pluginID := strings.TrimSpace(body.PluginID)
	accountID := strings.TrimSpace(body.AccountID)
	if pluginID == "" || accountID == "" {
		writeError(w, http.StatusBadRequest, "plugin_id and account_id are required", "bad_request")
		return
	}
	if !s.pluginForCall(w, pluginID) {
		return
	}
	result, err := s.opts.AccountCheckin(pluginID, accountID)
	if err != nil {
		writeAccountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}

// handleAccountRename stores the host overlay alias (empty clears it).
func (s *server) handleAccountRename(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.AccountRename == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		PluginID  string `json:"plugin_id"`
		AccountID string `json:"account_id"`
		Alias     string `json:"alias"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	pluginID := strings.TrimSpace(body.PluginID)
	accountID := strings.TrimSpace(body.AccountID)
	alias := strings.TrimSpace(body.Alias)
	if pluginID == "" || accountID == "" {
		writeError(w, http.StatusBadRequest, "plugin_id and account_id are required", "bad_request")
		return
	}
	if _, ok := s.pluginStatus(pluginID); !ok {
		writeError(w, http.StatusNotFound, "plugin not found: "+pluginID, "not_found")
		return
	}
	if !validAlias(alias) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("alias must be at most %d characters", maxAliasRunes), "invalid_alias")
		return
	}
	if err := s.opts.AccountRename(pluginID, accountID, alias); err != nil {
		writeAccountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAccountBulk runs one immediate action over many accounts.
func (s *server) handleAccountBulk(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.AccountBulk == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		Action string             `json:"action"`
		Items  []model.AccountRef `json:"items"`
		Params model.BulkParams   `json:"params"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))
	if !bulkActions[action] {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("action must be checkin, refresh, enable, disable, bind_ip, group or delete, got %q", action), "bad_request")
		return
	}
	action = normalizeBulkAction(action)
	if len(body.Items) == 0 {
		writeError(w, http.StatusBadRequest, "items must not be empty", "bad_request")
		return
	}
	if len(body.Items) > maxBulkItems {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("at most %d items per bulk request", maxBulkItems), "bad_request")
		return
	}
	for _, item := range body.Items {
		if strings.TrimSpace(item.PluginID) == "" || strings.TrimSpace(item.AccountID) == "" {
			writeError(w, http.StatusBadRequest, "every item needs plugin_id and account_id", "bad_request")
			return
		}
	}

	params := model.BulkParams{
		ProxyID: strings.TrimSpace(body.Params.ProxyID),
		Mode:    strings.ToLower(strings.TrimSpace(body.Params.Mode)),
		GroupID: strings.TrimSpace(body.Params.GroupID),
	}
	switch action {
	case model.BulkBindIP:
		mode, proxyID, ok := s.normalizeEgress(params.Mode, params.ProxyID)
		if !ok {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("mode must be direct, proxy or default with a known enabled proxy, got %q", params.Mode), "invalid_egress")
			return
		}
		params.Mode, params.ProxyID = mode, proxyID
	case model.BulkGroup:
		if params.GroupID == "" {
			writeError(w, http.StatusBadRequest, "params.group_id is required for action group", "bad_request")
			return
		}
		if s.opts.Groups == nil {
			s.unavailable(w)
			return
		}
		ids, err := s.groupIDSet(nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list groups: "+err.Error(), "internal_error")
			return
		}
		if !ids[params.GroupID] {
			writeError(w, http.StatusBadRequest, "unknown group: "+params.GroupID, "invalid_request")
			return
		}
	}

	results := s.opts.AccountBulk(action, body.Items, params)
	if results == nil {
		results = []model.BulkResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleAccountSave applies the whole account-center draft atomically. Every
// validation runs before the host callback, so a rejected draft applies
// nothing.
func (s *server) handleAccountSave(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.AccountSave == nil || s.opts.Plugins == nil || s.opts.Groups == nil {
		s.unavailable(w)
		return
	}
	var draft model.AccountDraft
	if !decodeJSON(w, r, &draft) {
		return
	}
	if len(draft.Accounts) > maxDraftItems || len(draft.Groups) > maxDraftItems ||
		len(draft.Memberships) > maxDraftItems || len(draft.DeletedGroups) > maxDraftItems {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("at most %d entries per draft section", maxDraftItems), "bad_request")
		return
	}

	groupIDs, err := s.groupIDSet(draft.Groups)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list groups: "+err.Error(), "internal_error")
		return
	}
	deleted := make(map[string]bool, len(draft.DeletedGroups))
	for _, id := range draft.DeletedGroups {
		id = strings.TrimSpace(id)
		if id != "" {
			deleted[id] = true
		}
	}

	// Groups: names are required; the store enforces uniqueness.
	for i := range draft.Groups {
		group := &draft.Groups[i]
		group.ID = strings.TrimSpace(group.ID)
		group.Name = strings.TrimSpace(group.Name)
		group.Color = strings.TrimSpace(group.Color)
		if group.Name == "" {
			writeError(w, http.StatusBadRequest, "every group needs a name", "invalid_group")
			return
		}
		if !validAlias(group.Name) {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("group name must be at most %d characters", maxAliasRunes), "invalid_group")
			return
		}
		if group.ID != "" && deleted[group.ID] {
			writeError(w, http.StatusBadRequest,
				"group "+group.ID+" cannot be both saved and deleted", "invalid_group")
			return
		}
	}

	for i := range draft.Accounts {
		item := &draft.Accounts[i]
		item.PluginID = strings.TrimSpace(item.PluginID)
		item.AccountID = strings.TrimSpace(item.AccountID)
		if item.PluginID == "" || item.AccountID == "" {
			writeError(w, http.StatusBadRequest, "every account needs plugin_id and account_id", "bad_request")
			return
		}
		if _, ok := s.pluginStatus(item.PluginID); !ok {
			writeError(w, http.StatusNotFound, "plugin not found: "+item.PluginID, "not_found")
			return
		}
		if item.Alias != nil {
			alias := strings.TrimSpace(*item.Alias)
			item.Alias = &alias
			if !validAlias(alias) {
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("alias must be at most %d characters", maxAliasRunes), "invalid_alias")
				return
			}
		}
		if item.GroupID != nil {
			groupID := strings.TrimSpace(*item.GroupID)
			item.GroupID = &groupID
			if groupID != "" && !groupIDs[groupID] {
				writeError(w, http.StatusBadRequest, "unknown group: "+groupID, "invalid_group")
				return
			}
		}
		if item.Priority != nil && (*item.Priority < minAccountPriority || *item.Priority > maxAccountPriority) {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("priority must be between %d and %d", minAccountPriority, maxAccountPriority), "invalid_priority")
			return
		}
		if item.Weight != nil {
			weight := *item.Weight
			if weight < minAccountWeight || weight > maxAccountWeight {
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("weight must be between %.1f and %.0f", minAccountWeight, maxAccountWeight), "invalid_weight")
				return
			}
		}
		if item.Egress != nil {
			mode, proxyID, ok := s.normalizeEgress(item.Egress.Mode, item.Egress.ProxyID)
			if !ok {
				writeError(w, http.StatusBadRequest, "invalid egress for "+item.PluginID+"/"+item.AccountID, "invalid_egress")
				return
			}
			item.Egress = &model.EgressDraft{Mode: mode, ProxyID: proxyID}
		}
	}

	for i := range draft.Memberships {
		membership := &draft.Memberships[i]
		membership.PluginID = strings.TrimSpace(membership.PluginID)
		membership.AccountID = strings.TrimSpace(membership.AccountID)
		if membership.PluginID == "" || membership.AccountID == "" {
			writeError(w, http.StatusBadRequest, "every membership needs plugin_id and account_id", "bad_request")
			return
		}
		if _, ok := s.pluginStatus(membership.PluginID); !ok {
			writeError(w, http.StatusNotFound, "plugin not found: "+membership.PluginID, "not_found")
			return
		}
	}

	applied, err := s.opts.AccountSave(draft)
	if err != nil {
		if errors.Is(err, ErrInvalidRequest) {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
			return
		}
		writeError(w, http.StatusInternalServerError, "save accounts: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": applied})
}

// handleAccountIdentityDetails returns the plugin-reported device identity,
// masked unless reveal is true.
func (s *server) handleAccountIdentityDetails(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.AccountIdentityDetails == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		PluginID  string `json:"plugin_id"`
		AccountID string `json:"account_id"`
		Reveal    bool   `json:"reveal"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	pluginID := strings.TrimSpace(body.PluginID)
	accountID := strings.TrimSpace(body.AccountID)
	if pluginID == "" || accountID == "" {
		writeError(w, http.StatusBadRequest, "plugin_id and account_id are required", "bad_request")
		return
	}
	if !s.pluginForCall(w, pluginID) {
		return
	}
	details, err := s.opts.AccountIdentityDetails(pluginID, accountID, body.Reveal)
	if err != nil {
		writeAccountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, details)
}

// handleAccountLoginState lists the login policy of every installed plugin.
func (s *server) handleAccountLoginState(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.AccountLoginState == nil {
		s.unavailable(w)
		return
	}
	states := s.opts.AccountLoginState()
	if states == nil {
		states = []model.LoginState{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": states})
}

// ---------------------------------------------------------------- groups

func (s *server) handleGroups(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.Method == http.MethodGet {
		if s.opts.Groups == nil {
			s.unavailable(w)
			return
		}
		groups, err := s.opts.Groups()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list groups: "+err.Error(), "internal_error")
			return
		}
		if groups == nil {
			groups = []model.AccountGroup{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
		return
	}

	if s.opts.CreateGroup == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required", "bad_request")
		return
	}
	if !validAlias(name) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("name must be at most %d characters", maxAliasRunes), "bad_request")
		return
	}
	id, err := s.opts.CreateGroup(name, strings.TrimSpace(body.Color))
	if err != nil {
		writeAccountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}
