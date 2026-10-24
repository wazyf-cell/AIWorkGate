package store

import (
	"testing"

	"aiworkgate/internal/model"
)

func TestCheckinUpsertKeepsStrongestResultAndMergeCredits(t *testing.T) {
	s := openTestStore(t)
	after := 12.0
	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "mock", AccountID: "acc1", Date: "2026-10-21",
		Result: model.CheckinOK, CreditsAfter: &after, At: 100,
		DedupScope: model.DedupScopeDevice, DeviceKey: "dev-a",
	}); err != nil {
		t.Fatalf("upsert ok: %v", err)
	}

	// A later failure without credits must neither downgrade the result nor
	// erase the credits or the device identity.
	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "mock", AccountID: "acc1", Date: "2026-10-21",
		Result: model.CheckinFail, At: 200,
	}); err != nil {
		t.Fatalf("upsert fail: %v", err)
	}
	records, err := s.QueryCheckinRecords("mock", "2026-10-21", "2026-10-21")
	if err != nil || len(records) != 1 {
		t.Fatalf("query = %+v err=%v", records, err)
	}
	rec := records[0]
	if rec.Result != model.CheckinOK {
		t.Fatalf("result = %q, want ok preserved over fail", rec.Result)
	}
	if rec.CreditsAfter == nil || *rec.CreditsAfter != 12 {
		t.Fatalf("credits_after = %v, want the known 12", rec.CreditsAfter)
	}
	if rec.DedupScope != model.DedupScopeDevice || rec.DeviceKey != "dev-a" {
		t.Fatalf("device identity lost: %+v", rec)
	}
	if ok, err := s.DeviceCheckedIn("mock", "2026-10-21", "dev-a"); err != nil || !ok {
		t.Fatalf("DeviceCheckedIn = %v/%v, want true", ok, err)
	}

	// A skip is weaker than the success as well.
	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "mock", AccountID: "acc1", Date: "2026-10-21",
		Result: model.CheckinSkipped, At: 300,
	}); err != nil {
		t.Fatalf("upsert skipped: %v", err)
	}
	records, _ = s.QueryCheckinRecords("mock", "2026-10-21", "2026-10-21")
	if records[0].Result != model.CheckinOK {
		t.Fatalf("result after skip = %q, want ok", records[0].Result)
	}

	// A stronger success still updates, and credits_after keeps the largest
	// reported value even when the later report is smaller.
	bigger := 20.0
	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "mock", AccountID: "acc1", Date: "2026-10-21",
		Result: model.CheckinOK, CreditsAfter: &bigger, At: 400,
	}); err != nil {
		t.Fatalf("upsert bigger: %v", err)
	}
	smaller := 9.0
	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "mock", AccountID: "acc1", Date: "2026-10-21",
		Result: model.CheckinFail, CreditsAfter: &smaller, At: 500,
	}); err != nil {
		t.Fatalf("upsert smaller fail: %v", err)
	}
	records, _ = s.QueryCheckinRecords("mock", "2026-10-21", "2026-10-21")
	rec = records[0]
	if rec.Result != model.CheckinOK || rec.CreditsAfter == nil || *rec.CreditsAfter != 20 {
		t.Fatalf("record = %+v, want ok with credits_after 20", rec)
	}
}
