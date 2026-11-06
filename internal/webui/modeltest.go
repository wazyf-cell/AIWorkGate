package webui

// Model test API (P12-1, decisions D-27 and D-39).
//
//	POST /api/models/{provider}/{model}/test
//	  -> 200 {"ok","status","ms","model","error","otherAPI"}
//
//	POST /api/models/test
//	  {"items":[{"provider":"trae","model":"trae-cn/claude-sonnet-4"},...]}
//	  -> 200 {"results":[{...},...]} (same order as the request)
//
// The single-test result mirrors magpie's provider/test.go Result: ok says the
// minimal request completed, status is the HTTP status the plugin answered (0
// for a transport failure), ms is the wall time of that request, model is the
// tested id as "provider/model", error carries a redacted human reason and
// otherAPI marks "the vendor says this model does not live on this API" so the
// UI can suggest switching the endpoint instead of declaring the model dead.
//
// D-39 tiers are chosen by the host, not by the client: 20s (light), 60s
// (standard) or 2min (reasoning, streamed) with max_tokens in 16..1024. The
// batch endpoint runs providers in parallel and persists each result in the
// diagnostics probes table (kind "model_test") through the host callback.
//
// The transport only validates and shapes; running the request, selecting the
// tier, classifying otherAPI and persisting the probe belong to the host.

import (
	"net/http"
	"strings"
)

const (
	modelTestBatchMaxItems = 50
)

// ModelTestItem is one request row of POST /api/models/test.
type ModelTestItem struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// ModelTestResult is one model test outcome (magpie Result shape).
type ModelTestResult struct {
	OK       bool   `json:"ok"`
	Status   int    `json:"status"`
	MS       int64  `json:"ms"`
	Model    string `json:"model"`
	Error    string `json:"error,omitempty"`
	OtherAPI bool   `json:"otherAPI"`
}

// handleModelTest serves POST /api/models/{provider}/{model}/test.
func (s *server) handleModelTest(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.ModelTest == nil {
		s.unavailable(w)
		return
	}
	provider := strings.TrimSpace(r.PathValue("provider"))
	model := strings.TrimSpace(r.PathValue("model"))
	if provider == "" || model == "" {
		writeError(w, http.StatusBadRequest, "provider 和 model 不能为空", "invalid_request")
		return
	}
	result, err := s.opts.ModelTest(provider, model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "模型测试失败: "+err.Error(), "internal_error")
		return
	}
	if strings.TrimSpace(result.Model) == "" {
		result.Model = provider + "/" + model
	}
	writeJSON(w, http.StatusOK, result)
}

// handleModelTestBatch serves POST /api/models/test.
func (s *server) handleModelTestBatch(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.ModelTestBatch == nil {
		s.unavailable(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		Items []ModelTestItem `json:"items"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Items) == 0 {
		writeError(w, http.StatusBadRequest, "items 不能为空", "invalid_request")
		return
	}
	if len(body.Items) > modelTestBatchMaxItems {
		writeError(w, http.StatusBadRequest, "单次最多测试 50 个模型", "invalid_request")
		return
	}
	for i := range body.Items {
		body.Items[i].Provider = strings.TrimSpace(body.Items[i].Provider)
		body.Items[i].Model = strings.TrimSpace(body.Items[i].Model)
		if body.Items[i].Provider == "" || body.Items[i].Model == "" {
			writeError(w, http.StatusBadRequest, "items 中 provider 和 model 不能为空", "invalid_request")
			return
		}
	}
	results, err := s.opts.ModelTestBatch(body.Items)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "批量模型测试失败: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
