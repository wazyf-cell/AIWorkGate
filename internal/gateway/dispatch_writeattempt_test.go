package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A transport failure leaves res.status at its zero value. Feeding that to
// writeAttempt must not reach WriteHeader(0): net/http panics on an invalid
// status code.
func TestWriteAttemptHandlesZeroStatusAsBadGateway(t *testing.T) {
	g := &Gateway{}
	rec := &reqRecord{}
	res := &upstreamAttemptResult{errText: "dial tcp 127.0.0.1:1: connect: connection refused"}
	w := httptest.NewRecorder()

	g.writeAttempt(w, rec, res)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if rec.status != http.StatusServiceUnavailable {
		t.Fatalf("rec.status = %d, want %d (the request log must not record 0)", rec.status, http.StatusServiceUnavailable)
	}
}
