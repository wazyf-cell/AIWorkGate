package webui

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

func TestUsageEndpointsAndExport(t *testing.T) {
	var lastQuery map[string][]string
	handler := New(Options{
		UsageSummary: func(query map[string][]string) (any, error) {
			lastQuery = query
			return map[string]any{"kpis": map[string]any{"requests": 3}, "series": []any{}}, nil
		},
		UsageExpiry: func(map[string][]string) (any, error) {
			return map[string]any{"accounts": []any{}}, nil
		},
		UsageExport: func(w io.Writer, query map[string][]string) (string, error) {
			if query["plugin"][0] != "mock" {
				return "", fmt.Errorf("%w: plugin filter lost", ErrInvalidRequest)
			}
			_, _ = io.WriteString(w, "section,day\nseries,2026-10-20\n")
			return "usage-2026-10-19-2026-10-20.csv", nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/usage/summary?plugin=mock&plugin=other&from=2026-10-19", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("summary status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if len(lastQuery["plugin"]) != 2 || lastQuery["from"][0] != "2026-10-19" {
		t.Fatalf("query forwarded as %+v", lastQuery)
	}
	var summary struct {
		KPIs struct {
			Requests int `json:"requests"`
		} `json:"kpis"`
	}
	decodeJSONBody(t, recorder, &summary)
	if summary.KPIs.Requests != 3 {
		t.Fatalf("summary = %+v", summary)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/usage/export?plugin=mock", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("export status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if ct := recorder.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("export content-type = %q", ct)
	}
	disposition := recorder.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, `attachment; filename="usage-2026-10-19-2026-10-20.csv"`) {
		t.Fatalf("disposition = %q", disposition)
	}
	if !strings.Contains(recorder.Body.String(), "series,2026-10-20") {
		t.Fatalf("export body = %q", recorder.Body.String())
	}

	// Bad queries map to 400.
	bad := New(Options{UsageSummary: func(map[string][]string) (any, error) {
		return nil, fmt.Errorf("%w: from must be YYYY-MM-DD", ErrInvalidRequest)
	}})
	recorder = doRequest(t, bad, http.MethodGet, "/api/usage/summary?from=bad", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad query status = %d, want 400", recorder.Code)
	}
	var payload struct {
		Error struct{ Type string } `json:"error"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.Error.Type != "invalid_query" {
		t.Fatalf("error type = %q", payload.Error.Type)
	}
}

func TestRequestLogsExtendedFilters(t *testing.T) {
	var captured map[string]string
	handler := New(Options{
		RequestLogs: func(query map[string]string) any {
			captured = query
			return RequestLogsResult{Logs: []model.APIRequestLog{{ReqID: "req-1", Status: 401}}, Total: 1}
		},
	})

	recorder := doRequest(t, handler, http.MethodGet,
		"/api/logs/requests?model=Claude&source=plugin%3Amock&account_id=acc_1&key_id=key-1&status_class=4XX&stream=true&limit=20", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	want := map[string]string{
		"model": "Claude", "source": "plugin:mock", "account_id": "acc_1",
		"key_id": "key-1", "status_class": "4xx", "stream": "true", "limit": "20",
	}
	for key, value := range want {
		if captured[key] != value {
			t.Fatalf("forwarded %s = %q, want %q (query %+v)", key, captured[key], value, captured)
		}
	}
	if _, ok := captured[RequestLogExportParam]; ok {
		t.Fatalf("client export marker was forwarded: %+v", captured)
	}

	// The marker is stripped even when a client sends it.
	doRequest(t, handler, http.MethodGet, "/api/logs/requests?export=1", "")
	if _, ok := captured[RequestLogExportParam]; ok {
		t.Fatalf("client export marker was forwarded: %+v", captured)
	}

	for _, target := range []string{
		"/api/logs/requests?status_class=3xx",
		"/api/logs/requests?stream=yes",
	} {
		recorder = doRequest(t, handler, http.MethodGet, target, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", target, recorder.Code)
		}
		var payload struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		decodeJSONBody(t, recorder, &payload)
		if payload.Error.Type != "invalid_query" || payload.Error.Message == "" {
			t.Fatalf("%s error = %+v", target, payload.Error)
		}
		if !strings.ContainsAny(payload.Error.Message, "只能") {
			t.Fatalf("%s message is not Chinese: %q", target, payload.Error.Message)
		}
	}
	recorder = doRequest(t, handler, http.MethodGet, "/api/logs/requests?status_class=3xx", "")
	if !strings.Contains(recorder.Body.String(), "status_class") {
		t.Fatalf("status_class message = %s", recorder.Body.String())
	}
}

func TestRequestLogsExportCSV(t *testing.T) {
	var captured map[string]string
	handler := New(Options{
		RequestLogs: func(query map[string]string) any {
			captured = query
			return RequestLogsResult{
				Total:     2,
				Truncated: true,
				Logs: []model.APIRequestLog{
					{
						TS: "2026-10-21T10:20:30+08:00", ReqID: "req-1", Source: "custom:cm-1",
						Model: `gpt,"quoted"` + "\nline", Account: "acc_1", KeyID: "key-id",
						Stream: true, Status: 200, LatencyMS: 120, TTFBMS: 40,
						PromptTokens: 10, CompletionTokens: 5, CachedTokens: 2,
						BodyReq: "SECRET-BODY", BodyResp: "SECRET-RESPONSE",
					},
					{
						TS: "not-a-time", ReqID: "req-2", Source: "plugin:mock", Model: "m2",
						KeyName: "laptop", Stream: false, Status: 0,
					},
				},
			}
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/logs/requests/export?status_class=error&model=gpt", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("export status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if captured[RequestLogExportParam] != "1" {
		t.Fatalf("export marker = %q, query %+v", captured[RequestLogExportParam], captured)
	}
	if captured["status_class"] != "error" || captured["model"] != "gpt" {
		t.Fatalf("filters lost: %+v", captured)
	}
	if ct := recorder.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type = %q", ct)
	}
	if got := recorder.Header().Get("X-Export-Truncated"); got != "true" {
		t.Fatalf("X-Export-Truncated = %q", got)
	}
	filename := "requests-" + time.Now().Format("20060102") + ".csv"
	if disposition := recorder.Header().Get("Content-Disposition"); !strings.Contains(disposition,
		`attachment; filename="`+filename+`"`) {
		t.Fatalf("disposition = %q, want %s", disposition, filename)
	}

	body := recorder.Body.Bytes()
	if !strings.HasPrefix(string(body), "\ufeff") {
		t.Fatalf("export is missing the UTF-8 BOM: %q", body[:min(8, len(body))])
	}
	records, err := csv.NewReader(strings.NewReader(string(body[3:]))).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("csv rows = %d, want 3 (header + 2)", len(records))
	}
	if strings.Join(records[0], ",") != strings.Join(RequestLogCSVHeader, ",") {
		t.Fatalf("header = %v, want %v", records[0], RequestLogCSVHeader)
	}
	first := records[1]
	if len(first) != len(RequestLogCSVHeader) {
		t.Fatalf("row has %d columns, want %d", len(first), len(RequestLogCSVHeader))
	}
	if first[3] != `gpt,"quoted"`+"\nline" {
		t.Fatalf("quoted model round-trip = %q", first[3])
	}
	if first[5] != "key-id" {
		t.Fatalf("key fallback = %q, want key-id", first[5])
	}
	if first[6] != "是" || records[2][6] != "否" {
		t.Fatalf("stream labels = %q/%q", first[6], records[2][6])
	}
	if first[0] != "2026-10-21 10:20:30" {
		t.Fatalf("timestamp = %q", first[0])
	}
	if records[2][0] != "not-a-time" {
		t.Fatalf("raw timestamp fallback = %q", records[2][0])
	}
	if strings.Contains(recorder.Body.String(), "SECRET") {
		t.Fatal("export leaked a logged body")
	}

	// Invalid enum values answer 400 before the host callback runs.
	bad := New(Options{RequestLogs: func(query map[string]string) any {
		t.Fatal("RequestLogs must not be called for an invalid query")
		return nil
	}})
	recorder = doRequest(t, bad, http.MethodGet, "/api/logs/requests/export?stream=maybe", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad export status = %d, want 400", recorder.Code)
	}
	var payload struct {
		Error struct{ Type string } `json:"error"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.Error.Type != "invalid_query" {
		t.Fatalf("bad export error type = %q", payload.Error.Type)
	}
}

func TestLogBodyHandler(t *testing.T) {
	handler := New(Options{
		LogBody: func(reqID, which string) (LogBody, error) {
			switch reqID {
			case "req-403":
				return LogBody{}, ErrDebugDisabled
			case "req-404":
				return LogBody{}, ErrLogNotFound
			}
			return LogBody{Body: which + "-body", Truncated: true}, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/logs/body?req_id=req-1&which=resp", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("body status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	var body LogBody
	decodeJSONBody(t, recorder, &body)
	if body.Body != "resp-body" || !body.Truncated || !body.Debug {
		t.Fatalf("body payload = %+v", body)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/logs/body?req_id=req-403", "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("debug off status = %d, want 403", recorder.Code)
	}
	var errPayload struct {
		Error struct{ Type string } `json:"error"`
	}
	decodeJSONBody(t, recorder, &errPayload)
	if errPayload.Error.Type != "debug_disabled" {
		t.Fatalf("403 type = %q", errPayload.Error.Type)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/logs/body?req_id=req-404", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing row status = %d, want 404", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodGet, "/api/logs/body", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing req_id status = %d, want 400", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodGet, "/api/logs/body?req_id=x&which=other", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad which status = %d, want 400", recorder.Code)
	}
}

func TestLogTimelineHandlerPassesFilters(t *testing.T) {
	var captured map[string][]string
	handler := New(Options{
		LogTimeline: func(query map[string][]string) (any, error) {
			captured = query
			if query["q"][0] == "bad" {
				return nil, fmt.Errorf("%w: bad range", ErrInvalidRequest)
			}
			return map[string]any{"items": []any{}, "total": 0, "page": 1, "page_size": 50}, nil
		},
	})
	recorder := doRequest(t, handler, http.MethodGet, "/api/logs/timeline?type=api&category=plugin&q=boom&page=2", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("timeline status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if captured["type"][0] != "api" || captured["q"][0] != "boom" {
		t.Fatalf("filters = %+v", captured)
	}
	recorder = doRequest(t, handler, http.MethodGet, "/api/logs/timeline?q=bad", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad timeline status = %d", recorder.Code)
	}
}

func TestLogStreamSSE(t *testing.T) {
	pollCalls := 0
	opts := Options{
		LogSeed: func(query map[string][]string) (string, error) {
			if query["level"][0] != "error" {
				return "", errors.New("level filter not forwarded")
			}
			return `{"api":7}`, nil
		},
		LogPoll: func(query map[string][]string, cursor string, limit int) (LogPollResult, error) {
			pollCalls++
			if cursor != `{"api":7}` {
				return LogPollResult{}, fmt.Errorf("seed cursor lost: %q", cursor)
			}
			if pollCalls == 1 {
				return LogPollResult{
					Events: []LogEvent{{
						ID:   `{"api":8}`,
						Data: json.RawMessage(`{"ts":"2026-10-21T10:00:00+08:00","type":"api","summary":"m → 500"}`),
					}},
					Cursor: `{"api":8}`,
				}, nil
			}
			return LogPollResult{Cursor: `{"api":8}`}, nil
		},
	}
	s := &server{opts: opts, streamPoll: 20 * time.Millisecond, streamHeartbeat: 60 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(s.handleLogStream))
	defer srv.Close()

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(srv.URL + "?level=error")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	var (
		gotEvent     bool
		gotID        bool
		gotData      bool
		gotHeartbeat bool
	)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "event: log":
			gotEvent = true
		case line == "id: "+`{"api":8}`:
			gotID = true
		case strings.HasPrefix(line, "data: ") && strings.Contains(line, `"summary":"m → 500"`):
			gotData = true
		case line == ": heartbeat":
			gotHeartbeat = true
		}
		if gotEvent && gotID && gotData && gotHeartbeat {
			break
		}
	}
	if !gotEvent {
		t.Fatalf("no log event received (hearbeat=%v)", gotHeartbeat)
	}
	if !gotID || !gotData {
		t.Fatal("no event id/data received")
	}
	if !gotHeartbeat {
		t.Fatal("no heartbeat received")
	}
	if pollCalls == 0 {
		t.Fatal("poll was never called")
	}
}

// TestRequestLogCSVRowFormulaInjection pins the CSV-injection mitigation:
// client-controlled cells whose first byte is a formula prefix get a leading
// single quote, normal values stay untouched.
func TestRequestLogCSVRowFormulaInjection(t *testing.T) {
	cases := []struct {
		name     string
		rec      model.APIRequestLog
		wantCols map[int]string
	}{
		{
			name: "formula prefixes are quoted",
			rec: model.APIRequestLog{
				ReqID:  "=cmd|' /C calc'!A0",
				Source: "\tsource", Model: "=HYPERLINK(\"http://evil\")",
				Account: "@SUM(1+1)", KeyName: "+key", Status: 200,
			},
			wantCols: map[int]string{
				1: "'=cmd|' /C calc'!A0",
				2: "'\tsource",
				3: "'=HYPERLINK(\"http://evil\")",
				4: "'@SUM(1+1)",
				5: "'+key",
			},
		},
		{
			name: "normal values are preserved",
			rec: model.APIRequestLog{
				ReqID: "req-1", Source: "plugin:mock",
				Model: "gpt-4o", Account: "acc_1", KeyName: "laptop", Status: 200,
			},
			wantCols: map[int]string{
				1: "req-1",
				2: "plugin:mock",
				3: "gpt-4o",
				4: "acc_1",
				5: "laptop",
			},
		},
		{
			name: "minus and carriage return are quoted",
			rec: model.APIRequestLog{
				Model: "-1+1", Account: "\rcmd", KeyName: "", KeyID: "-key-id",
			},
			wantCols: map[int]string{
				3: "'-1+1",
				4: "'\rcmd",
				5: "'-key-id",
			},
		},
		{
			name: "leading space is not a formula prefix",
			rec:  model.APIRequestLog{Model: " =1+1", Account: "a=b"},
			wantCols: map[int]string{
				3: " =1+1",
				4: "a=b",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := requestLogCSVRow(tc.rec)
			if len(row) != len(RequestLogCSVHeader) {
				t.Fatalf("row has %d columns, want %d", len(row), len(RequestLogCSVHeader))
			}
			for col, want := range tc.wantCols {
				if row[col] != want {
					t.Fatalf("column %d (%s) = %q, want %q",
						col, RequestLogCSVHeader[col], row[col], want)
				}
			}
		})
	}
}
