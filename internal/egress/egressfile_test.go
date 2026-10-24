package egress

import (
	"reflect"
	"testing"

	"aiworkgate/internal/model"
)

func TestBuildEgressFile(t *testing.T) {
	localURL := func(pluginID, accountID, proxyID string) (string, bool) {
		if proxyID == "px_ok" {
			return "http://127.0.0.1:7901", true
		}
		return "", false
	}
	proxyEntry := model.EgressEntry{Mode: model.EgressProxy, URL: "http://127.0.0.1:7901", ProxyID: "px_ok"}
	direct := model.EgressEntry{Mode: model.EgressDirect}
	blocked := model.EgressEntry{Mode: model.EgressBlocked}

	tests := []struct {
		name         string
		strict       bool
		defaultMode  string
		accounts     []string
		assignments  []model.AccountEgress
		wantDefault  model.EgressEntry
		wantAccounts map[string]model.EgressEntry
	}{
		{
			name:        "non-strict default direct",
			defaultMode: "direct",
			accounts:    []string{"acc_ok", "acc_missing", "acc_none", "acc_direct", "acc_blocked"},
			assignments: []model.AccountEgress{
				{PluginID: "plug", AccountID: "acc_ok", Mode: "proxy", ProxyID: "px_ok"},
				{PluginID: "plug", AccountID: "acc_missing", Mode: "proxy", ProxyID: "px_missing"},
				{PluginID: "plug", AccountID: "acc_direct", Mode: "direct"},
				{PluginID: "plug", AccountID: "acc_blocked", Mode: "blocked"},
				// Assignment of another plugin must be ignored.
				{PluginID: "other", AccountID: "acc_none", Mode: "proxy", ProxyID: "px_ok"},
			},
			wantDefault: direct,
			wantAccounts: map[string]model.EgressEntry{
				"acc_ok":      proxyEntry,
				"acc_missing": direct,
				"acc_none":    direct,
				"acc_direct":  direct,
				"acc_blocked": blocked,
			},
		},
		{
			name:        "strict default direct",
			strict:      true,
			defaultMode: "direct",
			accounts:    []string{"acc_ok", "acc_missing", "acc_none", "acc_direct", "acc_blocked"},
			assignments: []model.AccountEgress{
				{PluginID: "plug", AccountID: "acc_ok", Mode: "proxy", ProxyID: "px_ok"},
				{PluginID: "plug", AccountID: "acc_missing", Mode: "proxy", ProxyID: "px_missing"},
				{PluginID: "plug", AccountID: "acc_direct", Mode: "direct"},
				{PluginID: "plug", AccountID: "acc_blocked", Mode: "blocked"},
			},
			wantDefault: blocked,
			wantAccounts: map[string]model.EgressEntry{
				"acc_ok":      proxyEntry,
				"acc_missing": blocked,
				"acc_none":    blocked,
				"acc_direct":  direct,
				"acc_blocked": blocked,
			},
		},
		{
			name:        "strict default proxy",
			strict:      true,
			defaultMode: "proxy",
			accounts:    []string{"acc_none"},
			wantDefault: model.EgressEntry{Mode: model.EgressProxy},
			wantAccounts: map[string]model.EgressEntry{
				"acc_none": {Mode: model.EgressProxy},
			},
		},
		{
			name:        "empty default falls back to direct",
			defaultMode: "  ",
			accounts:    []string{"acc_none"},
			wantDefault: direct,
			wantAccounts: map[string]model.EgressEntry{
				"acc_none": direct,
			},
		},
		{
			name:        "assignment with proxy id but no mode",
			defaultMode: "direct",
			accounts:    []string{"acc_proxyid_only"},
			assignments: []model.AccountEgress{
				{PluginID: "plug", AccountID: "acc_proxyid_only", ProxyID: "px_ok"},
			},
			wantDefault: direct,
			wantAccounts: map[string]model.EgressEntry{
				"acc_proxyid_only": proxyEntry,
			},
		},
		{
			name:        "proxy mode without proxy id",
			strict:      true,
			defaultMode: "direct",
			accounts:    []string{"acc_weird"},
			assignments: []model.AccountEgress{
				{PluginID: "plug", AccountID: "acc_weird", Mode: "proxy"},
			},
			wantDefault: blocked,
			wantAccounts: map[string]model.EgressEntry{
				"acc_weird": blocked,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildEgressFile("plug", tc.strict, tc.defaultMode, tc.accounts, tc.assignments, localURL)
			if got.Version != 1 {
				t.Fatalf("version = %d, want 1", got.Version)
			}
			if got.Strict != tc.strict {
				t.Fatalf("strict = %v, want %v", got.Strict, tc.strict)
			}
			if !reflect.DeepEqual(got.Default, tc.wantDefault) {
				t.Fatalf("default = %+v, want %+v", got.Default, tc.wantDefault)
			}
			if !reflect.DeepEqual(got.Accounts, tc.wantAccounts) {
				t.Fatalf("accounts = %+v, want %+v", got.Accounts, tc.wantAccounts)
			}
		})
	}
}

func TestBuildEgressFileNilLocalURL(t *testing.T) {
	got := BuildEgressFile("plug", false, "direct", []string{"acc"}, []model.AccountEgress{
		{PluginID: "plug", AccountID: "acc", Mode: "proxy", ProxyID: "px"},
	}, nil)
	if entry := got.Accounts["acc"]; entry.Mode != model.EgressDirect {
		t.Fatalf("nil localURL fallback = %+v, want direct", entry)
	}
}

func TestBuildEgressFileEmptyAccounts(t *testing.T) {
	got := BuildEgressFile("plug", false, "direct", nil, nil, nil)
	if got.Accounts == nil || len(got.Accounts) != 0 {
		t.Fatalf("accounts = %+v, want empty non-nil map", got.Accounts)
	}
}

// TestBuildEgressFileDirectEntriesPointAtListener is the injection-point guard
// for direct mode: when the host binds a direct listener, every "direct" entry
// names it (url + the fixed proxy_id), while the mode itself stays "direct" so a
// plugin that ignores the url keeps dialling by itself. fail-open cannot be
// traded for accounting here: turning direct into "proxy" or "blocked" would
// make a missing listener a hard failure.
func TestBuildEgressFileDirectEntriesPointAtListener(t *testing.T) {
	// The resolver stands in for a Manager with direct logging switched on: it
	// only answers for the synthetic "direct" slot, one port per account.
	ports := map[string]string{"": "7910", "acc_direct": "7911", "acc_none": "7912", "acc_missing": "7913"}
	localURL := func(pluginID, accountID, proxyID string) (string, bool) {
		if proxyID != DirectProxyID {
			if proxyID == "px_ok" {
				return "http://127.0.0.1:7901", true
			}
			return "", false
		}
		port, ok := ports[accountID]
		if !ok {
			return "", false
		}
		return "http://127.0.0.1:" + port, true
	}

	got := BuildEgressFile("plug", false, "direct",
		[]string{"acc_direct", "acc_none", "acc_missing", "acc_blocked", "acc_nolisten"},
		[]model.AccountEgress{
			{PluginID: "plug", AccountID: "acc_direct", Mode: model.EgressDirect},
			{PluginID: "plug", AccountID: "acc_missing", Mode: model.EgressProxy, ProxyID: "px_gone"},
			{PluginID: "plug", AccountID: "acc_blocked", Mode: model.EgressBlocked},
		}, localURL)

	if def := got.Default; def.Mode != model.EgressDirect ||
		def.URL != "http://127.0.0.1:7910" || def.ProxyID != DirectProxyID {
		t.Fatalf("default entry = %+v, want direct on the plugin-wide listener", def)
	}

	assigned := got.Accounts["acc_direct"]
	if assigned.Mode != model.EgressDirect || assigned.URL != "http://127.0.0.1:7911" || assigned.ProxyID != DirectProxyID {
		t.Fatalf("explicit direct entry = %+v, want the account's own listener", assigned)
	}
	// An account without an assignment inherits direct, but must not share the
	// plugin-wide listener: its rows need its own account id.
	inherited := got.Accounts["acc_none"]
	if inherited.Mode != model.EgressDirect || inherited.URL != "http://127.0.0.1:7912" {
		t.Fatalf("inherited entry = %+v, want the account's own listener", inherited)
	}
	// And when the account has no listener of its own it must inherit a bare
	// direct entry, never the plugin-wide one: pointing an account at another
	// account's listener would attribute its traffic to the wrong row.
	nolisten := got.Accounts["acc_nolisten"]
	if nolisten.Mode != model.EgressDirect || nolisten.URL != "" || nolisten.ProxyID != "" {
		t.Fatalf("account without its own listener = %+v, want a bare direct entry", nolisten)
	}
	// A proxy assignment that cannot be resolved degrades to direct, and that
	// degraded path is recorded too.
	degraded := got.Accounts["acc_missing"]
	if degraded.Mode != model.EgressDirect || degraded.URL != "http://127.0.0.1:7913" {
		t.Fatalf("unresolvable proxy assignment = %+v, want a recorded direct fallback", degraded)
	}
	// Blocked stays blocked: no exit, so no listener to point at.
	if blocked := got.Accounts["acc_blocked"]; blocked.Mode != model.EgressBlocked || blocked.URL != "" {
		t.Fatalf("blocked entry = %+v, want no url", blocked)
	}
}

// TestBuildEgressFileStrictNeverNamesTheDirectListener pins the leak guard: in
// strict mode the default becomes blocked, and an explicit direct assignment is
// kept as the refusal marker it always was, without a working exit attached.
func TestBuildEgressFileStrictNeverNamesTheDirectListener(t *testing.T) {
	localURL := func(pluginID, accountID, proxyID string) (string, bool) {
		return "http://127.0.0.1:7911", true
	}
	got := BuildEgressFile("plug", true, "direct", []string{"acc_direct", "acc_none"},
		[]model.AccountEgress{{PluginID: "plug", AccountID: "acc_direct", Mode: model.EgressDirect}}, localURL)

	if def := got.Default; def.Mode != model.EgressBlocked || def.URL != "" {
		t.Fatalf("strict default = %+v, want blocked with no listener attached", def)
	}
	if inherited := got.Accounts["acc_none"]; inherited.Mode != model.EgressBlocked || inherited.URL != "" {
		t.Fatalf("strict inherited = %+v, want blocked with no listener attached", inherited)
	}
	// strict keeps the explicit direct entry as the bare refusal the plugin
	// contract promises; attaching a usable exit would defeat strict mode.
	if direct := got.Accounts["acc_direct"]; direct.Mode != model.EgressDirect || direct.URL != "" {
		t.Fatalf("strict explicit direct = %+v, want mode direct and no url", direct)
	}
}

// TestBuildEgressFileWithoutDirectLoggingIsUnchanged is the other half of the
// switch: with the listener absent, egress.json must be byte-for-byte the shape
// it always had, so enabling the feature is the only thing that can change a
// plugin's exit.
func TestBuildEgressFileWithoutDirectLoggingIsUnchanged(t *testing.T) {
	localURL := func(string, string, string) (string, bool) { return "", false }
	got := BuildEgressFile("plug", false, "direct", []string{"acc_direct", "acc_none"},
		[]model.AccountEgress{{PluginID: "plug", AccountID: "acc_direct", Mode: model.EgressDirect}}, localURL)

	for name, entry := range map[string]model.EgressEntry{"default": got.Default, "acc_direct": got.Accounts["acc_direct"], "acc_none": got.Accounts["acc_none"]} {
		if entry.Mode != model.EgressDirect || entry.URL != "" || entry.ProxyID != "" {
			t.Fatalf("%s = %+v, want the pre-existing bare direct entry", name, entry)
		}
	}
}
