package gateway

// task-83（方案 A'）：全 custom 路由组直发的执行层测试。
//
// 五类验收：
//  1. 组全 custom 成员 → 请求组 ID 命中直发（upstream URL=custom baseURL、
//     api_logs.source=custom:<id>）；
//  2. 成员禁用跳到下一成员；全禁用 → 400 no_usable_group_member；
//  3. 混合 Binding（插件+custom）不直发，交还调度链（保存侧另有 400 校验）；
//  4. 非组裸名直发与插件组调度由既有 TestCustomModelNonStreamUsage 与
//     intent/routing 测试回归（不在此重复）；
//  5. 组 ID 与启用 custom 裸名同名时，裸名直发优先（既有 handler 顺序锁定）。

import (
	"encoding/json"
	"net/http"
	"testing"

	"aiworkgate/internal/scheduler"
)

func customGroupSnapshot(bindings map[string]scheduler.Binding) *scheduler.Snapshot {
	return &scheduler.Snapshot{
		InterPool: scheduler.InterPoolSmart,
		Bindings:  bindings,
		Pools:     map[string]*scheduler.Pool{},
	}
}

func TestCustomGroupAllCustomServesFirstEnabledMember(t *testing.T) {
	const upstreamBody = `{"id":"c1","model":"vendor-b","choices":[{"message":{"content":"hi"}}]}`
	srvB, callsB := newUpstreamServer(t, jsonResponder(http.StatusOK, upstreamBody))
	srvA, callsA := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"unexpected":true}`))

	logs := &fakeLogSink{}
	g := newTestGateway(t, customFixture(t, []CustomModel{
		{ID: "cm-b", Name: "x-b", ConnectionID: "conn-b", ConnectionEnabled: true,
			BaseURL: srvB.URL + "/v1", UpstreamModel: "vendor-b", Enabled: true},
		{ID: "cm-a", Name: "x-a", ConnectionID: "conn-a", ConnectionEnabled: true,
			BaseURL: srvA.URL + "/v1", UpstreamModel: "vendor-a", Enabled: true},
	}, customUpstreamKey, Options{
		Plugins:  newFakeRegistry(),
		Log:      logs,
		Snapshot: staticSnapshot(customGroupSnapshot(map[string]scheduler.Binding{
			"grp-x": {Items: []scheduler.Source{
				// Binding order decides: the second entry is the fall-back, not
				// the second retry of the first one.
				{PluginID: "custom:conn-b", Model: "x-b"},
				{PluginID: "custom:conn-a", Model: "x-a"},
			}},
		})),
	}))

	body := `{"model":"grp-x","messages":[{"role":"user","content":"hi"}]}`
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get(sourceHeader); got != "custom:cm-b" {
		t.Fatalf("source header = %q, want custom:cm-b (first binding member)", got)
	}

	// The first member's upstream answered; the second never saw traffic.
	if got := callsB.last(t); got.Path != "/v1/chat/completions" {
		t.Fatalf("first member upstream path = %q", got.Path)
	}
	var forwarded struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(callsB.last(t).Body, &forwarded); err != nil || forwarded.Model != "vendor-b" {
		t.Fatalf("upstream body model = %q (err %v), want vendor-b", forwarded.Model, err)
	}
	if n := len(callsA.all()); n != 0 {
		t.Fatalf("second member upstream calls = %d, want 0 (binding order, not fail-over)", n)
	}

	rows := logs.wait(t, 1)
	if rows[0].Source != "custom:cm-b" || rows[0].Model != "grp-x" || rows[0].Status != http.StatusOK {
		t.Fatalf("log row = %+v, want source custom:cm-b model grp-x status 200", rows[0])
	}
}

func TestCustomGroupSkipsDisabledMemberThenExplicit400(t *testing.T) {
	srvLive, callsLive := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"id":"c1","model":"vendor-live","choices":[{"message":{"content":"hi"}}]}`))
	srvOff, callsOff := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"unexpected":true}`))
	srvOff2, callsOff2 := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"unexpected":true}`))

	// Member 1's model is disabled, member 2's connection is disabled, member 3
	// is fully enabled: the walk skips the first two and serves the third.
	logs := &fakeLogSink{}
	g := newTestGateway(t, customFixture(t, []CustomModel{
		{ID: "cm-off", Name: "x-off", ConnectionID: "conn-off", ConnectionEnabled: true,
			BaseURL: srvOff.URL, UpstreamModel: "v", Enabled: false},
		{ID: "cm-conoff", Name: "x-conoff", ConnectionID: "conn-dead", ConnectionEnabled: false,
			BaseURL: srvOff2.URL, UpstreamModel: "v", Enabled: true},
		{ID: "cm-live", Name: "x-live", ConnectionID: "conn-live", ConnectionEnabled: true,
			BaseURL: srvLive.URL + "/v1", UpstreamModel: "vendor-live", Enabled: true},
	}, customUpstreamKey, Options{
		Plugins: newFakeRegistry(),
		Log:     logs,
		Snapshot: staticSnapshot(customGroupSnapshot(map[string]scheduler.Binding{
			"grp-skip": {Items: []scheduler.Source{
				{PluginID: "custom:conn-off", Model: "x-off"},
				{PluginID: "custom:conn-dead", Model: "x-conoff"},
				{PluginID: "custom:conn-live", Model: "x-live"},
			}},
		})),
	}))

	body := `{"model":"grp-skip","messages":[{"role":"user","content":"hi"}]}`
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get(sourceHeader); got != "custom:cm-live" {
		t.Fatalf("source header = %q, want custom:cm-live (skipped disabled members)", got)
	}
	if n := len(callsOff.all()) + len(callsOff2.all()); n != 0 {
		t.Fatalf("disabled members saw %d upstream calls, want 0", n)
	}
	if got := callsLive.last(t); got.Path != "/v1/chat/completions" {
		t.Fatalf("live member upstream path = %q", got.Path)
	}

	// Every member unusable → explicit 400, not the scheduler's unknown-pool.
	g2 := newTestGateway(t, customFixture(t, []CustomModel{
		{ID: "cm-dead", Name: "x-dead", ConnectionID: "conn-dead", ConnectionEnabled: false,
			BaseURL: srvOff.URL, UpstreamModel: "v", Enabled: true},
	}, customUpstreamKey, Options{
		Plugins: newFakeRegistry(),
		Snapshot: staticSnapshot(customGroupSnapshot(map[string]scheduler.Binding{
			"grp-dead": {Items: []scheduler.Source{{PluginID: "custom:conn-dead", Model: "x-dead"}}},
		})),
	}))
	rr2 := serve(g2, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"grp-dead","messages":[{"role":"user","content":"hi"}]}`))
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("all-disabled status = %d, want 400 (body %s)", rr2.Code, rr2.Body.String())
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &errBody); err != nil ||
		errBody.Error.Code != "no_usable_group_member" {
		t.Fatalf("error = %s, want no_usable_group_member", rr2.Body.String())
	}
}

func TestCustomGroupMixedBindingStaysOnScheduledPath(t *testing.T) {
	srvCustom, callsCustom := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"unexpected":true}`))
	g := newTestGateway(t, customFixture(t, []CustomModel{
		{ID: "cm-x", Name: "x", ConnectionID: "conn-1", ConnectionEnabled: true,
			BaseURL: srvCustom.URL, UpstreamModel: "vendor-x", Enabled: true},
	}, customUpstreamKey, Options{
		Plugins: newFakeRegistry(),
		Snapshot: staticSnapshot(customGroupSnapshot(map[string]scheduler.Binding{
			"mixed": {Items: []scheduler.Source{
				{PluginID: "mock", Model: "m1"},
				{PluginID: "custom:conn-1", Model: "x"},
			}},
		})),
	}))

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"mixed","messages":[{"role":"user","content":"hi"}]}`))
	// The custom member must not be served directly; the request stays on the
	// scheduled path, which has no pool for these sources and answers an
	// explicit scheduling error (never a silent custom hit). The exact
	// scheduling error shape is owned by the dispatch tests — here only the
	// "no direct custom hit" half of the contract is asserted.
	if n := len(callsCustom.all()); n != 0 {
		t.Fatalf("mixed binding served a custom member directly (%d upstream calls)", n)
	}
	if rr.Code >= 200 && rr.Code < 300 {
		t.Fatalf("mixed binding status = %d, want a scheduling error", rr.Code)
	}
	if rr.Header().Get(sourceHeader) != "" {
		t.Fatalf("source header = %q, want empty (no custom hit)", rr.Header().Get(sourceHeader))
	}
}

func TestCustomGroupNameWinsOverAllCustomGroup(t *testing.T) {
	// "dup" is both an enabled custom bare name (conn-1) and an all-custom
	// group id (conn-2). The bare-name direct path runs first, so conn-1
	// serves and the group's member never sees traffic. This locks the
	// precedence: rename one side when both must coexist.
	srvNamed, callsNamed := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"id":"c1","model":"vendor-named","choices":[{"message":{"content":"hi"}}]}`))
	srvGroup, callsGroup := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"unexpected":true}`))
	g := newTestGateway(t, customFixture(t, []CustomModel{
		{ID: "cm-named", Name: "dup", ConnectionID: "conn-1", ConnectionEnabled: true,
			BaseURL: srvNamed.URL + "/v1", UpstreamModel: "vendor-named", Enabled: true},
		{ID: "cm-group", Name: "x", ConnectionID: "conn-2", ConnectionEnabled: true,
			BaseURL: srvGroup.URL, UpstreamModel: "vendor-group", Enabled: true},
	}, customUpstreamKey, Options{
		Plugins: newFakeRegistry(),
		Snapshot: staticSnapshot(customGroupSnapshot(map[string]scheduler.Binding{
			"dup": {Items: []scheduler.Source{{PluginID: "custom:conn-2", Model: "x"}}},
		})),
	}))

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"dup","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get(sourceHeader); got != "custom:cm-named" {
		t.Fatalf("source header = %q, want custom:cm-named (bare name wins)", got)
	}
	if n := len(callsGroup.all()); n != 0 {
		t.Fatalf("group member upstream calls = %d, want 0 (bare-name precedence)", n)
	}
	if got := callsNamed.last(t); got.Path != "/v1/chat/completions" {
		t.Fatalf("named model upstream path = %q", got.Path)
	}
}
