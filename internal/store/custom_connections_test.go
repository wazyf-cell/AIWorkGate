package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func newTestConnection(t *testing.T, s *Store, label, protocol, baseURL string) CustomConnection {
	t.Helper()
	conn, err := s.UpsertCustomConnection(CustomConnection{
		Label: label, Protocol: protocol, BaseURL: baseURL, Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpsertCustomConnection: %v", err)
	}
	return conn
}

func TestCustomConnectionCRUDAndCascade(t *testing.T) {
	s := openTestStore(t)
	conn := newTestConnection(t, s, " 主接入 ", "claude", "https://api.anthropic.com/")
	if !strings.HasPrefix(conn.ID, "cc-") || len(conn.ID) != len("cc-")+12 {
		t.Fatalf("id = %q, want cc-<12hex>", conn.ID)
	}
	if conn.Protocol != ProtocolAnthropic || conn.BaseURL != "https://api.anthropic.com" ||
		conn.Label != "主接入" {
		t.Fatalf("normalized connection = %+v", conn)
	}

	first, err := s.UpsertCustomModel(CustomModel{
		ConnectionID: conn.ID, Name: "claude-a", BaseURL: conn.BaseURL,
		UpstreamModel: "claude-sonnet-4", Enabled: true,
	})
	if err != nil {
		t.Fatalf("model a: %v", err)
	}
	if _, err := s.UpsertCustomModel(CustomModel{
		ConnectionID: conn.ID, Name: "claude-b", BaseURL: conn.BaseURL,
		UpstreamModel: "claude-opus-4", Enabled: true,
	}); err != nil {
		t.Fatalf("model b: %v", err)
	}

	list, err := s.ListCustomConnections()
	if err != nil {
		t.Fatalf("ListCustomConnections: %v", err)
	}
	if len(list) != 1 || list[0].ID != conn.ID || list[0].ModelCount != 2 {
		t.Fatalf("connections = %+v, want one with 2 models", list)
	}
	models, err := s.ListCustomModelsByConnection(conn.ID)
	if err != nil || len(models) != 2 {
		t.Fatalf("ListCustomModelsByConnection = %v, %v", models, err)
	}
	if models[0].Protocol != ProtocolAnthropic || models[0].BaseURL != "https://api.anthropic.com" {
		t.Fatalf("joined protocol/base_url = %+v", models[0])
	}

	removed, found, err := s.DeleteCustomConnection(conn.ID)
	if err != nil || !found {
		t.Fatalf("DeleteCustomConnection = %v, %v, %v", removed, found, err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed model ids = %v, want 2", removed)
	}
	if _, found, _ := s.GetCustomConnection(conn.ID); found {
		t.Fatal("connection still present after delete")
	}
	if _, found, _ := s.GetCustomModel(first.ID); found {
		t.Fatal("model still present after connection delete")
	}
	if _, found, err := s.DeleteCustomConnection("cc-missing"); err != nil || found {
		t.Fatalf("delete missing = %v, %v", found, err)
	}
}

func TestCustomConnectionRejectsUnknownProtocol(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.UpsertCustomConnection(CustomConnection{
		Protocol: "grpc", BaseURL: "https://api.example.com",
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v, want invalid argument", err)
	}
}

func TestCustomModelSettingsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	conn := newTestConnection(t, s, "openai", "openai", "https://api.example.com/v1")
	created, err := s.UpsertCustomModel(CustomModel{
		ConnectionID: conn.ID, Name: "my-model", BaseURL: conn.BaseURL,
		UpstreamModel: "vendor-model", Enabled: true,
		ContextWindow: 200000, MaxOutputTokens: 8192,
		InputTypes:       []string{"image", "text"},
		ReasoningTiers:   []string{"max", "low"},
		DefaultReasoning: "low",
	})
	if err != nil {
		t.Fatalf("UpsertCustomModel: %v", err)
	}
	got, found, err := s.GetCustomModel(created.ID)
	if err != nil || !found {
		t.Fatalf("GetCustomModel = %v, %v", found, err)
	}
	if got.ContextWindow != 200000 || got.MaxOutputTokens != 8192 {
		t.Fatalf("limits = %d/%d", got.ContextWindow, got.MaxOutputTokens)
	}
	if !reflect.DeepEqual(got.InputTypes, []string{"text", "image"}) {
		t.Fatalf("input types = %v", got.InputTypes)
	}
	if !reflect.DeepEqual(got.ReasoningTiers, []string{"low", "max"}) {
		t.Fatalf("reasoning tiers = %v", got.ReasoningTiers)
	}
	if got.DefaultReasoning != "low" || got.Protocol != ProtocolOpenAI || got.ConnectionID != conn.ID {
		t.Fatalf("settings = %+v", got)
	}
}

func TestCustomModelSettingsDefaultsAndValidation(t *testing.T) {
	s := openTestStore(t)
	conn := newTestConnection(t, s, "openai", "openai", "https://api.example.com")
	created, err := s.UpsertCustomModel(CustomModel{
		ConnectionID: conn.ID, Name: "defaults", BaseURL: conn.BaseURL,
		UpstreamModel: "m", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !reflect.DeepEqual(created.InputTypes, []string{"text", "image"}) {
		t.Fatalf("default input types = %v", created.InputTypes)
	}
	if len(created.ReasoningTiers) != 0 || created.DefaultReasoning != "" {
		t.Fatalf("default reasoning = %v/%q", created.ReasoningTiers, created.DefaultReasoning)
	}

	_, err = s.UpsertCustomModel(CustomModel{
		ConnectionID: conn.ID, Name: "bad-default", BaseURL: conn.BaseURL,
		UpstreamModel: "m2", Enabled: true,
		ReasoningTiers: []string{"low"}, DefaultReasoning: "high",
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad default err = %v", err)
	}
	_, err = s.UpsertCustomModel(CustomModel{
		ConnectionID: conn.ID, Name: "bad-input", BaseURL: conn.BaseURL,
		UpstreamModel: "m3", Enabled: true, InputTypes: []string{"audio"},
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad input err = %v", err)
	}
}

func TestCustomModelUpstreamUniquePerConnection(t *testing.T) {
	s := openTestStore(t)
	one := newTestConnection(t, s, "one", "openai", "https://one.example.com")
	two := newTestConnection(t, s, "two", "openai", "https://two.example.com")
	if _, err := s.UpsertCustomModel(CustomModel{
		ConnectionID: one.ID, Name: "a", BaseURL: one.BaseURL,
		UpstreamModel: "same-model", Enabled: true,
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := s.UpsertCustomModel(CustomModel{
		ConnectionID: one.ID, Name: "b", BaseURL: one.BaseURL,
		UpstreamModel: "same-model", Enabled: true,
	})
	if !errors.Is(err, ErrCustomModelConflict) {
		t.Fatalf("duplicate upstream err = %v", err)
	}
	if _, err := s.UpsertCustomModel(CustomModel{
		ConnectionID: two.ID, Name: "c", BaseURL: two.BaseURL,
		UpstreamModel: "same-model", Enabled: true,
	}); err != nil {
		t.Fatalf("same model in another connection: %v", err)
	}
}

func TestLegacyCustomModelsListAndAttach(t *testing.T) {
	s := openTestStore(t)
	legacy, err := s.UpsertCustomModel(CustomModel{
		Name: "legacy", BaseURL: "https://legacy.example.com/v1",
		UpstreamModel: "legacy-model", Enabled: true,
	})
	if err != nil {
		t.Fatalf("legacy create: %v", err)
	}
	rows, err := s.ListLegacyCustomModels()
	if err != nil || len(rows) != 1 || rows[0].ID != legacy.ID {
		t.Fatalf("ListLegacyCustomModels = %v, %v", rows, err)
	}
	conn := newTestConnection(t, s, "merged", "openai", legacy.BaseURL)
	if err := s.AttachCustomModelConnection(legacy.ID, conn.ID); err != nil {
		t.Fatalf("AttachCustomModelConnection: %v", err)
	}
	rows, err = s.ListLegacyCustomModels()
	if err != nil || len(rows) != 0 {
		t.Fatalf("legacy after attach = %v, %v", rows, err)
	}
	got, _, _ := s.GetCustomModel(legacy.ID)
	if got.ConnectionID != conn.ID || got.Protocol != ProtocolOpenAI {
		t.Fatalf("attached model = %+v", got)
	}
}
