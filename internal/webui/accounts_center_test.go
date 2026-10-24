package webui

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aiworkgate/internal/model"
)

func pluginsWithRunning(running bool) func() []model.PluginStatus {
	return func() []model.PluginStatus {
		return []model.PluginStatus{pluginStub("mock", running, []string{model.CapCheckin, model.CapRefresh}, false)}
	}
}

func TestAccountCheckinRoute(t *testing.T) {
	type call struct{ pluginID, accountID string }
	var calls []call
	handler := New(Options{
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{
				pluginStub("mock", true, []string{model.CapCheckin}, false),
				pluginStub("stopped", false, nil, false),
			}
		},
		AccountCheckin: func(pluginID, accountID string) (model.CheckinResult, error) {
			calls = append(calls, call{pluginID, accountID})
			if accountID == "acc_boom" {
				return model.CheckinResult{}, errors.New("upstream rejected the check-in")
			}
			return model.CheckinResult{Result: model.CheckinOK, Message: "+100"}, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodPost, "/api/accounts/checkin",
		`{"plugin_id":"mock","account_id":"acc_1"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("checkin = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		OK     bool                `json:"ok"`
		Result model.CheckinResult `json:"result"`
	}
	decodeJSONBody(t, recorder, &payload)
	if !payload.OK || payload.Result.Result != model.CheckinOK || payload.Result.Message != "+100" {
		t.Fatalf("checkin payload = %+v", payload)
	}
	if len(calls) != 1 || calls[0] != (call{"mock", "acc_1"}) {
		t.Fatalf("calls = %+v", calls)
	}

	for _, body := range []string{
		`{"plugin_id":"mock"}`,
		`{"account_id":"acc_1"}`,
		`not-json`,
		`{"plugin_id":1,"account_id":"acc_1"}`,
	} {
		recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/checkin", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400 (body %s)", body, recorder.Code, recorder.Body.String())
		}
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/checkin",
		`{"plugin_id":"ghost","account_id":"acc_1"}`)
	if recorder.Code != http.StatusNotFound || errorType(t, recorder) != "not_found" {
		t.Fatalf("unknown plugin = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/checkin",
		`{"plugin_id":"stopped","account_id":"acc_1"}`)
	if recorder.Code != http.StatusConflict || errorType(t, recorder) != "plugin_not_running" {
		t.Fatalf("stopped plugin = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/checkin",
		`{"plugin_id":"mock","account_id":"acc_boom"}`)
	if recorder.Code != http.StatusBadGateway || errorType(t, recorder) != "plugin_error" {
		t.Fatalf("plugin failure = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "upstream rejected the check-in") {
		t.Fatalf("plugin message missing: %s", recorder.Body.String())
	}
	recorder = doRequest(t, handler, http.MethodGet, "/api/accounts/checkin", "")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET checkin = %d, want 405", recorder.Code)
	}
}

func TestAccountRenameRoute(t *testing.T) {
	type call struct{ pluginID, accountID, alias string }
	var calls []call
	handler := New(Options{
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{pluginStub("mock", false, nil, false)}
		},
		AccountRename: func(pluginID, accountID, alias string) error {
			calls = append(calls, call{pluginID, accountID, alias})
			if accountID == "acc_boom" {
				return errors.New("store is full")
			}
			return nil
		},
	})

	// Renaming a stopped plugin is allowed: the overlay is host state.
	recorder := doRequest(t, handler, http.MethodPost, "/api/accounts/rename",
		`{"plugin_id":"mock","account_id":"acc_1","alias":"  主号  "}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("rename = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(calls) != 1 || calls[0] != (call{"mock", "acc_1", "主号"}) {
		t.Fatalf("calls = %+v", calls)
	}

	// An empty alias clears the override.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/rename",
		`{"plugin_id":"mock","account_id":"acc_1","alias":""}`)
	if recorder.Code != http.StatusOK || calls[len(calls)-1].alias != "" {
		t.Fatalf("clear alias = %d, %+v", recorder.Code, calls)
	}

	// 64 runes pass, 65 are rejected.
	alias64 := strings.Repeat("a", 64)
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/rename",
		fmt.Sprintf(`{"plugin_id":"mock","account_id":"acc_1","alias":%q}`, alias64))
	if recorder.Code != http.StatusOK {
		t.Fatalf("64-rune alias = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/rename",
		`{"plugin_id":"mock","account_id":"acc_1","alias":"`+strings.Repeat("a", 65)+`"}`)
	if recorder.Code != http.StatusBadRequest || errorType(t, recorder) != "invalid_alias" {
		t.Fatalf("65-rune alias = %d", recorder.Code)
	}

	for _, body := range []string{
		`{"plugin_id":"mock","alias":"x"}`,
		`{"account_id":"acc_1","alias":"x"}`,
		`{"plugin_id":"mock","account_id":"acc_1","alias":123}`,
	} {
		recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/rename", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", body, recorder.Code)
		}
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/rename",
		`{"plugin_id":"ghost","account_id":"acc_1","alias":"x"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin = %d, want 404", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/rename",
		`{"plugin_id":"mock","account_id":"acc_boom","alias":"x"}`)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("store failure = %d, want 502", recorder.Code)
	}
}

func TestAccountBulkRoute(t *testing.T) {
	var gotAction string
	var gotItems []model.AccountRef
	var gotParams model.BulkParams
	handler := New(Options{
		Plugins: pluginsWithRunning(true),
		Groups: func() ([]model.AccountGroup, error) {
			return []model.AccountGroup{{ID: "grp_1", Name: "Work"}}, nil
		},
		AccountBulk: func(action string, items []model.AccountRef, params model.BulkParams) []model.BulkResult {
			gotAction, gotItems, gotParams = action, items, params
			results := make([]model.BulkResult, 0, len(items))
			for i, item := range items {
				result := model.BulkResult{PluginID: item.PluginID, AccountID: item.AccountID, OK: i == 0}
				if i > 0 {
					result.Message = "plugin not found: " + item.PluginID
				}
				results = append(results, result)
			}
			return results
		},
	})

	body := `{"action":"CHECKIN","items":[{"plugin_id":"mock","account_id":"acc_1"},{"plugin_id":"ghost","account_id":"acc_2"}]}`
	recorder := doRequest(t, handler, http.MethodPost, "/api/accounts/bulk", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("bulk = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Results []model.BulkResult `json:"results"`
	}
	decodeJSONBody(t, recorder, &payload)
	if len(payload.Results) != 2 || !payload.Results[0].OK || payload.Results[1].OK {
		t.Fatalf("results = %+v", payload.Results)
	}
	if gotAction != model.BulkCheckin || len(gotItems) != 2 || gotItems[1].PluginID != "ghost" {
		t.Fatalf("forwarded = %q %+v", gotAction, gotItems)
	}

	// bind_ip normalizes the mode and proxy.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/bulk",
		`{"action":"bind_ip","items":[{"plugin_id":"mock","account_id":"acc_1"}],"params":{"mode":"direct","proxy_id":"px_ignored"}}`)
	if recorder.Code != http.StatusOK || gotParams.Mode != model.EgressDirect || gotParams.ProxyID != "" {
		t.Fatalf("bind_ip = %d params %+v", recorder.Code, gotParams)
	}

	// The UI's "egress" action is an alias of bind_ip.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/bulk",
		`{"action":"egress","items":[{"plugin_id":"mock","account_id":"acc_1"}],"params":{"mode":"direct"}}`)
	if recorder.Code != http.StatusOK || gotAction != model.BulkBindIP {
		t.Fatalf("egress alias = %d action %q", recorder.Code, gotAction)
	}

	// group requires a known group.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/bulk",
		`{"action":"group","items":[{"plugin_id":"mock","account_id":"acc_1"}],"params":{"group_id":"grp_1"}}`)
	if recorder.Code != http.StatusOK || gotParams.GroupID != "grp_1" {
		t.Fatalf("group = %d params %+v", recorder.Code, gotParams)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/bulk",
		`{"action":"group","items":[{"plugin_id":"mock","account_id":"acc_1"}],"params":{"group_id":"grp_missing"}}`)
	if recorder.Code != http.StatusBadRequest || errorType(t, recorder) != "invalid_request" {
		t.Fatalf("unknown group = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/bulk",
		`{"action":"group","items":[{"plugin_id":"mock","account_id":"acc_1"}]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("group without id = %d", recorder.Code)
	}

	for _, body := range []string{
		`{"action":"teleport","items":[{"plugin_id":"mock","account_id":"acc_1"}]}`,
		`{"action":"checkin","items":[]}`,
		`{"action":"checkin","items":[{"plugin_id":"mock"}]}`,
		`{"action":"bind_ip","items":[{"plugin_id":"mock","account_id":"acc_1"}],"params":{"mode":"bogus"}}`,
		`not-json`,
	} {
		recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/bulk", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", body, recorder.Code)
		}
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/bulk",
		`{"action":"bind_ip","items":[{"plugin_id":"mock","account_id":"acc_1"}],"params":{"mode":"proxy"}}`)
	if recorder.Code != http.StatusBadRequest || errorType(t, recorder) != "invalid_egress" {
		t.Fatalf("proxy without proxy_id = %d", recorder.Code)
	}
}

func TestAccountSaveRoute(t *testing.T) {
	var got model.AccountDraft
	handler := New(Options{
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{pluginStub("mock", true, nil, false)}
		},
		Proxies: func() []model.Proxy {
			return []model.Proxy{{ID: "px_on", Name: "on", Enabled: true}, {ID: "px_off", Name: "off"}}
		},
		Groups: func() ([]model.AccountGroup, error) {
			return []model.AccountGroup{{ID: "grp_old", Name: "Old"}}, nil
		},
		AccountSave: func(draft model.AccountDraft) (model.SaveApplied, error) {
			got = draft
			if len(draft.Accounts) > 0 && draft.Accounts[0].AccountID == "acc_reject" {
				return model.SaveApplied{}, fmt.Errorf("%w: duplicate group name", ErrInvalidRequest)
			}
			return model.SaveApplied{Accounts: len(draft.Accounts), Groups: 1, Memberships: 1}, nil
		},
	})

	valid := `{"accounts":[{"plugin_id":"mock","account_id":"acc_1","alias":" 主号 ","group_id":"grp_new",` +
		`"priority":5,"weight":2.5,"enabled_override":false,` +
		`"egress":{"mode":"proxy","proxy_id":"px_on"}}],` +
		`"groups":[{"id":"grp_new","name":"New","color":"#000","sort":2}],` +
		`"deleted_groups":["grp_old"],` +
		`"memberships":[{"plugin_id":"mock","account_id":"acc_1","member":true}]}`
	recorder := doRequest(t, handler, http.MethodPost, "/api/accounts/save", valid)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		OK      bool              `json:"ok"`
		Applied model.SaveApplied `json:"applied"`
	}
	decodeJSONBody(t, recorder, &payload)
	if !payload.OK || payload.Applied.Accounts != 1 || payload.Applied.Groups != 1 {
		t.Fatalf("save payload = %+v", payload)
	}
	if len(got.Accounts) != 1 || *got.Accounts[0].Alias != "主号" || *got.Accounts[0].GroupID != "grp_new" {
		t.Fatalf("forwarded draft = %+v", got.Accounts)
	}
	if got.Accounts[0].Egress == nil || got.Accounts[0].Egress.Mode != model.EgressProxy || got.Accounts[0].Egress.ProxyID != "px_on" {
		t.Fatalf("forwarded egress = %+v", got.Accounts[0].Egress)
	}

	rejecting := `{"accounts":[{"plugin_id":"mock","account_id":"acc_reject","alias":"x"}]}`
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/save", rejecting)
	if recorder.Code != http.StatusBadRequest || errorType(t, recorder) != "invalid_request" {
		t.Fatalf("host rejection = %d (body %s)", recorder.Code, recorder.Body.String())
	}

	before := len(got.Accounts)
	invalid := []struct {
		body string
		kind string
	}{
		{`{"accounts":[{"plugin_id":"mock","account_id":"acc_1","priority":1000}]}`, "invalid_priority"},
		{`{"accounts":[{"plugin_id":"mock","account_id":"acc_1","priority":-1}]}`, "invalid_priority"},
		{`{"accounts":[{"plugin_id":"mock","account_id":"acc_1","weight":0.05}]}`, "invalid_weight"},
		{`{"accounts":[{"plugin_id":"mock","account_id":"acc_1","weight":101}]}`, "invalid_weight"},
		{`{"accounts":[{"plugin_id":"mock","account_id":"acc_1","alias":"` + strings.Repeat("a", 65) + `"}]}`, "invalid_alias"},
		{`{"accounts":[{"plugin_id":"mock","account_id":"acc_1","group_id":"grp_missing"}]}`, "invalid_group"},
		{`{"groups":[{"id":"grp_x","name":""}]}`, "invalid_group"},
		{`{"groups":[{"id":"grp_x","name":"X"}],"deleted_groups":["grp_x"]}`, "invalid_group"},
		{`{"accounts":[{"plugin_id":"mock","account_id":"acc_1","egress":{"mode":"blocked"}}]}`, "invalid_egress"},
		{`{"accounts":[{"plugin_id":"mock","account_id":"acc_1","egress":{"mode":"proxy"}}]}`, "invalid_egress"},
		{`{"accounts":[{"plugin_id":"mock","account_id":"acc_1","egress":{"mode":"proxy","proxy_id":"px_off"}}]}`, "invalid_egress"},
		{`{"memberships":[{"plugin_id":"mock","account_id":""}]}`, "bad_request"},
	}
	for _, tc := range invalid {
		recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/save", tc.body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400 (body %s)", tc.body, recorder.Code, recorder.Body.String())
		}
		if kind := errorType(t, recorder); kind != tc.kind {
			t.Fatalf("%s error type = %q, want %q", tc.body, kind, tc.kind)
		}
	}
	if len(got.Accounts) != before {
		t.Fatalf("invalid draft reached the host: %+v", got.Accounts)
	}

	// Unknown plugin and malformed bodies.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/save",
		`{"accounts":[{"plugin_id":"ghost","account_id":"acc_1"}]}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin = %d, want 404", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/save", `not-json`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d, want 400", recorder.Code)
	}

	// Empty draft is valid (nothing to do).
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/save", `{}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("empty draft = %d, want 200", recorder.Code)
	}
}

func TestAccountIdentityDetailsRoute(t *testing.T) {
	var gotReveal bool
	handler := New(Options{
		Plugins: pluginsWithRunning(true),
		AccountIdentityDetails: func(pluginID, accountID string, reveal bool) (model.IdentityDetails, error) {
			gotReveal = reveal
			if accountID == "acc_missing" {
				return model.IdentityDetails{}, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
			}
			return model.IdentityDetails{
				PluginID: pluginID, AccountID: accountID, Masked: !reveal,
				Identity: &model.Identity{Source: model.IdentityDerived},
			}, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodPost, "/api/accounts/identity/details",
		`{"plugin_id":"mock","account_id":"acc_1"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("details = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var details model.IdentityDetails
	decodeJSONBody(t, recorder, &details)
	if !details.Masked || gotReveal {
		t.Fatalf("details = %+v reveal=%v", details, gotReveal)
	}

	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/identity/details",
		`{"plugin_id":"mock","account_id":"acc_1","reveal":true}`)
	if recorder.Code != http.StatusOK || !gotReveal {
		t.Fatalf("reveal = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/identity/details",
		`{"plugin_id":"mock","account_id":"acc_missing"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing account = %d, want 404", recorder.Code)
	}
	for _, body := range []string{`{"plugin_id":"mock"}`, `{"account_id":"acc_1"}`, `not-json`} {
		recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/identity/details", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", body, recorder.Code)
		}
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/identity/details",
		`{"plugin_id":"ghost","account_id":"acc_1"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin = %d", recorder.Code)
	}
}

func TestAccountLoginStateRoute(t *testing.T) {
	handler := New(Options{
		AccountLoginState: func() []model.LoginState {
			return []model.LoginState{{
				PluginID: "mock", Name: "Mock", APILevel: 2, Methods: []string{"api_key"},
				Source: model.LoginSourceManifest,
			}}
		},
	})
	recorder := doRequest(t, handler, http.MethodGet, "/api/accounts/login-state", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("login-state = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Plugins []model.LoginState `json:"plugins"`
	}
	decodeJSONBody(t, recorder, &payload)
	if len(payload.Plugins) != 1 || payload.Plugins[0].PluginID != "mock" ||
		payload.Plugins[0].APILevel != 2 || payload.Plugins[0].Source != model.LoginSourceManifest {
		t.Fatalf("payload = %+v", payload)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/login-state", "{}")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST login-state = %d, want 405", recorder.Code)
	}
}

func TestGroupsRoutes(t *testing.T) {
	type call struct{ op, name, color string }
	var calls []call
	handler := New(Options{
		Groups: func() ([]model.AccountGroup, error) {
			return []model.AccountGroup{{ID: "grp_1", Name: "Work", AccountCount: 2}}, nil
		},
		CreateGroup: func(name, color string) (string, error) {
			calls = append(calls, call{"create", name, color})
			if name == "dup" {
				return "", fmt.Errorf("%w: duplicate group name", ErrInvalidRequest)
			}
			return "grp_new", nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/groups", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET groups = %d", recorder.Code)
	}
	var listed struct {
		Groups []model.AccountGroup `json:"groups"`
	}
	decodeJSONBody(t, recorder, &listed)
	if len(listed.Groups) != 1 || listed.Groups[0].AccountCount != 2 {
		t.Fatalf("groups = %+v", listed.Groups)
	}

	recorder = doRequest(t, handler, http.MethodPost, "/api/groups", `{"name":" New ","color":"#abc"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST group = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	decodeJSONBody(t, recorder, &created)
	if created.ID != "grp_new" {
		t.Fatalf("created id = %q", created.ID)
	}
	if len(calls) != 1 || calls[0] != (call{"create", "New", "#abc"}) {
		t.Fatalf("create call = %+v", calls)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/groups", `{"name":""}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty name = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/groups", `{"name":"dup"}`)
	if recorder.Code != http.StatusBadRequest || errorType(t, recorder) != "invalid_request" {
		t.Fatalf("duplicate name = %d", recorder.Code)
	}

	// Group renames/deletes and membership writes go through the atomic
	// account draft, so the old per-group REST verbs must be gone.
	for _, probe := range []struct {
		method, path, body string
	}{
		{http.MethodPut, "/api/groups/grp_1", `{"name":"Renamed"}`},
		{http.MethodDelete, "/api/groups/grp_1", ""},
		{http.MethodPut, "/api/groups/grp_1/members", `{"items":[]}`},
	} {
		recorder = doRequest(t, handler, probe.method, probe.path, probe.body)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", probe.method, probe.path, recorder.Code)
		}
	}
}
