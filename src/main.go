// pg-guard -- PostgreSQL HA Supervisor.
// Starts and supervises the postgres binary directly (PG_GUARD_POSTGRES_BIN,
// against PGDATA) -- no docker-entrypoint.sh dependency on any platform;
// PGDATA is always assumed to be already initialized by something else.
// Forwards/relays shutdown and reaps zombie processes as real container
// PID 1 on Linux. On Windows it can also run as a registered Windows
// Service (see service_windows.go). Coordinated shutdown, switchover,
// rejoin, and automatic failover are implemented -- see handover.go,
// rewind.go, failover.go, and state.go for the runtime-state/channel
// design that keeps all supervisor mutation on a single goroutine.

package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// main dispatches on a bare subcommand (os.Args[1], no dash -- "status",
// "promote", "set-password", ...), matching conventional CLI tools
// (git/docker/kubectl): verb first, "-flags" reserved for options/modifiers
// on that verb (-force, -json). With no command at all, starts the
// supervisor (postgres + API server) -- the docker-compose entrypoint and
// the Windows Service Control Manager (see service_install_windows.go's
// CreateService -- no extra args registered) both always invoke pg-guard
// with zero args, so that path is untouched by anything below.
func main() {
	if len(os.Args) < 2 {
		if isWindowsService() {
			runServiceEntrypoint()
			return
		}
		runInteractive()
		return
	}

	cmd := os.Args[1]
	opts := os.Args[2:]

	switch cmd {
	case "help", "-h", "-help", "--help", "/help", "/?", "-?":
		// Both a bare "help" command and the traditional dash-prefixed
		// flags work -- help must never be gated behind anything that
		// could fail (config errors, etc.).
		printHelp()

	case "version", "-v", "-version", "--version":
		// Same unconditional treatment as help -- never gated behind
		// config loading, which could itself fail.
		fmt.Printf("pg-guard %s (commit %s, built %s)\n", version, commit, buildDate)

	case "install-service":
		requireNoOpts(cmd, opts)
		if err := installService(); err != nil {
			fmt.Fprintf(os.Stderr, "pg-guard: install failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("pg-guard service installed.")

	case "uninstall-service":
		requireNoOpts(cmd, opts)
		if err := uninstallService(); err != nil {
			fmt.Fprintf(os.Stderr, "pg-guard: uninstall failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("pg-guard service uninstalled.")

	case "set-password":
		requireNoOpts(cmd, opts)
		initLogging()
		cfg, err := loadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pg-guard: config error: %v\n", err)
			os.Exit(1)
		}
		if err := runSetPassword(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "pg-guard: set-password failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("pg-guard: password updated.")

	case "backup":
		// "-stdout" bypasses the running supervisor entirely -- pg_basebackup
		// runs directly from this short-lived CLI process with its tar output
		// wired straight to os.Stdout, so the operator can pipe it anywhere
		// ("| aws s3 cp - s3://...", "| ssh host 'cat > backup.tar.gz'")
		// without pg-guard knowing about any storage backend. Every other
		// option (bare "backup", "-force", "-json") stays on the normal
		// POST /api/backup path below, same as promote/rejoin/switchover --
		// that path is what PG_GUARD_BACKUP_DIR/retention/scheduling/metrics
		// all apply to; -stdout intentionally has none of that (see
		// runBackupToStdout's doc comment, backup.go).
		if len(opts) == 1 && opts[0] == "-stdout" {
			initLogging()
			cfg, err := loadConfig()
			if err != nil {
				fmt.Fprintf(os.Stderr, "pg-guard: config error: %v\n", err)
				os.Exit(1)
			}
			if err := runBackupToStdout(cfg); err != nil {
				fmt.Fprintf(os.Stderr, "pg-guard: backup -stdout failed: %v\n", err)
				os.Exit(1)
			}
			return
		}
		if apiCmd := findAPICommand(cmd); apiCmd != nil {
			runAPICommand(*apiCmd, opts)
		}

	default:
		if apiCmd := findAPICommand(cmd); apiCmd != nil {
			runAPICommand(*apiCmd, opts)
			return
		}
		fmt.Fprintf(os.Stderr, "pg-guard: unknown command: %s\n", cmd)
		fmt.Fprintln(os.Stderr, "Run 'pg-guard help' for usage.")
		os.Exit(1)
	}
}

// requireNoOpts rejects any leftover argument after a command that never
// takes options (install-service/uninstall-service/set-password) -- a
// typo'd or misplaced flag should fail loudly, not be silently ignored.
func requireNoOpts(cmd string, opts []string) {
	if len(opts) > 0 {
		fmt.Fprintf(os.Stderr, "pg-guard: %s takes no options (got %v)\n", cmd, opts)
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Println("pg-guard -- PostgreSQL HA Supervisor")
	fmt.Println()
	fmt.Println("Usage: pg-guard [command] [options]")
	fmt.Println()
	fmt.Println("With no command: starts the supervisor (postgres + API server).")
	fmt.Println()
	fmt.Println("Configuration is entirely environment variables (no config file):")
	fmt.Println()
	for _, vd := range varDefs {
		fmt.Printf("  %-22s %-18s %s\n", vd.name, vd.def, vd.desc)
	}
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Printf("  %-31s %s\n", "help", "show this help and exit (-h/-help/--help/-?/? also work)")
	fmt.Printf("  %-31s %s\n", "version", "show version/commit/build date and exit (-v/-version/--version also work)")
	if runtime.GOOS == "windows" {
		fmt.Printf("  %-31s %s\n", "install-service", "register as a Windows Service (elevated prompt required)")
		fmt.Printf("  %-31s %s\n", "uninstall-service", "remove the Windows Service")
	}
	fmt.Printf("  %-31s %s\n", "set-password", "set/rotate POSTGRES_PASSWORD on the local node (reads new password from stdin); run against each node")
	fmt.Println()
	fmt.Println("API commands (call the running local instance's own API and exit):")
	fmt.Printf("  %-31s %s\n", "health", "GET /health")
	fmt.Printf("  %-31s %s\n", "ready", "GET /ready")
	fmt.Printf("  %-31s %s\n", "status", "GET /status")
	fmt.Printf("  %-31s %s\n", "metrics", "GET /metrics")
	fmt.Printf("  %-31s %s\n", "promote [-force]", "POST /api/promote (-force adds ?force=true)")
	fmt.Printf("  %-31s %s\n", "shutdown", "POST /api/shutdown (behavior depends on PG_GUARD_SHUTDOWN_MODE)")
	fmt.Printf("  %-31s %s\n", "rejoin", "POST /api/rejoin")
	fmt.Printf("  %-31s %s\n", "switchover", "POST /api/switchover")
	fmt.Printf("  %-31s %s\n", "maintenance", "POST /api/maintenance (stop postgres, pg-guard stays running)")
	fmt.Printf("  %-31s %s\n", "start", "POST /api/start (resume postgres after maintenance)")
	fmt.Printf("  %-31s %s\n", "backup [-stdout]", "POST /api/backup (must be run on the primary); -stdout bypasses the API and streams pg_basebackup's tar output straight to stdout instead (see README's Backup section)")
	fmt.Println()
	fmt.Println("Options (for the API commands above):")
	fmt.Printf("  %-31s %s\n", "-force", "with promote: add ?force=true")
	fmt.Printf("  %-31s %s\n", "-json", "print the raw API response instead of key=value text")
	fmt.Printf("  %-31s %s\n", "-stdout", "with backup: stream to stdout instead of PG_GUARD_BACKUP_DIR/PG_GUARD_BACKUP_COMMAND -- see above")
}

func runServiceEntrypoint() {
	initLogging()
	cfg, err := loadConfig()
	if err != nil {
		logFatal("config error: %v", err)
	}
	if err := runService(cfg); err != nil {
		logFatal("service error: %v", err)
	}
}

func runInteractive() {
	initLogging()

	cfg, err := loadConfig()
	if err != nil {
		logFatal("config error: %v", err)
	}
	dumpConfig(cfg)
	initPersistentState(cfg)

	if err := initPeerHTTPClients(cfg); err != nil {
		logFatal("TLS config error: %v", err)
	}

	pool, err := newPool(cfg)
	if err != nil {
		logFatal("failed to create database pool: %v", err)
	}

	startReaper()
	sup := newSupervisor(cfg)

	// Start the metrics/API listeners before startPostgres, not after: a
	// startup rejoin can now retry for up to 30s (rejoinAtStartupIfNeeded),
	// and the peer's own periodic pg_hba.conf grant refresh (failover.go)
	// only runs while it can see this node as reachable via these same
	// listeners. Starting them only after startPostgres returned meant this
	// node was invisible to that check for the entire retry window --
	// confirmed as a real bug in testing: every retry failed with the exact
	// same stale-grant error, because the peer never got a chance to see
	// this node come back and fix it in time. Safe to start early: any
	// mutating POST /api/* call that arrives before the main loop below is
	// running just blocks on the unbuffered handoverRequests send (state.go)
	// until it is, rather than racing startPostgres's own direct calls.
	apiSrv := startAPIServer(cfg, pool)
	metricsSrv := startMetricsServer(cfg, pool)

	startPostgres(cfg, sup)
	ensureRoleAndDatabaseBlocking(cfg)

	startFailoverMonitor(cfg, pool)
	startBackupScheduler(cfg, pool)
	startTextfileWriter(cfg, pool)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// restartAt is non-nil only while a crash-restart backoff is pending --
	// same nil-channel-in-select pattern as childDone below (a nil channel
	// blocks forever, so the case simply never fires until armed). Using a
	// timer here instead of time.Sleep keeps the loop responsive to a
	// concurrent SIGTERM/handover request during the backoff window.
	var restartAt <-chan time.Time

	for {
		cw := currentChild.Load()
		var childDone <-chan struct{}
		if cw != nil {
			childDone = cw.done
		}

		select {
		case sig := <-sigCh:
			logInfo("received %s -- shutting down", sig)
			code := handleTerminationSignal(sup, cfg, pool, sig)
			stopServers(apiSrv, metricsSrv, pool)
			os.Exit(code)

		case <-childDone:
			code := cw.code
			logWarn("child exited unexpectedly (code %d)", code)
			currentChild.Store(nil)
			postgresRunning.Store(false)
			recordPostgresCrash()
			persistState(cfg)

			// PostgresRestartLimit == 0 means crash-restart is disabled
			// (see config.go) -- every crash falls straight through to the
			// exact pre-existing behavior below. Otherwise,
			// recordCrashAndCheckLimit reports whether this crash is still
			// within budget; once it isn't (a genuine crash loop -- bad
			// config, corrupt data dir, etc.), fall back to the same exit
			// behavior so Docker's restart:on-failure / an operator
			// restarting the Windows Service remains the final safety net.
			if cfg.PostgresRestartLimit == 0 || !recordCrashAndCheckLimit(cfg) {
				logError("postgres crash-restart budget exhausted (or PG_GUARD_POSTGRES_RESTART_LIMIT=0) -- exiting instead of restarting")
				stopServers(apiSrv, metricsSrv, pool)
				os.Exit(code)
			}

			logWarn("restarting postgres in %s (crash-restart)", cfg.PostgresRestartBackoff)
			restartPending.Store(true)
			restartAt = time.After(cfg.PostgresRestartBackoff)

		case <-restartAt:
			restartAt = nil
			restartPending.Store(false)

			// currentChild may already be non-nil here if a manual POST
			// /api/start or /api/rejoin raced the backoff and won --
			// restartPending being true while it ran should have made both
			// handlers refuse (handleHandoverRequest below), but this
			// double-check costs nothing and avoids ever starting a second
			// postgres against the same PGDATA if that guard is ever
			// bypassed some other way.
			if currentChild.Load() != nil {
				logInfo("postgres already running -- skipping crash-restart")
				break
			}

			incrementPostgresRestarts()
			persistState(cfg)
			startPostgres(cfg, sup)
			if currentChild.Load() == nil {
				logError("crash-restart failed to bring postgres back up -- exiting")
				stopServers(apiSrv, metricsSrv, pool)
				os.Exit(1)
			}

		case req := <-handoverRequests:
			handleHandoverRequest(sup, cfg, pool, req)

		case code := <-exitRequested:
			stopServers(apiSrv, metricsSrv, pool)
			os.Exit(code)
		}
	}
}

// crashTimestamps records each detected unexpected postgres exit within the
// current PG_GUARD_POSTGRES_RESTART_WINDOW -- like currentChild/etc.
// (state.go), only ever touched from the single main-loop goroutine, so no
// locking is needed here either.
var crashTimestamps []time.Time

// recordCrashAndCheckLimit appends now to crashTimestamps, prunes entries
// older than cfg.PostgresRestartWindow, and reports whether the number of
// crashes still within that window is within cfg.PostgresRestartLimit --
// false tells the caller to give up restarting and exit instead.
func recordCrashAndCheckLimit(cfg *Config) bool {
	now := time.Now()
	cutoff := now.Add(-cfg.PostgresRestartWindow)

	kept := crashTimestamps[:0]
	for _, t := range crashTimestamps {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	crashTimestamps = append(kept, now)

	return len(crashTimestamps) <= cfg.PostgresRestartLimit
}

// alreadyConfiguredAsStandby reports whether local PGDATA is already set up
// to stream from a primary (standby.signal present) -- a confirmed local
// fact, unlike anything about the peer's current reachability/role, which
// can be transiently unknown right after a restart.
func alreadyConfiguredAsStandby(cfg *Config) bool {
	_, err := os.Stat(filepath.Join(cfg.PGData, "standby.signal"))
	return err == nil
}

// startPostgres runs the first-run bootstrap check, then the startup
// rejoin check, then starts postgres -- unless bootstrap or rejoin was
// needed and failed, in which case postgres is deliberately left stopped
// so an operator can inspect /status and retry (via POST /api/rejoin,
// or fixing the underlying problem and restarting) rather than
// crash-looping the whole container on a problem a bare restart won't fix.
func startPostgres(cfg *Config, sup *supervisor) {
	if needsBootstrap(cfg) {
		bootstrapStart := time.Now()
		if err := runBootstrap(cfg); err != nil {
			logError("bootstrap failed: %v -- postgres NOT started", err)
			return
		}
		recordBootstrapDuration(time.Since(bootstrapStart))
	}

	if err := rejoinAtStartupIfNeeded(cfg); err != nil {
		logError("startup rejoin failed: %v -- postgres NOT started; fix the problem and retry via POST /api/rejoin", err)
		return
	}

	if err := sup.start(); err != nil {
		logFatal("failed to start child: %v", err)
	}
	currentChild.Store(watchChild(sup.exited))
	postgresRunning.Store(true)
}

// rejoinAtStartupIfNeeded decides whether this node should re-clone from
// the peer before starting postgres -- the peer is reachable and reports
// itself primary, and local PGDATA isn't already configured as a standby
// -- retrying the whole decision, not just the clone, for up to 30s.
//
// A single unretried peer-status check here is unsafe: a concurrent
// two-node restart (e.g. docker compose up -d --force-recreate, or both
// hosts rebooting together) means the peer's own listener may simply not
// be up yet on the very first check. Treating that as a confirmed "peer
// isn't primary, no rejoin needed" -- as this used to -- let a node start
// up as a stale primary instead of waiting a moment to find out its peer
// already holds that role. Confirmed as a real, repeatable split-brain in
// testing: two nodes restarted together each raced the other's listener
// coming up and both ended up primary.
//
// A definitive "no" (peer reachable and explicitly reports non-primary, or
// local PGDATA is already configured as a standby) returns immediately --
// only an inconclusive result (peer unreachable/erroring) keeps retrying.
// If the peer never becomes reachable at all within the window, this
// fails loud (postgres left stopped, matching bootstrap's -1 tiebreak
// precedent) rather than silently risk a second primary.
func rejoinAtStartupIfNeeded(cfg *Config) error {
	if cfg.PeerHost == "" || alreadyConfiguredAsStandby(cfg) {
		return nil
	}

	deadline := time.Now().Add(startupRejoinDecisionTimeout)
	for {
		status, err := fetchPeerStatus(cfg)
		if err != nil {
			if time.Now().After(deadline) {
				return fmt.Errorf("could not confirm peer %s's role within 30s (%w) -- refusing to start automatically in case it's actually primary", cfg.PeerHost, err)
			}
			time.Sleep(startupRejoinPollInterval)
			continue
		}
		if status.Role != "primary" {
			return nil
		}

		logWarn("peer %s reports primary and local PGDATA is not a standby -- rejoining before starting postgres", cfg.PeerHost)
		if err := rejoinAsStandby(cfg); err == nil {
			logInfo("startup rejoin complete -- starting as standby")
			return nil
		} else if time.Now().After(deadline) {
			return err
		} else {
			logWarn("startup rejoin attempt failed (%v) -- retrying", err)
			time.Sleep(startupRejoinRetryInterval)
		}
	}
}

// handleTerminationSignal runs a policy-aware coordinated handover in
// response to SIGTERM/SIGINT, falling back to a forced local stop if
// coordination refuses or fails -- the OS/orchestrator is asking us to
// terminate, so we must eventually comply even when a clean handover isn't
// possible right now. sig is forwarded to postgres as-received (not
// normalized to SIGTERM) -- see handover.go's shutdownSignal comment for
// why that distinction matters (Postgres's smart vs. fast shutdown).
//
// Returns the process exit code the caller should use: 0 (stay down) for
// PG_GUARD_SHUTDOWN_MODE=switchover (today's default), or the switchover
// sentinel restart code for PG_GUARD_SHUTDOWN_MODE=reboot -- see
// coordinateReboot (handover.go) for why reboot mode always expects to
// come back.
func handleTerminationSignal(sup *supervisor, cfg *Config, pool *pgxpool.Pool, sig os.Signal) int {
	cw := currentChild.Load()
	if cw == nil {
		return 0
	}

	if cfg.ShutdownMode == "reboot" {
		if err := coordinateReboot(sup, cw, cfg, pool, sig); err != nil {
			logError("reboot stop failed (%v) -- forcing local stop", err)
			if err := coordinateHandover(sup, cw, cfg, pool, sig, "force"); err != nil {
				logError("forced stop also failed: %v", err)
			}
		}
		currentChild.Store(nil)
		postgresRunning.Store(false)
		return exitCodeSwitchoverRestart
	}

	if err := coordinateHandover(sup, cw, cfg, pool, sig, cfg.ShutdownPolicy); err != nil {
		logError("coordinated shutdown failed (%v) -- forcing local stop", err)
		if err := coordinateHandover(sup, cw, cfg, pool, sig, "force"); err != nil {
			logError("forced stop also failed: %v", err)
		}
	}
	currentChild.Store(nil)
	postgresRunning.Store(false)
	return 0
}

// handleHandoverRequest is called only from the main loop's select, so it's
// the sole place (besides startup) that ever mutates sup/currentChild --
// coordinateHandover never runs concurrently with itself or with the crash
// detection path.
func handleHandoverRequest(sup *supervisor, cfg *Config, pool *pgxpool.Pool, req handoverRequest) {
	switch req.kind {
	case "shutdown", "switchover", "maintenance":
		cw := currentChild.Load()
		if cw == nil {
			req.result <- fmt.Errorf("postgres is not currently running")
			return
		}
		if err := coordinateHandover(sup, cw, cfg, pool, shutdownSignal, cfg.ShutdownPolicy); err != nil {
			req.result <- err
			return
		}
		currentChild.Store(nil)
		postgresRunning.Store(false)
		req.result <- nil

	case "reboot":
		cw := currentChild.Load()
		if cw == nil {
			req.result <- fmt.Errorf("postgres is not currently running")
			return
		}
		if err := coordinateReboot(sup, cw, cfg, pool, shutdownSignal); err != nil {
			req.result <- err
			return
		}
		currentChild.Store(nil)
		postgresRunning.Store(false)
		req.result <- nil

	case "rejoin":
		if restartPending.Load() {
			req.result <- fmt.Errorf("a crash-restart is currently pending -- wait for it to complete first")
			return
		}
		if currentChild.Load() != nil {
			req.result <- fmt.Errorf("postgres is currently running -- stop it first")
			return
		}
		if err := rejoinAsStandby(cfg); err != nil {
			req.result <- fmt.Errorf("rejoin failed: %w", err)
			return
		}
		if err := sup.start(); err != nil {
			req.result <- fmt.Errorf("starting postgres after rejoin: %w", err)
			return
		}
		currentChild.Store(watchChild(sup.exited))
		postgresRunning.Store(true)
		req.result <- nil

	case "start":
		if restartPending.Load() {
			req.result <- fmt.Errorf("a crash-restart is currently pending -- wait for it to complete first")
			return
		}
		if currentChild.Load() != nil {
			req.result <- fmt.Errorf("postgres is already running")
			return
		}
		// Reuses the exact startup logic (startPostgres, above) instead of
		// unconditionally rejoining like "rejoin" does above: if this node
		// was stopped as a standby and nothing changed, it should just come
		// back up as-is, not pay for a needless re-clone -- but if it was
		// stopped as primary and the peer got promoted in the meantime (the
		// maintenance-stop path can trigger exactly that), it needs to
		// rejoin as standby instead of starting a second primary.
		// startPostgres logs its own failure and leaves postgres stopped;
		// currentChild staying nil after the call is how that's detected
		// here, since startPostgres itself returns nothing.
		startPostgres(cfg, sup)
		if currentChild.Load() == nil {
			req.result <- fmt.Errorf("postgres failed to start -- check logs")
			return
		}
		req.result <- nil

	default:
		req.result <- fmt.Errorf("unknown handover request kind %q", req.kind)
	}
}
