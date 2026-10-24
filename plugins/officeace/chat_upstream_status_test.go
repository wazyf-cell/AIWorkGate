package main

import (
	"fmt"
	"io"
	"net/http"
	"testing"
)

// TestUpstream4xxKeepsItsStatus: a MaaS 404 (ModelArts.81009 "Invalid model")
// must reach the client as 404. Wrapping every 4xx as 400 told Chat clients the
// request was malformed, so they never surfaced "the deployment is gone".
// 429 stays a 502 upstream_rate_limited on purpose: the gateway's own 429 means
// the client exhausted its key quota, and the host retries a 502.
func TestUpstream4xxKeepsItsStatus(t *testing.T) {
	cases := []struct {
		upstream int
		want     int
		wantType string
	}{
		{http.StatusNotFound, http.StatusNotFound, "upstream_error"},
		{http.StatusUnprocessableEntity, http.StatusUnprocessableEntity, "upstream_error"},
		{http.StatusBadRequest, http.StatusBadRequest, "upstream_error"},
		{http.StatusInternalServerError, http.StatusBadGateway, "upstream_error"},
		{http.StatusTooManyRequests, http.StatusBadGateway, "upstream_rate_limited"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.upstream), func(t *testing.T) {
			h := setupChat(t, func(w http.ResponseWriter, r *http.Request, body string) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.upstream)
				_, _ = io.WriteString(w, `{"error":{"code":"ModelArts.81009","message":"Invalid model.","type":"NotFound"}}`)
			})
			rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.want, rec.Body.String())
			}
			if env := decodeAs[errorEnvelope](t, rec); env.Error.Type != tc.wantType {
				t.Fatalf("type = %q, want %q", env.Error.Type, tc.wantType)
			}
		})
	}
}
