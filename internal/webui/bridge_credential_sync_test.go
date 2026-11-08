package webui

// task-90 单测：宿主→桥凭据转发端点。
// 覆盖：①请求体形状与字段 ②成功路径脱敏响应 ③桥 401/不可达映射 ④幂等。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"aiworkgate/internal/model"
)

func bridgeTestHandler(t *testing.T, bridgeStatus int, bridgeBody string, captureBodies *[]map[string]any, callCount *atomic.Int32) http.Handler {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if captureBodies != nil {
			var body map[string]any
			_ = json.NewDecoder(req.Body).Decode(&body)
			*captureBodies = append(*captureBodies, body)
		}
		if callCount != nil {
			callCount.Add(1)
		}
		w.WriteHeader(bridgeStatus)
		_, _ = w.Write([]byte(bridgeBody))
	}))
	t.Cleanup(bridge.Close)

	return New(Options{
		Status: func() model.HostStatus { return model.HostStatus{} },
		BridgeCredentials: func(pluginID string) ([]BridgeCredentialItem, error) {
			return []BridgeCredentialItem{
				{AccountID: "acc-1", Secret: map[string]any{"access": "ak_test", "refresh": "rk_test"}, ExpiresAt: 1794144248},
				{AccountID: "acc-2", Secret: map[string]any{"access": "ak_2"}},
			}, nil
		},
		PluginBridgeEndpoint: func(pluginID string) (string, string, bool) {
			return bridge.URL, "bridge-bearer-token", true
		},
	})
}

func TestBridgeCredentialSyncSuccess(t *testing.T) {
	var captured []map[string]any
	var calls atomic.Int32
	handler := bridgeTestHandler(t, http.StatusOK, `{"ok":true}`, &captured, &calls)

	recorder := doRequest(t, handler, http.MethodPost, "/api/plugins/mock-bridge/credentials/sync", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// 请求体形状：桥应收到 2 笔 POST（每 credential 一笔），逐笔形状正确
	if len(captured) != 2 {
		t.Fatalf("bridge received %d POSTs, want 2", len(captured))
	}
	if captured[0]["plugin_id"] != "mock-bridge" {
		t.Fatalf("bridge body[0] plugin_id = %v", captured[0]["plugin_id"])
	}
	if captured[0]["account_id"] != "acc-1" {
		t.Fatalf("bridge body[0] account_id = %v", captured[0]["account_id"])
	}
	creds, ok := captured[0]["credentials"].(map[string]any)
	if !ok || creds["access"] != "ak_test" {
		t.Fatalf("bridge body[0] credentials = %v", captured[0]["credentials"])
	}
	if captured[1]["account_id"] != "acc-2" {
		t.Fatalf("bridge body[1] account_id = %v", captured[1]["account_id"])
	}

	// 响应脱敏：只含 keys（字段名列表），不含凭据值
	var result BridgeSyncResult
	decodeJSONBody(t, recorder, &result)
	if result.PluginID != "mock-bridge" || result.Sent != 2 || result.Failed != 0 {
		t.Fatalf("sync result = sent=%d failed=%d plugin=%s", result.Sent, result.Failed, result.PluginID)
	}
	for _, ch := range result.Channels {
		if !ch.OK {
			t.Fatalf("channel %s should be ok", ch.AccountID)
		}
		for _, k := range ch.Keys {
			if k == "ak_test" || k == "rk_test" {
				t.Fatalf("credential value leaked into response: %s", k)
			}
		}
	}
}

func TestBridgeCredentialSyncBridge401(t *testing.T) {
	handler := bridgeTestHandler(t, http.StatusUnauthorized, `{"error":{"message":"unauthorized"}}`, nil, nil)
	recorder := doRequest(t, handler, http.MethodPost, "/api/plugins/p1/credentials/sync", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP status = %d (should still be 200 with per-channel fail)", recorder.Code)
	}
	var result BridgeSyncResult
	decodeJSONBody(t, recorder, &result)
	if result.Failed != 2 || result.Sent != 0 {
		t.Fatalf("failed=%d sent=%d, want all failed", result.Failed, result.Sent)
	}
	for _, ch := range result.Channels {
		if ch.OK {
			t.Fatalf("channel %s should not be ok on 401", ch.AccountID)
		}
		if !strings.Contains(ch.Error, "401") {
			t.Fatalf("channel error should mention 401: %s", ch.Error)
		}
	}
}

func TestBridgeCredentialSyncBridgeUnreachable(t *testing.T) {
	handler := New(Options{
		Status: func() model.HostStatus { return model.HostStatus{} },
		BridgeCredentials: func(pluginID string) ([]BridgeCredentialItem, error) {
			return []BridgeCredentialItem{
				{AccountID: "acc-1", Secret: map[string]any{"access": "ak_x"}},
			}, nil
		},
		PluginBridgeEndpoint: func(pluginID string) (string, string, bool) {
			return "http://127.0.0.1:1", "tok", true // port 1 = unreachable
		},
	})
	recorder := doRequest(t, handler, http.MethodPost, "/api/plugins/p1/credentials/sync", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP status = %d", recorder.Code)
	}
	var result BridgeSyncResult
	decodeJSONBody(t, recorder, &result)
	if result.Failed != 1 || result.Sent != 0 {
		t.Fatalf("failed=%d sent=%d, want 1 failed", result.Failed, result.Sent)
	}
	if !strings.Contains(result.Channels[0].Error, "unreachable") {
		t.Fatalf("error should mention unreachable: %s", result.Channels[0].Error)
	}
}

func TestBridgeCredentialSyncIdempotent(t *testing.T) {
	var calls atomic.Int32
	handler := bridgeTestHandler(t, http.StatusOK, `{"ok":true}`, nil, &calls)
	// 同一 handler 调两次 → 桥收两次 POST，但桥的 put 是 upsert → 幂等
	doRequest(t, handler, http.MethodPost, "/api/plugins/p1/credentials/sync", "")
	doRequest(t, handler, http.MethodPost, "/api/plugins/p1/credentials/sync", "")
	if got := calls.Load(); got != 4 { // 2 credentials × 2 calls
		t.Fatalf("bridge calls = %d, want 4 (2 creds × 2 sync calls)", got)
	}
}

func TestBridgeCredentialSyncNoCredentials(t *testing.T) {
	handler := New(Options{
		Status: func() model.HostStatus { return model.HostStatus{} },
		BridgeCredentials: func(pluginID string) ([]BridgeCredentialItem, error) {
			return nil, nil
		},
		PluginBridgeEndpoint: func(pluginID string) (string, string, bool) {
			return "http://127.0.0.1:19999", "tok", true
		},
	})
	recorder := doRequest(t, handler, http.MethodPost, "/api/plugins/p1/credentials/sync", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var result BridgeSyncResult
	decodeJSONBody(t, recorder, &result)
	if result.Sent != 0 || result.Failed != 0 || len(result.Channels) != 0 {
		t.Fatalf("empty credentials should return empty result: %+v", result)
	}
}

func TestBridgeCredentialSyncNotWired(t *testing.T) {
	handler := New(Options{Status: func() model.HostStatus { return model.HostStatus{} }})
	recorder := doRequest(t, handler, http.MethodPost, "/api/plugins/p1/credentials/sync", "")
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", recorder.Code)
	}
}
