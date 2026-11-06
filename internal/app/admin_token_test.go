package app

import "testing"

func TestInitialAdminToken(t *testing.T) {
	t.Setenv(AdminTokenEnv, "short")
	if got := initialAdminToken(); len(got) < 32 {
		t.Fatalf("short env must be ignored, got %q", got)
	}
	t.Setenv(AdminTokenEnv, "0123456789abcdef0123456789abcdef")
	if got := initialAdminToken(); got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("env token = %q, want it used verbatim", got)
	}
	t.Setenv(AdminTokenEnv, "")
	if got := initialAdminToken(); len(got) < 32 || got == "0123456789abcdef0123456789abcdef" {
		t.Fatalf("empty env must mint a random token, got %q", got)
	}
}
