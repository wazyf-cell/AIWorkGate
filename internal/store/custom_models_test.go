package store

import (
	"errors"
	"strings"
	"testing"
)

func TestCustomModelCRUD(t *testing.T) {
	s := openTestStore(t)

	empty, err := s.ListCustomModels()
	if err != nil {
		t.Fatalf("ListCustomModels on empty table: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("custom models on empty table = %v, want none", empty)
	}

	created, err := s.UpsertCustomModel(CustomModel{
		Name:          "  My-Model  ",
		BaseURL:       " https://api.example.com/v1/ ",
		UpstreamModel: " vendor-model ",
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("UpsertCustomModel create: %v", err)
	}
	if !strings.HasPrefix(created.ID, "cm-") || len(created.ID) != len("cm-")+12 {
		t.Fatalf("generated id = %q, want cm-<12hex>", created.ID)
	}
	if created.Name != "My-Model" || created.BaseURL != "https://api.example.com/v1" ||
		created.UpstreamModel != "vendor-model" {
		t.Fatalf("created row = %+v, want trimmed fields", created)
	}
	if created.CreatedAt == 0 || created.UpdatedAt == 0 {
		t.Fatalf("timestamps = %d/%d, want non-zero", created.CreatedAt, created.UpdatedAt)
	}

	got, found, err := s.GetCustomModel(" " + created.ID + " ")
	if err != nil || !found {
		t.Fatalf("GetCustomModel = %v, %v, %v", got, found, err)
	}
	if got.Name != "My-Model" || !got.Enabled {
		t.Fatalf("stored row = %+v", got)
	}
	if _, found, err := s.GetCustomModel("cm-missing"); err != nil || found {
		t.Fatalf("GetCustomModel missing = %v, %v", found, err)
	}

	// Update in place: created_at stays, every other field follows.
	updated, err := s.UpsertCustomModel(CustomModel{
		ID:            created.ID,
		Name:          "my-model",
		BaseURL:       "https://api.example.com/v2",
		UpstreamModel: "vendor-model-2",
		Enabled:       false,
	})
	if err != nil {
		t.Fatalf("UpsertCustomModel update: %v", err)
	}
	if updated.CreatedAt != created.CreatedAt {
		t.Fatalf("created_at = %d, want %d", updated.CreatedAt, created.CreatedAt)
	}
	if updated.Name != "my-model" || updated.Enabled {
		t.Fatalf("updated row = %+v", updated)
	}

	list, err := s.ListCustomModels()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListCustomModels = %v, %v", list, err)
	}

	removed, err := s.DeleteCustomModel(created.ID)
	if err != nil || !removed {
		t.Fatalf("DeleteCustomModel = %v, %v", removed, err)
	}
	removed, err = s.DeleteCustomModel(created.ID)
	if err != nil || removed {
		t.Fatalf("second DeleteCustomModel = %v, %v, want false", removed, err)
	}
}

func TestCustomModelCanonicalUniqueness(t *testing.T) {
	s := openTestStore(t)

	first, err := s.UpsertCustomModel(CustomModel{
		Name: "Team-Model", BaseURL: "https://api.example.com/v1", UpstreamModel: "m1", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	for _, duplicate := range []string{"team-model", " TEAM-MODEL ", "Team-Model"} {
		_, err := s.UpsertCustomModel(CustomModel{
			Name: duplicate, BaseURL: "https://other.example.com/v1", UpstreamModel: "m2", Enabled: true,
		})
		if !errors.Is(err, ErrCustomModelConflict) {
			t.Fatalf("duplicate %q err = %v, want ErrCustomModelConflict", duplicate, err)
		}
	}

	// The same row may keep its name (case change) without colliding with itself.
	if _, err := s.UpsertCustomModel(CustomModel{
		ID: first.ID, Name: "TEAM-MODEL", BaseURL: first.BaseURL, UpstreamModel: first.UpstreamModel, Enabled: true,
	}); err != nil {
		t.Fatalf("self rename: %v", err)
	}
}

func TestCustomModelValidation(t *testing.T) {
	s := openTestStore(t)

	cases := []struct {
		name string
		in   CustomModel
	}{
		{"empty name", CustomModel{BaseURL: "https://a.example.com", UpstreamModel: "m"}},
		{"slash name", CustomModel{Name: "p/m", BaseURL: "https://a.example.com", UpstreamModel: "m"}},
		{"long name", CustomModel{Name: strings.Repeat("x", MaxCustomModelNameRunes+1),
			BaseURL: "https://a.example.com", UpstreamModel: "m"}},
		{"empty url", CustomModel{Name: "m", UpstreamModel: "m"}},
		{"relative url", CustomModel{Name: "m", BaseURL: "/v1", UpstreamModel: "m"}},
		{"ftp url", CustomModel{Name: "m", BaseURL: "ftp://a.example.com", UpstreamModel: "m"}},
		{"query url", CustomModel{Name: "m", BaseURL: "https://a.example.com/v1?x=1", UpstreamModel: "m"}},
		{"remote http", CustomModel{Name: "m", BaseURL: "http://api.example.com/v1", UpstreamModel: "m"}},
		{"empty upstream", CustomModel{Name: "m", BaseURL: "https://a.example.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.UpsertCustomModel(tc.in); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}

	// Loopback http is the supported local-upstream escape hatch.
	if _, err := s.UpsertCustomModel(CustomModel{
		Name: "local", BaseURL: "http://127.0.0.1:8080/v1", UpstreamModel: "m", Enabled: true,
	}); err != nil {
		t.Fatalf("loopback http: %v", err)
	}
	if err := ValidateCustomModelBaseURL("http://localhost:9000"); err != nil {
		t.Fatalf("localhost http: %v", err)
	}
	if err := ValidateCustomModelBaseURL("http://[::1]:9000"); err != nil {
		t.Fatalf("ipv6 loopback http: %v", err)
	}
}

func TestCustomModelSchemaVersion(t *testing.T) {
	s := openTestStore(t)
	version, err := s.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("schema version = %d, want %d", version, len(migrations))
	}
}
