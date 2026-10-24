package logging

import (
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// openTestStore gives logging tests a migrated temporary database.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(testTempDir(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return st
}

// storeFilter selects every API log row.
func storeFilter() store.Filter { return store.Filter{} }

// apiRec builds a minimal request log record with the given bodies.
func apiRec(reqBody, respBody string) model.APIRequestLog {
	return model.APIRequestLog{
		TS:        fixedNow().Format("2006-01-02T15:04:05Z07:00"),
		ReqID:     "req-1",
		Source:    "plugin:mock",
		Model:     "mock/model-a",
		KeyID:     "key-1",
		KeyName:   "default",
		Stream:    true,
		Status:    200,
		LatencyMS: 120,
		BodyReq:   reqBody,
		BodyResp:  respBody,
	}
}
