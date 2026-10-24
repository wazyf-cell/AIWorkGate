package main

import (
	"crypto/md5"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Endpoint, header and relay-base fidelity. These assertions encode the values
// lifted from the reference source, so a careless edit cannot silently change
// what the upstream is asked for.

func TestUserapiOrigins(t *testing.T) {
	if got := userapiBase[regionCN]; got != "https://autoglm-acceleration-api.zhipuai.cn" {
		t.Fatalf("cn origin = %q", got)
	}
	if got := userapiBase[regionGlobal]; got != "https://autoglm-api.autoglm.ai" {
		t.Fatalf("global origin = %q", got)
	}
}

func TestEndpointPaths(t *testing.T) {
	cases := map[string]string{
		"proxyChat":    proxyChatPath,
		"configModels": configModelsPath,
		"checkinTask":  checkinTaskPath,
		"taskList":     taskListPath,
		"relaySuffix":  relaySuffix,
	}
	want := map[string]string{
		"proxyChat":    "/autoclaw-proxy/proxy/autoclaw/v1/chat/completions",
		"configModels": "/autoclaw-proxy/proxy/autoclaw-model-config",
		"checkinTask":  "/autoclaw-proxy/proxy/autoclaw-task-complete",
		"taskList":     "/autoclaw-proxy/proxy/autoclaw-task-list",
		"relaySuffix":  "/autoclaw-cloud",
	}
	for k, got := range cases {
		if got != want[k] {
			t.Errorf("%s = %q, want %q", k, got, want[k])
		}
	}
}

func TestAuthSignIsMD5OfAppidTimeSecret(t *testing.T) {
	// authSign is the shipped client signature, not a user secret. Pinning the
	// shape keeps a refactor from quietly changing it.
	sign := authSign("1700000000")
	if len(sign) != 32 {
		t.Fatalf("sign length = %d, want 32 hex chars", len(sign))
	}
	for _, c := range sign {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("sign %q is not lowercase hex", sign)
		}
	}
	// Different timestamps must produce different signatures.
	if authSign("1700000001") == sign {
		t.Fatal("the signature must depend on the timestamp")
	}
	// The digest is md5(appid&ts&secret); recomputing it here pins the formula
	// independently of the implementation under test.
	sum := md5.Sum([]byte(appID + "&1700000000&" + appSecret))
	if want := fmt.Sprintf("%x", sum); sign != want {
		t.Fatalf("authSign = %q, want %q", sign, want)
	}
}

func TestUserapiHeadersShape(t *testing.T) {
	h := userapiHeaders("tok")
	for _, k := range []string{
		"Content-Type", "Accept", "X-Version", "X-Tm", "X-Product", "X-Client-Type",
		"X-Channel", "X-Auth-Appid", "X-Auth-TimeStamp", "X-Auth-Sign", "X-Trace-Id", "X-Lang",
	} {
		if _, ok := h[k]; !ok {
			t.Errorf("userapi headers are missing %q", k)
		}
	}
	// The web fingerprint, not the desktop one.
	if h["X-Tm"] != "web" || h["X-Client-Type"] != "web" || h["X-Version"] != webVersion {
		t.Errorf("web fingerprint = %q/%q/%q", h["X-Tm"], h["X-Client-Type"], h["X-Version"])
	}
	if h["X-Product"] != "autoclaw" || h["X-Channel"] != "official" || h["X-Auth-Appid"] != appID {
		t.Errorf("identity = %q/%q/%q", h["X-Product"], h["X-Channel"], h["X-Auth-Appid"])
	}
	if h["Authorization"] != "Bearer tok" {
		t.Errorf("Authorization = %q", h["Authorization"])
	}
	// With no token there must be no Authorization header at all.
	if _, ok := userapiHeaders("")["Authorization"]; ok {
		t.Error("an anonymous request must not send Authorization")
	}
}

func TestCheckinHeadersUseDesktopFingerprint(t *testing.T) {
	h := checkinHeaders("tok")
	// The task endpoints reject the web fingerprint, so these three must differ.
	if h["X-Tm"] != "mac" || h["X-Client-Type"] != "pc" || h["X-Version"] != clientVer {
		t.Fatalf("checkin fingerprint = %q/%q/%q", h["X-Tm"], h["X-Client-Type"], h["X-Version"])
	}
	if h["Accept"] != "application/json" {
		t.Fatalf("Accept = %q", h["Accept"])
	}
	// It must still carry the same signature trio.
	for _, k := range []string{"X-Auth-Appid", "X-Auth-TimeStamp", "X-Auth-Sign", "X-Trace-Id"} {
		if _, ok := h[k]; !ok {
			t.Errorf("checkin headers are missing %q", k)
		}
	}
}

func TestProxyChatHeaders(t *testing.T) {
	h := proxyChatHeaders("tok", "zai_auto")
	// The proxy channel sends the token twice; both are load-bearing.
	if h["Authorization"] != "Bearer tok" || h["X-Authorization"] != "Bearer tok" {
		t.Fatalf("Authorization/X-Authorization = %q/%q", h["Authorization"], h["X-Authorization"])
	}
	if h["X-Request-Model"] != "zai_auto" {
		t.Fatalf("X-Request-Model = %q", h["X-Request-Model"])
	}
	if h["User-Agent"] != "AutoClaw/"+clientVer {
		t.Fatalf("User-Agent = %q", h["User-Agent"])
	}
	if h["x_trace_id"] != "autoclaw-bridge" {
		t.Fatalf("x_trace_id = %q", h["x_trace_id"])
	}
	if h["X-Lang"] != "zh-CN" || h["X-Tm"] != "mac" || h["X-Client-Type"] != "pc" {
		t.Fatalf("proxy fingerprint = %q/%q/%q", h["X-Lang"], h["X-Tm"], h["X-Client-Type"])
	}
}

func TestRelayHeadersSetModelOnlyWhenBackend(t *testing.T) {
	h := relayHeaders("tok", "zai_auto")
	if h["x-openclaw-model"] != "zai_auto" {
		t.Fatalf("x-openclaw-model = %q", h["x-openclaw-model"])
	}
	if _, ok := relayHeaders("tok", "")["x-openclaw-model"]; ok {
		t.Fatal("an agent target must not pin x-openclaw-model")
	}
	if relayHeaders("", "")["Authorization"] != "" {
		t.Fatal("an anonymous relay call must not send Authorization")
	}
}

func TestRelayBaseRewriting(t *testing.T) {
	cases := []struct{ in, want string }{
		// A bare host gets the relay suffix appended.
		{"abc.example.com", "abc.example.com" + relaySuffix},
		// An explicit scheme keeps it, and the path gains the suffix.
		{"https://abc.example.com/", "https://abc.example.com" + relaySuffix},
		// An endpoint that already points inside a sandbox proxy keeps its path
		// and gets the suffix appended at the end — this is the reference's
		// behaviour (the suffix is looked for, then appended when absent).
		{"https://abc.example.com/proxy/sb1", "https://abc.example.com/proxy/sb1" + relaySuffix},
		// ws/wss are normalised to http/https first.
		{"wss://abc.example.com", "https://abc.example.com" + relaySuffix},
		{"ws://abc.example.com", "http://abc.example.com" + relaySuffix},
		// An already-suffixed base is left alone (one suffix, never two).
		{"https://abc.example.com" + relaySuffix, "https://abc.example.com" + relaySuffix},
		// A suffix already present mid-path is truncated back to the suffix.
		{"https://abc.example.com" + relaySuffix + "/proxy/sb1",
			"https://abc.example.com" + relaySuffix},
	}
	for _, tc := range cases {
		if got := relayBase(tc.in); got != tc.want {
			t.Errorf("relayBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRelayProxyBaseAndBack(t *testing.T) {
	got := relayProxyBase("https://abc.example.com", "sb-1")
	if got != "https://abc.example.com"+relaySuffix+"/proxy/sb-1" {
		t.Fatalf("relayProxyBase = %q", got)
	}
	// An empty sandbox id must not produce a dangling /proxy/.
	if got := relayProxyBase("https://abc.example.com", ""); strings.HasSuffix(got, "/proxy/") {
		t.Fatalf("relayProxyBase with no id = %q", got)
	}
	if back := relayBaseFromProxy(got); back != "https://abc.example.com"+relaySuffix {
		t.Fatalf("relayBaseFromProxy = %q", back)
	}
	// A base without /proxy/ comes back unchanged.
	if back := relayBaseFromProxy("https://x/autoclaw-cloud"); back != "https://x/autoclaw-cloud" {
		t.Fatalf("relayBaseFromProxy = %q", back)
	}
}

func TestClassifyUpstreamErrors(t *testing.T) {
	cases := []struct {
		status int
		code   int64
		body   string
		want   string
	}{
		{200, 410000, "", "session_dead"},
		{402, 0, "", "hard_credit"},
		{200, 0, "余额不足", "hard_credit"},
		{200, 0, "insufficient credit", "hard_credit"},
		{200, 0, "token expired", "session_dead"},
		{200, 0, "请先登录", "session_dead"},
		{429, 0, "", "soft_rate"},
		{404, 0, "", "not_found"},
		{503, 0, "", "server"},
		{400, 0, "bad request", "client"},
		{200, 0, "fine", "none"},
	}
	for _, tc := range cases {
		if got := classify(tc.status, tc.code, tc.body); got != tc.want {
			t.Errorf("classify(%d,%d,%q) = %q, want %q", tc.status, tc.code, tc.body, got, tc.want)
		}
	}
}

func TestJWTClaimsReadWithoutVerifying(t *testing.T) {
	// header {"alg":"none"} payload {"user_id":"u1","exp":1700000000} unsigned
	const tok = "eyJhbGciOiJub25lIn0.eyJ1c2VyX2lkIjoidTEiLCJleHAiOjE3MDAwMDAwMDB9."
	if got := jwtPayloadField(tok, "user_id"); got != "u1" {
		t.Fatalf("user_id = %v", got)
	}
	if got := jwtClaimInt(tok, "exp"); got != 1700000000 {
		t.Fatalf("exp = %d", got)
	}
	// A non-JWT must degrade to zero/nil rather than panic.
	if got := jwtClaimInt("not-a-jwt", "exp"); got != 0 {
		t.Fatalf("exp on garbage = %d", got)
	}
	if got := jwtPayloadField("a.b", "x"); got != nil {
		t.Fatalf("field on garbage = %v", got)
	}
}

func TestBearerNormalisation(t *testing.T) {
	if got := bearer("tok"); got != "Bearer tok" {
		t.Fatalf("bearer = %q", got)
	}
	if got := bearer("Bearer tok"); got != "Bearer tok" {
		t.Fatalf("bearer = %q (must not double-prefix)", got)
	}
	if got := stripBearer("Bearer tok"); got != "tok" {
		t.Fatalf("stripBearer = %q", got)
	}
	if got := stripBearer("tok"); got != "tok" {
		t.Fatalf("stripBearer = %q", got)
	}
}

func TestFingerprintIsShortAndStable(t *testing.T) {
	a := fingerprint("Bearer secret")
	b := fingerprint("secret")
	if a != b {
		t.Fatalf("the prefix must not change the fingerprint: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "sha256:") || len(a) != 15 {
		t.Fatalf("fingerprint = %q", a)
	}
	if strings.Contains(a, "secret") {
		t.Fatal("the fingerprint must not embed the secret")
	}
	if fingerprint("") != "" {
		t.Fatal("an empty secret must fingerprint to an empty string")
	}
}

func TestResolveRegion(t *testing.T) {
	hd := http.Header{}
	hd.Set("X-Autoclaw-Region", "GLOBAL")
	// Header wins over the body and the model prefix.
	if got := resolveRegion(hd, map[string]any{"region": "cn"}, "cn/m"); got != regionGlobal {
		t.Fatalf("resolveRegion = %q, want global from the header", got)
	}
	// Body region applies when the header is absent.
	if got := resolveRegion(nil, map[string]any{"region": "global"}, ""); got != regionGlobal {
		t.Fatalf("resolveRegion = %q, want global from the body", got)
	}
	// Model prefix is the last resort.
	if got := resolveRegion(nil, nil, "global/zai_auto"); got != regionGlobal {
		t.Fatalf("resolveRegion = %q, want global from the model", got)
	}
	if got := resolveRegion(nil, nil, "zai_auto"); got != "" {
		t.Fatalf("resolveRegion = %q, want empty when nothing hints", got)
	}
}
