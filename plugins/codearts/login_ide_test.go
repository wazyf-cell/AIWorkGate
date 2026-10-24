package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBuildIDEAuthorizeURL(t *testing.T) {
	const ticket = "11111111-2222-4333-8444-555555555555"
	raw := buildIDEAuthorizeURL(32123, ticket)

	outer, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse login url: %v", err)
	}
	if outer.Host != "auth.huaweicloud.com" || outer.Path != "/authui/login.html" {
		t.Fatalf("outer url = %s, want Huawei auth page", raw)
	}
	inner, err := url.Parse(outer.Query().Get("service"))
	if err != nil {
		t.Fatalf("parse service: %v", err)
	}
	if inner.Host != "devcloud.cn-north-4.huaweicloud.com" || inner.Path != "/doer/redirect" {
		t.Fatalf("service = %s, want doer redirect", inner.String())
	}
	q := inner.Query()
	if q.Get("IdeaType") != "jetbrains" {
		t.Errorf("IdeaType = %q", q.Get("IdeaType"))
	}
	if got, want := q.Get("auth_callback_url"), "http://127.0.0.1:32123/authentication"; got != want {
		t.Errorf("auth_callback_url = %q, want %q", got, want)
	}
	if q.Get("plugin-name") != idePluginName || q.Get("plugin-version") != idePluginVersion {
		t.Errorf("plugin = %q/%q, want %q/%q",
			q.Get("plugin-name"), q.Get("plugin-version"), idePluginName, idePluginVersion)
	}
	if q.Get("ticket_id") != ticket {
		t.Errorf("ticket_id = %q, want %q", q.Get("ticket_id"), ticket)
	}
}

func TestIDEPluginLoginStartUsesIDEChannel(t *testing.T) {
	p, _ := newTestPlugin(t, nil)
	s, err := p.startLoginSession("ide_plugin")
	if err != nil {
		t.Fatalf("startLoginSession: %v", err)
	}
	defer p.cancelAllSessions()

	if s.ticketPluginName != idePluginName || s.ticketPluginVersion != idePluginVersion {
		t.Fatalf("ticket identity = %q/%q, want %q/%q",
			s.ticketPluginName, s.ticketPluginVersion, idePluginName, idePluginVersion)
	}
	if !strings.Contains(s.URL, "authui/login.html") || !strings.Contains(s.URL, "IdeaType%3Djetbrains") {
		t.Fatalf("url = %q, want wrapped doer authorize url", s.URL)
	}
	if len(s.ticket) != 36 || strings.Count(s.ticket, "-") != 4 {
		t.Fatalf("ticket = %q, want a UUID", s.ticket)
	}
}

func TestIDEPluginLoginRejectsTokenOnlyCallback(t *testing.T) {
	p, _ := newTestPlugin(t, nil)
	s := &loginSession{ID: "ls_ide_token", Status: "pending", ticket: "t"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, ideCallbackPath+"?access_token=raw-token", nil)
	p.handleIDECallback(s, rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if status, _, msg := s.snapshot(); status != "failed" || !strings.Contains(msg, "secret") {
		t.Fatalf("session = %q/%q, want failed with secret hint", status, msg)
	}
}

func TestIDEPollSendsJetbrainsIdentity(t *testing.T) {
	var gotName, gotVersion, gotTicket, gotSecret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotName = r.Header.Get("plugin-name")
		gotVersion = r.Header.Get("plugin-version")
		gotTicket = r.URL.Query().Get("ticket_id")
		gotSecret = r.URL.Query().Get("secret")
		writeJSON(w, http.StatusOK, map[string]any{
			"credential": map[string]any{
				"access":        "AK-ide",
				"secret":        "SK-ide",
				"securitytoken": "ST-ide",
				"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			},
		})
	}))
	defer srv.Close()

	p, _ := newTestPlugin(t, func(o *Options) {
		o.Endpoints.Ticket = srv.URL
	})
	s := &loginSession{
		ID: "ls_ide_poll", ticket: "ticket-ide",
		ticketPluginName: idePluginName, ticketPluginVersion: idePluginVersion,
	}
	cred, err := p.pollTicketCredential(context.Background(), s, "secret-ide", 3*time.Second)
	if err != nil {
		t.Fatalf("pollTicketCredential: %v", err)
	}
	if gotName != idePluginName || gotVersion != idePluginVersion {
		t.Fatalf("poll identity = %q/%q, want %q/%q", gotName, gotVersion, idePluginName, idePluginVersion)
	}
	if gotTicket != "ticket-ide" || gotSecret != "secret-ide" {
		t.Fatalf("poll query = ticket %q secret %q", gotTicket, gotSecret)
	}
	if cred.AccessKeyID != "AK-ide" || cred.SecurityToken != "ST-ide" {
		t.Fatalf("credential = %+v", cred)
	}
}
