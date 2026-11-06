package main

// Daily check-in and the task list (ported from checkin.go), plus the credit
// ledger and wallet reads.
//
// The task endpoints sign with the *desktop* header set (checkinHeaders), not
// the web set the rest of userapi uses — that difference is in the reference and
// is preserved here, because the task endpoint rejects the web fingerprint.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const checkinTimeout = 20 * time.Second

// httpPlain issues a request with the check-in header set and returns the parsed
// body, status and a raw error text. It does not interpret the envelope.
func httpPlain(client *http.Client, method, fullURL, token string, body any) (map[string]any, int, string) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err.Error()
		}
		rd = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, fullURL, rd)
	if err != nil {
		return nil, 0, err.Error()
	}
	for k, v := range checkinHeaders(token) {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err.Error()
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, resp.StatusCode, err.Error()
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return nil, resp.StatusCode, string(raw)
	}
	return doc, resp.StatusCode, ""
}

// checkinResult is one account's check-in outcome, shaped for the host contract
// (result: ok / already / fail).
type checkinResult struct {
	UID          string `json:"uid"`
	Region       string `json:"region"`
	URL          string `json:"url,omitempty"`
	TaskID       string `json:"task_id,omitempty"`
	OK           bool   `json:"ok"`
	AlreadyDone  bool   `json:"already_completed"`
	RewardPoints int    `json:"reward_points"`
	Code         any    `json:"code"`
	Msg          string `json:"msg"`
	Error        string `json:"error"`
	Tasks        []any  `json:"tasks,omitempty"`
}

// checkinOne runs the daily_signin task. dryRun only echoes the plan.
func (e *engine) checkinOne(a *account, dryRun bool) checkinResult {
	snap := a.snapshot()
	base := e.up.base(snap.Region)
	res := checkinResult{UID: snap.UserID, Region: snap.Region, URL: base + checkinTaskPath, TaskID: "daily_signin"}
	if dryRun {
		res.OK = true
		res.Msg = "dry-run"
		return res
	}
	doc, status, errText := httpPlain(e.up.http, http.MethodPost, base+checkinTaskPath, snap.AccessToken,
		map[string]any{"task_id": "daily_signin"})
	if doc == nil {
		res.Error = fmt.Sprintf("http %d: %s", status, errText)
		return res
	}
	code := doc["code"]
	res.Code = code
	if m, ok := doc["msg"].(string); ok {
		res.Msg = m
	}
	data, _ := doc["data"].(map[string]any)
	if status >= 400 && code == nil {
		res.Error = fmt.Sprintf("http %d: %s", status, res.Msg)
		return res
	}
	if !(toInt64(code) == 0 || (code == nil && len(data) > 0)) {
		if res.Msg != "" {
			res.Error = res.Msg
		} else {
			res.Error = fmt.Sprintf("code=%v", code)
		}
		return res
	}
	res.OK = true
	if data != nil {
		if v, ok := data["already_completed"].(bool); ok {
			res.AlreadyDone = v
		}
		res.RewardPoints = int(toInt64(data["reward_points"]))
		if v := toString(data["task_id"]); v != "" {
			res.TaskID = v
		}
	}
	return res
}

// taskListOne reads one account's task list.
func (e *engine) taskListOne(a *account, dryRun bool) checkinResult {
	snap := a.snapshot()
	base := e.up.base(snap.Region)
	res := checkinResult{UID: snap.UserID, Region: snap.Region, URL: base + taskListPath + "?lang=en"}
	if dryRun {
		res.Error = "dry-run"
		return res
	}
	doc, status, errText := httpPlain(e.up.http, http.MethodGet, base+taskListPath+"?lang=en", snap.AccessToken, nil)
	if doc == nil {
		res.Error = fmt.Sprintf("http %d: %s", status, errText)
		return res
	}
	if code := doc["code"]; code != nil && toInt64(code) != 0 {
		if m, ok := doc["msg"].(string); ok && m != "" {
			res.Error = m
		} else {
			res.Error = fmt.Sprintf("code=%v", code)
		}
		return res
	}
	res.OK = true
	switch data := doc["data"].(type) {
	case []any:
		res.Tasks = data
	case map[string]any:
		if t, ok := data["tasks"].([]any); ok {
			res.Tasks = t
		} else if l, ok := data["list"].([]any); ok {
			res.Tasks = l
		} else {
			res.Tasks = []any{data}
		}
	}
	return res
}

// recoverToken refreshes a token after a 401 so check-in can retry once.
func (e *engine) recoverToken(a *account) bool {
	if strings.TrimSpace(a.snapshot().RefreshToken) == "" {
		return false
	}
	if err := e.up.refresh(a); err != nil {
		logf("%s checkin token refresh failed: %v", shortUID(a), err)
		return false
	}
	if err := e.st.syncFlush(); err != nil {
		logf("%s checkin token save failed: %v", shortUID(a), err)
		return false
	}
	logf("%s checkin 401 -> token refreshed", shortUID(a))
	return true
}

// checkinRun walks the accounts selected by region/uid, retrying once on 401.
func (e *engine) checkinRun(region string, dryRun bool, mode, uid string) map[string]any {
	results := make([]checkinResult, 0, 4)
	for _, row := range e.st.statusList() {
		if region != "" && region != "all" && row.Region != region {
			continue
		}
		if uid != "" && row.UID != uid {
			continue
		}
		a := e.st.byID(row.UID)
		if a == nil || strings.TrimSpace(a.snapshot().AccessToken) == "" {
			continue
		}
		var res checkinResult
		if mode == "tasks" {
			res = e.taskListOne(a, dryRun)
		} else {
			res = e.checkinOne(a, dryRun)
		}
		if !res.OK && strings.Contains(res.Error, "http 401") && e.recoverToken(a) {
			if mode == "tasks" {
				res = e.taskListOne(a, dryRun)
			} else {
				res = e.checkinOne(a, dryRun)
			}
		}
		results = append(results, res)
	}
	return map[string]any{"results": results}
}

// creditDetail reads the points ledger and the expiring balance for one account.
func (e *engine) creditDetail(region string) map[string]any {
	if region != regionCN && region != regionGlobal {
		region = regionCN
	}
	a := e.pickFor(region, nil)
	if a == nil {
		return map[string]any{"error": "no " + region + " account"}
	}
	snap := a.snapshot()
	out := map[string]any{"region": region, "uid": snap.UserID, "entries": []any{}, "expiring": nil}
	full := fmt.Sprintf("%s/agent-assetmgr/api/v1/ledgers_std?page=1&page_size=20", e.up.base(region))
	if raw, err := e.up.getJSON(full, stateKey(snap.Region, snap.UserID), snap.AccessToken); err == nil {
		var env struct {
			Code any `json:"code"`
			Data struct {
				Entries []map[string]any `json:"entries"`
				Total   any              `json:"total"`
			} `json:"data"`
			Msg any `json:"msg"`
		}
		if json.Unmarshal(raw, &env) == nil && toInt64(env.Code) == 0 {
			entries := make([]map[string]any, 0, len(env.Data.Entries))
			for _, en := range env.Data.Entries {
				meta, _ := en["metadata"].(map[string]any)
				row := map[string]any{
					"time": en["occurred_at"], "amount": en["amount"],
					"model": "", "actual_model": "", "reason": "", "charged": "", "cap": "", "token_usage": "",
				}
				if meta != nil {
					row["model"] = orStr(meta["model"], meta["display_model"])
					row["actual_model"] = toString(meta["actual_model"])
					row["reason"] = orStr(meta["reason"], en["mutation_type"])
					row["charged"] = toString(meta["charged_amount"])
					row["cap"] = toString(meta["request_point_cap"])
					row["token_usage"] = toString(meta["token_usage"])
				} else {
					row["reason"] = toString(en["mutation_type"])
				}
				row["description"] = toString(en["description"])
				entries = append(entries, row)
			}
			out["entries"] = entries
			if env.Data.Total != nil {
				out["total"] = env.Data.Total
			} else {
				out["total"] = len(entries)
			}
		} else {
			out["error"] = fmt.Sprintf("ledgers code=%v %.100s", env.Code, toString(env.Msg))
		}
	} else {
		out["error"] = truncStr(err.Error(), 150)
	}
	if raw, err := e.up.getJSON(e.up.base(region)+"/agent-assetmgr/api/v1/points/expiring", stateKey(snap.Region, snap.UserID), snap.AccessToken); err == nil {
		var env struct {
			Code any            `json:"code"`
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal(raw, &env) == nil && toInt64(env.Code) == 0 && env.Data != nil {
			out["expiring"] = map[string]any{
				"points": env.Data["expiring_points"],
				"total":  env.Data["total_points"],
				"days":   env.Data["expire_time_value"],
				"text":   env.Data["expiring_points_text"],
			}
		}
	}
	return out
}

func logf(format string, args ...any) {
	stdlogger.Printf(format, args...)
}
