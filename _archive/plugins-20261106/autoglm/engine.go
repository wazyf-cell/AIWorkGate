package main

// The chat pipeline: pick an account, prepare it (token refresh + sandbox +
// wake), then walk the channel ladder. Ported from chat.go's Engine.chat /
// dispatch / streamOut / prepare / handleErr.
//
// The ladder is the reference's:
//
//	proxy  -> skipped for agent targets
//	sandbox-> 404/501/not-enabled falls through in auto mode
//	relay  -> the fallback that always produces a stream
//
// Every step either returns a written response or an account-level failure that
// feeds the scheduler.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
)

// engineConfig is the tunable slice of the reference's Config this plugin needs.
type engineConfig struct {
	// Mode: auto / proxy / openai / relay.
	Mode string
	// MaxRotate caps how many accounts one request may try.
	MaxRotate int
	// HardCooldown / SoftCooldown / ErrCooldown in seconds.
	HardCooldown float64
	SoftCooldown float64
	ErrCooldown  float64
	// ErrThreshold is the consecutive-error count that parks an account.
	ErrThreshold int
}

func defaultEngineConfig() engineConfig {
	return engineConfig{
		Mode:         "auto",
		MaxRotate:    3,
		HardCooldown: 12 * 3600,
		SoftCooldown: 60,
		ErrCooldown:  600,
		ErrThreshold: 3,
	}
}

type engine struct {
	cfg engineConfig
	st  *store
	up  *upstream
}

// resolveRegion reads a region hint from the headers or the body.
func resolveRegion(h http.Header, obj map[string]any, model string) string {
	for _, key := range []string{"X-AutoClaw-Region", "X-Region"} {
		if v := strings.ToLower(strings.TrimSpace(h.Get(key))); v == regionCN || v == regionGlobal {
			return v
		}
	}
	if v := strings.ToLower(strings.TrimSpace(toString(obj["region"]))); v == regionCN || v == regionGlobal {
		return v
	}
	low := strings.ToLower(strings.TrimSpace(model))
	for _, p := range []string{"global/", "cn/", "global:", "cn:"} {
		if strings.HasPrefix(low, p) {
			return p[:len(p)-1]
		}
	}
	return ""
}

// handleErr books an upstream failure. It returns true when a refresh rescued a
// dead session, in which case the same request may retry.
func (e *engine) handleErr(a *account, err error) bool {
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		e.st.noteError(a, e.cfg.ErrThreshold, e.cfg.ErrCooldown)
		return false
	}
	if ue.Kind == "session_dead" {
		if strings.TrimSpace(a.snapshot().RefreshToken) != "" {
			if rerr := e.up.refresh(a); rerr == nil {
				if serr := e.st.syncFlush(); serr == nil {
					e.st.reenable(a)
					log.Printf("autoglm: %s session_dead recovered via refresh", shortUID(a))
					return true
				}
			} else {
				log.Printf("autoglm: %s refresh after session_dead failed: %v", shortUID(a), rerr)
			}
		}
		e.st.disable(a, "session dead")
		return false
	}
	switch ue.Kind {
	case "hard_credit":
		e.st.cooldown(a, "hard_credit", e.cfg.HardCooldown, "余额不足")
	case "soft_rate":
		e.st.cooldown(a, "soft_rate", e.cfg.SoftCooldown, "429 rate limit")
	default:
		e.st.noteError(a, e.cfg.ErrThreshold, e.cfg.ErrCooldown)
	}
	return false
}

// prepare refreshes the token, ensures a sandbox and wakes it.
//
// cn requires a live sandbox for every channel; global is served by the proxy
// path only and therefore needs no sandbox at all, which is exactly what the
// reference does.
func (e *engine) prepare(a *account) error {
	snap := a.snapshot()
	if snap.Region == regionGlobal {
		if a.needsRefresh(600) && strings.TrimSpace(snap.RefreshToken) != "" {
			if err := e.up.refresh(a); err == nil {
				_ = e.st.syncFlush()
			}
		}
		return nil
	}
	if a.needsRefresh(600) {
		if strings.TrimSpace(snap.RefreshToken) == "" {
			return upstreamErrf("session_dead", 401, 0, "no refreshToken — re-login required")
		}
		if err := e.up.refresh(a); err != nil {
			return err
		}
		if err := e.st.syncFlush(); err != nil {
			return err
		}
	}
	sid, _, err := e.up.ensureSandbox(a)
	if err != nil {
		return err
	}
	if err := e.st.syncFlush(); err != nil {
		return err
	}
	return e.up.wakeSandbox(a, sid)
}

// dispatch walks the channel ladder and writes the response itself.
// It returns (served, retryable): retryable=false means the ladder exhausted for
// this account.
func (e *engine) dispatch(a *account, w http.ResponseWriter, body []byte, model string, wantStream bool) (bool, bool) {
	// channel 1: proxy. Agent targets never go here.
	if (e.cfg.Mode == "auto" || e.cfg.Mode == "proxy") && !isAgentTarget(model) {
		m := model
		if m == "" {
			m = "zai_auto"
		}
		resp, err := e.up.chatProxy(a, body, m)
		if err != nil {
			log.Printf("autoglm: proxy chat failed (model=%s), falling back: %v", model, err)
			if e.cfg.Mode == "proxy" {
				e.handleErr(a, err)
				return false, false
			}
		} else {
			return true, e.streamOut(a, w, resp, wantStream, model)
		}
	}
	// channel 2: the sandbox's own OpenAI surface.
	if e.cfg.Mode != "relay" {
		resp, status, text := e.up.chatOpenAI(a, body)
		if resp == nil {
			if !(e.cfg.Mode == "auto" && notEnabled(status, text)) {
				e.handleErr(a, &UpstreamError{Kind: classify(status, 0, text), Status: status, Msg: truncStr(text, 200)})
				return false, false
			}
		} else {
			return true, e.streamOut(a, w, resp, wantStream, model)
		}
	}
	// channel 3: the relay event stream.
	runID, session, events, closeWS, err := e.up.relayStart(a, body)
	if err != nil {
		e.handleErr(a, err)
		return false, false
	}
	// The device socket stays open for the run: the relay routes agent events to
	// an online device, and closing it early is what made the old port fail.
	defer closeWS()
	if wantStream {
		writeSSEHeaders(w)
		convertRelayEvents(events, runID, session, body, newSSEWriter(w))
	} else {
		lw := &listWriter{}
		convertRelayEvents(events, runID, session, body, lw)
		writeJSON(w, http.StatusOK, aggregateSSE(lw.snapshot()))
	}
	e.st.noteSuccess(a)
	return true, true
}

// streamOut writes a proxy/sandbox response: passthrough SSE, aggregated SSE, or
// a JSON completion re-expressed as SSE.
func (e *engine) streamOut(a *account, w http.ResponseWriter, resp *http.Response, wantStream bool, model string) bool {
	e.st.noteSuccess(a)
	defer resp.Body.Close()
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/event-stream") {
		if wantStream {
			writeSSEHeaders(w)
			pumpSSE(resp, newSSEWriter(w))
			return true
		}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 256*1024), 8<<20)
		var lines []string
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		writeJSON(w, http.StatusOK, aggregateSSE(lines))
		return true
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil || !json.Valid(bytes.TrimSpace(raw)) {
		writeError(w, http.StatusBadGateway, "upstream_parse", "invalid JSON from upstream")
		return true
	}
	if !wantStream {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return true
	}
	var full map[string]any
	_ = json.Unmarshal(raw, &full)
	writeSSEHeaders(w)
	singleChunkSSE(full, newSSEWriter(w))
	return true
}
