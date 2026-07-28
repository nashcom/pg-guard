// pg-guard -- api.go -- the HTTP API from README's REST API section, split
// across two listeners: PG_GUARD_API_PORT (this file's startAPIServer)
// serves the mutating POST /api/* routes (gated by requireHAAuth's optional
// bearer-token/peer-origin checks, auth.go) AND the same read-only GET
// /health|/ready|/status|/metrics routes as the metrics listener -- so a
// client that only reaches the API port/TLS endpoint (e.g. through a
// firewall that doesn't open PG_GUARD_METRICS_PORT) can still check status
// without a second listener. PG_GUARD_METRICS_PORT (metrics_server.go's
// startMetricsServer) keeps serving the same GET routes too, unauthenticated,
// always plain HTTP -- for monitoring tools (Prometheus, node_exporter-style
// scraping) that expect a plain, always-on, unauthenticated port and have no
// reason to go through TLS or a bearer token. The two listeners exist
// separately (not one mux) so TLS applies to the API port alone (see
// tls.go): a single net/http.Server is TLS-or-plaintext for every route it
// serves, so path-based exceptions aren't possible on one listener -- the
// metrics port staying plain HTTP always is an explicit scoping decision,
// not a temporary gap. Only POST /api/* is exclusive to the API port.
//
// shutdown/switchover/rejoin don't touch *supervisor/*childWatcher
// directly -- they send a handoverRequest and block on its result channel
// so the actual work always runs on the main loop goroutine (see
// state.go, main.go's handleHandoverRequest). This is what keeps
// coordinateHandover from ever running concurrently with itself or with
// the main loop's own crash-detection path over the same child.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type apiServer struct {
	cfg  *Config
	pool *pgxpool.Pool
}

// startAPIServer starts the mutating-only API listener (PG_GUARD_API_PORT)
// in a background goroutine and returns it so the caller can Shutdown() it
// during pg-guard's own shutdown sequence -- see startMetricsServer
// (metrics_server.go) for the separate GET listener.
func startAPIServer(cfg *Config, pool *pgxpool.Pool) *http.Server {
	s := &apiServer{cfg: cfg, pool: pool}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("POST /api/promote", s.requireHAAuth(s.handlePromote))
	mux.HandleFunc("POST /api/shutdown", s.requireHAAuth(s.handleShutdown))
	mux.HandleFunc("POST /api/rejoin", s.requireHAAuth(s.handleRejoin))
	mux.HandleFunc("POST /api/switchover", s.requireHAAuth(s.handleSwitchover))
	mux.HandleFunc("POST /api/maintenance", s.requireHAAuth(s.handleMaintenance))
	mux.HandleFunc("POST /api/start", s.requireHAAuth(s.handleStart))
	mux.HandleFunc("POST /api/reboot-notice", s.requireHAAuth(s.handleRebootNotice))
	mux.HandleFunc("POST /api/backup", s.requireHAAuth(s.handleBackup))

	addr := net.JoinHostPort(cfg.APIBind, strconv.Itoa(cfg.APIPort))
	srv := &http.Server{Addr: addr, Handler: mux}

	if cfg.TLSEnabled {
		tlsCfg, err := buildServerTLSConfig(cfg)
		if err != nil {
			// Already validated once in loadConfig -- this would mean the
			// cert files changed or vanished between startup and here.
			logFatal("API server: %v", err)
		}
		srv.TLSConfig = tlsCfg

		go func() {
			mode := "TLS"
			if cfg.MTLSRequire {
				mode = "mutual TLS -- client certificate required"
			}
			logInfo("API server listening on %s (%s)", addr, mode)
			if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				logError("API server error: %v", err)
			}
		}()

		return srv
	}

	go func() {
		logInfo("API server listening on %s (plain HTTP -- set PG_GUARD_SSL_CERT_FILE/PG_GUARD_SSL_KEY_FILE to switch to TLS)", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logError("API server error: %v", err)
		}
	}()

	return srv
}

// handlePromote guards against the exact scenario that motivated this
// milestone: promoting a standby while the peer is still reachable and
// still reports itself primary would create a split-brain, since
// pg_promote() has no way to notify/demote the old primary. ?force=true
// bypasses the check for a genuine manual override.
func (s *apiServer) handlePromote(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), promoteOperationTimeout)
	defer cancel()

	if r.URL.Query().Get("force") != "true" {
		if status, err := fetchPeerStatus(s.cfg); err == nil && status.Role == "primary" {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "peer is reachable and reports itself as primary -- promoting now would cause split-brain; pass ?force=true to override",
			})
			return
		}
	}

	promoteStart := time.Now()
	if err := promote(ctx, s.pool); err != nil {
		logError("promote failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	recordPromotionDuration(time.Since(promoteStart))
	incrementPromotions()
	// Refresh the pg_hba.conf replication grant and confirm the role/
	// database exist before reporting success: this node's own /status
	// starts reporting "primary" the instant pg_promote() above returns,
	// and the caller (or an automatic-failover peer) treats that as the
	// handover being complete -- see ensureReplicationHBA for why a stale,
	// inherited grant would otherwise silently break the next rejoin.
	ensureRoleAndDatabaseBlocking(s.cfg)
	logInfo("promoted to primary via POST /api/promote")
	writeJSON(w, http.StatusOK, map[string]string{"result": "promoted"})
}

// handleShutdown runs a policy-aware coordinated handover (see handover.go)
// and, on success, exits -- under "restart: on-failure", exit 0 means the
// container stays down (the point of a real shutdown), while
// PG_GUARD_SHUTDOWN_MODE=reboot instead runs coordinateReboot (no peer
// promotion, just a "suppress your failover" notice) and exits with the
// same sentinel switchover restart code, so the container comes back up --
// startPostgres's existing rejoin check resumes it as primary unchanged if
// the peer stayed standby, or rejoins as standby if not.
func (s *apiServer) handleShutdown(w http.ResponseWriter, r *http.Request) {
	shutdownInProgress.Store(true)
	defer shutdownInProgress.Store(false)

	kind := "shutdown"
	if s.cfg.ShutdownMode == "reboot" {
		kind = "reboot"
	}

	req := handoverRequest{kind: kind, result: make(chan error, 1)}
	handoverRequests <- req
	if err := <-req.result; err != nil {
		logError("shutdown failed: %v", err)
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	logInfo("shutdown complete via POST /api/shutdown (mode=%s)", s.cfg.ShutdownMode)
	writeJSON(w, http.StatusOK, map[string]string{"result": "shutdown complete"})
	if s.cfg.ShutdownMode == "reboot" {
		exitRequested <- exitCodeSwitchoverRestart
	} else {
		exitRequested <- 0
	}
}

// handleRebootNotice is called by the peer (see notifyPeerReboot, peer.go)
// right before it stops under PG_GUARD_SHUTDOWN_MODE=reboot: it suppresses
// this node's own automatic-failover promotion for
// PG_GUARD_REBOOT_GRACE_PERIOD, so a brief, expected outage doesn't
// trigger the promotion the peer explicitly asked to avoid. Idempotent --
// a repeat notice just extends the suppression window.
func (s *apiServer) handleRebootNotice(w http.ResponseWriter, r *http.Request) {
	until := time.Now().Add(s.cfg.RebootGracePeriod)
	rebootSuppressUntil.Store(until.UnixNano())
	logInfo("received planned-reboot notice from peer -- suppressing automatic failover promotion until %s", until.UTC().Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]string{
		"result":         "reboot notice acknowledged",
		"suppress_until": until.UTC().Format(time.RFC3339),
	})
}

// handleMaintenance runs the same policy-aware coordinated handover as
// shutdown (peer promotion if currently primary, per PG_GUARD_SHUTDOWN_POLICY)
// -- but, unlike shutdown, does NOT exit pg-guard itself afterward: the API,
// metrics, and /status stay reachable so an operator can work on the data
// directory (or anything else needing postgres stopped) without the
// container restarting. Resume with POST /api/start when done.
func (s *apiServer) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	shutdownInProgress.Store(true)
	defer shutdownInProgress.Store(false)

	// Captured before the stop, while postgres can still be queried --
	// /status's live "role" field is necessarily "unknown" once stopped,
	// so this is the only chance to record it (see maintenanceWasPrimary's
	// own doc comment, state.go). Best-effort: if this check itself fails,
	// role_before_maintenance just won't be reported -- not worth refusing
	// the actual maintenance stop over.
	roleCtx, roleCancel := context.WithTimeout(r.Context(), roleCheckTimeout)
	if inRecovery, err := isInRecovery(roleCtx, s.pool); err == nil {
		maintenanceWasPrimary.Store(!inRecovery)
	}
	roleCancel()

	req := handoverRequest{kind: "maintenance", result: make(chan error, 1)}
	handoverRequests <- req
	if err := <-req.result; err != nil {
		logError("maintenance stop failed: %v", err)
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	maintenanceActive.Store(true)
	logInfo("postgres stopped for maintenance via POST /api/maintenance -- pg-guard staying online")
	writeJSON(w, http.StatusOK, map[string]string{"result": "postgres stopped for maintenance -- pg-guard still running; resume with POST /api/start"})
}

// handleSwitchover is the same coordinated handover as shutdown, but only
// valid when currently primary, and exits with a distinct sentinel code
// afterward so "restart: on-failure" brings the container straight back up
// -- the startup rejoin check then rejoins it as standby automatically.
func (s *apiServer) handleSwitchover(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), roleCheckTimeout)
	defer cancel()

	if !postgresRunning.Load() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "postgres is not currently running"})
		return
	}
	inRecovery, err := isInRecovery(ctx, s.pool)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	if inRecovery {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "switchover must be requested on the current primary, not a standby"})
		return
	}

	switchoverInProgress.Store(true)
	defer switchoverInProgress.Store(false)

	req := handoverRequest{kind: "switchover", result: make(chan error, 1)}
	handoverRequests <- req
	if err := <-req.result; err != nil {
		logError("switchover failed: %v", err)
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	logInfo("switchover complete via POST /api/switchover -- restarting to rejoin as standby")
	writeJSON(w, http.StatusOK, map[string]string{"result": "switchover complete -- restarting to rejoin as standby"})
	exitRequested <- exitCodeSwitchoverRestart
}

// handleRejoin is valid only when postgres isn't currently running under
// the supervisor (see main.go's handleHandoverRequest): it re-clones from
// the peer (pg_rewind, falling back to pg_basebackup) and starts postgres.
func (s *apiServer) handleRejoin(w http.ResponseWriter, r *http.Request) {
	req := handoverRequest{kind: "rejoin", result: make(chan error, 1)}
	handoverRequests <- req
	if err := <-req.result; err != nil {
		logError("rejoin failed: %v", err)
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	maintenanceActive.Store(false)
	logInfo("rejoin complete via POST /api/rejoin")
	writeJSON(w, http.StatusOK, map[string]string{"result": "rejoined as standby and started"})
}

// handleStart brings postgres back up after a maintenance stop (or any
// other externally-stopped state) via the same startup logic used at
// process boot (main.go's startPostgres, see the "start" case in
// handleHandoverRequest) -- it decides whether a rejoin is actually needed
// rather than blindly restarting in whatever role PGDATA was last left in.
func (s *apiServer) handleStart(w http.ResponseWriter, r *http.Request) {
	req := handoverRequest{kind: "start", result: make(chan error, 1)}
	handoverRequests <- req
	if err := <-req.result; err != nil {
		logError("start failed: %v", err)
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	maintenanceActive.Store(false)
	// Deliberately NOT blocking this response on ensureRoleAndDatabaseBlocking
	// (unlike runInteractive's startup call and handlePromote's, which both
	// have a real race to protect against -- see ensureRoleAndDatabaseBlocking's
	// own doc comment): the API here is already live and has been correctly
	// reporting postgres_reachable=false the whole time postgres was down, so
	// there's no equivalent "peer sees primary before the role exists" window
	// to close. Blocking here just means a slow-to-start postgres (recovery,
	// a rejoin needing another basebackup, etc.) makes this HTTP call itself
	// hang for up to 60s, which looks indistinguishable from "nothing
	// happened" -- confirmed as confusing while testing. Run it in the
	// background instead and let /status and /metrics reflect real-time
	// progress; log its own outcome separately so that's still visible.
	go func() {
		ensureRoleAndDatabaseBlocking(s.cfg)
		logInfo("post-start role/database check finished")
	}()
	logInfo("postgres started via POST /api/start")
	writeJSON(w, http.StatusOK, map[string]string{"result": "postgres started"})
}

// handleBackup triggers an on-demand pg_basebackup archive via
// performBackup (backup.go) -- shares the exact same path (role check,
// backupInProgress guard, metrics, pruning) the periodic scheduler uses, so
// a manual trigger behaves identically to a scheduled one. Unlike
// promote/shutdown/rejoin/switchover, this never touches
// *supervisor/*childWatcher (only shells out against the already-running
// instance), so it doesn't go through handoverRequests.
func (s *apiServer) handleBackup(w http.ResponseWriter, r *http.Request) {
	dest, duration, err := performBackup(s.cfg, s.pool)
	switch {
	case errors.Is(err, errBackupNotPrimary):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "backup must be requested on the current primary, not a standby"})
	case errors.Is(err, errBackupInProgress):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a backup is already in progress"})
	case err != nil:
		logError("backup failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	default:
		logInfo("backup complete via POST /api/backup (%s, took %s)", dest, duration.Round(time.Second))
		writeJSON(w, http.StatusOK, map[string]string{
			"result":           "backup complete",
			"path":             dest,
			"duration_seconds": strconv.FormatFloat(duration.Seconds(), 'f', -1, 64),
		})
	}
}

// stopServers gracefully shuts down both HTTP listeners (API and metrics)
// and closes the DB pool. Called explicitly at each shutdown call site
// rather than via defer, since the existing shutdown paths in
// main.go/service_windows.go call os.Exit() directly, which skips
// deferred functions.
func stopServers(apiSrv, metricsSrv *http.Server, pool *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
	defer cancel()
	if err := apiSrv.Shutdown(ctx); err != nil {
		logWarn("API server shutdown: %v", err)
	}
	if err := metricsSrv.Shutdown(ctx); err != nil {
		logWarn("metrics server shutdown: %v", err)
	}
	pool.Close()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
