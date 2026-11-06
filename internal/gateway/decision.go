package gateway

// P6-3..9: generic structured-decision routing (D-12/D-18). The host does not
// hard-code a vendor: config.decision.upstreams names a base_url, protocol and
// model id, and /v1/systemone (or its alias /v1/decision) forwards the JSON
// body verbatim to that fixed endpoint. Nothing here retries: a timeout or
// upstream error is passed to the caller as-is (D-18⑤).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	decisionTextLimit  = 1 << 20
	decisionImageLimit = 8 << 20
)

// DecisionUpstream is one resolved structured-decision endpoint. The API key is
// read from the host's secret store on each resolution; it is never logged.
type DecisionUpstream struct {
	ID       string
	BaseURL  string
	Protocol string // systemone | decision
	ModelID  string
	APIKey   string
}

func (g *Gateway) decisionList() []DecisionUpstream {
	if g == nil || g.decisionUpstreams == nil {
		return nil
	}
	return g.decisionUpstreams()
}

// decisionFor picks the upstream whose protocol matches the route alias; when
// only one upstream is configured, that one serves both aliases.
func (g *Gateway) decisionFor(alias string) (DecisionUpstream, bool) {
	list := g.decisionList()
	var fallback *DecisionUpstream
	for i := range list {
		if strings.EqualFold(list[i].Protocol, alias) {
			return list[i], true
		}
		if fallback == nil {
			fallback = &list[i]
		}
	}
	if len(list) == 1 && fallback != nil {
		return *fallback, true
	}
	return DecisionUpstream{}, false
}

// isDecisionModel reports whether a chat request names a decision model, which
// must use /v1/systemone instead (D-18④).
func (g *Gateway) isDecisionModel(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, upstream := range g.decisionList() {
		if strings.EqualFold(upstream.ModelID, name) {
			return true
		}
	}
	return false
}

func (g *Gateway) handleDecision(w http.ResponseWriter, r *http.Request, rec *reqRecord, alias string) {
	upstream, ok := g.decisionFor(alias)
	if !ok {
		g.fail(w, rec, http.StatusServiceUnavailable, "service_unavailable", "decision_unavailable",
			fmt.Sprintf("没有可用的 %s 决策上游：请在 config.json 的 decision.upstreams 配置 base_url/model_id", alias))
		return
	}
	body, tooLarge, err := readDecisionBody(r)
	if err != nil {
		g.fail(w, rec, http.StatusBadRequest, "invalid_request_error", "invalid_json",
			"请求体不是合法的 JSON 对象")
		return
	}
	if tooLarge {
		g.fail(w, rec, http.StatusRequestEntityTooLarge, "invalid_request_error", "payload_too_large",
			fmt.Sprintf("决策请求体超限（纯文本 1 MiB / 带图 8 MiB）"))
		return
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		g.fail(w, rec, http.StatusBadRequest, "invalid_request_error", "invalid_json",
			"请求体必须是 JSON 对象")
		return
	}
	payload["model"], _ = json.Marshal(upstream.ModelID)
	forwarded, err := json.Marshal(payload)
	if err != nil {
		g.fail(w, rec, http.StatusBadRequest, "invalid_request_error", "invalid_json", "请求体不可编码")
		return
	}

	rec.source = "decision:" + upstream.ID
	target := strings.TrimRight(upstream.BaseURL, "/") + "/" + upstream.Protocol
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(forwarded)))
	if err != nil {
		g.fail(w, rec, http.StatusBadGateway, "upstream_unavailable", "upstream_unavailable", "决策上游地址无效")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if upstream.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+upstream.APIKey)
	}
	resp, err := g.client.Do(req) // deliberately one attempt: D-18⑤ no retry
	if err != nil {
		g.fail(w, rec, http.StatusBadGateway, "upstream_unavailable", "upstream_unavailable",
			"决策上游不可达："+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, decisionImageLimit))
	rec.status = resp.StatusCode
	rec.errText = ""
	if resp.StatusCode >= 400 {
		rec.errText = truncateUTF8(strings.TrimSpace(string(respBody)), 512)
	}
	var usage struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(respBody, &usage) == nil {
		rec.prompt, rec.completion = usage.Usage.InputTokens, usage.Usage.OutputTokens
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set(sourceHeader, rec.source)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// readDecisionBody enforces the local size limits before anything leaves the
// host. The image check is a cheap top-level look: {"image": "data:..."}.
func readDecisionBody(r *http.Request) (body []byte, tooLarge bool, err error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, decisionImageLimit+1))
	if err != nil {
		return nil, false, err
	}
	limit := int64(decisionTextLimit)
	var probe struct {
		Image json.RawMessage `json:"image"`
	}
	if json.Unmarshal(raw, &probe) == nil {
		image := strings.TrimSpace(string(probe.Image))
		if image != "" && image != "null" && image != `""` {
			limit = decisionImageLimit
		}
	}
	if int64(len(raw)) > limit || int64(len(raw)) > decisionImageLimit {
		return nil, true, nil
	}
	return raw, false, nil
}
