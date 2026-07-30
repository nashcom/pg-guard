//go:build windows

// service_windows.go implements pg-guard as a registered Windows Service.
// The Service Control Manager (SCM) drives lifecycle via ChangeRequests
// instead of OS signals -- Windows has no real SIGTERM/SIGINT delivery to a
// process. Stop/Shutdown/PreShutdown requests all run the same
// handleTerminationSignal logic main.go's interactive path uses, in a
// goroutine, while this loop keeps SCM informed with periodic StopPending
// checkpoints so it doesn't decide the service is hung and kill it out from
// under a legitimately-long PG_GUARD_SHUTDOWN_WAIT. Shares main.go's
// currentChild/handoverRequests/exitRequested state (see state.go) so
// promote/shutdown/switchover/rejoin via the HTTP API work identically to
// the Linux path.
//
// PreShutdown (svc.AcceptPreShutdown/svc.PreShutdown, both present in the
// golang.org/x/sys version already in go.mod -- no dependency bump needed)
// matters specifically because of PG_GUARD_SHUTDOWN_WAIT's 300s default
// plus coordinated-handover overhead on top: an ordinary Stop/Shutdown
// during a real system shutdown/reboot only gets Windows's regular
// shutdown budget, which is much tighter than that. A service that
// registers for PreShutdown instead gets SERVICE_CONTROL_PRESHUTDOWN
// first, with a 3-minute default budget (PreshutdownTimeout, itself
// extendable via the same checkpoint mechanism used below) specifically
// meant for services that need real time to finish something -- exactly
// this. Handled identically to Stop/Shutdown, not specially: PreShutdown
// is itself the complete "shut down now, you have more time" signal for a
// preshutdown-aware service, not a precursor to a separate Shutdown
// notification for the same event.

package main

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sys/windows/svc"
)

func isWindowsService() bool {
	v, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return v
}

func runService(cfg *Config) error {
	return svc.Run("pg-guard", &pgGuardService{cfg: cfg})
}

type pgGuardService struct {
	cfg *Config
}

func (m *pgGuardService) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (svcSpecificEC bool, exitCode uint32) {
	status <- svc.Status{State: svc.StartPending}

	if err := initPeerHTTPClients(m.cfg); err != nil {
		logError("TLS config error: %v", err)
		status <- svc.Status{State: svc.Stopped}
		return false, 1
	}

	pool, err := newPool(m.cfg)
	if err != nil {
		logError("failed to create database pool: %v", err)
		status <- svc.Status{State: svc.Stopped}
		return false, 1
	}

	startReaper()
	initPersistentStats(m.cfg)
	sup := newSupervisor(m.cfg)

	// See runInteractive's (main.go) matching comment: the metrics/API
	// listeners must be up before startPostgres's rejoin retries run, or
	// the peer can never see this node as reachable in time to refresh its
	// pg_hba.conf grant.
	apiSrv := startAPIServer(m.cfg, pool)
	metricsSrv := startMetricsServer(m.cfg, pool)

	startPostgres(m.cfg, sup)
	ensureRoleAndDatabaseBlocking(m.cfg)

	startFailoverMonitor(m.cfg, pool)

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown}

	// restartAt mirrors main.go's runInteractive exactly -- see its own
	// comment for why a timer-in-select is used instead of time.Sleep
	// (keeps this loop responsive to a concurrent SCM Stop/Shutdown during
	// the backoff window).
	var restartAt <-chan time.Time

	for {
		cw := currentChild.Load()
		var childDone <-chan struct{}
		if cw != nil {
			childDone = cw.done
		}

		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown, svc.PreShutdown:
				logInfo("received %s from Service Control Manager -- shutting down", scmCmdName(c.Cmd))
				status <- svc.Status{State: svc.StopPending, WaitHint: 5000}
				waitForShutdown(m.cfg, sup, pool, status)
				stopServers(apiSrv, metricsSrv, pool)
				status <- svc.Status{State: svc.Stopped}
				return false, 0
			}

		case <-childDone:
			code := cw.code
			logWarn("child exited unexpectedly (code %d)", code)
			currentChild.Store(nil)
			postgresRunning.Store(false)
			recordPostgresCrash()
			persistStats(m.cfg)

			// Same crash-restart budget as main.go's runInteractive --
			// recordCrashAndCheckLimit is defined in main.go with no build
			// tag, so it's shared, not duplicated.
			if m.cfg.PostgresRestartLimit == 0 || !recordCrashAndCheckLimit(m.cfg) {
				logError("postgres crash-restart budget exhausted (or PG_GUARD_POSTGRES_RESTART_LIMIT=0) -- exiting instead of restarting")
				stopServers(apiSrv, metricsSrv, pool)
				status <- svc.Status{State: svc.Stopped}
				return false, uint32(code)
			}

			logWarn("restarting postgres in %s (crash-restart)", m.cfg.PostgresRestartBackoff)
			restartPending.Store(true)
			restartAt = time.After(m.cfg.PostgresRestartBackoff)

		case <-restartAt:
			restartAt = nil
			restartPending.Store(false)

			if currentChild.Load() != nil {
				logInfo("postgres already running -- skipping crash-restart")
				break
			}

			incrementPostgresRestarts()
			persistStats(m.cfg)
			startPostgres(m.cfg, sup)
			if currentChild.Load() == nil {
				logError("crash-restart failed to bring postgres back up -- exiting")
				stopServers(apiSrv, metricsSrv, pool)
				status <- svc.Status{State: svc.Stopped}
				return false, 1
			}

		case hreq := <-handoverRequests:
			handleHandoverRequest(sup, m.cfg, pool, hreq)

		case code := <-exitRequested:
			stopServers(apiSrv, metricsSrv, pool)
			status <- svc.Status{State: svc.Stopped}
			return false, uint32(code)
		}
	}
}

// scmCmdName gives the three shutdown-triggering control codes readable
// names for logging -- svc.Cmd has no String() method, and telling these
// apart in the log is exactly how to confirm PreShutdown is actually the
// one firing during a real Windows shutdown/reboot, not just Stop/Shutdown.
func scmCmdName(cmd svc.Cmd) string {
	switch cmd {
	case svc.Stop:
		return "Stop"
	case svc.Shutdown:
		return "Shutdown"
	case svc.PreShutdown:
		return "PreShutdown"
	default:
		return fmt.Sprintf("cmd %d", cmd)
	}
}

// waitForShutdown runs handleTerminationSignal in a goroutine and sends
// periodic StopPending checkpoints back to SCM while it waits. Which SCM
// timeout that extends depends on what triggered it -- ServicesPipeTimeout
// (~30s default) for an ordinary Stop/Shutdown, or the much larger
// PreshutdownTimeout (3 minutes default) for PreShutdown -- but the
// mechanism is the same either way: CheckPoint has to keep advancing.
func waitForShutdown(cfg *Config, sup *supervisor, pool *pgxpool.Pool, status chan<- svc.Status) {
	done := make(chan struct{})
	go func() {
		// SCM Stop/Shutdown commands have no OS-signal equivalent to
		// forward -- stopGracefullyPlatform on Windows ignores this value
		// anyway and always uses "pg_ctl stop -m fast". The returned exit
		// code is intentionally ignored here: unlike Docker's
		// "restart: on-failure", there's no SCM recovery action configured
		// in this codebase that inspects it, so PG_GUARD_SHUTDOWN_MODE=reboot
		// relies on a real OS reboot restarting the service on next boot,
		// not on this exit code triggering a same-session SCM restart.
		handleTerminationSignal(sup, cfg, pool, shutdownSignal)
		close(done)
	}()

	ticker := time.NewTicker(serviceStopPendingInterval)
	defer ticker.Stop()
	var checkpoint uint32

	for {
		select {
		case <-ticker.C:
			checkpoint++
			status <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 2000}
		case <-done:
			return
		}
	}
}
