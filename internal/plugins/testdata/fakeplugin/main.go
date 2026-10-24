// Command fakeplugin is a minimal plugin used only by the plugins package
// tests. It implements enough of contract v0.2 to exercise health checks,
// supervision and the typed client.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const version = "0.1.0"

func main() {
	dataDir := strings.TrimSpace(os.Getenv("AIWORKGATE_PLUGIN_DATA_DIR"))
	if dataDir != "" {
		if _, err := os.Stat(filepath.Join(dataDir, "crash")); err == nil {
			fmt.Fprintln(os.Stderr, "fake plugin: crash marker present")
			os.Exit(3)
		}
	}
	port := strings.TrimSpace(os.Getenv("AIWORKGATE_PLUGIN_PORT"))
	if port == "" {
		fmt.Fprintln(os.Stderr, "fake plugin: AIWORKGATE_PLUGIN_PORT is required")
		os.Exit(2)
	}
	token := os.Getenv("AIWORKGATE_PLUGIN_TOKEN")
	id := os.Getenv("AIWORKGATE_PLUGIN_ID")
	started := time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if dataDir != "" {
			if _, err := os.Stat(filepath.Join(dataDir, "healthfail")); err == nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"version":  version,
			"uptime_s": int64(time.Since(started).Seconds()),
		})
	})
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":            id,
			"version":       version,
			"api_level":     1,
			"capabilities":  []string{"upstream", "accounts", "checkin", "quota", "login_api_key", "refresh"},
			"account_count": 1,
			"uptime_s":      int64(time.Since(started).Seconds()),
			"started_at":    started.Unix(),
			"pid":           os.Getpid(),
		})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data": []map[string]any{{
				"id":             "fake-chat",
				"object":         "model",
				"name":           "Fake Chat",
				"context_length": 8192,
				"tools":          true,
			}},
		})
	})
	mux.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("fake plugin: reload requested")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		route(w, r, dataDir)
	})

	addr := "127.0.0.1:" + port
	fmt.Printf("fake plugin listening on %s (pid %d, data %s)\n", addr, os.Getpid(), dataDir)
	fmt.Printf("rules=%s egress=%s api_level=%s http_proxy=%s\n",
		os.Getenv("AIWORKGATE_RULES_FILE"), os.Getenv("AIWORKGATE_EGRESS_FILE"), os.Getenv("AIWORKGATE_API_LEVEL"),
		os.Getenv("http_proxy"))

	srv := &http.Server{
		Addr:              addr,
		Handler:           requireToken(token, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "fake plugin:", err)
		os.Exit(1)
	}
}

func route(w http.ResponseWriter, r *http.Request, dataDir string) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(segs) == 1 && segs[0] == "accounts" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"accounts": []map[string]any{{
				"id":       "acc_1",
				"label":    "fake-user",
				"enabled":  true,
				"status":   "ok",
				"added_at": time.Now().Add(-time.Hour).Unix(),
				"quota":    map[string]any{"remaining": 100, "total": 200},
			}},
		})
	case len(segs) == 2 && segs[0] == "accounts" && segs[1] == "login" && r.Method == http.MethodPost:
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id": "ls_1",
			"status":     "need_input",
			"method":     "api_key",
			"fields": []map[string]any{{
				"name": "api_key", "label": "API Key", "type": "password", "required": true,
			}},
		})
	case len(segs) == 3 && segs[0] == "accounts" && segs[1] == "login" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": "acc_1"})
	case len(segs) == 4 && segs[0] == "accounts" && segs[1] == "login" && segs[3] == "submit" && r.Method == http.MethodPost:
		var body struct {
			Values map[string]string `json:"values"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Values["api_key"] == "" {
			writeError(w, http.StatusBadRequest, "invalid", "api_key is required")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": "acc_1"})
	case len(segs) == 3 && segs[0] == "accounts" && segs[1] == "login" && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case len(segs) == 3 && segs[0] == "accounts" && segs[2] == "checkin" && r.Method == http.MethodPost:
		writeJSON(w, http.StatusOK, map[string]any{
			"result": "ok", "credits_before": 100, "credits_after": 110, "message": "+10", "at": time.Now().Unix(),
		})
	case len(segs) == 3 && segs[0] == "accounts" && segs[2] == "refresh" && r.Method == http.MethodPost:
		writeJSON(w, http.StatusOK, map[string]any{"result": "ok", "expires_at": time.Now().Add(24 * time.Hour).Unix()})
	case len(segs) == 3 && segs[0] == "accounts" && segs[2] == "history" && r.Method == http.MethodGet:
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		writeJSON(w, http.StatusOK, map[string]any{"days": days, "records": []any{}})
	case len(segs) == 2 && segs[0] == "accounts" && r.Method == http.MethodPatch:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case len(segs) == 2 && segs[0] == "accounts" && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": "no such endpoint: " + r.URL.Path, "type": "not_found"},
		})
	}
}

func requireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "plugin token is not configured")
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			writeError(w, http.StatusUnauthorized, "unauthorized", "bad bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": typ}})
}
