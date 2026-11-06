package main

// Process entry point: read the host-injected environment, open the sealed
// credential store, start the maintenance loop, and serve loopback only.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// pluginConfig is the slice of the host environment this plugin needs.
type pluginConfig struct {
	ID         string
	APILevel   int
	Port       string
	Token      string
	DataDir    string
	EgressFile string
}

func loadConfig() pluginConfig {
	return pluginConfig{
		ID:         env("AIWORKGATE_PLUGIN_ID", "autoglm"),
		APILevel:   2,
		Port:       env("AIWORKGATE_PLUGIN_PORT", "0"),
		Token:      env("AIWORKGATE_PLUGIN_TOKEN", ""),
		DataDir:    env("AIWORKGATE_PLUGIN_DATA_DIR", defaultDataDir()),
		EgressFile: env("AIWORKGATE_EGRESS_FILE", ""),
	}
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// defaultDataDir is only used for a manual run, so the plugin still has a place
// to keep its sealed credentials.
func defaultDataDir() string {
	if base := os.Getenv("APPDATA"); base != "" {
		return base + string(os.PathSeparator) + "AIWorkGate" +
			string(os.PathSeparator) + "plugin-data" + string(os.PathSeparator) + "autoglm"
	}
	return "autoglm-data"
}

func main() {
	cfg := loadConfig()
	st, err := newStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "autoglm:", err)
		os.Exit(1)
	}
	up := newUpstream("")
	// The account egress bindings live in the host-written file named by
	// AIWORKGATE_EGRESS_FILE. Absent or broken, the plugin keeps routing direct
	// and only logs: startup must not depend on the file.
	if _, err := up.applyEgress(cfg.EgressFile); err != nil {
		fmt.Fprintln(os.Stderr, "autoglm: egress:", err)
	}
	eng := &engine{cfg: defaultEngineConfig(), st: st, up: up}
	srv := &server{cfg: cfg, st: st, up: up, engine: eng, models: &modelCache{}}

	addr := ":" + cfg.Port
	if cfg.Port == "" || cfg.Port == "0" {
		addr = "127.0.0.1:0"
	} else {
		addr = "127.0.0.1:" + cfg.Port
	}
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go maintainLoop(ctx, srv)
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "autoglm: listen:", err)
		os.Exit(1)
	}
}

// maintainLoop keeps balances fresh and retries dead sessions on a timer, the
// same job the reference's maintainLoop does (default 30 minutes).
func maintainLoop(ctx context.Context, srv *server) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("autoglm: maintain panic: %v", r)
					}
				}()
				srv.maintainNow()
			}()
		}
	}
}

// maintainNow refreshes tokens, retries session-dead accounts, and re-reads
// balances. A failure on one account never stops the loop.
func (s *server) maintainNow() {
	for _, row := range s.st.statusList() {
		a := s.st.byID(row.UID)
		if a == nil {
			continue
		}
		snap := a.snapshot()
		if row.Disabled {
			// Only self-heal a session-dead account; a manual disable is left alone.
			if strings.Contains(row.Reason, "session dead") && strings.TrimSpace(snap.RefreshToken) != "" {
				if err := s.up.refresh(a); err == nil {
					_ = s.st.syncFlush()
					if pts, perr := s.up.checkPoints(a); perr == nil {
						s.st.setPoints(a, pts)
					}
					s.st.reenable(a)
				}
			}
			continue
		}
		if strings.TrimSpace(snap.RefreshToken) != "" && a.needsRefresh(600) {
			if err := s.up.refresh(a); err != nil {
				var ue *UpstreamError
				if errors.As(err, &ue) && ue.Kind == "session_dead" {
					s.st.disable(a, "refresh session dead")
					continue
				}
			} else {
				_ = s.st.syncFlush()
			}
		}
		if pts, err := s.up.checkPoints(a); err == nil {
			s.st.reenableIfPoints(a, pts)
		}
	}
}
