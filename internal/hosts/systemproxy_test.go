package hosts

import "testing"

// TestParseSystemProxy covers both shapes Windows writes and the failures we
// must refuse instead of guessing.
func TestParseSystemProxy(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    SystemProxy
		wantErr bool
	}{
		{name: "empty", raw: "", want: SystemProxy{}},
		{
			name: "plain host:port",
			raw:  "127.0.0.1:7897",
			want: SystemProxy{Configured: true, Type: "http", Host: "127.0.0.1", Port: 7897},
		},
		{
			name: "per protocol prefers https",
			raw:  "http=127.0.0.1:7890;https=127.0.0.1:7891;socks=127.0.0.1:7892",
			want: SystemProxy{Configured: true, Type: "http", Host: "127.0.0.1", Port: 7891},
		},
		{
			name: "per protocol falls back to http",
			raw:  "http=10.0.0.2:8080",
			want: SystemProxy{Configured: true, Type: "http", Host: "10.0.0.2", Port: 8080},
		},
		{
			name: "socks",
			raw:  "socks=127.0.0.1:7892",
			want: SystemProxy{Configured: true, Type: "socks5", Host: "127.0.0.1", Port: 7892},
		},
		{
			name: "scheme prefix",
			raw:  "http://127.0.0.1:7897",
			want: SystemProxy{Configured: true, Type: "http", Host: "127.0.0.1", Port: 7897},
		},
		{
			name: "socks scheme prefix",
			raw:  "socks5://[::1]:7891",
			want: SystemProxy{Configured: true, Type: "socks5", Host: "::1", Port: 7891},
		},
		{name: "missing port", raw: "127.0.0.1", wantErr: true},
		{name: "bad port", raw: "127.0.0.1:0", wantErr: true},
		{name: "no recognised protocol", raw: "ftp=127.0.0.1:21", want: SystemProxy{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSystemProxy(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSystemProxy(%q) = %+v, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSystemProxy(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ParseSystemProxy(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}
