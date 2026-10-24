package webui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func notifyFixture(status *NotifyStatus, saved *NotifyUpdate) Options {
	return Options{
		GetNotify: func() (NotifyStatus, error) {
			return *status, nil
		},
		SaveNotify: func(update NotifyUpdate) (NotifyStatus, error) {
			*saved = update
			if update.Enabled != nil {
				status.Enabled = *update.Enabled
			}
			if update.Channel != nil {
				status.Channel = *update.Channel
			}
			if update.URL != nil {
				status.URLSet = strings.TrimSpace(*update.URL) != ""
			}
			if update.Events != nil {
				status.Events = append([]string(nil), (*update.Events)...)
			}
			if update.MinIntervalSeconds != nil {
				status.MinIntervalSeconds = *update.MinIntervalSeconds
			}
			return *status, nil
		},
		TestNotify: func(ctx context.Context) error {
			return nil
		},
	}
}

func TestNotifyGETShape(t *testing.T) {
	status := NotifyStatus{
		Enabled:            true,
		Channel:            "bark",
		URLSet:             true,
		Events:             []string{"checkin_fail"},
		MinIntervalSeconds: 600,
		LastSentAt:         1760000000,
		LastError:          "notify: Bark 返回 HTTP 500",
	}
	handler := New(notifyFixture(&status, &NotifyUpdate{}))

	recorder := doRequest(t, handler, http.MethodGet, "/api/notify", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/notify = %d (%s)", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	decodeJSONBody(t, recorder, &payload)
	for _, key := range []string{"enabled", "channel", "url_set", "events", "min_interval_seconds", "last_sent_at", "last_error"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("GET payload missing %q: %v", key, payload)
		}
	}
	if _, leaked := payload["url"]; leaked {
		t.Fatalf("GET payload exposed the webhook URL: %v", payload)
	}
	if payload["last_sent_at"].(float64) != 1760000000 {
		t.Fatalf("last_sent_at = %v", payload["last_sent_at"])
	}
}

func TestNotifyGETEmptyEventsIsArray(t *testing.T) {
	status := NotifyStatus{Channel: "generic", MinIntervalSeconds: 600}
	handler := New(notifyFixture(&status, &NotifyUpdate{}))
	recorder := doRequest(t, handler, http.MethodGet, "/api/notify", "")
	if got := strings.TrimSpace(recorder.Body.String()); !strings.Contains(got, `"events":[]`) {
		t.Fatalf("events should serialize as [], got %s", got)
	}
}

func TestNotifyPUTPartialUpdate(t *testing.T) {
	status := NotifyStatus{Enabled: false, Channel: "generic", Events: []string{"checkin_fail"}, MinIntervalSeconds: 600}
	saved := NotifyUpdate{}
	handler := New(notifyFixture(&status, &saved))

	recorder := doRequest(t, handler, http.MethodPut, "/api/notify",
		`{"enabled":true,"channel":"wecom","events":["checkin_fail","quota_low"],"min_interval_seconds":900}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if saved.URL != nil {
		t.Fatalf("absent url must keep the stored secret, got %v", *saved.URL)
	}
	if saved.Enabled == nil || !*saved.Enabled || saved.Channel == nil || *saved.Channel != "wecom" {
		t.Fatalf("saved = %+v", saved)
	}
	if saved.MinIntervalSeconds == nil || *saved.MinIntervalSeconds != 900 {
		t.Fatalf("saved interval = %+v", saved.MinIntervalSeconds)
	}
	if got := strings.Join(*saved.Events, ","); got != "checkin_fail,quota_low" {
		t.Fatalf("saved events = %q", got)
	}
}

func TestNotifyPUTClearsURLWithEmptyString(t *testing.T) {
	status := NotifyStatus{Channel: "generic", URLSet: true, MinIntervalSeconds: 600}
	saved := NotifyUpdate{}
	handler := New(notifyFixture(&status, &saved))
	recorder := doRequest(t, handler, http.MethodPut, "/api/notify", `{"url":""}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if saved.URL == nil || *saved.URL != "" {
		t.Fatalf("saved url = %v, want an explicit empty string", saved.URL)
	}
	if status.URLSet {
		t.Fatal("status.url_set should be false after clearing")
	}
}

func TestNotifyPUTValidation(t *testing.T) {
	status := NotifyStatus{Channel: "generic", MinIntervalSeconds: 600}
	saved := NotifyUpdate{}
	handler := New(notifyFixture(&status, &saved))

	cases := []struct {
		name string
		body string
	}{
		{"unknown event", `{"events":["checkin_fail","telegram"]}`},
		{"unknown channel", `{"channel":"email"}`},
		{"interval too small", `{"min_interval_seconds":5}`},
		{"interval too large", `{"min_interval_seconds":86401}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, handler, http.MethodPut, "/api/notify", tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s = %d (%s)", tc.body, recorder.Code, recorder.Body.String())
			}
			var payload struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			decodeJSONBody(t, recorder, &payload)
			if payload.Error.Type != "invalid_request" {
				t.Fatalf("error type = %q, want invalid_request", payload.Error.Type)
			}
			if payload.Error.Message == "" || !containsHan(payload.Error.Message) {
				t.Fatalf("message should be Chinese: %q", payload.Error.Message)
			}
		})
	}
}

func TestNotifyPUTSaveErrorMapping(t *testing.T) {
	handler := New(Options{
		GetNotify: func() (NotifyStatus, error) { return NotifyStatus{}, nil },
		SaveNotify: func(NotifyUpdate) (NotifyStatus, error) {
			return NotifyStatus{}, errors.Join(ErrInvalidRequest, errors.New("通知地址必须是完整的 http(s) URL"))
		},
	})
	recorder := doRequest(t, handler, http.MethodPut, "/api/notify", `{"url":"not-a-url"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d (%s)", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Error struct{ Type string } `json:"error"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.Error.Type != "invalid_request" {
		t.Fatalf("error type = %q", payload.Error.Type)
	}
}

func TestNotifyTestEndpoint(t *testing.T) {
	sent := 0
	handler := New(Options{
		TestNotify: func(context.Context) error {
			sent++
			return nil
		},
	})
	recorder := doRequest(t, handler, http.MethodPost, "/api/notify/test", "{}")
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST test = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != `{"ok":true}` {
		t.Fatalf("body = %s", got)
	}
	if sent != 1 {
		t.Fatalf("TestNotify calls = %d", sent)
	}

	failing := New(Options{
		TestNotify: func(context.Context) error {
			return errors.New("notify: Server酱 返回 HTTP 500")
		},
	})
	recorder = doRequest(t, failing, http.MethodPost, "/api/notify/test", "{}")
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("POST failing test = %d", recorder.Code)
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.Error.Type != "notify_failed" {
		t.Fatalf("error type = %q", payload.Error.Type)
	}
}

func TestNotifyNotImplemented(t *testing.T) {
	handler := New(Options{})
	for _, target := range []string{"/api/notify", "/api/notify/test"} {
		method := http.MethodGet
		if target == "/api/notify/test" {
			method = http.MethodPost
		}
		recorder := doRequest(t, handler, method, target, "")
		if recorder.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s = %d, want 501", method, target, recorder.Code)
		}
	}
}

func TestNotifyMethodNotAllowed(t *testing.T) {
	handler := New(notifyFixture(&NotifyStatus{}, &NotifyUpdate{}))
	recorder := doRequest(t, handler, http.MethodDelete, "/api/notify", "")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodGet, "/api/notify/test", "")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET test = %d", recorder.Code)
	}
}

// containsHan reports whether s contains at least one CJK character, a cheap
// guard that the user-facing validation text stayed Chinese.
func containsHan(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}
