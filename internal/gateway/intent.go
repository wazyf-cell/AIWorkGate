package gateway

// P11 intent routing: one structured-decision call classifies the current user
// turn, and the label selects another routing group. The call is bounded at 3s,
// never retried, and a failure keeps the request on its original group (or the
// configured default). Classifications are cached per probe fingerprint for 10
// minutes so a long conversation is not billed on every turn.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"aiworkgate/internal/scheduler"
)

const (
	defaultIntentTimeout = 3 * time.Second
	defaultIntentTTL     = 10 * time.Minute
	intentResponseLimit  = 1 << 20
)

type intentCacheEntry struct {
	label   string
	expires time.Time
}

// intentLabel asks the decision upstream to pick one of intent.Labels for the
// probe text. ok=false covers every failure path: no upstream, empty probe,
// timeout, non-2xx, unparsable answer.
func (g *Gateway) intentLabel(ctx context.Context, intent scheduler.IntentBinding, probe string) (string, bool) {
	probe = strings.TrimSpace(probe)
	if g == nil || probe == "" || len(intent.Labels) == 0 {
		return "", false
	}
	upstream, ok := g.decisionUpstreamByID(intent.Upstream)
	if !ok || strings.TrimSpace(upstream.BaseURL) == "" {
		return "", false
	}
	fingerprint := intentFingerprint(upstream, intent, probe)
	if label, ok := g.intentCacheGet(fingerprint); ok {
		return label, true
	}

	timeout := g.intentTimeout
	if timeout <= 0 {
		timeout = defaultIntentTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 实测 JEV 形状（2026-11-08 TokenRhythm，task-70 卡内实测记录）：
	// state 是对象 {"message":...}，questions 是**对象**（键=题id，文本字段是
	// instructions）；questions 数组、choice 的 options/criteria 字段全部实测
	// 422 被拒——choice 的选项字段待 JEV 协议文档确认，选项语义暂并入
	// instructions 文本（待确认后结构化，勿提前写死）。
	payload, err := json.Marshal(map[string]any{
		"state": map[string]any{"message": probe},
		"questions": map[string]any{
			"route": map[string]any{
				"type": "choice",
				"instructions": "判断这条用户请求的类型，并从选项中选择一项：" +
					strings.Join(intent.Labels, " / "),
			},
		},
	})
	if err != nil {
		return "", false
	}
	target := strings.TrimRight(upstream.BaseURL, "/") + "/" + upstream.Protocol
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, target, strings.NewReader(string(payload)))
	if err != nil {
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")
	if upstream.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+upstream.APIKey)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, intentResponseLimit))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false
	}
	label, ok := parseIntentLabel(body, intent.Labels)
	if !ok {
		return "", false
	}
	g.intentCachePut(fingerprint, label)
	return label, true
}

// resolveIntentTarget turns a classification into the group id to schedule on.
// It returns "" when the request should stay on the original group.
func (g *Gateway) resolveIntentTarget(ctx context.Context, snap *scheduler.Snapshot, binding scheduler.Binding, probe string) (target, note string) {
	intent := binding.Intent
	if intent == nil {
		return "", ""
	}
	fallback := func(note string) (string, string) {
		if intent.Default != "" {
			if next, ok := snap.Bindings[intent.Default]; ok && len(next.Items) > 0 {
				return intent.Default, note
			}
		}
		return "", note
	}
	label, ok := g.intentLabel(ctx, *intent, probe)
	if !ok {
		return fallback("intent: classification failed, staying on the default group")
	}
	target = strings.TrimSpace(intent.Route[label])
	if target == "" {
		return fallback(fmt.Sprintf("intent: label %q has no route, staying on the default group", label))
	}
	if next, ok := snap.Bindings[target]; !ok || len(next.Items) == 0 {
		return fallback(fmt.Sprintf("intent: %s has no members, staying on the default group", target))
	}
	return target, fmt.Sprintf("intent: %s → %s", label, target)
}

// decisionUpstreamByID resolves an intent's upstream reference; an empty id
// means the first configured structured-decision upstream.
func (g *Gateway) decisionUpstreamByID(id string) (DecisionUpstream, bool) {
	list := g.decisionList()
	if len(list) == 0 {
		return DecisionUpstream{}, false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return list[0], true
	}
	for _, upstream := range list {
		if strings.EqualFold(upstream.ID, id) {
			return upstream, true
		}
	}
	return DecisionUpstream{}, false
}

func intentFingerprint(upstream DecisionUpstream, intent scheduler.IntentBinding, probe string) string {
	sum := sha256.Sum256([]byte(upstream.ID + "\x00" + upstream.Protocol + "\x00" + upstream.ModelID +
		"\x00" + strings.Join(intent.Labels, "\x1f") + "\x00" + probe))
	return hex.EncodeToString(sum[:])
}

func (g *Gateway) intentCacheGet(fingerprint string) (string, bool) {
	g.intentMu.Lock()
	defer g.intentMu.Unlock()
	entry, ok := g.intentCache[fingerprint]
	if !ok || g.now().After(entry.expires) {
		return "", false
	}
	return entry.label, true
}

func (g *Gateway) intentCachePut(fingerprint, label string) {
	ttl := g.intentTTL
	if ttl <= 0 {
		ttl = defaultIntentTTL
	}
	g.intentMu.Lock()
	defer g.intentMu.Unlock()
	if g.intentCache == nil {
		g.intentCache = make(map[string]intentCacheEntry)
	}
	g.intentCache[fingerprint] = intentCacheEntry{label: label, expires: g.now().Add(ttl)}
}

// intentAnswer is one structured-decision answer entry. The JEV upstream
// answers with an **object** keyed by question id (实测 2026-11-08); the array
// spelling is kept for backwards compatibility with the earlier mock contract.
// choice carries either the label string or a 1-based index (JSON number);
// both are matched against the intent labels.
type intentAnswer struct {
	Type   string          `json:"type"`
	Label  string          `json:"label"`
	Answer string          `json:"answer"`
	Choice json.RawMessage `json:"choice"`
	Value  json.RawMessage `json:"value"`
}

// matchIntentAnswer tries label / answer / choice (string or 1-based index) /
// value in that order against the intent labels.
func matchIntentAnswer(answer intentAnswer, labels []string, match func(string) (string, bool)) (string, bool) {
	if label, ok := match(answer.Label); ok {
		return label, true
	}
	if label, ok := match(answer.Answer); ok {
		return label, true
	}
	if len(answer.Choice) > 0 {
		var text string
		if json.Unmarshal(answer.Choice, &text) == nil {
			if label, ok := match(text); ok {
				return label, true
			}
		}
		var index int
		if json.Unmarshal(answer.Choice, &index) == nil && index >= 1 && index <= len(labels) {
			return labels[index-1], true
		}
	}
	if len(answer.Value) > 0 {
		var text string
		if json.Unmarshal(answer.Value, &text) == nil {
			if label, ok := match(text); ok {
				return label, true
			}
		}
		var index int
		if json.Unmarshal(answer.Value, &index) == nil && index >= 1 && index <= len(labels) {
			return labels[index-1], true
		}
	}
	return "", false
}

// parseIntentLabel accepts the spellings a structured-decision upstream may
// answer with: a top-level label/answer, answers as an **object** keyed by
// question id (实测 JEV 形状), or answers[] carrying a label, an answer string,
// a 1-based choice index, or a string value.
func parseIntentLabel(body []byte, labels []string) (string, bool) {
	match := func(candidate string) (string, bool) {
		candidate = strings.TrimSpace(candidate)
		for _, label := range labels {
			if strings.EqualFold(label, candidate) {
				return label, true
			}
		}
		return "", false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", false
	}
	readString := func(key string) string {
		var text string
		if raw, ok := probe[key]; ok && json.Unmarshal(raw, &text) == nil {
			return text
		}
		return ""
	}
	if label, ok := match(readString("label")); ok {
		return label, true
	}
	if label, ok := match(readString("answer")); ok {
		return label, true
	}
	raw, ok := probe["answers"]
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(string(raw))
	// 实测 JEV：answers 是对象（键=题id）。键序无意义（单题路由），按键排序
	// 保证多题时结果确定。
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var answers map[string]intentAnswer
		if json.Unmarshal([]byte(trimmed), &answers) != nil {
			return "", false
		}
		keys := make([]string, 0, len(answers))
		for key := range answers {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if label, ok := matchIntentAnswer(answers[key], labels, match); ok {
				return label, true
			}
		}
		return "", false
	}
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var answers []intentAnswer
		if json.Unmarshal([]byte(trimmed), &answers) != nil {
			return "", false
		}
		for _, answer := range answers {
			if label, ok := matchIntentAnswer(answer, labels, match); ok {
				return label, true
			}
		}
	}
	return "", false
}

// promptProbe extracts the last user turn for classification and truncates it
// to limit runes. Supported spellings mirror AnalyzePrompt.
func promptProbe(body []byte, limit int) string {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	last := ""
	for _, key := range []string{"messages", "input", "contents"} {
		value, ok := obj[key]
		if !ok {
			continue
		}
		if text, ok := value.(string); ok {
			last = text
			break
		}
		list, ok := value.([]any)
		if !ok {
			continue
		}
		found := ""
		for _, rawMessage := range list {
			message, _ := rawMessage.(map[string]any)
			if !strings.EqualFold(jsonStringValue(message["role"]), "user") {
				continue
			}
			text := collectText(message["content"])
			if text == "" {
				text = collectText(message["parts"])
			}
			if text != "" {
				found = text
			}
		}
		if found != "" {
			last = found
			break
		}
	}
	if last == "" {
		if text, ok := obj["prompt"].(string); ok {
			last = text
		}
	}
	last = strings.TrimSpace(last)
	if limit <= 0 || len([]rune(last)) <= limit {
		return last
	}
	return string([]rune(last)[:limit])
}

func collectText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, rawPart := range typed {
			if text := collectText(rawPart); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return text
		}
		if parts, ok := typed["parts"]; ok {
			return collectText(parts)
		}
	}
	return ""
}
