package app

import (
	"testing"

	"aiworkgate/internal/model"
)

// TestSchedulerAccountRPMOverlay proves the P5 每分钟请求数 travels from the
// account overlay into the scheduler snapshot: an explicit value is carried,
// an absent overlay means "off", and a negative hand-edited value folds onto
// unlimited instead of pausing the account forever.
func TestSchedulerAccountRPMOverlay(t *testing.T) {
	rpm := 30
	if got := schedulerAccount(model.Account{ID: "acc_1", Enabled: true},
		model.AccountMeta{RPM: &rpm}, true).RPM; got != 30 {
		t.Fatalf("rpm overlay = %d, want 30", got)
	}
	if got := schedulerAccount(model.Account{ID: "acc_1", Enabled: true},
		model.AccountMeta{}, false).RPM; got != 0 {
		t.Fatalf("rpm without overlay = %d, want 0 (off)", got)
	}
	neg := -1
	if got := schedulerAccount(model.Account{ID: "acc_1", Enabled: true},
		model.AccountMeta{RPM: &neg}, true).RPM; got != 0 {
		t.Fatalf("negative rpm = %d, want 0 (unlimited)", got)
	}
	zero := 0
	if got := schedulerAccount(model.Account{ID: "acc_1", Enabled: true},
		model.AccountMeta{RPM: &zero}, true).RPM; got != 0 {
		t.Fatalf("explicit rpm 0 = %d, want 0 (unlimited)", got)
	}
}
