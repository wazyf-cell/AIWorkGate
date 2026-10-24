package app

import (
	"errors"
	"strings"
	"testing"

	"aiworkgate/internal/logging"
	"aiworkgate/internal/secrets"
	"aiworkgate/internal/store"
	"aiworkgate/internal/webui"
)

func newCustomConnectionApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	sec, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	logger, err := logging.NewLogger(dir)
	if err != nil {
		t.Fatalf("logging.NewLogger: %v", err)
	}
	a := &App{store: st, secrets: sec, logger: logger}
	t.Cleanup(func() {
		_ = logger.Close()
		_ = sec.Close()
		_ = st.Close()
	})
	return a
}

func TestMigrateLegacyCustomModelsGroupsByEndpointAndKey(t *testing.T) {
	a := newCustomConnectionApp(t)
	rows := []store.CustomModel{
		{Name: "one", BaseURL: "https://one.example.com/v1", UpstreamModel: "m1", Enabled: true},
		{Name: "two", BaseURL: "https://one.example.com/v1", UpstreamModel: "m2", Enabled: true},
		{Name: "three", BaseURL: "https://one.example.com/v1", UpstreamModel: "m3", Enabled: true},
		{Name: "four", BaseURL: "https://two.example.com/v1", UpstreamModel: "m4", Enabled: true},
	}
	keys := []string{"sk-a", "sk-a", "sk-b", "sk-a"}
	for i, row := range rows {
		created, err := a.store.UpsertCustomModel(row)
		if err != nil {
			t.Fatalf("legacy insert %d: %v", i, err)
		}
		if err := a.secrets.SetExtra(store.CustomModelSecretKey(created.ID), keys[i]); err != nil {
			t.Fatalf("legacy key %d: %v", i, err)
		}
	}

	a.migrateLegacyCustomModels()

	connections, err := a.store.ListCustomConnections()
	if err != nil {
		t.Fatalf("ListCustomConnections: %v", err)
	}
	if len(connections) != 3 {
		t.Fatalf("connections = %+v, want 3 groups", connections)
	}
	legacy, err := a.store.ListLegacyCustomModels()
	if err != nil || len(legacy) != 0 {
		t.Fatalf("legacy rows after migration = %v, %v", legacy, err)
	}
	all, _ := a.store.ListCustomModels()
	if len(all) != 4 {
		t.Fatalf("models = %d, want 4", len(all))
	}
	for _, row := range all {
		if row.ConnectionID == "" {
			t.Fatalf("model %s has no connection", row.Name)
		}
		if _, ok := a.secrets.GetExtra(store.CustomModelSecretKey(row.ID)); ok {
			t.Fatalf("legacy secret for %s was not cleared", row.Name)
		}
	}

	// Idempotent: a second run adds nothing.
	a.migrateLegacyCustomModels()
	again, _ := a.store.ListCustomConnections()
	if len(again) != len(connections) {
		t.Fatalf("second migration added connections: %d -> %d", len(connections), len(again))
	}
}

func TestCreateCustomConnectionReusesEndpointAndKey(t *testing.T) {
	a := newCustomConnectionApp(t)
	first, created, err := a.createCustomConnection(webui.CustomConnectionInput{
		Label: "主", Protocol: "anthropic", BaseURL: "https://api.anthropic.com/", APIKey: "sk-1", Enabled: true,
	})
	if err != nil || !created {
		t.Fatalf("first create = %+v, %v, %v", first, created, err)
	}
	second, created, err := a.createCustomConnection(webui.CustomConnectionInput{
		Protocol: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "sk-1", Enabled: true,
	})
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("duplicate create = %+v, %v, %v; want existing %s", second, created, err, first.ID)
	}
	_, _, err = a.createCustomConnection(webui.CustomConnectionInput{
		Protocol: "openai", BaseURL: "https://api.anthropic.com", APIKey: "sk-1", Enabled: true,
	})
	if !errors.Is(err, webui.ErrCustomConnectionExists) {
		t.Fatalf("protocol conflict err = %v", err)
	}
	_, _, err = a.createCustomConnection(webui.CustomConnectionInput{
		Protocol: "openai", BaseURL: "https://api.anthropic.com", APIKey: "sk-2", Enabled: true,
	})
	if err != nil {
		t.Fatalf("different key should create a second connection: %v", err)
	}
	connections, _ := a.store.ListCustomConnections()
	if len(connections) != 2 {
		t.Fatalf("connections = %d, want 2", len(connections))
	}
}

func TestImportCustomConnectionModelsSanitizesAndSkips(t *testing.T) {
	a := newCustomConnectionApp(t)
	conn, _, err := a.createCustomConnection(webui.CustomConnectionInput{
		Label: "本地", Protocol: "openai", BaseURL: "https://api.example.com/v1", APIKey: "sk", Enabled: true,
	})
	if err != nil {
		t.Fatalf("connection: %v", err)
	}
	result, err := a.importCustomConnectionModels(conn.ID, []webui.ImportModelInput{
		{UpstreamModel: "vendor/model", Name: "", ContextWindow: 128000, MaxOutputTokens: 4096,
			InputTypes: []string{"text", "image"}, ReasoningTiers: []string{"low", "high"}, DefaultReasoning: "low"},
		{UpstreamModel: "vendor/model", Name: "duplicate"},
		{UpstreamModel: "another", Name: "vendor-model"},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(result.Created) != 2 || len(result.Skipped) != 1 {
		t.Fatalf("result = %+v, want 2 created / 1 skipped", result)
	}
	if result.Created[0].Name != "vendor-model" || result.Created[0].UpstreamModel != "vendor/model" {
		t.Fatalf("sanitized row = %+v", result.Created[0])
	}
	if result.Created[1].Name != "vendor-model-2" {
		t.Fatalf("collision name = %q, want vendor-model-2", result.Created[1].Name)
	}
	if result.Created[0].ContextWindow != 128000 || result.Created[0].DefaultReasoning != "low" {
		t.Fatalf("settings = %+v", result.Created[0])
	}
	if !result.Created[0].ConnectionEnabled {
		t.Fatal("created model should inherit the connection's enabled state")
	}
	if !strings.Contains(result.Skipped[0].Reason, "已经有") {
		t.Fatalf("skip reason = %q", result.Skipped[0].Reason)
	}
}

func TestCreateCustomModelLegacyPayloadCreatesConnection(t *testing.T) {
	a := newCustomConnectionApp(t)
	row, err := a.createCustomModel(webui.CustomModelInput{
		Name: "legacy", BaseURL: "https://legacy.example.com/v1",
		UpstreamModel: "m", APIKey: "sk-legacy", Enabled: true,
	})
	if err != nil {
		t.Fatalf("createCustomModel: %v", err)
	}
	if row.ConnectionID == "" || !row.KeySet {
		t.Fatalf("row = %+v, want a connection and key_set", row)
	}
	conn, found, err := a.store.GetCustomConnection(row.ConnectionID)
	if err != nil || !found {
		t.Fatalf("connection = %+v, %v, %v", conn, found, err)
	}
	if conn.Protocol != store.ProtocolOpenAI || conn.BaseURL != "https://legacy.example.com/v1" {
		t.Fatalf("connection = %+v", conn)
	}
	if key, _ := a.connectionKey(conn.ID); key != "sk-legacy" {
		t.Fatalf("connection key = %q", key)
	}
}
