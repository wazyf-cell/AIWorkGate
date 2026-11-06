package webui

import (
	"encoding/json"
	"testing"
)

func TestKeyExpiresAtParsing(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{"null means never", `null`, 0, false},
		{"zero means never", `0`, 0, false},
		{"unix second", `1792651200`, 1_792_651_200, false},
		{"negative rejected", `-1`, 0, true},
		{"string rejected", `"2026-10-01"`, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := keyExpiresAt(json.RawMessage(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("keyExpiresAt(%s) = %d, want an error", tc.raw, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("keyExpiresAt(%s) = %d, %v; want %d", tc.raw, got, err, tc.want)
			}
		})
	}
}

func TestKeyAllowedIPsParsing(t *testing.T) {
	if got, err := keyAllowedIPs(json.RawMessage(`null`)); err != nil || got != nil {
		t.Fatalf("null = %v, %v", got, err)
	}
	if got, err := keyAllowedIPs(json.RawMessage(`["10.0.0.5","192.168.0.0/16"]`)); err != nil || len(got) != 2 {
		t.Fatalf("list = %v, %v", got, err)
	}
	if _, err := keyAllowedIPs(json.RawMessage(`"10.0.0.5"`)); err == nil {
		t.Fatalf("a bare string must be rejected")
	}
}
