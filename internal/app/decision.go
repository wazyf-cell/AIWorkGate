package app

// P6/D-18: resolve the configured structured-decision endpoints. The API keys
// live in the secret store (never in config.json) and are read on each
// resolution so a rotation takes effect without a restart.

import (
	"os"
	"strings"

	"aiworkgate/internal/gateway"
)

// DecisionSecretKey names the secret-store entry of one decision upstream.
func DecisionSecretKey(id string) string {
	return "decision." + strings.TrimSpace(id) + ".api_key"
}

// decisionEnvKey is the headless override for one upstream's key, e.g.
// AIWORKGATE_DECISION_JEV_API_KEY. An env value wins over the secret store.
func decisionEnvKey(id string) string {
	var b strings.Builder
	b.WriteString("AIWORKGATE_DECISION_")
	for _, r := range strings.ToUpper(strings.TrimSpace(id)) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	b.WriteString("_API_KEY")
	return b.String()
}

func (a *App) decisionUpstreams() []gateway.DecisionUpstream {
	if a == nil {
		return nil
	}
	cfg := a.configSnapshot()
	if len(cfg.Decision.Upstreams) == 0 {
		return nil
	}
	out := make([]gateway.DecisionUpstream, 0, len(cfg.Decision.Upstreams))
	for _, upstream := range cfg.Decision.Upstreams {
		if upstream.Disabled {
			continue
		}
		key := strings.TrimSpace(os.Getenv(decisionEnvKey(upstream.ID)))
		if key == "" && a.secrets != nil {
			key, _ = a.secrets.GetExtra(DecisionSecretKey(upstream.ID))
		}
		out = append(out, gateway.DecisionUpstream{
			ID:       upstream.ID,
			BaseURL:  upstream.BaseURL,
			Protocol: upstream.Protocol,
			ModelID:  upstream.ModelID,
			APIKey:   key,
		})
	}
	return out
}
