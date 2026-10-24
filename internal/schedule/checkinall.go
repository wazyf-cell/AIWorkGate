package schedule

import (
	"context"
	"strings"

	"aiworkgate/internal/hosts"
	"aiworkgate/internal/model"
)

// SourceA is the Result.Source value of an A CLI task. Plugin items use
// "plugin:<id>" to match the API request log convention.
const SourceA = "a"

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
// A's three platforms are deliberately NOT included here. The user signs those
// in himself and asked for the assistant to be left out of "sign all", so this
// path stays inside the plugin system: a plugin that declares the checkin
// capability is the only thing that can be signed. checkinAllA still exists for
// the scheduled A pass, but it is no longer reachable from the manual action.
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

// checkinAllA runs A's three scheduled check-in tasks in contract order.
func (m *Manager) checkinAllA(ctx context.Context) []Result {
	if m.A == nil || ctx.Err() != nil {
		return nil
	}
	exePath := strings.TrimSpace(m.A.ExePath())
	if exePath == "" {
		return nil
	}
	tasks := []string{hosts.TaskTraeCheckin, hosts.TaskWBCheckin, hosts.TaskQoderCheckin}
	out := make([]Result, 0, len(tasks))
	for _, task := range tasks {
		if ctx.Err() != nil {
			return out
		}
		taskResult, err := m.A.RunTask(ctx, exePath, task)
		item := Result{Source: SourceA, Account: task}
		switch {
		case err != nil:
			item.Result = model.CheckinFail
			item.Message = err.Error()
		case taskResult.OK:
			item.Result = model.CheckinOK
			item.Message = taskResult.Summary
		default:
			item.Result = model.CheckinFail
			item.Message = taskResult.Summary
			if item.Message == "" {
				item.Message = "task failed"
			}
		}
		out = append(out, item)
		fields := map[string]any{
			"task":    task,
			"ok":      taskResult.OK,
			"summary": taskResult.Summary,
		}
		if err != nil {
			fields["error"] = err.Error()
			m.warn("A task failed", fields)
		} else {
			m.info("A task finished", fields)
		}
	}
	return out
}
