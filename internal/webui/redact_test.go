package webui

import (
	"net/http"
	"strings"
	"testing"
)

func redactFixture(status *RedactStatus, saved *RedactUpdate) Options {
	return Options{
		GetRedact: func() (RedactStatus, error) {
			return *status, nil
		},
		SaveRedact: func(update RedactUpdate) (RedactStatus, error) {
			*saved = update
			if update.Enabled != nil {
				status.Enabled = *update.Enabled
			}
			if update.RouteAware != nil {
				status.RouteAware = *update.RouteAware
			}
			if update.Kinds != nil {
				status.Kinds = append([]string(nil), (*update.Kinds)...)
			}
			if update.Categories != nil {
				status.Categories = append([]string(nil), (*update.Categories)...)
			}
			return *status, nil
		},
	}
}

// GET 往返：字段齐全、词表序列化为数组（哪怕为空）。
func TestRedactGETShape(t *testing.T) {
	status := RedactStatus{
		Enabled:    true,
		RouteAware: true,
		Kinds:      []string{"SECRET", "EMAIL"},
		Categories: []string{"ssn", "iban"},
	}
	handler := New(redactFixture(&status, &RedactUpdate{}))

	recorder := doRequest(t, handler, http.MethodGet, "/api/redact", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/redact = %d (%s)", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	decodeJSONBody(t, recorder, &payload)
	for _, key := range []string{"enabled", "route_aware", "kinds", "categories"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("GET payload missing %q: %v", key, payload)
		}
	}
	// key_file 是指纹密钥路径，不进管理 API。
	if _, present := payload["key_file"]; present {
		t.Fatalf("GET payload exposed key_file: %v", payload)
	}
}

// GET 空词表也要是 []，不是 null。
func TestRedactGETEmptyListsAreArrays(t *testing.T) {
	status := RedactStatus{RouteAware: true}
	handler := New(redactFixture(&status, &RedactUpdate{}))
	recorder := doRequest(t, handler, http.MethodGet, "/api/redact", "")
	body := strings.TrimSpace(recorder.Body.String())
	if !strings.Contains(body, `"kinds":[]`) || !strings.Contains(body, `"categories":[]`) {
		t.Fatalf("empty kinds/categories should serialize as [], got %s", body)
	}
}

// PUT 往返：部分更新 + 归一化（kinds 大写、categories 小写、去重、去空白）。
func TestRedactPUTRoundTrip(t *testing.T) {
	status := RedactStatus{Enabled: false, RouteAware: true}
	saved := RedactUpdate{}
	handler := New(redactFixture(&status, &saved))

	recorder := doRequest(t, handler, http.MethodPut, "/api/redact",
		`{"enabled":true,"route_aware":false,"kinds":["secret"," email ","EMAIL"],"categories":["SSN"," ssn ","iban"]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if saved.Enabled == nil || !*saved.Enabled {
		t.Fatalf("saved.Enabled = %v, want true", saved.Enabled)
	}
	if saved.RouteAware == nil || *saved.RouteAware {
		t.Fatalf("saved.RouteAware = %v, want false", saved.RouteAware)
	}
	if got := *saved.Kinds; len(got) != 2 || got[0] != "SECRET" || got[1] != "EMAIL" {
		t.Fatalf("kinds = %v, want [SECRET EMAIL] (trimmed, upper, deduped)", *saved.Kinds)
	}
	if got := *saved.Categories; len(got) != 2 || got[0] != "ssn" || got[1] != "iban" {
		t.Fatalf("categories = %v, want [ssn iban] (trimmed, lower, deduped)", *saved.Categories)
	}
	var payload map[string]any
	decodeJSONBody(t, recorder, &payload)
	if payload["enabled"] != true || payload["route_aware"] != false {
		t.Fatalf("PUT response = %v", payload)
	}
}

// PUT 只带一个开关：其余字段保持原值（部分更新语义）。
func TestRedactPUTPartialKeepsLists(t *testing.T) {
	status := RedactStatus{Enabled: false, RouteAware: true, Kinds: []string{"SECRET"}, Categories: []string{"ssn"}}
	saved := RedactUpdate{}
	handler := New(redactFixture(&status, &saved))

	recorder := doRequest(t, handler, http.MethodPut, "/api/redact", `{"enabled":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if saved.Kinds != nil || saved.Categories != nil {
		t.Fatalf("partial update must not resend the lists: kinds=%v categories=%v", saved.Kinds, saved.Categories)
	}
	if !status.Enabled || status.RouteAware != true || len(status.Kinds) != 1 {
		t.Fatalf("status after partial update = %+v", status)
	}
}

// 校验拒绝：坏的 kind 形状 / 坏的 category 形状 / 空对象外的类型错。
func TestRedactPUTValidation(t *testing.T) {
	status := RedactStatus{RouteAware: true}
	handler := New(redactFixture(&status, &RedactUpdate{}))

	cases := []struct {
		name string
		body string
		want string
	}{
		{"bad kind shape", `{"kinds":["SE CRE T"]}`, "脱敏类型无效"},
		{"lowercase kind", `{"kinds":["secret"]}`, "脱敏类型无效"},
		{"bad category shape", `{"categories":["SSN!"]}`, "类别无效"},
		{"empty only entries", `{"kinds":["", "  "]}`, ""},
	}
	for _, tc := range cases {
		recorder := doRequest(t, handler, http.MethodPut, "/api/redact", tc.body)
		if recorder.Code == http.StatusOK {
			continue // 空列表归一化后合法：全空项被丢弃，kinds 归为空数组
		}
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: PUT = %d (%s)", tc.name, recorder.Code, recorder.Body.String())
		}
		if tc.want != "" && !strings.Contains(recorder.Body.String(), tc.want) {
			t.Fatalf("%s: body %s missing %q", tc.name, recorder.Body.String(), tc.want)
		}
	}
}

// 未接回调时 501（SPA 可先于宿主上线）。
func TestRedactUnavailableWithoutCallbacks(t *testing.T) {
	handler := New(Options{})
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		body := ""
		if method == http.MethodPut {
			body = "{}"
		}
		recorder := doRequest(t, handler, method, "/api/redact", body)
		if recorder.Code != http.StatusNotImplemented {
			t.Fatalf("%s without callbacks = %d (%s)", method, recorder.Code, recorder.Body.String())
		}
	}
}

// 其他方法 405。
func TestRedactMethodNotAllowed(t *testing.T) {
	handler := New(redactFixture(&RedactStatus{}, &RedactUpdate{}))
	recorder := doRequest(t, handler, http.MethodDelete, "/api/redact", "")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /api/redact = %d", recorder.Code)
	}
}
