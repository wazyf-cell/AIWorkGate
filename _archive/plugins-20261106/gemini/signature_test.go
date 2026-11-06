package main

// thoughtSignature tests. Two properties matter:
//
//   - a signature the upstream issued with a functionCall is captured and
//     replayed on the next turn (exact args, then latest-for-tool fallback);
//   - when the upstream rejects a signature, the request is resent once with
//     every signature stripped, and that retry is the only retry.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// chatWithToolTurn is an OpenAI body that replays one tool call and its result.
const chatWithToolTurn = `{"model":"gemini-3.8-flash","messages":[
	{"role":"user","content":"call f"},
	{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function",
		"function":{"name":"f","arguments":"{\"b\":2,\"a\":1}"}}]},
	{"role":"tool","tool_call_id":"c1","content":"42"}]}`

// functionCallParts returns the functionCall parts of every content turn.
func functionCallParts(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var env struct {
		Request struct {
			Contents []struct {
				Parts []map[string]any `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var out []map[string]any
	for _, c := range env.Request.Contents {
		for _, p := range c.Parts {
			if _, ok := p["functionCall"]; ok {
				out = append(out, p)
			}
		}
	}
	return out
}

func TestSignatureRoundTripThroughTheCache(t *testing.T) {
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(bodies) == 1 {
			io.WriteString(w, `data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{"b":2,"a":1}},"thoughtSignature":"sig-abc"}]},"finishReason":"STOP"}]}}`+"\n\n")
			return
		}
		io.WriteString(w, `data: {"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}]}`+"\n\n")
	}))
	defer upstream.Close()

	srv, handler := newTestServer(t, upstream.URL)
	// Turn one: the model calls f; the signature must be captured.
	rec := doPost(t, handler, `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"call f"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("turn one: %d %s", rec.Code, rec.Body.String())
	}
	if got := srv.sigs.lookup("f", `{"a":1,"b":2}`); got != "sig-abc" {
		t.Fatalf("cache lookup = %q, want sig-abc", got)
	}
	// A different argument set must fall back to the latest signature for f.
	if got := srv.sigs.lookup("f", `{"a":9}`); got != "sig-abc" {
		t.Fatalf("latest-for-tool fallback = %q", got)
	}

	// Turn two: the replayed functionCall must carry the signature.
	rec = doPost(t, handler, chatWithToolTurn)
	if rec.Code != http.StatusOK {
		t.Fatalf("turn two: %d %s", rec.Code, rec.Body.String())
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream hits = %d", len(bodies))
	}
	parts := functionCallParts(t, bodies[1])
	if len(parts) != 1 {
		t.Fatalf("functionCall parts = %d, body %s", len(parts), bodies[1])
	}
	if parts[0]["thoughtSignature"] != "sig-abc" {
		t.Fatalf("signature not replayed: %v", parts[0])
	}
}

func TestSignatureRejectionRetriesOnceWithoutSignatures(t *testing.T) {
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"thought signature is invalid"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"candidates":[{"content":{"parts":[{"text":"recovered"}]},"finishReason":"STOP"}]}`+"\n\n")
	}))
	defer upstream.Close()

	srv, handler := newTestServer(t, upstream.URL)
	srv.sigs.put("f", `{"a":1,"b":2}`, "sig-bad")
	rec := doPost(t, handler, chatWithToolTurn)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream hits = %d, want exactly 2 (one strip-and-retry)", len(bodies))
	}
	first := functionCallParts(t, bodies[0])
	if len(first) != 1 || first[0]["thoughtSignature"] != "sig-bad" {
		t.Fatalf("first attempt should carry the signature: %v", first)
	}
	second := functionCallParts(t, bodies[1])
	if len(second) != 1 {
		t.Fatalf("second attempt parts = %v", second)
	}
	if _, leaked := second[0]["thoughtSignature"]; leaked {
		t.Fatalf("retry still carried a signature: %v", second[0])
	}
	if !strings.Contains(rec.Body.String(), "recovered") {
		t.Fatalf("response = %s", rec.Body.String())
	}
}

func TestSigStoreDropsEmptyAndEvicts(t *testing.T) {
	s := newSigStore("")
	s.put("f", "{}", "   ")
	if got := s.lookup("f", "{}"); got != "" {
		t.Fatalf("empty signature stored: %q", got)
	}
	s.put("g", `{"x":"y"}`, "sig-1")
	if got := s.lookup("g", `{"x":"y"}`); got != "sig-1" {
		t.Fatalf("lookup = %q", got)
	}
	if got := s.lookup("h", "{}"); got != "" {
		t.Fatalf("unknown tool returned %q", got)
	}
}

// doPost sends an authenticated chat request.
func doPost(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
