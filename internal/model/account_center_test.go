package model

import (
	"encoding/json"
	"testing"
)

// TestEffectiveRPM pins the P5 per-minute cap resolution: an unset or negative
// overlay value means "off" (0 = unlimited); only positive values cap.
func TestEffectiveRPM(t *testing.T) {
	if got := (AccountMeta{}).EffectiveRPM(); got != 0 {
		t.Fatalf("unset rpm = %d, want 0", got)
	}
	neg := -5
	if got := (AccountMeta{RPM: &neg}).EffectiveRPM(); got != 0 {
		t.Fatalf("negative rpm = %d, want 0", got)
	}
	zero := 0
	if got := (AccountMeta{RPM: &zero}).EffectiveRPM(); got != 0 {
		t.Fatalf("explicit 0 = %d, want 0 (unlimited)", got)
	}
	pos := 42
	if got := (AccountMeta{RPM: &pos}).EffectiveRPM(); got != 42 {
		t.Fatalf("rpm 42 = %d, want 42", got)
	}
}

// TestAccountDraftRPMWire pins the draft wire contract for rpm: present means
// "set (or cleared by null)", absent means "keep the stored value".
func TestAccountDraftRPMWire(t *testing.T) {
	var withValue AccountDraftItem
	if err := json.Unmarshal([]byte(`{"plugin_id":"p","account_id":"a","rpm":120}`), &withValue); err != nil {
		t.Fatal(err)
	}
	if !withValue.RPMSet() || withValue.RPM == nil || *withValue.RPM != 120 {
		t.Fatalf("item = %+v, want rpm 120 set", withValue)
	}

	var withNull AccountDraftItem
	if err := json.Unmarshal([]byte(`{"plugin_id":"p","account_id":"a","rpm":null}`), &withNull); err != nil {
		t.Fatal(err)
	}
	if !withNull.RPMSet() || withNull.RPM != nil {
		t.Fatalf("item = %+v, want rpm cleared (set with null)", withNull)
	}

	var absent AccountDraftItem
	if err := json.Unmarshal([]byte(`{"plugin_id":"p","account_id":"a"}`), &absent); err != nil {
		t.Fatal(err)
	}
	if absent.RPMSet() {
		t.Fatalf("item = %+v, want rpm untouched", absent)
	}
}
