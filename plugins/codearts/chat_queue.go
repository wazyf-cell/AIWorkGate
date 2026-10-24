package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type queueStatus struct {
	Status   string `json:"status"`
	Position int    `json:"queue_position"`
	Message  string `json:"message"`
}

func (p *Plugin) queryQueueStatus(ctx context.Context, acct *Account, proxyURL, model string) (queueStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out queueStatus
	rawURL := p.endpoints.Queue + "?model=" + url.QueryEscape(model) + "&task_id=" + url.QueryEscape(acct.Identity.SessionID)
	extra := map[string]string{
		"x-snap-traceid": randomHex(16),
		"Agent-Type":     "INFERHUB_AGENT",
		"X-Language":     "en",
	}
	resp, err := p.signedRequest(ctx, http.MethodGet, rawURL, nil, acct, proxyURL, acct.ID, nil, extra)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("queue status HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &out); err != nil {
		return out, err
	}
	switch out.Status {
	case "waiting", "working", "error", "queue_full":
		return out, nil
	}
	return out, errors.New("unrecognized queue status")
}

// waitForQueue performs one queue-status probe per failed chat attempt. When
// the queue reports a terminal error it fails fast; otherwise it sleeps for the
// 10s retry interval and lets the caller resend the chat request. The overall
// deadline enforces the 30-minute cap.
func (p *Plugin) waitForQueue(ctx context.Context, acct *Account, proxyURL, model string, deadline time.Time) error {
	st, err := p.queryQueueStatus(ctx, acct, proxyURL, model)
	if err == nil {
		if st.Status == "error" || st.Status == "queue_full" {
			msg := st.Message
			if msg == "" {
				msg = "queue status: " + st.Status
			}
			return errors.New("codearts: " + msg)
		}
	}
	select {
	case <-ctx.Done():
		return errors.New("codearts: request aborted while waiting in queue")
	case <-time.After(p.opts.QueueRetryDelay):
	}
	if !deadline.IsZero() && time.Now().After(deadline) {
		return errors.New("codearts: queue wait timed out after 30 minutes")
	}
	return nil
}
