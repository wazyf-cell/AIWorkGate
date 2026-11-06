package app

import "testing"

func TestDecisionEnvKey(t *testing.T) {
	if got := decisionEnvKey("jev"); got != "AIWORKGATE_DECISION_JEV_API_KEY" {
		t.Fatalf("env key = %q", got)
	}
	if got := decisionEnvKey("my-provider.1"); got != "AIWORKGATE_DECISION_MY_PROVIDER_1_API_KEY" {
		t.Fatalf("env key = %q", got)
	}
}
