package store

import "testing"

// The plugin-id expression used to special-case 'a' and strip eight bytes from
// everything else. Any non-plugin producer shorter than the prefix length was
// silently mangled: "gateway" became "", "scheduler" became "er", and a custom
// model source lost its "custom:" marker. Keying on the prefix keeps non-plugin
// values intact while still resolving plugin rows.
func TestPluginIDExprPreservesNonPluginValues(t *testing.T) {
	s := openTestStore(t)
	cases := []struct {
		stored string
		want   string
	}{
		{"plugin:a", "a"},
		{"plugin:mock", "mock"},
		{"plugin:officeace", "officeace"},
		// A legacy 'a' predates the migration and must survive unchanged.
		{"a", "a"},
		{"", ""},
		// Non-plugin producers must not be sliced.
		{"gateway", "gateway"},
		{"scheduler", "scheduler"},
		{"custom:cm-1a278e9e5728", "custom:cm-1a278e9e5728"},
		{"plugin:", ""},
	}
	for _, c := range cases {
		var got string
		// The expression binds its argument twice (LIKE and substr), so pass a
		// literal rather than placeholders to keep the test readable.
		q := "SELECT " + pluginIDExpr(`'` + c.stored + `'`)
		if err := s.db.QueryRow(q).Scan(&got); err != nil {
			t.Fatalf("eval %q: %v", c.stored, err)
		}
		if got != c.want {
			t.Fatalf("pluginIDExpr(%q) = %q, want %q", c.stored, got, c.want)
		}
	}
}

// A bare "a" filter must still resolve to the legacy stored value: A keeps
// recording source="a" until forwardA is deleted, so resolving it to "plugin:a"
// now would make the filter miss all live A traffic.
func TestUsagePluginValueKeepsLegacyA(t *testing.T) {
	cases := map[string]string{
		"a":                  "a",
		"":                   "",
		"plugin:a":           "plugin:a",
		"plugin:mock":        "plugin:mock",
		"mock":               "plugin:mock",
		"  mock  ":           "plugin:mock",
		"custom:cm-1a278e9e": "plugin:custom:cm-1a278e9e",
	}
	for raw, want := range cases {
		if got := usagePluginValue(raw); got != want {
			t.Fatalf("usagePluginValue(%q) = %q, want %q", raw, got, want)
		}
	}
}
