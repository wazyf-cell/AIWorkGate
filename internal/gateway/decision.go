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
	// D-18⑧/F-12: an image-bearing request is single-question, plain-text
	// requests carry at most 16 questions. Reject locally instead of letting
	// the upstream refuse after the upload.
	hasImage, questions, probeOK := decisionImageDetection(body)
	if probeOK && questions > decisionMaxTextQuestions {
		g.fail(w, rec, http.StatusBadRequest, "invalid_request_error", "too_many_questions",
			fmt.Sprintf("决策请求最多 %d 题（当前 %d 题）", decisionMaxTextQuestions, questions))
		return
	}
	if probeOK && hasImage && questions > 1 {
		g.fail(w, rec, http.StatusBadRequest, "invalid_request_error", "image_single_question",
			"带图决策请求只支持单题：请把图片题单独发送")
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
	// D-18⑦: the usage object is a pointer so "usage": null / a missing key
	// stay unknown instead of masquerading as reported-zero usage. A present
	// (parseable) object marks the row usage-known, zero counts included —
	// same rule as the bodyTap paths.
	var usage struct {
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(respBody, &usage) == nil && usage.Usage != nil {
		rec.usageKnown = true
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

// decisionQuestionLimits mirrors the JEV upstream contract (plan F-12):
// plain-text requests carry 1..16 questions, an image-bearing request carries
// exactly one. Enforcing them locally (D-18⑧ spirit: reject, never forward a
// doomed request) saves a pointless upstream round trip.
const decisionMaxTextQuestions = 16

// decisionImageDetection reports whether the request carries an image and how
// many questions it asks. Images may ride top-level (`image`) or per question
// (`questions[i].image`); the upstream accepts both shapes. The JEV upstream
// also accepts questions as an **object** keyed by question id (实测
// 2026-11-08, task-70), so both the array and the object spellings are counted.
func decisionImageDetection(body []byte) (hasImage bool, questions int, ok bool) {
	var probe struct {
		Image     json.RawMessage `json:"image"`
		Questions json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false, 0, false
	}
	hasImage = hasImageField(probe.Image)
	trimmed := strings.TrimSpace(string(probe.Questions))
	if len(trimmed) == 0 || trimmed[0] != '[' && trimmed[0] != '{' {
		return hasImage, 0, true
	}
	if trimmed[0] == '{' {
		// 实测对象形状：键=题id，值带可选 image。
		var questions map[string]struct {
			Image json.RawMessage `json:"image"`
		}
		if json.Unmarshal([]byte(trimmed), &questions) != nil {
			return false, 0, false
		}
		for _, question := range questions {
			if hasImageField(question.Image) {
				hasImage = true
			}
		}
		return hasImage, len(questions), true
	}
	var list []json.RawMessage
	if json.Unmarshal([]byte(trimmed), &list) != nil {
		return false, 0, false
	}
	for _, question := range list {
		var qProbe struct {
			Image json.RawMessage `json:"image"`
		}
		if json.Unmarshal(question, &qProbe) == nil && hasImageField(qProbe.Image) {
			hasImage = true
		}
	}
	return hasImage, len(list), true
}

func hasImageField(raw json.RawMessage) bool {
	image := strings.TrimSpace(string(raw))
	return image != "" && image != "null" && image != `""`
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
