package main

import "testing"

// The authorize entry point differs between Huawei's web portal and its desktop
// IDE channel. The override must be strictly opt-in: with no environment set the
// plugin has to keep the historical default, or an existing install would change
// behaviour on upgrade without anyone asking for it.

func clearAuthorizeEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envAuthorizeMode, "")
	t.Setenv("CODEARTS_AUTHORIZE_URL", "")
}

func TestEndpointsFromEnvDefaultsToEmpty(t *testing.T) {
	clearAuthorizeEnv(t)

	ep := endpointsFromEnv()

	// Empty means "use defaultEndpoints()", which keeps portal/authorize.
	if ep.Authorize != "" {
		t.Fatalf("Authorize = %q, want empty so New() applies the built-in default", ep.Authorize)
	}
	// Nothing else may be overridden either.
	if ep.STS != "" || ep.Chat != "" || ep.Ticket != "" || ep.PortalLogin != "" {
		t.Fatalf("unexpected overrides with a clean environment: %+v", ep)
	}
}

func TestEndpointsFromEnvSelectsDoerChannel(t *testing.T) {
	clearAuthorizeEnv(t)
	t.Setenv(envAuthorizeMode, "doer")

	ep := endpointsFromEnv()

	if ep.Authorize != authorizeDoer {
		t.Fatalf("Authorize = %q, want %q", ep.Authorize, authorizeDoer)
	}
	// The doer channel must not touch any other endpoint.
	if ep.STS != "" || ep.Chat != "" || ep.Ticket != "" {
		t.Fatalf("doer mode must only change Authorize: %+v", ep)
	}
}

func TestEndpointsFromEnvPortalKeepsDefault(t *testing.T) {
	for _, mode := range []string{"portal", "web", "doer", "DOER", "  doer  "} {
		t.Run(mode, func(t *testing.T) {
			clearAuthorizeEnv(t)
			t.Setenv(envAuthorizeMode, mode)
			ep := endpointsFromEnv()

			switch norm := trimLower(mode); norm {
			case "doer":
				if ep.Authorize != authorizeDoer {
					t.Fatalf("mode %q -> %q, want doer", mode, ep.Authorize)
				}
			default:
				if ep.Authorize != "" {
					t.Fatalf("mode %q -> %q, want the default preserved", mode, ep.Authorize)
				}
			}
		})
	}
}

// A typo must not stop the plugin: an unknown mode is ignored and the default
// kept, which is the safe outcome.
func TestEndpointsFromEnvIgnoresUnknownMode(t *testing.T) {
	clearAuthorizeEnv(t)
	t.Setenv(envAuthorizeMode, "portalish-typo")

	ep := endpointsFromEnv()

	if ep.Authorize != "" {
		t.Fatalf("unknown mode should keep the default, got %q", ep.Authorize)
	}
}

func TestEndpointsFromEnvExplicitURLWins(t *testing.T) {
	clearAuthorizeEnv(t)
	t.Setenv(envAuthorizeMode, "doer")
	t.Setenv("CODEARTS_AUTHORIZE_URL", "https://example.test/auth")

	ep := endpointsFromEnv()

	if ep.Authorize != "https://example.test/auth" {
		t.Fatalf("explicit URL should win over the mode, got %q", ep.Authorize)
	}
}

// A malformed override must be dropped rather than sent to the browser, which
// would strand the login flow on a URL that never responds.
func TestEndpointsFromEnvRejectsNonHTTPURL(t *testing.T) {
	for _, bad := range []string{"javascript:alert(1)", "file:///etc/passwd", "not a url"} {
		t.Run(bad, func(t *testing.T) {
			clearAuthorizeEnv(t)
			t.Setenv("CODEARTS_AUTHORIZE_URL", bad)
			ep := endpointsFromEnv()
			if ep.Authorize != "" {
				t.Fatalf("malformed URL %q must be dropped, got %q", bad, ep.Authorize)
			}
		})
	}
}

// The log must report the effective entry point so an operator can tell which one
// is live without reading the environment.
func TestDescribeAuthorizeMode(t *testing.T) {
	cases := map[string]string{
		authorizeDoer:         "doer (desktop IDE channel)",
		authorizePortal:       "portal (web, default)",
		"":                    "portal (web, default)",
		"https://x.test/auth": "custom (https://x.test/auth)",
	}
	for in, want := range cases {
		if got := describeAuthorizeMode(in); got != want {
			t.Fatalf("describeAuthorizeMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func trimLower(s string) string {
	out := ""
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out += string(c)
	}
	return out
}
