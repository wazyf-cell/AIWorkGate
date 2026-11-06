package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

func (p *Plugin) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
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
	model := normalizeChatModel(req.Model)
	if model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	messages, ruleHits, stripHits := p.buildWireMessages(&req)
	p.addRuleHits(ruleHits, stripHits)
	tools := convertTools(req.Tools)
	if len(tools) > 0 && needsDsmlToolMode(model) {
		messages = applyDsmlMode(messages, tools)
		tools = nil
	}

	forcedID := forcedAccountID(r)
	hedge := isHedge(r)
	// A pinned account disables rotation; a hedge request disables every
	// internal retry (contract v0.3 §6.4).
	allowRotation := forcedID == "" && !hedge

	acct, err := p.pickChatAccount(r)
	if err != nil {
		if forcedID != "" {
			writeAccountUnavailable(w, forcedID, err.Error())
			return
		}
		writeError(w, http.StatusServiceUnavailable, "no_account", err.Error())
		return
	}
	if forcedID != "" && !accountServable(acct) {
		p.logLine("warn", "forced account unavailable", map[string]any{"account": acct.ID, "req_id": requestID(r)})
		writeAccountUnavailable(w, forcedID, "account "+acct.ID+" is disabled or has no usable credential")
		return
	}
	setAccount(r, acct.ID)

	ctx := r.Context()
	tried := map[string]bool{}
	rotated := false
	authRefreshed := false
	benefitDropped := false
	queueDeadline := time.Time{}
	benefitModel := p.isBenefitModel(model)

	// The stream sink is created up-front but only writes headers on the first
	// real chunk, so queue/benefit retries stay invisible to the client.
	var streamSink *sseSink
	var agg *aggregateSink
	if req.Stream {
		streamSink = &sseSink{p: p, w: w, r: r, model: model, created: p.now().Unix(), id: "chatcmpl-" + randomHex(8)}
	} else {
		agg = &aggregateSink{}
	}
	started := func() bool { return streamSink != nil && streamSink.started }

	for {
		tried[acct.ID] = true
		// Written before any byte (including the first SSE chunk); a rotation
		// below updates it before the stream starts.
		setAccountHeader(w, acct.ID)
		proxyURL, eerr := p.egressProxyFor(acct.ID)
		if eerr != nil {
			var em *egressMissingError
			if errors.As(eerr, &em) {
				writeError(w, http.StatusForbidden, "egress_missing", em.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, "egress_error", eerr.Error())
			return
		}
		acct, err = p.ensureCredential(ctx, acct.ID, false)
		if err != nil {
			var em *egressMissingError
			if errors.As(err, &em) {
				writeError(w, http.StatusForbidden, "egress_missing", em.Error())
				return
			}
			if next := p.nextChatAccount(tried); allowRotation && next != nil && !rotated {
				rotated = true
				acct = next
				continue
			}
			writeError(w, http.StatusServiceUnavailable, "credential_error", err.Error())
			return
		}
		bodyOut := buildUpstreamBody(&req, model, messages, tools, acct.Identity.SessionID)
		rawBody, _ := json.Marshal(bodyOut)
		resp, err := p.postChat(ctx, acct, proxyURL, rawBody, benefitModel && !benefitDropped)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed: "+err.Error())
			return
		}
		if resp.StatusCode != http.StatusOK {
			text := readLimited(resp.Body, 1<<20)
			_ = resp.Body.Close()
			detail := errorDetail(text)
			p.logLine("warn", "chat upstream error", map[string]any{"account": acct.ID, "status": resp.StatusCode, "detail": truncate(detail, 300), "req_id": requestID(r), "hedge": hedge})
			switch {
			case isAuthError(resp.StatusCode, text):
				// A single refresh (same account) is not rotation, so it is
				// still allowed for a pinned account — but never for a hedge.
				if !hedge && !authRefreshed {
					authRefreshed = true
					if _, rerr := p.ensureCredential(ctx, acct.ID, true); rerr == nil {
						continue
					}
					if next := p.nextChatAccount(tried); allowRotation && next != nil && !rotated {
						rotated = true
						acct = next
						continue
					}
				}
				// Surface the upstream auth status itself: the host only refreshes
				// or switches accounts on 401/403, and a 502 here hides a dead
				// credential behind a generic upstream error.
				writeError(w, http.StatusUnauthorized, "upstream_auth", detail)
				return
			case isQuotaFailure(resp.StatusCode, text):
				p.markRateLimited(acct.ID)
				p.setRateLimitHeaders(w, acct.ID)
				if next := p.nextChatAccount(tried); allowRotation && next != nil && !rotated {
					rotated = true
					acct = next
					authRefreshed = false
					continue
				}
				writeError(w, http.StatusTooManyRequests, "quota_exceeded", detail)
				return
			case isRateLimitFailure(resp.StatusCode, text):
				p.markRateLimited(acct.ID)
				p.setRateLimitHeaders(w, acct.ID)
				if next := p.nextChatAccount(tried); allowRotation && next != nil && !rotated {
					rotated = true
					acct = next
					authRefreshed = false
					continue
				}
				status := resp.StatusCode
				if status < 400 || status > 599 {
					status = http.StatusTooManyRequests
				}
				writeError(w, status, "rate_limited", detail)
				return
			case isQueueFailure(resp.StatusCode, text):
				if hedge {
					// Peak-usage is a rate limit for the host: no queue wait on
					// a hedged attempt, just fail fast.
					p.setRateLimitHeaders(w, acct.ID)
					writeError(w, http.StatusTooManyRequests, "rate_limited", detail)
					return
				}
				if queueDeadline.IsZero() {
					queueDeadline = time.Now().Add(time.Duration(p.opts.QueueMaxAttempts) * p.opts.QueueRetryDelay)
				}
				if err := p.waitForQueue(ctx, acct, proxyURL, model, queueDeadline); err != nil {
					writeError(w, http.StatusGatewayTimeout, "queue_timeout", err.Error())
					return
				}
				continue
			default:
				if !hedge {
					if st, qerr := p.queryQueueStatus(ctx, acct, proxyURL, model); qerr == nil && st.Status == "waiting" {
						if queueDeadline.IsZero() {
							queueDeadline = time.Now().Add(time.Duration(p.opts.QueueMaxAttempts) * p.opts.QueueRetryDelay)
						}
						if err := p.waitForQueue(ctx, acct, proxyURL, model, queueDeadline); err != nil {
							writeError(w, http.StatusGatewayTimeout, "queue_timeout", err.Error())
							return
						}
						continue
					}
				}
				status := resp.StatusCode
				if status < 400 || status > 599 {
					status = http.StatusBadGateway
				}
				writeError(w, status, "upstream_error", detail)
				return
			}
		}

		sink := chatSink(agg)
		if streamSink != nil {
			sink = streamSink
		}
		outcome := p.consumeChatStream(ctx, resp.Body, sink)
		_ = resp.Body.Close()
		switch outcome.kind {
		case "done":
			p.clearRateLimited(acct.ID)
			if streamSink != nil {
				streamSink.finalize(outcome)
			} else {
				p.writeAggregate(w, model, agg, outcome)
			}
			return
		case "queue":
			if started() || (agg != nil && !agg.empty()) {
				if started() {
					streamSink.writeError("queue", outcome.message)
				} else {
					writeError(w, http.StatusGatewayTimeout, "queue_timeout", outcome.message)
				}
				return
			}
			if hedge {
				p.setRateLimitHeaders(w, acct.ID)
				writeError(w, http.StatusTooManyRequests, "rate_limited", outcome.message)
				return
			}
			if queueDeadline.IsZero() {
				queueDeadline = time.Now().Add(time.Duration(p.opts.QueueMaxAttempts) * p.opts.QueueRetryDelay)
			}
			if err := p.waitForQueue(ctx, acct, proxyURL, model, queueDeadline); err != nil {
				writeError(w, http.StatusGatewayTimeout, "queue_timeout", err.Error())
				return
			}
			continue
		case "quota":
			p.markRateLimited(acct.ID)
			if !started() && (agg == nil || agg.empty()) {
				if next := p.nextChatAccount(tried); allowRotation && next != nil && !rotated {
					rotated = true
					acct = next
					authRefreshed = false
					continue
				}
			}
			if started() {
				streamSink.writeError("quota_exceeded", outcome.message)
			} else {
				p.setRateLimitHeaders(w, acct.ID)
				writeError(w, http.StatusTooManyRequests, "quota_exceeded", outcome.message)
			}
			return
		case "benefit":
			if !started() && (agg == nil || agg.empty()) && benefitModel && !benefitDropped && !hedge {
				benefitDropped = true
				continue
			}
			if started() {
				streamSink.writeError("upstream_error", outcome.message)
			} else {
				writeError(w, http.StatusBadGateway, "upstream_error", outcome.message)
			}
			return
		case "timeout":
			if started() {
				streamSink.writeError("upstream_timeout", outcome.message)
			} else {
				writeError(w, http.StatusGatewayTimeout, "upstream_timeout", outcome.message)
			}
			return
		default:
			if started() {
				streamSink.writeError("upstream_error", outcome.message)
			} else {
				writeError(w, http.StatusBadGateway, "upstream_error", outcome.message)
			}
			return
		}
	}
}
