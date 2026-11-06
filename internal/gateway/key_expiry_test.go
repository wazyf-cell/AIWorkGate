package gateway

import (
	"strings"
	"testing"
)

// keyReasonVerifier implements the optional detailed verifier: the gateway must
// surface its reason in the 401 instead of the generic message.
type keyReasonVerifier struct{ reason string }

func (v keyReasonVerifier) Verify(string) (string, string, bool) { return "", "", false }
func (v keyReasonVerifier) Touch(string)                         {}
func (v keyReasonVerifier) VerifyReason(string) (string, string, string, bool) {
	return "", "", v.reason, false
}

func TestAuthenticateSurfacesVerifierReason(t *testing.T) {
	g := newTestGateway(t, Options{Keys: keyReasonVerifier{reason: "该客户端 Key 已于 2026-10-01 00:00 过期"}})
	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != 401 {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "过期") {
		t.Fatalf("body = %s, want the verifier's reason", rr.Body.String())
	}
}
