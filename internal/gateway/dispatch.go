package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

// Scheduling contract (design §6): the gateway picks one account per attempt,
// pins it with X-Aiworkgate-Account for api_level >= 2 plugins, reports the
// outcome back to the scheduler and retries the next candidate when the
// failure happened before the first response byte.
const (
	maxScheduleAttempts = 4
	retryBodyCap        = 64 << 10
	// maxRetryWait bounds one backoff step so a hostile or misconfigured
	// Retry-After cannot pin a handler for hours (A caps it at 60s too).
	maxRetryWait = 60 * time.Second
	// maxSameAccountRetries is the per-account retry budget from A retry.rs:
	// the first failure plus three waits, then the account is left behind.
	maxSameAccountRetries = 3

	accountHeader       = "X-Aiworkgate-Account"
	requestIDHeader     = "X-Aiworkgate-Request-Id"
	hedgeHeader         = "X-Aiworkgate-Hedge"
	creditsHeader       = "X-Aiworkgate-Credits"
	downgradedHeader    = "X-Aiworkgate-Downgraded"
	conversationHeader  = "X-Conversation-Id"
	clientKeyHeader     = "X-Client-Key"
	scheduledSourceName = "scheduler"
)

// errNotScheduled marks a model that the scheduler does not own: the request
// must keep flowing to A.
var errNotScheduled = errors.New("model is not scheduled by a plugin")

// accountRefresher is the optional host-side credential refresh entry
// (implemented by *plugins.Manager). A 401/403 gets at most one self-heal per
// account and request; when the host cannot refresh we simply switch accounts.
type accountRefresher interface {
	Refresh(ctx context.Context, pluginID, accountID string) (model.RefreshResult, error)
}

// retryAction mirrors A's RetryAction (TraeWorkAssistant, MIT, 朱天伟;
// src-tauri/src/api_server/retry.rs:10-19).
type retryAction int

const (
	retryStop retryAction = iota
	retrySameAccount
	retrySwitchAccount
)

func (a retryAction) String() string {
	switch a {
	case retrySameAccount:
		return "retry_same"
	case retrySwitchAccount:
		return "switch_account"
	default:
		return "stop"
	}
}

// retryPlan is one status-specific decision. delay is only meaningful for
// retrySameAccount.
type retryPlan struct {
	action retryAction
	delay  time.Duration
}

// planRetry ports the status table of A retry.rs:63-115 (MIT, 朱天伟):
//
//	429        Retry-After, else 1s/2s/3s; after 3 waits -> switch
//	503 / 529  10s/20s/40s; after 3 waits -> switch
//	502        500ms once -> switch
//	401 / 403  switch immediately (the caller may refresh once first)
//	404        switch immediately: the model may exist on another account
//	400        body decides: thinking-signature / 11128 retry once, credit
//	           exhaustion switches accounts, everything else stops
//	other 4xx  stop; switching accounts cannot fix the request
//
// body is the buffered upstream error text (short errText is enough for the 400
// classes). sameAttempt counts retries already spent on this account (0 = first
// failure). now is injectable so Retry-After HTTP dates stay testable.
func planRetry(status int, header http.Header, body string, sameAttempt int, now time.Time) retryPlan {
	switch status {
	case http.StatusBadRequest:
		return planRetry400(body, sameAttempt)
	case http.StatusTooManyRequests:
		if sameAttempt >= maxSameAccountRetries {
			return retryPlan{action: retrySwitchAccount}
		}
		delay := time.Duration(sameAttempt+1) * time.Second
		if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
			if parsed, ok := parseRetryAfter(raw, now); ok {
				delay = parsed
			}
		}
		return retryPlan{action: retrySameAccount, delay: clampRetryWait(delay)}
	case http.StatusServiceUnavailable, 529:
		if sameAttempt >= maxSameAccountRetries {
			return retryPlan{action: retrySwitchAccount}
		}
		return retryPlan{
			action: retrySameAccount,
			delay:  clampRetryWait(10 * time.Second << uint(min(sameAttempt, 2))),
		}
	case http.StatusBadGateway:
		if sameAttempt == 0 {
			return retryPlan{action: retrySameAccount, delay: 500 * time.Millisecond}
		}
		return retryPlan{action: retrySwitchAccount}
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return retryPlan{action: retrySwitchAccount}
	case 0, http.StatusRequestTimeout, http.StatusConflict:
		// Transport failures and the legacy retryable statuses keep the old
		// fail-over behaviour: no same-account wait.
		return retryPlan{action: retrySwitchAccount}
	}
	if status >= 500 {
		return retryPlan{action: retrySwitchAccount}
	}
	return retryPlan{action: retryStop}
}

// planRetry400 ports A's 400-classification (retry.rs:63-83, routes.rs:1943-1997):
// only the three body signatures below are retryable; every other 400 stays
// terminal because switching accounts cannot fix a malformed request.
func planRetry400(body string, sameAttempt int) retryPlan {
	switch {
	case isThinkingSignature400(body):
		// The client's signed thinking block was rejected after a model/effort
		// change. A retries the same account once after 200ms so the upstream can
		// re-derive the block (retry.rs:22-24, 69-73).
		if sameAttempt == 0 {
			return retryPlan{action: retrySameAccount, delay: 200 * time.Millisecond}
		}
		return retryPlan{action: retryStop}
	case isCreditExhausted400(body):
		// The request is valid but this account is out of credits: never retry it
		// here, move down the retry ladder (A retry.rs:74-76). When no account is
		// left, forwardScheduled surfaces this original 400.
		return retryPlan{action: retrySwitchAccount}
	case isIllegalChannel400(body):
		// 11128 channel risk control (A retry.rs:26-59, routes.rs:1944-1990): the
		// block follows the request fingerprint, not the account, so A cleans the
		// fingerprint and retries the same account once after 200ms. This host
		// has no fingerprint templates; the single immediate retry is the part
		// that can help.
		if sameAttempt == 0 {
			return retryPlan{action: retrySameAccount, delay: 200 * time.Millisecond}
		}
		return retryPlan{action: retryStop}
	}
	return retryPlan{action: retryStop}
}

// isThinkingSignature400 ports A's is_thinking_signature_error (retry.rs:22-24):
// both words must appear, which also matches the "invalid thinking signature"
// spellings some upstreams use instead of "thinking.signature".
func isThinkingSignature400(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "thinking") && strings.Contains(lower, "signature")
}

// isCreditExhausted400 ports A's is_credit_exhausted_error (mod.rs:624-630,
// issue #67) plus the phrases this host's upstreams use. A deliberately omits
// "quota exceeded" (its 4008 is a rate limit); here a 400 that names quota
// exhaustion is an account failure, so it is included.
func isCreditExhausted400(body string) bool {
	lower := strings.ToLower(body)
	for _, needle := range []string{
		"insufficient credit", "insufficient balance", "credit balance",
		"quota exceeded", "余额不足", "额度不足", "积分不足",
	} {
		if needle != "" && strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// isIllegalChannel400 ports A's is_illegal_channel_error (retry.rs:26-59):
// the upstream thinks the caller is not an approved client.
func isIllegalChannel400(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "illegal api invocation") ||
		strings.Contains(lower, "unapproved channel") ||
		containsCode11128(body)
}

// containsCode11128 ports A's `"code": 11128` matcher (retry.rs:40-59): it
// tolerates whitespace around key and colon, and requires a digit boundary so
// 111280/111289 and token substrings do not match.
func containsCode11128(body string) bool {
	const key = `"code"`
	const code = "11128"
	for rest := body; ; {
		pos := strings.Index(rest, key)
		if pos < 0 {
			return false
		}
		after := strings.TrimLeft(rest[pos+len(key):], " \t\r\n")
		if strings.HasPrefix(after, ":") {
			after = strings.TrimLeft(after[1:], " \t\r\n")
			if strings.HasPrefix(after, code) {
				tail := after[len(code):]
				if tail == "" || tail[0] < '0' || tail[0] > '9' {
					return true
				}
			}
		}
		rest = rest[pos+len(key):]
	}
}

// parseRetryAfter accepts both forms of RFC 9110 Retry-After: delay-seconds
// and an HTTP date. A past date means "retry now" (0s); garbage is ignored and
// lets the caller fall back to its linear backoff.
func parseRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		return clampRetryWait(time.Duration(secs) * time.Second), true
	}
	if at, err := http.ParseTime(raw); err == nil {
		delay := at.Sub(now)
		if delay < 0 {
			delay = 0
		}
		return clampRetryWait(delay), true
	}
	return 0, false
}

func clampRetryWait(d time.Duration) time.Duration {
	switch {
	case d < 0:
		return 0
	case d > maxRetryWait:
		return maxRetryWait
	default:
		return d
	}
}

// retryableStatus is the set of upstream answers that deliver buffers for the
// retry loop. 401/403 are included so a potential account switch can happen
// before the error reaches the client. 404 is included because a model that is
// absent on one account can still exist on another; after the pool is exhausted
// writeAttempt replays the last 404 verbatim. 400 is included because its body
// decides between retry / switch / stop (A retry.rs:63-83); terminal 400s are
// replayed verbatim by writeAttempt. The other 4xx stay terminal and stream
// through.
func retryableStatus(status int) bool {
	switch status {
	case 0, http.StatusBadRequest, http.StatusRequestTimeout, http.StatusConflict,
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}

// retryEvent is one scheduler log row explaining why the next attempt runs on
// the same account, another account, or not at all.
type retryEvent struct {
	status     int
	errText    string
	retryAfter string
	wait       time.Duration
	action     string
}

// logRetry records the status-specific decision (design §6: explain "why the
// account changed"). retry_after keeps the raw upstream header for audits.
func (g *Gateway) logRetry(rec *reqRecord, decision scheduler.Decision, attempt int, ev *retryEvent) {
	if g.schedLog == nil || ev == nil {
		return
	}
	fields := map[string]any{
		"req_id":  rec.reqID,
		"model":   rec.model,
		"plugin":  decision.PluginID,
		"account": decision.AccountID,
		"attempt": attempt + 1,
		"status":  ev.status,
		"action":  ev.action,
		"wait_ms": ev.wait.Milliseconds(),
	}
	if ev.retryAfter != "" {
		fields["retry_after"] = ev.retryAfter
	}
	if ev.errText != "" {
		fields["error"] = truncateUTF8(ev.errText, 256)
	}
	g.schedLog.Info("scheduler", "retry decision", fields)
}

// hasBinding reports whether a logical model is bound to plugin sources.
func (g *Gateway) hasBinding(ctx context.Context, modelID string) bool {
	if g == nil || g.snapshot == nil || strings.TrimSpace(modelID) == "" {
		return false
	}
	snap, err := g.snapshot(ctx)
	if err != nil || snap == nil {
		return false
	}
	binding, ok := snap.Bindings[modelID]
	return ok && len(binding.Items) > 0
}

// scheduleModel resolves the model against the snapshot and asks the scheduler
// for an account. It returns errNotScheduled for models that belong to A.
func (g *Gateway) scheduleModel(ctx context.Context, modelID, conversationID, clientKey string, exclude map[scheduler.AccountKey]bool) (*scheduler.Snapshot, scheduler.Decision, error) {
	if g == nil || strings.TrimSpace(modelID) == "" {
		return nil, scheduler.Decision{}, errNotScheduled
	}
	pluginID, physical, prefixed := strings.Cut(modelID, "/")
	prefixed = prefixed && strings.TrimSpace(pluginID) != "" && strings.TrimSpace(physical) != ""

	if g.snapshot == nil {
		if !prefixed {
			return nil, scheduler.Decision{}, errNotScheduled
		}
		return nil, scheduler.Decision{
			PluginID: strings.TrimSpace(pluginID),
			Model:    strings.TrimSpace(physical),
		}, nil
	}

	snap, err := g.snapshot(ctx)
	if err != nil {
		if !prefixed {
			return nil, scheduler.Decision{}, errNotScheduled
		}
		return nil, scheduler.Decision{}, err
	}
	if snap == nil {
		if !prefixed {
			return nil, scheduler.Decision{}, errNotScheduled
		}
		return nil, scheduler.Decision{
			PluginID: strings.TrimSpace(pluginID),
			Model:    strings.TrimSpace(physical),
		}, nil
	}

	if prefixed && snap.Pools[strings.TrimSpace(pluginID)] == nil {
		// The prefix is not an installed plugin, so this is a logical binding
		// whose name happens to contain a slash. Keep it on the scheduled path
		// instead of failing with an unknown plugin.
		if binding, ok := snap.Bindings[modelID]; ok && len(binding.Items) > 0 {
			prefixed = false
		}
	}
	if !prefixed {
		if binding, ok := snap.Bindings[modelID]; !ok || len(binding.Items) == 0 {
			return nil, scheduler.Decision{}, errNotScheduled
		}
	}
	if g.sched == nil {
		if !prefixed {
			return nil, scheduler.Decision{}, errNotScheduled
		}
		return snap, scheduler.Decision{
			PluginID: strings.TrimSpace(pluginID),
			Model:    strings.TrimSpace(physical),
		}, nil
	}

	// api_level 1 plugins pick their own account; forcing one is meaningless.
	// Logical bindings always go through the scheduler (the first source may
	// still be an L1 pool, which then serves without a pinned account).
	if prefixed {
		if pool := snap.Pools[strings.TrimSpace(pluginID)]; pool != nil && pool.APIVersion < 2 {
			return snap, scheduler.Decision{
				PluginID: strings.TrimSpace(pluginID),
				Model:    strings.TrimSpace(physical),
			}, nil
		}
	}

	decision, err := g.sched.Pick(ctx, snap, scheduler.Request{
		Model:          modelID,
		ConversationID: conversationID,
		ClientKey:      clientKey,
		Exclude:        exclude,
	})
	if err != nil {
		return snap, scheduler.Decision{}, err
	}
	return snap, decision, nil
}

// forwardScheduled implements the chat path for plugin-bound models: pick,
// rewrite, forward, report, and fail over before the first byte.
//
// Retry waits and account switching live here; account/model cooldowns remain
// scheduler.Report's responsibility. Its existing constants (429: 30s doubling
// to 10m, 5xx: 60s) are intentionally left untouched: A's 30m/6h server curve
// (mod.rs:551-563, pool.rs:823-896) and Retry-After-based 429 cooldown would
// change pinned scheduler tests.
func (g *Gateway) forwardScheduled(w http.ResponseWriter, r *http.Request, rec *reqRecord, body []byte, path string) {
	exclude := map[scheduler.AccountKey]bool{}
	// refreshed remembers the single 401/403 self-heal spent per account.
	refreshed := map[scheduler.AccountKey]bool{}
	var buffered *upstreamAttemptResult
	var lastErr error
	// activeRelease releases the concurrency slot and scheduler lease of the
	// current attempt if the handler unwinds unexpectedly (panic). It is
	// idempotent, so the normal release paths are unchanged.
	var activeRelease func()
	defer func() {
		if activeRelease != nil {
			activeRelease()
		}
	}()

	for attempt := 0; attempt < maxScheduleAttempts; attempt++ {
		snap, decision, err := g.scheduleModel(r.Context(), rec.model, rec.conversationID, requestClientKey(r, rec.keyID), exclude)
		if err != nil {
			if errors.Is(err, errNotScheduled) {
				// The scheduler does not own this name. A bare model name may
				// still belong to the configured default plugin: rewrite it to
				// "<id>/<model>" and let the same loop resolve it, so the request
				// goes back through account routing instead of bypassing it.
				tgt, ok, derr := g.bareModelTarget(rec.model)
				switch {
				case derr != nil && errors.Is(derr, errNoDefaultRoute):
					g.writeUnknownModel(w, rec, rec.model, "")
					return
				case derr != nil:
					g.fail(w, rec, http.StatusServiceUnavailable, "plugin_unavailable", "plugin_unavailable", derr.Error())
					return
				case ok:
					rec.model = tgt.status.Manifest.ID + "/" + tgt.inner
					rec.source = "plugin:" + tgt.status.Manifest.ID
					continue
				default:
					g.forwardA(w, r, rec, body)
					return
				}
			}
			if buffered != nil {
				break // a real upstream failure is more informative than no candidate
			}
			g.failSchedule(w, rec, err)
			return
		}

		g.logDecision(rec, decision, attempt)

		id := strings.TrimSpace(decision.PluginID)
		if id == "" || strings.TrimSpace(decision.Model) == "" {
			if buffered != nil {
				break
			}
			g.failSchedule(w, rec, fmt.Errorf("%w: empty decision", scheduler.ErrNoCandidate))
			return
		}
		pool := snap.Pools[id]
		pinned := decision.AccountID
		if pool != nil && pool.APIVersion < 2 {
			pinned = "" // L1: the plugin chooses its own account
		}

		// Mark the plugin busy before starting it: a scheduled stream can run
		// far longer than the manifest idle timeout, and without this the idle
		// reaper could stop the plugin mid-response.

		// Mark the plugin busy before starting it: a scheduled stream can run
		// far longer than the manifest idle timeout, and without this the idle
		// reaper could stop the plugin mid-response.
		if tracker, ok := g.plugins.(interface{ BeginCall(string) func() }); ok {
			release := tracker.BeginCall(id)
			defer release()
		}

		if err := g.plugins.EnsureRunning(r.Context(), id); err != nil {
			lastErr = fmt.Errorf("插件 %s 无法启动: %w", id, err)
			g.reportFailure(decision, id, pinned, rec, 0)
			exclude[decisionKey(decision)] = true
			continue
		}
		baseURL, token, ok := g.plugins.Endpoint(id)
		if !ok || strings.TrimSpace(baseURL) == "" {
			lastErr = fmt.Errorf("插件 %s 未运行", id)
			g.reportFailure(decision, id, pinned, rec, 0)
			exclude[decisionKey(decision)] = true
			continue
		}
		if err := g.validateModel(r.Context(), id, decision.Model, strings.Contains(rec.model, "/")); err != nil {
			g.writeUnknownModel(w, rec, rec.model, id)
			return
		}
		// The resource page's "接入" switch removes a model from the pool's
		// catalog; the pool then no longer serves it.
		if catalogPool := snap.Pools[id]; catalogPool != nil && len(catalogPool.Models) > 0 {
			if _, ok := catalogPool.Models[decision.Model]; !ok {
				g.writeUnknownModel(w, rec, rec.model, id, exposedPluginModels(snap))
				return
			}
		}

		rec.source = "plugin:" + id
		rec.logicalModel = decision.LogicalModel
		if pinned != "" {
			rec.account = pinned
		}

		prepared, physical, downgraded, err := prepareBody(body, decision.Model, snap, decision)
		if err != nil {
			g.fail(w, rec, http.StatusBadRequest, "invalid_request_error", "invalid_json",
				"request body must be a JSON object")
			return
		}
		decision.Model = physical
		rec.downgraded = downgraded

		headers := forwardHeaders(r.Header)
		if token != "" {
			headers.Set("Authorization", "Bearer "+token)
		}
		headers.Set(requestIDHeader, rec.reqID)
		headers.Del(accountHeader)
		headers.Del(hedgeHeader)
		if pinned != "" {
			headers.Set(accountHeader, pinned)
		}

		release, err := g.lim.acquire(r.Context(), id, g.plugins.Concurrency(id), g.queueTimeout)
		if err != nil {
			if errors.Is(err, errQueueTimeout) {
				g.fail(w, rec, http.StatusTooManyRequests, "concurrency_limit", "concurrency_limit",
					fmt.Sprintf("插件 %s 并发已满，排队超时", id))
				return
			}
			rec.status = 499
			rec.errText = "client canceled while queuing"
			return
		}

		g.plugins.Touch(id)
		target := strings.TrimRight(baseURL, "/") + path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}

		key := decisionKey(decision)
		if pinned != "" && g.sched != nil {
			g.sched.Acquire(key)
		}
		// One idempotent release for both the normal and the panic path: the
		// limiter's release and scheduler.Release are not re-entrant.
		releaseAttempt := sync.OnceFunc(func() {
			release()
			if pinned != "" && g.sched != nil {
				g.sched.Release(key)
			}
		})
		activeRelease = releaseAttempt
		pa := &preparedAttempt{
			decision: decision,
			pinned:   pinned,
			target:   target,
			headers:  headers,
			body:     prepared,
			release:  releaseAttempt,
		}

		hedgeMS := 0
		if pool != nil {
			hedgeMS = pool.HedgeMS
		}
		var (
			res         *upstreamAttemptResult
			winDecision = decision
			winPinned   = pinned
			secondKey   *scheduler.AccountKey
			hedged      bool
		)
		// A first-byte hedge (A routes.rs:2655-2775) already races two accounts;
		// the retry loop below only handles plain attempts.
		if attempt == 0 && rec.stream && hedgeMS >= 1000 && g.sched != nil {
			hedged = true
			out := g.hedgedAttempt(w, r, rec, body, snap, pa, time.Duration(hedgeMS)*time.Millisecond)
			res, winDecision, winPinned, secondKey = out.res, out.decision, out.pinned, out.secondKey
		} else {
			res = g.attempt(w, r, rec, pa.target, pa.headers, pa.body, pa.pinned)
		}
		// The concurrency slot and the scheduler lease stay held while the same
		// account backs off, so no other request can jump onto it.
		releaseHeld := func() {
			if !hedged && pa.release != nil {
				pa.release()
				pa.release = nil
			}
		}

		sameRetries := 0
		stop := false
		for {
			if res.streamed {
				releaseHeld()
				g.reportAttempt(winDecision, winDecision.PluginID, winPinned, rec, res)
				return
			}
			if res.clientGone {
				releaseHeld()
				rec.status = 499
				if rec.errText == "" {
					rec.errText = "client canceled"
				}
				return
			}

			// Report only the final outcome of this account. Reporting every
			// same-account retry would stack cooldowns (3x429 -> 30/60/120s)
			// for one client request.
			buffered = res
			lastErr = nil
			// 401/403: A retry.rs switches keys; the host manager can refresh a
			// credential first, so spend at most one self-heal per account and
			// retry it immediately. No header means we only switch accounts.
			refreshNote := ""
			refreshKey := decisionKey(winDecision)
			if !hedged && winPinned != "" &&
				(res.status == http.StatusUnauthorized || res.status == http.StatusForbidden) &&
				!refreshed[refreshKey] {
				if refresher, ok := g.plugins.(accountRefresher); ok {
					refreshed[refreshKey] = true
					out, rerr := refresher.Refresh(r.Context(), winDecision.PluginID, winPinned)
					if rerr == nil && strings.EqualFold(strings.TrimSpace(out.Result), "ok") {
						g.logRetry(rec, winDecision, attempt, &retryEvent{
							status:  res.status,
							errText: "credential refreshed",
							action:  "refresh_same",
						})
						res = g.attempt(w, r, rec, pa.target, pa.headers, pa.body, winPinned)
						continue
					}
					switch {
					case rerr != nil:
						refreshNote = "credential refresh failed: " + rerr.Error()
					case strings.TrimSpace(out.Message) != "":
						refreshNote = "credential refresh failed: " + out.Message
					default:
						refreshNote = "credential refresh failed"
					}
				}
			}

			plan := planRetry(res.status, res.header, attemptBodyText(res), sameRetries, g.now())
			if hedged && plan.action == retrySameAccount {
				// A hedge already spent two accounts; fail over rather than
				// sleeping on one of them.
				plan = retryPlan{action: retrySwitchAccount}
			}
			if (res.status == http.StatusUnauthorized || res.status == http.StatusForbidden) &&
				refreshed[refreshKey] {
				// The single self-heal was already spent (or failed).
				plan = retryPlan{action: retrySwitchAccount}
			}
			if winPinned == "" &&
				(res.status == http.StatusUnauthorized || res.status == http.StatusForbidden) {
				// An api_level 1 plugin owns its accounts and no account id was
				// pinned, so there is nothing to switch to; re-running the same
				// request would only repeat the same rejection.
				plan = retryPlan{action: retryStop}
			}
			errText := res.errText
			if refreshNote != "" {
				errText = refreshNote
			}
			g.logRetry(rec, winDecision, attempt, &retryEvent{
				status:     res.status,
				errText:    errText,
				retryAfter: strings.TrimSpace(res.header.Get("Retry-After")),
				wait:       plan.delay,
				action:     plan.action.String(),
			})

			if plan.action == retrySameAccount {
				sameRetries++
				if !g.sleep(r.Context(), plan.delay) {
					releaseHeld()
					rec.status = 499
					rec.errText = "client canceled during retry backoff"
					return
				}
				res = g.attempt(w, r, rec, pa.target, pa.headers, pa.body, winPinned)
				continue
			}
			g.reportAttempt(winDecision, winDecision.PluginID, winPinned, rec, res)
			stop = plan.action == retryStop
			break
		}
		releaseHeld()

		if stop || attempt == maxScheduleAttempts-1 {
			break
		}
		exclude[decisionKey(winDecision)] = true
		if secondKey != nil {
			exclude[*secondKey] = true
		}
	}

	if buffered != nil {
		g.writeAttempt(w, rec, buffered)
		return
	}
	if lastErr != nil {
		g.fail(w, rec, http.StatusServiceUnavailable, "plugin_unavailable", "plugin_unavailable",
			lastErr.Error())
		return
	}
	g.failSchedule(w, rec, scheduler.ErrNoCandidate)
}

// preparedAttempt is one fully built upstream call.
type preparedAttempt struct {
	decision   scheduler.Decision
	pinned     string
	target     string
	headers    http.Header
	body       []byte
	downgraded string
	release    func()
}

// hedgeResult reports which attempt won the race.
type hedgeResult struct {
	res       *upstreamAttemptResult
	decision  scheduler.Decision
	pinned    string
	secondKey *scheduler.AccountKey
}

type hedgeOutcome struct {
	resp     *http.Response
	err      error
	prepared *preparedAttempt
}

// hedgedAttempt races the first candidate against a second one when the first
// has not produced response headers within hedge_ms (design §6.4). Only the
// winner streams to the client; the loser is cancelled and its body closed.
func (g *Gateway) hedgedAttempt(w http.ResponseWriter, r *http.Request, rec *reqRecord, rawBody []byte, snap *scheduler.Snapshot, first *preparedAttempt, hedgeAfter time.Duration) *hedgeResult {
	ctx1, cancel1 := context.WithCancel(r.Context())
	defer cancel1()
	// Both prepared attempts hold a concurrency slot and possibly a scheduler
	// lease; release them even if the race unwinds with a panic.
	var second *preparedAttempt
	defer func() {
		if first != nil && first.release != nil {
			first.release()
		}
		if second != nil && second.release != nil {
			second.release()
		}
	}()
	firstCh := make(chan hedgeOutcome, 1)
	go func() {
		resp, err := g.start(ctx1, r.Method, first.target, first.headers, first.body)
		firstCh <- hedgeOutcome{resp: resp, err: err, prepared: first}
	}()

	timer := time.NewTimer(hedgeAfter)
	defer timer.Stop()
	var firstOut *hedgeOutcome
	select {
	case o := <-firstCh:
		firstOut = &o
	case <-timer.C:
	}
	if firstOut != nil {
		return g.finishHedge(w, r, rec, firstOut, nil)
	}

	exclude := map[scheduler.AccountKey]bool{decisionKey(first.decision): true}
	second, secondKey, err := g.prepareHedge(r, rec, rawBody, snap, exclude)
	if err != nil || second == nil {
		o := <-firstCh
		return g.finishHedge(w, r, rec, &o, nil)
	}

	ctx2, cancel2 := context.WithCancel(r.Context())
	defer cancel2()
	secondCh := make(chan hedgeOutcome, 1)
	go func() {
		resp, err := g.start(ctx2, r.Method, second.target, second.headers, second.body)
		secondCh <- hedgeOutcome{resp: resp, err: err, prepared: second}
	}()

	var secondOut *hedgeOutcome
	for {
		select {
		case o := <-firstCh:
			firstOut = &o
		case o := <-secondCh:
			secondOut = &o
		}
		if usableHedge(firstOut) != nil {
			cancel2()
			g.closeHedgeLoser(secondOut, secondCh)
			return g.finishHedge(w, r, rec, firstOut, secondKey)
		}
		if usableHedge(secondOut) != nil {
			cancel1()
			g.closeHedgeLoser(firstOut, firstCh)
			return g.finishHedge(w, r, rec, secondOut, secondKey)
		}
		if firstOut != nil && secondOut != nil {
			winner := firstOut
			if winner.err != nil && secondOut.err == nil {
				winner = secondOut
			}
			return g.finishHedge(w, r, rec, winner, secondKey)
		}
	}
}

// usableHedge returns an outcome whose headers already prove a usable answer.
func usableHedge(o *hedgeOutcome) *hedgeOutcome {
	if o == nil || o.err != nil || o.resp == nil {
		return nil
	}
	if retryableStatus(o.resp.StatusCode) {
		return nil
	}
	return o
}

// closeHedgeLoser waits for the cancelled attempt to return and closes it.
func (g *Gateway) closeHedgeLoser(out *hedgeOutcome, ch chan hedgeOutcome) {
	if out != nil {
		if out.resp != nil {
			_ = out.resp.Body.Close()
		}
		if out.prepared != nil && out.prepared.release != nil {
			out.prepared.release()
		}
		return
	}
	select {
	case o := <-ch:
		if o.resp != nil {
			_ = o.resp.Body.Close()
		}
		if o.prepared != nil && o.prepared.release != nil {
			o.prepared.release()
		}
	case <-time.After(time.Second):
	}
}

// finishHedge streams or buffers the winning attempt.
func (g *Gateway) finishHedge(w http.ResponseWriter, r *http.Request, rec *reqRecord, o *hedgeOutcome, secondKey *scheduler.AccountKey) *hedgeResult {
	p := o.prepared
	out := &hedgeResult{decision: p.decision, pinned: p.pinned, secondKey: secondKey}
	if o.err != nil {
		if p.release != nil {
			p.release()
		}
		res := &upstreamAttemptResult{errText: o.err.Error()}
		if r.Context().Err() != nil {
			res.clientGone = true
		}
		out.res = res
		return out
	}
	rec.source = "plugin:" + p.decision.PluginID
	rec.logicalModel = p.decision.LogicalModel
	rec.downgraded = p.downgraded
	if p.pinned != "" {
		rec.account = p.pinned
	}
	out.res = g.deliver(w, rec, o.resp, p.pinned)
	_ = o.resp.Body.Close()
	if p.release != nil {
		p.release()
	}
	return out
}

// prepareHedge builds the second candidate with Explain so the sticky binding
// of the first attempt is never overwritten by a hedge that may lose.
func (g *Gateway) prepareHedge(r *http.Request, rec *reqRecord, rawBody []byte, snap *scheduler.Snapshot, exclude map[scheduler.AccountKey]bool) (*preparedAttempt, *scheduler.AccountKey, error) {
	explanation, err := g.sched.Explain(r.Context(), snap, scheduler.Request{
		Model:          rec.model,
		ConversationID: rec.conversationID,
		ClientKey:      requestClientKey(r, rec.keyID),
		Exclude:        exclude,
	})
	if err != nil {
		return nil, nil, err
	}
	if explanation.Decision == nil {
		return nil, nil, scheduler.ErrNoCandidate
	}
	decision := *explanation.Decision
	id := strings.TrimSpace(decision.PluginID)
	if id == "" || strings.TrimSpace(decision.Model) == "" {
		return nil, nil, scheduler.ErrNoCandidate
	}
	pool := snap.Pools[id]
	pinned := decision.AccountID
	if pool == nil || pool.APIVersion < 2 {
		pinned = ""
	}
	if err := g.plugins.EnsureRunning(r.Context(), id); err != nil {
		return nil, nil, err
	}
	baseURL, token, ok := g.plugins.Endpoint(id)
	if !ok || strings.TrimSpace(baseURL) == "" {
		return nil, nil, fmt.Errorf("插件 %s 未运行", id)
	}
	if err := g.validateModel(r.Context(), id, decision.Model, strings.Contains(rec.model, "/")); err != nil {
		return nil, nil, err
	}
	if catalogPool := snap.Pools[id]; catalogPool != nil && len(catalogPool.Models) > 0 {
		if _, ok := catalogPool.Models[decision.Model]; !ok {
			return nil, nil, errUnknownModel
		}
	}
	prepared, physical, downgraded, err := prepareBody(rawBody, decision.Model, snap, decision)
	if err != nil {
		return nil, nil, err
	}
	decision.Model = physical

	headers := forwardHeaders(r.Header)
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	headers.Set(requestIDHeader, rec.reqID)
	headers.Del(accountHeader)
	if pinned != "" {
		headers.Set(accountHeader, pinned)
	}
	headers.Set(hedgeHeader, "1")

	release, err := g.lim.acquire(r.Context(), id, g.plugins.Concurrency(id), g.queueTimeout)
	if err != nil {
		return nil, nil, err
	}
	g.plugins.Touch(id)
	key := decisionKey(decision)
	if pinned != "" {
		g.sched.Acquire(key)
	}
	releaseOnce := sync.OnceFunc(func() {
		release()
		if pinned != "" {
			g.sched.Release(key)
		}
	})
	target := strings.TrimRight(baseURL, "/") + "/v1/chat/completions"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	return &preparedAttempt{
		decision:   decision,
		pinned:     pinned,
		target:     target,
		headers:    headers,
		body:       prepared,
		downgraded: downgraded,
		release:    releaseOnce,
	}, &key, nil
}

// validateModel keeps the v0.2 model check for explicit <plugin>/<model>
// requests. Logical bindings name their physical models explicitly, so they
// are not filtered here.
func (g *Gateway) validateModel(ctx context.Context, pluginID, physical string, direct bool) error {
	if !direct {
		return nil
	}
	st, ok := g.pluginStatus(pluginID)
	if !ok {
		return nil
	}
	mods, known := g.pluginModelList(ctx, st)
	if !known {
		return nil
	}
	for _, m := range mods {
		if m.ID == physical {
			return nil
		}
	}
	return errUnknownModel
}

var errUnknownModel = errors.New("unknown model")

// exposedPluginModels mirrors the GET /v1/models exposure rule: a pool with
// catalog models only exposes those; a pool without any has no filter.
func exposedPluginModels(snap *scheduler.Snapshot) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	if snap == nil {
		return out
	}
	for pluginID, pool := range snap.Pools {
		if pool == nil || len(pool.Models) == 0 {
			continue
		}
		set := make(map[string]bool, len(pool.Models))
		for modelID := range pool.Models {
			set[CanonicalModelID(modelID)] = true
		}
		out[pluginID] = set
	}
	return out
}

func (g *Gateway) pluginStatus(pluginID string) (model.PluginStatus, bool) {
	if g.plugins == nil {
		return model.PluginStatus{}, false
	}
	for _, s := range g.plugins.List() {
		if s.Manifest.ID == pluginID {
			return s, true
		}
	}
	return model.PluginStatus{}, false
}

// failSchedule turns a scheduler error into an HTTP error.
func (g *Gateway) failSchedule(w http.ResponseWriter, rec *reqRecord, err error) {
	msg := err.Error()
	switch {
	case errors.Is(err, scheduler.ErrUnknownModel):
		g.writeUnknownModel(w, rec, rec.model, "")
	case errors.Is(err, scheduler.ErrNoCandidate):
		w.Header().Set("Retry-After", "10")
		g.fail(w, rec, http.StatusServiceUnavailable, "no_available_account",
			"no_available_account", "暂无可用的模型账号: "+msg)
	default:
		w.Header().Set("Retry-After", "5")
		g.fail(w, rec, http.StatusServiceUnavailable, "scheduler_unavailable",
			"scheduler_unavailable", "调度失败: "+msg)
	}
}

// reportFailure records a pre-request failure (cold start / endpoint loss).
func (g *Gateway) reportFailure(decision scheduler.Decision, pluginID, pinned string, rec *reqRecord, status int) {
	if g.sched == nil || pinned == "" {
		return
	}
	g.sched.Report(scheduler.AccountKey{PluginID: pluginID, AccountID: decision.AccountID},
		decision.Model, status, 0, 0)
}

func decisionKey(decision scheduler.Decision) scheduler.AccountKey {
	return scheduler.AccountKey{PluginID: decision.PluginID, AccountID: decision.AccountID}
}

// logDecision writes one scheduler event per attempt (design §7).
func (g *Gateway) logDecision(rec *reqRecord, decision scheduler.Decision, attempt int) {
	if g.schedLog == nil {
		return
	}
	fields := map[string]any{
		"req_id":         rec.reqID,
		"model":          rec.model,
		"plugin":         decision.PluginID,
		"account":        decision.AccountID,
		"model_upstream": decision.Model,
		"sticky":         decision.Sticky,
		"attempt":        attempt + 1,
	}
	if decision.LogicalModel != "" {
		fields["logical_model"] = decision.LogicalModel
	}
	if len(decision.Reasons) > 0 {
		fields["reasons"] = strings.Join(decision.Reasons, "; ")
	}
	g.schedLog.Info("scheduler", "dispatch decision", fields)
}

// upstreamAttemptResult is one upstream call. Streamed responses were already
// written to the client; buffered ones are retryable failures kept for the
// final response when no candidate succeeds.
type upstreamAttemptResult struct {
	status     int
	header     http.Header
	body       []byte
	streamed   bool
	clientGone bool
	account    string
	credits    *float64
	errText    string
}

func (r *upstreamAttemptResult) retryable() bool {
	if r == nil || r.streamed {
		return false
	}
	return retryableStatus(r.status)
}

// attempt performs the upstream call. Successful responses stream through
// unchanged; retryable failures are buffered so another account can be tried.
func (g *Gateway) attempt(w http.ResponseWriter, r *http.Request, rec *reqRecord, target string, headers http.Header, body []byte, pinned string) *upstreamAttemptResult {
	res := &upstreamAttemptResult{}
	resp, err := g.start(r.Context(), r.Method, target, headers, body)
	if err != nil {
		if r.Context().Err() != nil {
			res.clientGone = true
			return res
		}
		res.errText = err.Error()
		return res
	}
	defer resp.Body.Close()
	return g.deliver(w, rec, resp, pinned)
}

// start issues one upstream request and returns once headers arrive. The
// returned body carries the idle-read deadline and owns the per-request cancel.
func (g *Gateway) start(ctx context.Context, method, target string, headers http.Header, body []byte) (*http.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header = headers
	req.ContentLength = int64(len(body))
	if len(body) == 0 {
		req.Body = nil
		req.GetBody = nil
		req.ContentLength = 0
	}
	resp, err := g.client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = newIdleReadBody(resp.Body, upstreamIdleReadTimeout, cancel)
	return resp, nil
}

// deliver streams a successful response or buffers a retryable failure.
func (g *Gateway) deliver(w http.ResponseWriter, rec *reqRecord, resp *http.Response, pinned string) *upstreamAttemptResult {
	res := &upstreamAttemptResult{}
	res.status = resp.StatusCode
	res.header = resp.Header.Clone()
	res.account = strings.TrimSpace(resp.Header.Get(accountHeader))
	if res.account == "" {
		res.account = pinned
	}
	res.credits = parseCredits(resp.Header.Get(creditsHeader))

	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") ||
		rec.stream

	if res.retryable() {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, retryBodyCap))
		res.body = buf
		if rec.errText == "" {
			res.errText = truncateUTF8(strings.TrimSpace(string(buf)), 512)
		}
		return res
	}

	rec.status = resp.StatusCode
	dst := w.Header()
	copyResponseHeaders(dst, resp.Header)
	dst.Set(sourceHeader, rec.source)
	if rec.downgraded != "" {
		dst.Set(downgradedHeader, rec.downgraded)
	}

	w.WriteHeader(resp.StatusCode)
	headerAt := time.Now()

	flusher, _ := w.(http.Flusher)
	if isSSE && flusher != nil {
		flusher.Flush()
	}

	tap := newBodyTap(g.bodyCap, isSSE)
	ttfbSet := false
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if !ttfbSet {
				rec.ttfb = time.Since(rec.start)
				rec.firstTokenAt = g.now()
				ttfbSet = true
			}
			tap.feed(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				rec.errText = "client write failed"
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				rec.errText = "upstream read failed: " + readErr.Error()
			}
			break
		}
	}
	if !ttfbSet {
		rec.ttfb = headerAt.Sub(rec.start)
		rec.firstTokenAt = headerAt
	}
	tap.finish()
	rec.prompt, rec.completion, rec.cached = tap.prompt, tap.completion, tap.cached

	if g.debugBody.Load() {
		rec.bodyResp = string(tap.preview)
	}
	if rec.status >= 400 && rec.errText == "" {
		rec.errText = truncateUTF8(strings.TrimSpace(string(tap.preview)), 512)
	}
	res.streamed = true
	return res
}

// writeAttempt writes a buffered upstream response verbatim, except for the
// human-readable message of a 5xx: when the request was routed to a
// (plugin, account) that egresses through a recently failed proxy, one clause
// naming that proxy is appended (egress_hint.go). Status, code and shape are
// untouched.
func (g *Gateway) writeAttempt(w http.ResponseWriter, rec *reqRecord, res *upstreamAttemptResult) {
	// A transport failure leaves status at zero. net/http panics on an invalid
	// status code, so a pool whose upstream is unreachable would crash the
	// request goroutine instead of returning 503.
	if res.status < 200 {
		msg := "上游不可达：" + res.errText
		if hint := g.egressFailureHint(rec); hint != "" {
			msg += hint
		}
		g.fail(w, rec, http.StatusServiceUnavailable, "upstream_unavailable", "upstream_unavailable", msg)
		return
	}
	if res.status >= 500 {
		// Best-effort and off the success path: "" keeps res.body byte-identical.
		res.body = appendEgressHint(res.body, g.egressFailureHint(rec))
	}
	if res.header != nil {
		dst := w.Header()
		copyResponseHeaders(dst, res.header)
		dst.Set(sourceHeader, rec.source)
		if rec.downgraded != "" {
			dst.Set(downgradedHeader, rec.downgraded)
		}
	}
	rec.status = res.status
	if res.errText != "" && rec.errText == "" {
		rec.errText = res.errText
	}
	w.WriteHeader(res.status)
	_, _ = w.Write(res.body)
}

func (g *Gateway) reportAttempt(decision scheduler.Decision, pluginID, pinned string, rec *reqRecord, res *upstreamAttemptResult) {
	if res == nil {
		return
	}
	if res.account != "" {
		rec.account = res.account
	}
	if res.credits != nil {
		rec.credits = res.credits
	}
	if g.sched == nil || pinned == "" || res.clientGone {
		return
	}
	status := res.status
	if status == http.StatusBadRequest && isCreditExhausted400(attemptBodyText(res)) {
		// A treats a 400 credit exhaustion as a hard-credit failure
		// (mod.rs:624-630). The scheduler's quota class is 429, so reporting it
		// that way parks account+model instead of re-picking it next request.
		status = http.StatusTooManyRequests
	}
	credits := 0.0
	if res.credits != nil {
		credits = *res.credits
	}
	g.sched.Report(scheduler.AccountKey{PluginID: pluginID, AccountID: decision.AccountID},
		decision.Model, status, rec.ttfb.Milliseconds(), credits)
}

// attemptBodyText returns the buffered error body of one attempt, falling back
// to the short errText used for transport failures.
func attemptBodyText(res *upstreamAttemptResult) string {
	if res == nil {
		return ""
	}
	if len(res.body) > 0 {
		return string(res.body)
	}
	return res.errText
}

func parseCredits(raw string) *float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		return nil
	}
	return &v
}

// prepareBody rewrites the model to the physical source and injects the pool's
// default reasoning effort when the request does not carry one. Downgrades are
// applied before the rewrite and marked on the response.
func prepareBody(body []byte, physical string, snap *scheduler.Snapshot, decision scheduler.Decision) ([]byte, string, string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, physical, "", err
	}
	downgraded := ""
	if snap != nil {
		meta := bodyMeta(top, len(body))
		if next, reason := scheduler.ApplyDowngrades(snap, decision, meta); reason != "" {
			physical = next.Model
			downgraded = reason
		}
	}
	rawModel, err := json.Marshal(physical)
	if err != nil {
		return nil, physical, downgraded, err
	}
	top["model"] = rawModel

	if snap != nil {
		hasReasoning := false
		if raw, ok := top["reasoning_effort"]; ok {
			var s string
			hasReasoning = json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != ""
		}
		if level, ok := scheduler.DefaultReasoning(snap, decision, hasReasoning); ok {
			raw, err := json.Marshal(level)
			if err != nil {
				return nil, physical, downgraded, err
			}
			top["reasoning_effort"] = raw
		}
	}
	out, err := json.Marshal(top)
	return out, physical, downgraded, err
}

// bodyMeta extracts the fields the downgrade rules need.
func bodyMeta(top map[string]json.RawMessage, size int) scheduler.BodyMeta {
	meta := scheduler.BodyMeta{PromptChars: size}
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if raw, ok := top[key]; ok {
			var n int
			if json.Unmarshal(raw, &n) == nil && n > 0 {
				meta.MaxTokens = n
				break
			}
		}
	}
	return meta
}

// conversationID prefers the explicit header, then the body fields that carry
// a conversation identity (design §6.4).
func conversationID(r *http.Request, body []byte) string {
	if v := strings.TrimSpace(r.Header.Get(conversationHeader)); v != "" {
		return v
	}
	var probe struct {
		ConversationID string `json:"conversation_id"`
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	if v := strings.TrimSpace(probe.ConversationID); v != "" {
		return v
	}
	return strings.TrimSpace(probe.PromptCacheKey)
}

// clientKey identifies the caller for pool stickiness: an explicit header wins
// over the verified client key id.
func requestClientKey(r *http.Request, keyID string) string {
	if v := strings.TrimSpace(r.Header.Get(clientKeyHeader)); v != "" {
		return v
	}
	return keyID
}
