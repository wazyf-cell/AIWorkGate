package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// D-56: protocol slots. customSlotFor resolves one inbound face onto its
// outbound protocol/base; these tests lock the fallback ladder (slot → legacy
// default), the invalid-config errors and the forwarding behaviour.

func TestCustomSlotSelection(t *testing.T) {
	base, err := customSlotFor(CustomModel{ID: "cm", Name: "n", Protocol: "openai", BaseURL: "https://a.example"}, CustomFaceChat)
	if err != nil {
		t.Fatalf("plain model slot: %v", err)
	}
	if base.Protocol != "openai" || base.BaseURL != "https://a.example" {
		t.Fatalf("plain model slot = %+v", base)
	}
	// Unknown face is a programming error and must fail loudly.
	if _, err := customSlotFor(CustomModel{Protocol: "openai", BaseURL: "https://a"}, "completions"); err == nil {
		t.Fatal("unknown face accepted")
	}

	slots := CustomModel{
		ID: "cm", Name: "n", Protocol: "openai", BaseURL: "https://a.example",
		Protocols:     map[string]string{"messages": "anthropic", "responses": "gemini", "chat": "openai"},
		ProtocolPaths: map[string]string{"messages": "https://m.example", "responses": "https://r.example"},
	}
	for face, wantProtocol := range map[string]string{
		CustomFaceChat:      "openai",
		CustomFaceResponses: "gemini",
		CustomFaceMessages:  "anthropic",
	} {
		got, err := customSlotFor(slots, face)
		if err != nil {
			t.Fatalf("face %s: %v", face, err)
		}
		if got.Protocol != wantProtocol {
			t.Fatalf("face %s protocol = %q, want %q", face, got.Protocol, wantProtocol)
		}
	}
	if got, _ := customSlotFor(slots, CustomFaceChat); got.BaseURL != "https://a.example" {
		t.Fatalf("chat slot path = %q, want the legacy base (path slot absent)", got.BaseURL)
	}
	if got, _ := customSlotFor(slots, CustomFaceMessages); got.BaseURL != "https://m.example" {
		t.Fatalf("messages slot path = %q", got.BaseURL)
	}

	// Partial slot: protocol from the slot, path from the legacy default.
	partial := CustomModel{
		Protocol: "gemini", BaseURL: "https://legacy.example",
		Protocols: map[string]string{"messages": "openai"},
	}
	got, err := customSlotFor(partial, CustomFaceMessages)
	if err != nil {
		t.Fatalf("partial slot: %v", err)
	}
	if got.Protocol != "openai" || got.BaseURL != "https://legacy.example" {
		t.Fatalf("partial slot = %+v, want openai @ legacy base", got)
	}
	// The untouched faces keep the legacy pair wholesale.
	gotChat, err := customSlotFor(partial, CustomFaceChat)
	if err != nil || gotChat.Protocol != "gemini" || gotChat.BaseURL != "https://legacy.example" {
		t.Fatalf("chat face after partial slot = %+v (err %v)", gotChat, err)
	}

	// Alias spellings fold to the canonical protocol names.
	aliased := CustomModel{
		Protocol: "openai-compat", BaseURL: "https://a.example",
		Protocols: map[string]string{"messages": "claude"},
	}
	got, err = customSlotFor(aliased, CustomFaceMessages)
	if err != nil || got.Protocol != "anthropic" {
		t.Fatalf("alias slot = %+v (err %v), want anthropic", got, err)
	}
}

func TestCustomSlotInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		cm   CustomModel
		face string
		want string
	}{
		{
			name: "unknown slot key",
			cm: CustomModel{Protocol: "openai", BaseURL: "https://a",
				Protocols: map[string]string{"completions": "openai"}},
			face: CustomFaceChat,
			want: "协议槽键",
		},
		{
			name: "unknown slot protocol",
			cm: CustomModel{Protocol: "openai", BaseURL: "https://a",
				Protocols: map[string]string{"messages": "grpc"}},
			face: CustomFaceMessages,
			want: "槽协议",
		},
		{
			name: "no base url anywhere",
			cm:   CustomModel{Protocol: "openai"},
			face: CustomFaceChat,
			want: "API 地址",
		},
	}
	for _, tc := range cases {
		_, err := customSlotFor(tc.cm, tc.face)
		if err == nil {
			t.Fatalf("%s: accepted invalid config", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}
}

// TestCustomSlotForwardingPerFaceURL proves the request goes out through the
// selected slot: the messages face reaches the anthropic slot's endpoint while
// the chat face still uses the legacy OpenAI base.
func TestCustomSlotForwardingPerFaceURL(t *testing.T) {
	chatSrv, chatRec := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"choices":[{"message":{"content":"chat ok"}}]}`))
	msgSrv, msgRec := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","content":[{"type":"text","text":"messages ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})

	cm := CustomModel{
		ID: "cm-slot", Name: "dual-face", Protocol: "openai", BaseURL: chatSrv.URL,
		UpstreamModel: "up-1", Enabled: true,
		Protocols:     map[string]string{"messages": "anthropic"},
		ProtocolPaths: map[string]string{"messages": msgSrv.URL},
	}
	g := newTestGateway(t, customFixture(t, []CustomModel{cm}, customUpstreamKey, Options{Plugins: newFakeRegistry()}))

	rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
		`{"model":"dual-face","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("messages status = %d (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"text":"messages ok"`) {
		t.Fatalf("messages reply = %s", rr.Body.String())
	}
	got := msgRec.last(t)
	if got.Path != "/v1/messages" {
		t.Fatalf("messages face hit %q, want the slot's /v1/messages", got.Path)
	}
	if got.Header.Get("x-api-key") != customUpstreamKey || got.Header.Get("anthropic-version") == "" {
		t.Fatalf("messages face did not speak anthropic: %+v", got.Header)
	}
	if chatRec.count() != 0 {
		t.Fatalf("messages face leaked to the chat base (%d calls)", chatRec.count())
	}

	rr2 := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"dual-face","messages":[{"role":"user","content":"hi"}]}`))
	if rr2.Code != http.StatusOK {
		t.Fatalf("chat status = %d (body %s)", rr2.Code, rr2.Body.String())
	}
	chatHit := chatRec.last(t)
	if chatHit.Path != "/v1/chat/completions" {
		t.Fatalf("chat face hit %q, want the legacy /v1/chat/completions", chatHit.Path)
	}
	if chatHit.Header.Get("Authorization") != "Bearer "+customUpstreamKey {
		t.Fatalf("chat face auth = %q, want bearer key", chatHit.Authorization)
	}

	// The responses face has no slot of its own here: it falls back to the
	// legacy default (documented trade-off) and reaches the chat base.
	chatCalls := chatRec.count()
	rr3 := serve(g, newChatRequest(t, "/v1/responses", clientKey,
		`{"model":"dual-face","input":"hi"}`))
	if rr3.Code != http.StatusOK {
		t.Fatalf("responses status = %d (body %s)", rr3.Code, rr3.Body.String())
	}
	if got := chatRec.count() - chatCalls; got != 1 {
		t.Fatalf("responses face calls on the chat base = %d, want 1", got)
	}
}

// TestCustomSlotInvalidForwardFails 清楚报错路径：非法槽配置在转发前失败，
// 上游不被调用。
func TestCustomSlotInvalidForwardFails(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	cm := CustomModel{
		ID: "cm-bad", Name: "bad-slot", Protocol: "openai", BaseURL: srv.URL,
		UpstreamModel: "up-1", Enabled: true,
		Protocols: map[string]string{"messages": "grpc"},
	}
	g := newTestGateway(t, customFixture(t, []CustomModel{cm}, customUpstreamKey, Options{Plugins: newFakeRegistry()}))

	rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
		`{"model":"bad-slot","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d (body %s), want 502 with a clear message", rr.Code, rr.Body.String())
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil || !strings.Contains(payload.Error.Message, "槽协议") {
		t.Fatalf("error body = %s (err %v), want 槽协议 message", rr.Body.String(), err)
	}
	if rec.count() != 0 {
		t.Fatalf("upstream received %d calls, want 0", rec.count())
	}
}
