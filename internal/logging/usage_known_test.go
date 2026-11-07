package logging

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"aiworkgate/internal/model"
)

// D-18⑦: APILogger passes UsageKnown through to both sinks untouched —
// gateway emit fills it, this layer must not lose it.
func TestAPILoggerUsageKnownPassthrough(t *testing.T) {
	dataDir := testTempDir(t)
	st := openTestStore(t)
	a, err := NewAPILogger(st, dataDir, false)
	if err != nil {
		t.Fatalf("NewAPILogger: %v", err)
	}
	a.now = fixedNow

	known := apiRec("", "")
	known.ReqID = "req-known"
	known.PromptTokens = 11
	known.CompletionTokens = 7
	known.UsageKnown = true
	unknown := apiRec("", "")
	unknown.ReqID = "req-unknown"
	for _, rec := range []model.APIRequestLog{known, unknown} {
		if err := a.Write(rec); err != nil {
			t.Fatalf("write %s: %v", rec.ReqID, err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rows, n, err := st.QueryAPIRequestLogs(storeFilter())
	if err != nil || n != 2 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
	byReq := map[string]model.APIRequestLog{}
	for _, row := range rows {
		byReq[row.ReqID] = row
	}
	if got := byReq["req-known"]; !got.UsageKnown || got.PromptTokens != 11 || got.CompletionTokens != 7 {
		t.Fatalf("known row = %+v, want usage_known true with 11/7", got)
	}
	if got := byReq["req-unknown"]; got.UsageKnown {
		t.Fatalf("unknown row = %+v, want usage_known false", got)
	}

	// The JSONL mirror keeps the same fact.
	lines := readLines(t, filepath.Join(dataDir, "logs", "api-2026-10-21.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("api jsonl lines = %d, want 2", len(lines))
	}
	seen := map[string]bool{}
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		reqID, _ := entry["req_id"].(string)
		knownFlag, _ := entry["usage_known"].(bool)
		seen[reqID] = knownFlag
	}
	if !seen["req-known"] {
		t.Fatalf("jsonl lost usage_known on the known row: %v", seen)
	}
	if seen["req-unknown"] {
		t.Fatalf("jsonl invented usage_known on the unknown row: %v", seen)
	}
}
