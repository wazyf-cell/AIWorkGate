package schedule

import (
	"context"

	"aiworkgate/internal/model"
)

// Result is one row of the manual "sign all" outcome (also served by the
// admin API). Result is ok, already or fail.
type Result struct {
	Source        string   `json:"source"`
	Account       string   `json:"account,omitempty"`
	Result        string   `json:"result"`
	Message       string   `json:"message,omitempty"`
	CreditsBefore *float64 `json:"credits_before,omitempty"`
	CreditsAfter  *float64 `json:"credits_after,omitempty"`
}

// CheckinAll performs the manual "sign all": every enabled check-in account of
// every enabled plugin, in List order. It never retries (the user is watching)
// and persists each plugin result.
//
// Only the plugin system takes part: a plugin that declares the checkin
// capability is the only thing that can be signed.
func (m *Manager) CheckinAll(ctx context.Context) []Result {
	if ctx == nil {
		ctx = context.Background()
	}
	return m.checkinAllPlugins(ctx)
}

func (m *Manager) checkinAllPlugins(ctx context.Context) []Result {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	var out []Result
	for _, st := range m.Plugins.List() {
		if ctx.Err() != nil {
			return out
		}
		pluginID := st.Manifest.ID
		if pluginID == "" || !st.Enabled || !hasCapability(st.Manifest, model.CapCheckin) {
			continue
		}
		accounts, err := m.Plugins.Accounts(ctx, pluginID)
		if err != nil {
			m.warn("manual check-in: list accounts failed", map[string]any{
				"plugin": pluginID, "error": err.Error(),
			})
			out = append(out, Result{
				Source:  "plugin:" + pluginID,
				Result:  model.CheckinFail,
				Message: err.Error(),
			})
			continue
		}
		for _, acc := range accounts {
			if ctx.Err() != nil {
				return out
			}
			if !acc.Enabled {
				continue
			}
			date := m.now().Format(dateLayout)
			result, _ := m.checkinAccount(ctx, pluginID, acc, date, false, false)
			out = append(out, Result{
				Source:        "plugin:" + pluginID,
				Account:       acc.ID,
				Result:        result.Result,
				Message:       result.Message,
				CreditsBefore: result.CreditsBefore,
				CreditsAfter:  result.CreditsAfter,
			})
		}
	}
	return out
}
