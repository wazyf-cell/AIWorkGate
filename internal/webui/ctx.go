// GET /api/gateway/ctx?id=<req_id>（task-62 六维 Treemap 一期后端）。
// 管理面 session 鉴权之下的只读端点；宿主回调（internal/app）查独立表
// ctx_compositions。未采样的请求返回 sampled=false（不是 404——前端一次
// 调用即可区分「未采样」与「无此请求」两种含义之外的状态）。
package webui

import (
	"encoding/json"
	"net/http"
)

// handleGatewayCtxSampling serves GET/POST /api/gateway/ctx-sampling（task-67：
// 双开关持久化到 config.json）。GET 回读 {"enabled","heuristics"}；POST body
// {"enabled":bool,"heuristics":bool?}——heuristics 缺省保持存储值；两开关均
// 即时生效且重启保持。
func (s *server) handleGatewayCtxSampling(w http.ResponseWriter, r *http.Request) {
	if s.opts.CtxSamplingStatus == nil || s.opts.CtxSamplingSet == nil {
		s.unavailable(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		enabled, heuristics := s.opts.CtxSamplingStatus()
		writeJSON(w, http.StatusOK, map[string]any{"enabled": enabled, "heuristics": heuristics})
		return
	case http.MethodPost:
		var body struct {
			Enabled    *bool `json:"enabled"`
			Heuristics *bool `json:"heuristics"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
			writeError(w, http.StatusBadRequest, "body 必须是 {\"enabled\":bool,\"heuristics\":bool?}（enabled 必填）", "invalid_json")
			return
		}
		if err := s.opts.CtxSamplingSet(*body.Enabled, body.Heuristics); err != nil {
			writeError(w, http.StatusInternalServerError, "ctx sampling persist failed: "+err.Error(), "internal_error")
			return
		}
		enabled, heuristics := s.opts.CtxSamplingStatus()
		writeJSON(w, http.StatusOK, map[string]any{"enabled": enabled, "heuristics": heuristics})
		return
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "仅支持 GET/POST", "method_not_allowed")
	}
}

// handleGatewayCtx serves GET /api/gateway/ctx?id=<req_id>.
func (s *server) handleGatewayCtx(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.GatewayCtxComposition == nil {
		s.unavailable(w)
		return
	}
	reqID := r.URL.Query().Get("id")
	if reqID == "" {
		writeError(w, http.StatusBadRequest, "缺少 id 参数（req_id）", "invalid_request")
		return
	}
	payload, err := s.opts.GatewayCtxComposition(reqID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ctx composition: "+err.Error(), "internal_error")
		return
	}
	if payload == nil {
		// Not sampled: the gate was closed for this request (or it predates
		// the feature). The shape stays queryable and explicit.
		writeJSON(w, http.StatusOK, map[string]any{"req_id": reqID, "sampled": false})
		return
	}
	writeJSON(w, http.StatusOK, payload)
}
