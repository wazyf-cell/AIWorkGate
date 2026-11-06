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

	payload, err := json.Marshal(map[string]any{
		"state": probe,
		"questions": []any{map[string]any{
			"id":       "route",
			"type":     "choice",
			"question": "判断这条用户请求的类型，并从选项中选择一项：" + strings.Join(intent.Labels, " / "),
			"options":  intent.Labels,
		}},
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

// parseIntentLabel accepts the spellings a structured-decision upstream may
// answer with: a top-level label/answer, or answers[] carrying a label, an
// answer string, a 1-based choice index, or a string value.
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
	var payload struct {
		Label   string `json:"label"`
		Answer  string `json:"answer"`
		Answers []struct {
			Label  string          `json:"label"`
			Answer string          `json:"answer"`
			Choice int             `json:"choice"`
			Value  json.RawMessage `json:"value"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", false
	}
	if label, ok := match(payload.Label); ok {
		return label, true
	}
	if label, ok := match(payload.Answer); ok {
		return label, true
	}
	for _, answer := range payload.Answers {
		if label, ok := match(answer.Label); ok {
			return label, true
		}
		if label, ok := match(answer.Answer); ok {
			return label, true
		}
		if answer.Choice >= 1 && answer.Choice <= len(labels) {
			return labels[answer.Choice-1], true
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
