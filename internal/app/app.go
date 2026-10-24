// Package app assembles the AIWorkGate host: configuration, storage, secrets,
// logging, plugins, egress, rules, the gateway, the admin web UI and the tray,
// and owns the process lifecycle (startup order and idempotent shutdown).
//
// New is deliberately side-effect free with respect to the GUI: it binds the
// listening socket and starts plugins but leaves the tray message loop to Run,
// which must be called from the process main goroutine.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/egress"
	"aiworkgate/internal/gateway"
	"aiworkgate/internal/hosts"
	"aiworkgate/internal/logging"
	"aiworkgate/internal/model"
	"aiworkgate/internal/notify"
	"aiworkgate/internal/plugins"
	"aiworkgate/internal/rules"
	"aiworkgate/internal/safego"
	"aiworkgate/internal/schedule"
	"aiworkgate/internal/scheduler"
	"aiworkgate/internal/secrets"
	"aiworkgate/internal/store"
	"aiworkgate/internal/tray"
	"aiworkgate/internal/usage"
	"aiworkgate/internal/webui"
	"aiworkgate/internal/winutil"
)

const (
	// AppName is both the single-instance mutex name, the HKCU Run value name
	// and the window title.
	AppName = "AIWorkGate"

	trayTooltip = "AIWorkGate 本地网关"

	portDirName  = "run"
	portFileName = "gateway.port"

	shutdownTimeout = 5 * time.Second

	// aProbeTTLDefault is how long a health result is reused before BaseURL
	// re-probes (and possibly re-discovers) A's loopback port.
	aProbeTTLDefault = 30 * time.Second
	// aHealthTimeout bounds one GET /health probe against A.
	aHealthTimeout = 2 * time.Second
)

// AdminLaunchTokenSecretKey is the secrets.dat key holding the admin launch
// token. It is DPAPI user-scoped, so only this Windows user can read it; it
// exists so a second launch of the executable can reopen the admin page of the
// process that owns the instance, which is the only holder of the live token.
const AdminLaunchTokenSecretKey = "admin_launch_token"

// ErrAlreadyRunning is returned by New when another AIWorkGate instance owns
// the single-instance mutex. The existing UI has already been activated; main
// must treat this as a successful exit.
var ErrAlreadyRunning = errors.New("aiworkgate: another instance is already running")

// Version is the host version shown in the admin UI. It is a var so the build
// script can stamp it with -X; a const would make that flag silently no-op.
var Version = "0.1.0-dev"

// App is the assembled host. Create it with New and run it with Run; Shutdown
// is idempotent and may also be called by main after Run returns.
type App struct {
	dataDir string
	// firstRun is true when the database did not exist at startup, so Run can
	// open the admin page once for a fresh install (docs 2.1 promises this).
	firstRun bool

	cfgMu sync.RWMutex
	cfg   config.Config

	store     *store.Store
	secrets   *secrets.Store
	logger    *logging.Logger
	apiLog    *logging.APILogger
	pluginLog *logging.PluginLogWriter

	pluginMgr *plugins.Manager
	egressMgr *egress.Manager
	rulesMgr  *rules.Manager
	sched     *schedule.Manager

	// notify delivers operational events to the user's phone/chat (F-19).
	// checkinNotify aggregates one check-in run into a single message so a
	// failing plugin cannot push once per account.
	notify        *notify.Dispatcher
	checkinNotify *checkinNotifier

	usageSvc *usage.Service
	roller   *usage.Roller
	timeline *logging.Timeline

	// dispatch is the S3 scheduling core. It owns in-flight counters, EWMA
	// latency, cooldowns, pool health and the sticky cache; the gateway reads
	// snapshots through schedulerSnapshot.
	dispatch *scheduler.Scheduler

	// egressHealth caches the recent-evidence verdict for account proxies. It
	// feeds the scheduler's soft filter, /healthz and the account list.
	egressHealth *egress.Health

	// systemProxy carries the reason the reserved "system" proxy row is off. The
	// row itself lives in the proxy table and follows Windows Internet Settings.
	systemProxy systemProxyState
	// systemProxyRead is a test seam; nil reads the real Windows settings.
	systemProxyRead func() (hosts.SystemProxy, error)

	// healthSnapshot is a test seam for GET /healthz; nil reads the live
	// scheduling view through schedulerSnapshot.
	healthSnapshot func(context.Context) (*scheduler.Snapshot, error)

	// Snapshot caches: account lists (10s) and plugin model lists (30s).
	snapMu       sync.Mutex
	snapAccounts map[string]cachedAccountSet
	snapModels   map[string]cachedModelSet

	// snapReads holds the plugin reads currently in flight, keyed by
	// "accounts:<id>" / "models:<id>". It is guarded by snapMu and gives a
	// snapshot round two things: concurrent fan-out across plugins, and
	// deduplication so a burst of rounds cannot stampede one plugin with a call
	// each.
	snapReads map[string]*pluginRead

	// identityMu serializes OS-level identity resets host-wide (M8): two
	// plugins must never rewrite machine identifiers at the same time.
	identityMu sync.Mutex

	gatewayHandler http.Handler
	adminHandler   http.Handler
	keyVerifier    *keyAdapter

	// adminToken is the one-shot launch token minted in New once this process
	// owns the single-instance lock. It is what the admin session cookie is made
	// of, so it lives only in memory and only in the URL the tray opens; a
	// second process cannot know it (see openExistingUI).
	adminToken string

	listener net.Listener
	server   *http.Server
	port     int

	aBaseURL string
	aPort    int

	// Live A upstream state, guarded by aMu. aBaseURL/aPort stay the startup
	// snapshot so unlocked readers in other host files keep racing nothing.
	aMu       sync.Mutex
	aURL      string
	aPortLive int
	aProbeAt  time.Time
	aState    hosts.AState
	aDetail   string

	// Test seams for the lazy resolver; nil selects the real implementations.
	aDiscoverPort func() (int, bool)
	aProbeFn      func(ctx context.Context, baseURL string) (hosts.AState, string)
	aProbeTTL     time.Duration

	releaseInstance func()

	shutdownCh   chan struct{}
	quitOnce     sync.Once
	shutdownOnce sync.Once
	shutdownErr  error
}

// New builds the host in the contract's startup order. It returns
// ErrAlreadyRunning (after activating the existing instance's web UI) when a
// previous process is alive.
// NewOptions carries test-only overrides. The zero value reproduces the
// production behaviour exactly, so callers outside tests keep using New().
type NewOptions struct {
	// DataDir overrides config.DataDir(). Tests need it because a construction
	// failure path (another instance running, a locked database) cannot be
	// provoked without pointing the host at a throwaway directory.
	DataDir string
}

// resolveDataDir returns the override or the per-user default.
func (o NewOptions) resolveDataDir() (string, error) {
	if dir := strings.TrimSpace(o.DataDir); dir != "" {
		return dir, nil
	}
	return config.DataDir()
}

func New() (a *App, err error) {
	return NewWithOptions(NewOptions{})
}

// NewWithOptions builds the host. It exists so the failure paths below are
// reachable from a test: every one of them returns before the process is
// serving, and before this separation none of them had any coverage.
func NewWithOptions(opts NewOptions) (a *App, err error) {
	dataDir, err := opts.resolveDataDir()
	if err != nil {
		return nil, err
	}
	if err := config.EnsureDir(dataDir); err != nil {
		return nil, err
	}
	cfg, err := config.Load(dataDir)
	if err != nil {
		return nil, err
	}

	a = &App{
		dataDir:    dataDir,
		cfg:        cfg,
		shutdownCh: make(chan struct{}),
	}
	if _, statErr := os.Stat(store.DBPath(dataDir)); errors.Is(statErr, os.ErrNotExist) {
		a.firstRun = true
	}

	// Close everything opened so far if construction fails halfway through.
	//
	// Each cleanup runs inside its own recover boundary. A cleanup closure that
	// touches a field which was never assigned (or a receiver that `return nil`
	// already cleared on the named return value) would otherwise panic here and
	// replace the real construction error with a nil-pointer crash. The caller
	// then sees a stack trace instead of "another instance is running" or
	// "port already in use", which is exactly the message a user needs.
	var cleanups []func() error
	defer func() {
		if err == nil {
			return
		}
		for i := len(cleanups) - 1; i >= 0; i-- {
			func() {
				defer func() { _ = recover() }()
				_ = cleanups[i]()
			}()
		}
	}()

	// 2. Storage, secrets and logs.
	st, err := store.Open(dataDir)
	if err != nil {
		return nil, err
	}
	a.store = st
	cleanups = append(cleanups, st.Close)

	// Egress health reads the same store: recent tunnel rows and manual
	// exit-IP checks decide whether an assigned proxy is passing traffic.
	// Lookups are cached, because the scheduler consults it under its lock.
	a.egressHealth = egress.NewHealth(st)

	// The scheduling core needs the sticky store and the durable cooldown
	// store; plugin data is fed in through schedulerSnapshot on every request.
	// The log callback is resolved lazily because the logger is opened below.
	a.dispatch = scheduler.New(scheduler.Options{
		Sticky:    stickyStoreAdapter{store: st},
		Cooldowns: schedulerCooldownStore{store: st},
		Log: func(msg string, fields map[string]any) {
			if a.logger != nil {
				a.logger.Info("app", msg, fields)
			}
		},
	})
	// Bind the dispatcher locally: the closure must not read the named return
	// value `a`, which a later `return nil, err` sets to nil before this defer
	// runs. Capturing the field through `a` is what made a failed New() panic.
	disp := a.dispatch
	cleanups = append(cleanups, func() error {
		disp.Close()
		return nil
	})

	sec, err := secrets.Open(dataDir)
	if err != nil {
		return nil, err
	}
	a.secrets = sec
	cleanups = append(cleanups, sec.Close)

	logger, err := logging.NewLogger(dataDir)
	if err != nil {
		return nil, err
	}
	a.logger = logger
	cleanups = append(cleanups, logger.Close)

	// Merge custom models created before connections existed. Idempotent, and a
	// failure logs instead of stopping startup: the rows stay usable through
	// the legacy key path until the next attempt.
	a.migrateLegacyCustomModels()

	// Restore durable request-path cooldowns now that the logger exists. The
	// store prunes expired rows on load, so a restart neither resurrects a
	// finished cooldown nor restarts a 429 backoff at the base duration. Not
	// before the first request: the gateway is not serving yet.
	a.dispatch.RestoreCooldowns()

	// Out-of-app notifications (F-19). The worker owns every HTTP call, so a
	// slow webhook can never delay a check-in pass. The URL is read from the
	// DPAPI store on each send: the config file never contains it.
	a.notify = notify.NewDispatcher(notify.DispatcherOptions{
		Endpoint: func() string {
			if a.secrets == nil {
				return ""
			}
			url, _ := a.secrets.GetExtra(notify.URLSecretKey)
			return url
		},
		Log:      logger,
		Settings: notifySettingsFromConfig(cfg.Notify),
	})
	a.notify.Start()
	// Captured locally for the same reason as the dispatcher above: the closure
	// must not reach through the named return value `a`, which a later
	// `return nil, err` clears before this defer runs.
	notifier := a.notify
	cleanups = append(cleanups, func() error {
		notifier.Stop()
		return nil
	})
	a.checkinNotify = newCheckinNotifier(a)

	apiLog, err := logging.NewAPILogger(st, dataDir, cfg.Logging.DebugBody)
	if err != nil {
		return nil, err
	}
	a.apiLog = apiLog
	cleanups = append(cleanups, apiLog.Close)

	pluginLog, err := logging.NewPluginLogWriter(dataDir)
	if err != nil {
		return nil, err
	}
	a.pluginLog = pluginLog
	cleanups = append(cleanups, pluginLog.Close)

	// Usage views and the unified timeline read the same store and logs.
	a.usageSvc = usage.NewService(st)
	a.timeline = logging.NewTimeline(st, dataDir, a.pluginIDs)

	a.logger.Info("app", "AIWorkGate starting", map[string]any{
		"version":  Version,
		"data_dir": dataDir,
	})

	// 3. Single instance: the second process only opens the existing UI.
	release, already, err := winutil.AcquireSingleInstance(AppName)
	if err != nil {
		return nil, fmt.Errorf("app: single instance: %w", err)
	}
	if release != nil {
		cleanups = append(cleanups, func() error {
			release()
			return nil
		})
	}
	if already {
		a.logger.Info("app", "another instance is running; opening its web UI", nil)
		a.openExistingUI(cfg)
		return nil, ErrAlreadyRunning
	}
	a.releaseInstance = release

	// Mint the admin launch token now that this process owns the instance: the
	// value is the secret behind the admin session cookie, so it must never be
	// generated before the single-instance check (a losing second process would
	// otherwise overwrite the live token). It is DPAPI-sealed into secrets.dat so
	// a later launch of this executable can reopen this UI (openExistingUI); a
	// failure is not fatal, the first window still opens from memory.
	a.adminToken = webui.NewSessionToken()
	if err := a.secrets.SetExtra(AdminLaunchTokenSecretKey, a.adminToken); err != nil {
		a.logger.Warn("app", "cannot persist the admin launch token; a second launch will not reopen this UI", nil)
	}

	// 4. Choose the listening port (cfg.Port, then +1..+10) and remember it.
	bindHost := "127.0.0.1"
	if cfg.ListenLAN {
		if sec.HasLANPassword() {
			bindHost = "0.0.0.0"
		} else {
			a.logger.Warn("app", "LAN mode requested without a LAN password; forcing loopback", nil)
		}
	}
	ln, port, err := listenGateway(bindHost, cfg.Port, maxPortOffset)
	if err != nil {
		return nil, fmt.Errorf("app: cannot bind a listening port starting at %d: %w", cfg.Port, err)
	}
	a.listener = ln
	a.port = port
	cleanups = append(cleanups, ln.Close)

	if err := writeGatewayPort(dataDir, port); err != nil {
		return nil, err
	}
	cleanups = append(cleanups, func() error {
		return os.Remove(gatewayPortPath(dataDir))
	})
	a.logger.Info("app", "gateway port selected", map[string]any{
		"host": bindHost, "port": port,
	})

	// 5. Plugins.
	a.pluginMgr = plugins.New(plugins.Options{
		DataDir:      dataDir,
		Config:       a.configSnapshot,
		Store:        st,
		Secrets:      sec,
		Log:          logger,
		PluginLog:    pluginLog,
		DirectEgress: a.directEgressURL,
	})
	cleanups = append(cleanups, a.pluginMgr.Close)
	if err := a.pluginMgr.Discover(); err != nil {
		return nil, fmt.Errorf("app: discover plugins: %w", err)
	}

	// 6. Account-level egress forwarders. Each listener reports its tunnels to
	// the proxy traffic log, which is the only trail of who dialled what through
	// a proxy (the payload itself is opaque TLS).
	a.egressMgr = egress.NewManager(st)
	a.egressMgr.SetLogSink(func(row model.ProxyLog) {
		if _, err := a.store.InsertProxyLog(row); err != nil {
			a.logger.Warn("app", "cannot write proxy log row", map[string]any{"error": err.Error()})
		}
		// The newest tunnel row is the freshest evidence of whether this
		// account's exit works; drop the cached verdict for that pair.
		a.egressHealth.Invalidate(row.PluginID, row.AccountID)
	})
	// Direct (unproxied) egress is only recorded after the user opts in: the
	// listeners it binds become part of the plugin's exit path, so the shipped
	// default keeps plugins dialling by themselves.
	if err := a.egressMgr.SetDirectLogging(cfg.Egress.DirectLogging); err != nil {
		a.logger.Warn("app", "cannot apply direct egress logging", map[string]any{"error": err.Error()})
	}
	cleanups = append(cleanups, a.egressMgr.CloseAll)
	// Mirror the Windows system proxy into the reserved proxy row before the
	// forwarders start, so an account already bound to "system" is resolvable on
	// the first egress file write. The loop keeps it current while it runs.
	a.startSystemProxySync()
	if err := a.egressMgr.StartEnabled(); err != nil {
		a.logger.Warn("app", "some egress proxies failed to start", map[string]any{"error": err.Error()})
	}

	// 7. Rule packs and runtime files. Both must exist before a plugin starts,
	// because the plugin reads them from the paths passed in its environment.
	a.rulesMgr = rules.New(st, dataDir, logger, pluginReloader{mgr: a.pluginMgr})
	if err := a.rulesMgr.SeedBuiltins(); err != nil {
		a.logger.Warn("app", "cannot seed builtin rule packs", map[string]any{"error": err.Error()})
	}
	a.writeAllRules()
	a.writeAllEgress()

	if err := a.pluginMgr.StartAll(); err != nil {
		a.logger.Warn("app", "some plugins failed to start", map[string]any{"error": err.Error()})
	}
	// Running plugins can now report their accounts; rewrite egress files with
	// the per-account assignments.
	a.writeAllEgress()
	a.initSchedule()

	// 8. A (AI Work 助手) upstream adapter.
	baseURL, aPort, discovered := resolveABaseURL(cfg, logger)
	a.aBaseURL, a.aPort = baseURL, aPort
	a.aURL, a.aPortLive = baseURL, aPort
	a.logger.Info("app", "A gateway configured", map[string]any{
		"base_url": a.aBaseURL, "port": a.aPort, "discovered": discovered,
	})
	// One-time handover of A's credentials to the aichat plugin (Q18): background, failure only warns.
	a.startAIChatUpstreamPush()

	// 9. Client key verification.
	a.keyVerifier = &keyAdapter{app: a}

	// 10. Local API gateway.
	a.gatewayHandler = gateway.New(gateway.Options{
		Plugins:          a.pluginMgr,
		A:                &aAdapter{app: a, baseURL: a.aBaseURL},
		Keys:             a.keyVerifier,
		Log:              apiLogSink{app: a},
		DebugBody:        cfg.Logging.DebugBody,
		AModelCacheTTL:   time.Duration(cfg.A.ModelCacheMinutes) * time.Minute,
		CorsAllowOrigins: cfg.Gateway.CorsAllowOrigins,
		// Bare model names route to the configured default plugin; the legacy A
		// fallback only applies while that slot is empty and the switch is on.
		DefaultPlugin: cfg.Gateway.DefaultPlugin,
		LegacyA:       cfg.Gateway.LegacyAFallback,
		Schedule:      a.dispatch,
		Snapshot:      a.schedulerSnapshot,
		SchedLog:      logger,
		Overrides:     a.store.GetModelOverrides,
		KeyRecord:     a.keyRecord,
		KeyUsageToday: a.keyUsageToday,
		KeyUsage:      a.recordKeyUsage,
		// User-defined custom models route straight to their own upstream
		// (Settings → 自定义模型); the key is read from the DPAPI store.
		CustomModels:   a.gatewayCustomModels,
		CustomModelKey: a.customModelKey,
	})
	// Atomic daily-quota reservation for client keys (A api_keys.rs:327-390).
	gateway.InstallKeyUsageReserver(a.store)

	// 11. Admin API and SPA. The handler enforces the launch token this process
	// minted above, so the URL the tray opens is the only way in.
	a.adminHandler = webui.New(a.webOptions())

	// 12/13. Router and HTTP server. SSE needs no WriteTimeout, but request
	// reads must not stay open forever (slowloris-on-body).
	a.server = &http.Server{
		Handler:           http.HandlerFunc(a.routeHTTP),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 14. Autostart matches the persisted switch; failures never block startup.
	if cfg.Autostart {
		if exe, exeErr := os.Executable(); exeErr != nil {
			a.logger.Warn("app", "cannot resolve executable path for autostart", map[string]any{"error": exeErr.Error()})
		} else if err := winutil.SetAutostart(AppName, exe); err != nil {
			a.logger.Warn("app", "cannot register autostart", map[string]any{"error": err.Error()})
		}
	} else if err := winutil.RemoveAutostart(AppName); err != nil {
		a.logger.Warn("app", "cannot remove autostart", map[string]any{"error": err.Error()})
	}

	// 15. Usage rollup. One synchronous pass runs before retention cleanup, so
	// api_logs that a long offline period left un-folded are not deleted before
	// they reach usage_daily. The background loop starts below.
	a.roller = usage.NewRoller(st, logger)
	a.startupRollup()

	// 16. Retention cleanup, then every 6h: a tray host that never restarts must
	// still stop growing.
	a.cleanupFiles(cfg, time.Now())
	safego.Go(a.logger, "retentionLoop", func() { a.retentionLoop() })
	safego.Go(a.logger, "modelSyncLoop", func() { a.modelSyncLoop() })

	list := a.pluginMgr.List()
	running := 0
	for _, p := range list {
		if p.Running {
			running++
		}
	}
	a.logger.Info("app", "plugins discovered", map[string]any{
		"total": len(list), "running": running,
	})

	// 17. Background rollup. Errors are logged by the roller and never block
	// startup.
	a.roller.Start(context.Background())

	return a, nil
}

// Run starts the HTTP server, installs the tray icon and blocks until the tray
// quits, the user presses Ctrl+C, or shutdown is requested. It always runs the
// idempotent Shutdown before returning.
func (a *App) Run() error {
	if a == nil {
		return errors.New("app: nil App")
	}
	if a.server == nil || a.listener == nil {
		return errors.New("app: Run called on an uninitialized host")
	}

	serveErr := make(chan error, 1)
	go func() {
		err := a.server.Serve(a.listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			a.logger.Error("app", "gateway server stopped", map[string]any{"error": err.Error()})
			select {
			case serveErr <- err:
			default:
			}
			tray.Quit()
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
			a.logger.Info("app", "interrupt received; shutting down", nil)
			tray.Quit()
		case <-a.shutdownCh:
		}
	}()

	if a.sched != nil {
		a.sched.Start(context.Background())
	}
	a.startAIfConfigured()
	a.logger.Info("app", "AIWorkGate ready", map[string]any{"port": a.port})
	if a.firstRun {
		a.openUI()
	}
	tray.Run(AppName, trayTooltip, tray.Actions{
		OpenUI:    a.openUI,
		PauseAll:  a.pauseAll,
		ResumeAll: a.resumeAll,
		Quit:      a.requestShutdown,
	})

	err := a.Shutdown()
	select {
	case serr := <-serveErr:
		return errors.Join(err, serr)
	default:
		return err
	}
}

// requestShutdown signals the tray loop to exit. It is safe to call multiple
// times and from any goroutine.
func (a *App) requestShutdown() {
	if a == nil || a.shutdownCh == nil {
		return
	}
	a.quitOnce.Do(func() {
		close(a.shutdownCh)
	})
}

// Shutdown tears the host down in reverse startup order. It is idempotent:
// later calls return the error of the first one.
func (a *App) Shutdown() error {
	if a == nil {
		return nil
	}
	a.shutdownOnce.Do(func() {
		a.requestShutdown()

		var errs []error
		if a.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			if err := a.server.Shutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("app: http shutdown: %w", err))
			}
			cancel()
		}
		if a.listener != nil {
			if err := a.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, fmt.Errorf("app: close listener: %w", err))
			}
		}
		if closer, ok := a.gatewayHandler.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
		if a.sched != nil {
			a.sched.Stop()
		}
		if a.notify != nil {
			// Stop before secrets/logger close: a queued delivery reads the
			// endpoint and logs its outcome.
			a.notify.Stop()
		}
		if a.dispatch != nil {
			a.dispatch.Close()
		}
		if a.roller != nil {
			a.roller.Stop()
		}
		if a.pluginMgr != nil {
			if err := a.pluginMgr.Close(); err != nil {
				errs = append(errs, fmt.Errorf("app: close plugins: %w", err))
			}
		}
		if a.egressMgr != nil {
			if err := a.egressMgr.CloseAll(); err != nil {
				errs = append(errs, fmt.Errorf("app: close egress: %w", err))
			}
		}
		// Rule packs hold no resources: the merged files stay on disk.
		if a.pluginLog != nil {
			if err := a.pluginLog.Close(); err != nil {
				errs = append(errs, fmt.Errorf("app: close plugin log: %w", err))
			}
		}
		if a.apiLog != nil {
			if err := a.apiLog.Close(); err != nil {
				errs = append(errs, fmt.Errorf("app: close api log: %w", err))
			}
		}
		if a.logger != nil {
			a.logger.Info("app", "AIWorkGate stopped", nil)
			if err := a.logger.Close(); err != nil {
				errs = append(errs, fmt.Errorf("app: close host log: %w", err))
			}
		}
		if a.store != nil {
			if err := a.store.Close(); err != nil {
				errs = append(errs, fmt.Errorf("app: close store: %w", err))
			}
		}
		if a.secrets != nil {
			if err := a.secrets.Close(); err != nil {
				errs = append(errs, fmt.Errorf("app: close secrets: %w", err))
			}
		}
		if a.releaseInstance != nil {
			a.releaseInstance()
			a.releaseInstance = nil
		}
		a.shutdownErr = errors.Join(errs...)
	})
	return a.shutdownErr
}

// configSnapshot returns a copy of the current configuration.
func (a *App) configSnapshot() config.Config {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.cfg
}

// openExistingUI activates the web UI of a process that already owns the
// single-instance mutex. The live session token belongs to that process, so it
// is read back from the DPAPI-sealed secret this user shares with it; when the
// read fails the page is opened without a token, which the admin API answers
// with 403 admin_session_required rather than silently granting access.
func (a *App) openExistingUI(cfg config.Config) {
	port := readGatewayPort(a.dataDir)
	if port == 0 {
		port = cfg.Port
	}
	token := ""
	if a.secrets != nil {
		if saved, ok := a.secrets.GetExtra(AdminLaunchTokenSecretKey); ok {
			token = saved
		}
	}
	if token == "" {
		a.logger.Warn("app", "cannot read the running instance's launch token; the page will open without a session", nil)
	}
	url := "http://127.0.0.1:" + strconv.Itoa(port) + adminUIQuery(token)
	if err := winutil.OpenBrowser(url); err != nil {
		a.logger.Warn("app", "cannot open the existing web UI", map[string]any{
			"url": url, "error": err.Error(),
		})
	}
}

// openUI opens the local admin page in the default browser.
func (a *App) openUI() {
	url := "http://127.0.0.1:" + strconv.Itoa(a.port) + adminUIQuery(a.adminToken)
	if err := winutil.OpenBrowser(url); err != nil {
		a.logger.Warn("app", "cannot open browser", map[string]any{"url": url, "error": err.Error()})
	}
}

// adminUIQuery renders the launch-token query string. The token is never
// logged: only the URL as passed to the shell is built here, and the callers
// above deliberately do not include it in their warning fields.
func adminUIQuery(token string) string {
	if token == "" {
		return "/"
	}
	return "/?" + launchTokenQueryKey + "=" + url.QueryEscape(token)
}

// launchTokenQueryKey matches the parameter internal/webui reads on
// POST /api/session.
const launchTokenQueryKey = "t"

func (a *App) pauseAll() {
	a.logger.Info("app", "pausing all plugins", nil)
	a.pluginMgr.PauseAll()
}

func (a *App) resumeAll() {
	a.logger.Info("app", "resuming plugins", nil)
	a.pluginMgr.ResumeAll()
}

// routeHTTP sends /v1 (and sub-paths) to the API gateway and everything else
// to the admin UI. Non-loopback admin requests need the LAN password; /v1 is
// authenticated with client bearer keys instead.
func (a *App) routeHTTP(w http.ResponseWriter, r *http.Request) {
	// A-compatible liveness probe (A's /health shape). It stays outside the
	// admin auth gate because it reveals nothing but "the host is up"; local
	// launchers and health checks use it to wait for the port.
	if r.URL.Path == "/health" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"status":"ok","running":true}`))
		}
		return
	}
	// A-compatible readiness probe (A /healthz: api_server/routes.rs:489-524,
	// route registered at api_server/server.rs:176). It must stay outside the
	// admin auth gate so launchers and watchdogs can poll it.
	if r.URL.Path == "/healthz" {
		a.handleHealthz(w, r)
		return
	}
	cfg := a.configSnapshot()
	// See hostcheck.go: a rebinding page presents its own hostname, which the
	// same-origin guard would otherwise accept.
	if !allowedRequestHost(r.Host, cfg.ListenLAN) {
		http.Error(w, "invalid host header", http.StatusForbidden)
		a.logger.Warn("app", "rejected request with unexpected Host", map[string]any{"host": r.Host})
		return
	}
	if adminAuthRequired(cfg.ListenLAN, isLoopbackRemote(r.RemoteAddr), isV1Path(r.URL.Path)) {
		_, password, ok := r.BasicAuth()
		if !ok || a.secrets == nil || !a.secrets.VerifyLANPassword(password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="AIWorkGate", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			a.logger.Warn("app", "rejected LAN admin request", map[string]any{"remote": r.RemoteAddr})
			return
		}
	}
	if isV1Path(r.URL.Path) {
		a.gatewayHandler.ServeHTTP(w, r)
		return
	}
	a.adminHandler.ServeHTTP(w, r)
}

// retentionLoop re-applies the retention windows while the host runs. A tray
// host can stay up for weeks, and startup-only cleanup would let the database
// and logs grow for that whole period.
func (a *App) retentionLoop() {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-a.shutdownCh:
			return
		case now := <-ticker.C:
			a.cleanupFiles(a.configSnapshot(), now)
		}
	}
}

// cleanupFiles applies the retention windows to logs and database rows.
func (a *App) cleanupFiles(cfg config.Config, now time.Time) {
	if err := logging.CleanupFiles(a.dataDir, cfg.Retention.HostLogDays, cfg.Retention.APIRequestDays, now); err != nil {
		a.logger.Warn("app", "log cleanup failed", map[string]any{"error": err.Error()})
	}
	apiCutoff := now.AddDate(0, 0, -cfg.Retention.APIRequestDays).Unix()
	if n, err := a.store.CleanupAPIRequestLogs(apiCutoff); err != nil {
		a.logger.Warn("app", "api log cleanup failed", map[string]any{"error": err.Error()})
	} else if n > 0 {
		a.logger.Info("app", "api logs removed", map[string]any{"rows": n})
	}
	checkinCutoff := now.AddDate(0, 0, -cfg.Retention.CheckinDays).Format("2006-01-02")
	if n, err := a.store.CleanupCheckinRecords(checkinCutoff); err != nil {
		a.logger.Warn("app", "check-in cleanup failed", map[string]any{"error": err.Error()})
	} else if n > 0 {
		a.logger.Info("app", "check-in rows removed", map[string]any{"rows": n})
	}
	// D97: bodies expire faster than their request row. Debug mode keeps the
	// short preview window, otherwise the preview lives with the API log.
	bodyDays := cfg.Retention.APIRequestDays
	if cfg.Logging.DebugBody {
		bodyDays = cfg.Retention.BodyPreviewDays
	}
	if n, err := a.store.CleanupAPILogBodies(now.AddDate(0, 0, -bodyDays).Unix()); err != nil {
		a.logger.Warn("app", "api log body cleanup failed", map[string]any{"error": err.Error()})
	} else if n > 0 {
		a.logger.Info("app", "api log bodies removed", map[string]any{"rows": n})
	}
	// usage_daily follows the widest range the admin API accepts, so a 366-day
	// query never silently under-reports pruned rows (§8.3).
	usageCutoff := now.AddDate(0, 0, -usage.MaxRangeDays).Format("2006-01-02")
	if n, err := a.store.CleanupUsageDaily(usageCutoff); err != nil {
		a.logger.Warn("app", "usage cleanup failed", map[string]any{"error": err.Error()})
	} else if n > 0 {
		a.logger.Info("app", "usage rows removed", map[string]any{"rows": n})
	}
	// Per-key counters use the same window; deleted keys stay in the ranking
	// until their history ages out instead of living forever.
	if n, err := a.store.CleanupClientKeyUsage(usageCutoff); err != nil {
		a.logger.Warn("app", "key usage cleanup failed", map[string]any{"error": err.Error()})
	} else if n > 0 {
		a.logger.Info("app", "key usage rows removed", map[string]any{"rows": n})
	}
	// The audit trail and quota snapshots have no UI window of their own; both
	// grow with uptime, so they follow fixed windows here.
	if n, err := a.store.PruneAudit(now.AddDate(0, 0, -auditRetentionDays).Unix()); err != nil {
		a.logger.Warn("app", "audit cleanup failed", map[string]any{"error": err.Error()})
	} else if n > 0 {
		a.logger.Info("app", "audit rows removed", map[string]any{"rows": n})
	}
	// The proxy traffic log keeps its own (shorter) window: one row per tunnel.
	if n, err := a.store.PruneProxyLogs(now.AddDate(0, 0, -cfg.Retention.ProxyLogDays).Unix()); err != nil {
		a.logger.Warn("app", "proxy log cleanup failed", map[string]any{"error": err.Error()})
	} else if n > 0 {
		a.logger.Info("app", "proxy log rows removed", map[string]any{"rows": n})
	}
	if n, err := a.store.CleanupQuotaSnapshots(now.AddDate(0, 0, -quotaRetentionDays).Unix()); err != nil {
		a.logger.Warn("app", "quota snapshot cleanup failed", map[string]any{"error": err.Error()})
	} else if n > 0 {
		a.logger.Info("app", "quota snapshots removed", map[string]any{"rows": n})
	}
}

// startupRollup folds pending api_logs once before cleanupFiles runs. Without
// it a host that stayed offline past the request-log window would delete rows
// that were never aggregated into usage_daily.
func (a *App) startupRollup() {
	if a == nil || a.roller == nil {
		return
	}
	n, err := a.roller.RollOnce(context.Background())
	if err != nil {
		a.logger.Warn("usage", "usage rollup failed", map[string]any{"error": err.Error()})
		return
	}
	if n > 0 {
		a.logger.Info("usage", "usage rollup advanced", map[string]any{"rows": n})
	}
}

// auditRetentionDays and quotaRetentionDays bound the two tables that no
// settings page controls.
const (
	auditRetentionDays = 90
	quotaRetentionDays = 30
)

// pluginIDs lists installed plugin ids for the timeline's plugin logs.
func (a *App) pluginIDs() []string {
	if a.pluginMgr == nil {
		return nil
	}
	list := a.pluginMgr.List()
	out := make([]string, 0, len(list))
	for _, st := range list {
		if id := strings.TrimSpace(st.Manifest.ID); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// writeAllRules materializes the merged rules file of every discovered plugin.
func (a *App) writeAllRules() {
	if a.rulesMgr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, st := range a.pluginMgr.List() {
		id := strings.TrimSpace(st.Manifest.ID)
		if id == "" {
			continue
		}
		if _, err := a.rulesMgr.WriteForPlugin(ctx, id, st.Manifest.Rules.Packs); err != nil {
			a.logger.Warn("app", "cannot write rules for plugin", map[string]any{
				"plugin": id, "error": err.Error(),
			})
		}
	}
}

// resolveABaseURL prefers A's discovered loopback port over the configured
// base URL; it falls back to the configured URL (contract default 7864).
func resolveABaseURL(cfg config.Config, logger *logging.Logger) (baseURL string, port int, discovered bool) {
	baseURL = strings.TrimSpace(cfg.A.BaseURL)
	if baseURL == "" {
		baseURL = config.DefaultABaseURL
	}
	port = portFromBaseURL(baseURL)

	dataDir, err := hosts.ADataDir()
	if err != nil {
		return baseURL, port, false
	}
	info, err := os.Stat(dataDir)
	if err != nil || !info.IsDir() {
		return baseURL, port, false
	}
	discoveredPort, err := hosts.DiscoverPort(dataDir)
	if err != nil {
		if logger != nil {
			logger.Info("app", "A port discovery failed; using the configured base URL", map[string]any{
				"error": err.Error(),
			})
		}
		return baseURL, port, false
	}
	return fmt.Sprintf("http://127.0.0.1:%d", discoveredPort), discoveredPort, true
}

// aUpstreamProbe classifies A's health over GET /health with a short timeout.
func (a *App) aUpstreamProbe(ctx context.Context, baseURL string) (hosts.AState, string) {
	if a.aProbeFn != nil {
		return a.aProbeFn(ctx, baseURL)
	}
	key := ""
	if a.secrets != nil {
		key, _ = a.secrets.GetAAPIKey()
	}
	client := hosts.NewClient(baseURL, key)
	if client.HTTP != nil {
		client.HTTP.Timeout = aHealthTimeout
	}
	return client.HealthState(ctx)
}

// aResolvePort re-runs A's loopback port discovery; (0, false) when it fails.
func (a *App) aResolvePort() (int, bool) {
	if a.aDiscoverPort != nil {
		return a.aDiscoverPort()
	}
	dataDir, err := hosts.ADataDir()
	if err != nil {
		return 0, false
	}
	port, err := hosts.DiscoverPort(dataDir)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// aEnsure returns A's health state, detail and live base URL, cached for
// aProbeTTL. When the current URL stops answering it re-runs port discovery,
// so an A restart on a new loopback port heals without a host restart; a
// down/unauthorized -> ok transition invalidates the gateway's A model cache.
func (a *App) aEnsure(force bool) (hosts.AState, string, string) {
	if a == nil {
		return hosts.AStateDown, "app is not running", ""
	}
	ttl := a.aProbeTTL
	if ttl <= 0 {
		ttl = aProbeTTLDefault
	}

	a.aMu.Lock()
	url := a.aURL
	if url == "" {
		url = a.aBaseURL
	}
	if url == "" {
		a.aMu.Unlock()
		return hosts.AStateDown, "A base URL is not configured", ""
	}
	if !force && a.aState != "" && time.Since(a.aProbeAt) < ttl {
		state, detail := a.aState, a.aDetail
		a.aMu.Unlock()
		return state, detail, url
	}

	prev := a.aState
	state, detail := a.aUpstreamProbe(context.Background(), url)
	if state == hosts.AStateDown {
		if port, ok := a.aResolvePort(); ok {
			if candidate := fmt.Sprintf("http://127.0.0.1:%d", port); candidate != url {
				cState, cDetail := a.aUpstreamProbe(context.Background(), candidate)
				if cState != hosts.AStateDown {
					url, state, detail = candidate, cState, cDetail
					a.aPortLive = port
					if a.logger != nil {
						a.logger.Info("app", "A base URL followed the discovered port", map[string]any{
							"base_url": url, "port": port,
						})
					}
				}
			}
		}
	}
	a.aURL, a.aState, a.aDetail = url, state, detail
	a.aProbeAt = time.Now()
	recovered := state == hosts.AStateOK && (prev == hosts.AStateDown || prev == hosts.AStateUnauthorized)
	a.aMu.Unlock()

	if recovered {
		a.InvalidateAModelCache()
	}
	return state, detail, url
}

// aLivePort returns the loopback port the host currently dials for A.
func (a *App) aLivePort() int {
	if a == nil {
		return 0
	}
	a.aMu.Lock()
	defer a.aMu.Unlock()
	if a.aPortLive > 0 {
		return a.aPortLive
	}
	return a.aPort
}

// aStatusProbe forces a fresh health check for GET /api/status and returns
// the three-state result, a short detail, the live port and whether A answers
// at all (the a_running compatibility value).
func (a *App) aStatusProbe() (state string, detail string, port int, running bool) {
	st, dt, url := a.aEnsure(true)
	port = a.aLivePort()
	if port == 0 {
		port = portFromBaseURL(url)
	}
	return string(st), dt, port, st != hosts.AStateDown
}

// InvalidateAModelCache asks the gateway to drop its cached A model list
// after A recovered, using the same interface-assertion pattern as SetDebug.
// internal/gateway exposes no such method yet: the call then only logs, and
// starts working the moment the gateway adds InvalidateAModelCache().
func (a *App) InvalidateAModelCache() {
	if g, ok := a.gatewayHandler.(interface{ InvalidateAModelCache() }); ok {
		g.InvalidateAModelCache()
		return
	}
	if a.logger != nil {
		a.logger.Warn("app", "cannot invalidate the A model cache: gateway exposes no hook yet", nil)
	}
}

// ---------------------------------------------------------------- client keys

// keyRecord adapts a stored client key to the gateway's scope view (plan
// 2026-10-21 §1). A missing row or an unwired store degrades to "unscoped":
// authentication already succeeded through the secret store, so a database
// problem must not reject traffic that used to work.
func (a *App) keyRecord(id string) (gateway.KeyRecord, bool, error) {
	if a == nil || a.store == nil || id == "" {
		return gateway.KeyRecord{}, false, nil
	}
	key, ok, err := a.store.GetClientKey(id)
	if err != nil || !ok {
		return gateway.KeyRecord{}, false, err
	}
	return gateway.KeyRecord{
		ID:         key.ID,
		Name:       key.Name,
		Enabled:    key.Enabled,
		ModelScope: key.ModelScope,
		BindPool:   key.BindPool,
		DailyQuota: key.DailyQuota,
	}, true, nil
}

// keyUsageToday reads the request counter of one key and local day (quota gate).
func (a *App) keyUsageToday(keyID, day string) (int, error) {
	if a == nil || a.store == nil {
		return 0, nil
	}
	return a.store.KeyUsageRequests(keyID, day)
}

// recordKeyUsage accumulates one finished gateway request. Errors are logged and
// otherwise ignored: the response is already on the wire.
func (a *App) recordKeyUsage(keyID, day string, requests, prompt, completion, cached int, credits *float64) {
	if a == nil || a.store == nil {
		return
	}
	if err := a.store.IncrementKeyUsage(keyID, day, requests, prompt, completion, cached, credits); err != nil && a.logger != nil {
		a.logger.Warn("app", "client key usage write failed", map[string]any{
			"key_id": keyID, "day": day, "error": err.Error(),
		})
	}
}
