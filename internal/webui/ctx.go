// GET /api/gateway/ctx?id=<req_id>（task-62 六维 Treemap 一期后端）。
// 管理面 session 鉴权之下的只读端点；宿主回调（internal/app）查独立表
// ctx_compositions。未采样的请求返回 sampled=false（不是 404——前端一次
// 调用即可区分「未采样」与「无此请求」两种含义之外的状态）。
package webui

import (
	"encoding/json"
	"net/http"
)

// handleGatewayCtxSampling serves GET/POST /api/gateway/ctx-sampling（sticky
// 门控开关；POST body {"enabled":true|false}，GET 回读当前态）。
func (s *server) handleGatewayCtxSampling(w http.ResponseWriter, r *http.Request) {
	if s.opts.CtxSamplingGet == nil || s.opts.CtxSamplingSet == nil {
		s.unavailable(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"enabled": s.opts.CtxSamplingGet()})
		return
	case http.MethodPost:
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "body 必须是 {\"enabled\":bool}", "invalid_json")
			return
		}
		s.opts.CtxSamplingSet(body.Enabled)
		writeJSON(w, http.StatusOK, map[string]any{"enabled": body.Enabled})
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
