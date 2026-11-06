package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxChatBodyBytes matches the host's own request cap
// (internal/gateway: maxRequestBodyBytes = 32 << 20) so a body the host
// accepted is never rejected here.
const maxChatBodyBytes = 32 << 20

type chatRequest struct {
	Model               string            `json:"model"`
	Messages            []json.RawMessage `json:"messages"`
	Stream              bool              `json:"stream"`
	Tools               json.RawMessage   `json:"tools"`
	ToolChoice          json.RawMessage   `json:"tool_choice"`
	StreamOptions       json.RawMessage   `json:"stream_options"`
	N                   *int              `json:"n"`
	Temperature         *float64          `json:"temperature"`
	TopP                *float64          `json:"top_p"`
	PresencePenalty     *float64          `json:"presence_penalty"`
	FrequencyPenalty    *float64          `json:"frequency_penalty"`
	Seed                *int64            `json:"seed"`
	MaxTokens           *int              `json:"max_tokens"`
	MaxCompletionTokens *int              `json:"max_completion_tokens"`
	Thinking            json.RawMessage   `json:"thinking"`
	ReasoningEffort     *string           `json:"reasoning_effort"`
	Stop                json.RawMessage   `json:"stop"`
	ResponseFormat      json.RawMessage   `json:"response_format"`
}

// upstreamError is a non-2xx MaaS response.
type upstreamError struct {
	Status int
	Body   string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("MaaS 返回 %d：%s", e.Status, truncate(e.Body, 500))
}

func (p *Plugin) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}
	req.Model = strings.TrimPrefix(strings.TrimSpace(req.Model), "officeace/")
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	forcedID := forcedAccountID(r)
	hedge := isHedge(r)

	cred, err := p.liveCredential(ctx)
	if err != nil {
		code, msg := statusFromDBError(err)
		p.setStatus(code, msg)
		if forcedID != "" {
			// The host asked for a specific account and there is none to serve.
			writeAccountUnavailable(w, forcedID, msg)
			return
		}
		writeError(w, http.StatusServiceUnavailable, "account_unavailable", msg)
		return
	}
	acct := p.accountForCredential(cred)
	if forcedID != "" {
		// Single-account plugin: either it is this account or 409. Never fall
		// back to the account the host did not choose.
		if acct.ID != forcedID {
			setAccount(r, acct.ID)
			writeAccountUnavailable(w, forcedID, "OfficeAce 只有一个账号 "+acct.ID+"，与请求指定的账号不一致")
			return
		}
		if !acct.Enabled {
			writeAccountUnavailable(w, forcedID, "account "+acct.ID+" is disabled")
			return
		}
		if !cred.HasUpstreamCredential() {
			p.setAccountStatus(acct.ID, statusInvalid, "OfficeAce 会话缺少 MaaS 认证材料")
			writeAccountUnavailable(w, forcedID, "account "+acct.ID+" has no usable credential")
			return
		}
	}
	setAccount(r, acct.ID)
	// Reported before the first byte (SSE included) so the host can book the
	// request against the real account.
	setAccountHeader(w, acct.ID)
	if !acct.Enabled {
		writeError(w, http.StatusForbidden, "account_disabled", "account "+acct.ID+" is disabled")
		return
	}
	if !cred.HasUpstreamCredential() {
		msg := "OfficeAce 会话缺少 MaaS 认证材料，请打开 OfficeAce 完成模型配置"
		p.setAccountStatus(acct.ID, statusInvalid, msg)
		writeError(w, http.StatusServiceUnavailable, "credential_unavailable", msg)
		return
	}

	entry, err := p.resolveEgress(acct.ID)
	if err != nil {
		var em *egressMissingError
		if errors.As(err, &em) {
			writeError(w, http.StatusForbidden, "egress_missing", em.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "egress_error", err.Error())
		return
	}
	proxyURL := ""
	if entry.Mode == egressProxy {
		proxyURL = entry.URL
	}

	upstreamBody, stripCount, ruleHits, stripHits, err := p.buildMaaSChatBody(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if ruleHits+stripHits > 0 {
		p.addRuleHits(ruleHits, stripHits)
	}
	if stripCount > 0 {
		p.logLine("warn", "images stripped: model does not advertise vision support", map[string]any{
			"account": acct.ID,
			"model":   req.Model,
			"count":   stripCount,
		})
	}

	endpoint := cred.ChatEndpoint()
	if m, ok := p.modelByName(req.Model); ok && m.BaseURL != "" {
		endpoint = m.BaseURL + "/chat/completions"
	}

	// A hedged request must stay a single attempt: no refresh, no replay.
	allowRefresh := p.autoRefreshEnabled() && !hedge
	if cred.Expired(p.now()) && allowRefresh {
		if refreshed, rerr := p.refreshCredential(ctx, cred, acct.ID, proxyURL, false); rerr == nil {
			cred = refreshed
			p.logLine("info", "lazy credential refresh before chat", map[string]any{"account": acct.ID})
		} else {
			p.logLine("warn", "lazy credential refresh failed", map[string]any{"account": acct.ID, "error": rerr.Error()})
		}
	}

	resp, mode, err := p.sendChat(ctx, cred, acct.ID, proxyURL, endpoint, upstreamBody, allowRefresh)
	if err != nil {
		var ue *upstreamError
		switch {
		case errors.As(err, &ue) && (ue.Status == http.StatusUnauthorized || ue.Status == http.StatusForbidden):
			msg := "MaaS 认证失败（已尝试可用的认证方式）：" + ue.Error()
			p.setAccountStatus(acct.ID, statusExpired, msg)
			p.setStatus(pluginStatusExpired, msg)
			writeError(w, http.StatusBadGateway, "upstream_auth_error", msg)
		case errors.As(err, &ue):
			// Keep the upstream's own 4xx: a 404 means the deployment is gone
			// (MaaS ModelArts.81009), not that the request was malformed. The
			// host treats every 4xx except 400 as terminal, and clients read
			// 400 as "fix your parameters".
			typ := "upstream_error"
			status := http.StatusBadGateway
			if ue.Status == http.StatusTooManyRequests {
				typ = "upstream_rate_limited"
				p.setAccountStatus(acct.ID, statusRateLimited, ue.Error())
				setRateLimitHeaders(w, acct.ID)
			} else if ue.Status >= 400 && ue.Status < 500 {
				status = ue.Status
			}
			writeError(w, status, typ, ue.Error())
		case errors.Is(err, context.Canceled):
			return
		default:
			writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		}
		return
	}
	defer resp.Body.Close()
	p.logLine("info", "maas auth ok", map[string]any{
		"account": acct.ID,
		"model":   req.Model,
		"auth":    mode,
		"stream":  req.Stream,
	})
	p.setAccountStatus(acct.ID, statusOK, "")
	p.setStatus(pluginStatusOK, "")

	if req.Stream {
		p.relaySSE(w, r, resp)
		return
	}
	p.aggregateChat(w, req.Model, resp)
}

// addRuleHits persists rule counters (used when shared rule packs are enabled).
func (p *Plugin) addRuleHits(rules, strips int) {
	p.mu.Lock()
	p.state.RuleHits += int64(rules)
	p.state.StripHits += int64(strips)
	p.mu.Unlock()
	if err := p.save(); err != nil {
		p.logLine("warn", "save state failed", map[string]any{"error": err.Error()})
	}
}
