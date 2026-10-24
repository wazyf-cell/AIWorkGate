package hosts

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientModels(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"object":"list","data":[
			{"id":"gpt-4o","display":"GPT-4o","context_length":128000,"supports_image":true},
			{"id":"qoder-gpt-5","context_length":200000,"tools":true},
			{"id":""}
		]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/", "sk-abc")
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-abc" {
		t.Fatalf("Authorization = %q, want Bearer sk-abc", gotAuth)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(models), models)
	}
	first := models[0]
	if first.ID != "gpt-4o" || first.Name != "GPT-4o" || first.ContextLength != 128000 || !first.Vision {
		t.Fatalf("unexpected first model: %+v", first)
	}
	if second := models[1]; second.ID != "qoder-gpt-5" || !second.Tools || second.ContextLength != 200000 {
		t.Fatalf("unexpected second model: %+v", second)
	}
}

func TestClientModelsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "sk-bad").Models(context.Background())
	if err == nil {
		t.Fatal("expected error for HTTP 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should mention status: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "not json")
	}))
	defer bad.Close()
	if _, err := NewClient(bad.URL, "").Models(context.Background()); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestClientHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q, want /health", r.URL.Path)
		}
		io.WriteString(w, `{"status":"ok","running":true,"total_requests":3}`)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL, "sk").Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}

	notRunning := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"degraded","running":false}`)
	}))
	defer notRunning.Close()
	if err := NewClient(notRunning.URL, "").Health(context.Background()); err == nil {
		t.Fatal("expected unhealthy error")
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	if err := NewClient(down.URL, "").Health(context.Background()); err == nil {
		t.Fatal("expected HTTP error")
	}
}

func TestClientMissingBaseURL(t *testing.T) {
	if err := NewClient("", "k").Health(context.Background()); err == nil {
		t.Fatal("expected error for empty base URL")
	}
}
