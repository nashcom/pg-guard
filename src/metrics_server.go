// pg-guard -- metrics_server.go -- the read-only, unauthenticated listener
// (PG_GUARD_METRICS_PORT, default 9100 -- matches node_exporter's
// conventional port): GET /health, /ready, /status, /metrics. Always plain
// HTTP, even when PG_GUARD_API_PORT has switched to TLS -- monitoring
// tools (Prometheus etc.) expect a plain, always-on, unauthenticated
// scrape target and have no reason to go through TLS or a bearer token.
// The same four GET routes are also served on the API listener (api.go) --
// see its top comment for the full split; this file's port is never the
// only place to reach them.

package main

import (
	"context"
	"net"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// startMetricsServer starts the metrics/status listener in a background
// goroutine and returns it so the caller can Shutdown() it during
// pg-guard's own shutdown sequence -- see startAPIServer (api.go) for the
// separate mutating-only listener.
func startMetricsServer(cfg *Config, pool *pgxpool.Pool) *http.Server {
	s := &apiServer{cfg: cfg, pool: pool}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	addr := net.JoinHostPort(cfg.APIBind, strconv.Itoa(cfg.MetricsPort))
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		logInfo("metrics server listening on %s (unauthenticated, plain HTTP)", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logError("metrics server error: %v", err)
		}
	}()

	return srv
}

func (s *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *apiServer) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), statusQueryTimeout)
	defer cancel()

	if err := s.pool.Ping(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready: " + err.Error() + "\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

type statusResponse struct {
	Version                           string  `json:"version"`    // build-time version (see version.go/build.sh) -- "dev" outside a git checkout or a plain "go build ." with no -ldflags
	Commit                            string  `json:"commit"`     // build-time short commit hash -- "unknown" under the same conditions as Version
	BuildDate                         string  `json:"build_date"` // build-time UTC timestamp (RFC3339) -- "unknown" if built without build.sh
	Role                              string  `json:"role"`       // "primary" | "standby" | "unknown"
	PostgresReachable                 bool    `json:"postgres_reachable"`
	PeerReachable                     bool    `json:"peer_reachable"`
	PeerHost                          string  `json:"peer_host,omitempty"`
	ReplicationLagBytes               *int64  `json:"replication_lag_bytes,omitempty"`
	ServerVersion                     string  `json:"server_version,omitempty"`
	ShutdownInProgress                bool    `json:"shutdown_in_progress"`
	SwitchoverInProgress              bool    `json:"switchover_in_progress"`
	MaintenanceActive                 bool    `json:"maintenance_active"`                   // true from a successful POST /api/maintenance until postgres is running again (unlike ShutdownInProgress, this persists)
	RoleBeforeMaintenance             string  `json:"role_before_maintenance,omitempty"`    // "primary" | "standby" -- only set while MaintenanceActive; Role itself can't be live-queried while postgres is stopped
	ShutdownMode                      string  `json:"shutdown_mode"`                        // PG_GUARD_SHUTDOWN_MODE -- "switchover" | "reboot", see README's Shutdown Modes
	FailoverMode                      string  `json:"failover_mode"`                        // PG_GUARD_FAILOVER_MODE -- "automatic" | "manual"
	FailoverTimeoutSeconds            float64 `json:"failover_timeout_seconds"`             // configured PG_GUARD_FAILOVER_TIMEOUT -- how long the peer must be continuously unhealthy before automatic failover promotes (irrelevant when FailoverMode is "manual")
	FailoverCountdownActive           bool    `json:"failover_countdown_active"`            // true while the peer is currently being tracked as unreachable, counting toward automatic promotion
	FailoverCountdownRemainingSeconds float64 `json:"failover_countdown_remaining_seconds"` // 0 if FailoverCountdownActive is false
	RebootGracePeriodSeconds          float64 `json:"reboot_grace_period_seconds"`          // configured PG_GUARD_REBOOT_GRACE_PERIOD
	RebootSuppressActive              bool    `json:"reboot_suppress_active"`
	RebootSuppressRemainingSeconds    float64 `json:"reboot_suppress_remaining_seconds"` // 0 if RebootSuppressActive is false
	TLSEnabled                        bool    `json:"tls_enabled"`                       // true once PG_GUARD_SSL_CERT_FILE/PG_GUARD_SSL_KEY_FILE are set -- the API listener and outbound peer calls serve/use TLS instead of plain HTTP; never applies to the metrics listener
	MTLSRequired                      bool    `json:"mtls_required"`                     // PG_GUARD_MTLS_REQUIRE -- only meaningful while TLSEnabled is true
	MetricsMode                       string  `json:"metrics_mode"`                      // PG_GUARD_METRICS_MODE -- endpoint|textfile|both; /health|/ready|/status are always served regardless of this setting -- see Metrics section
	TextfileDir                       string  `json:"textfile_dir,omitempty"`            // PG_GUARD_TEXTFILE_DIR -- only meaningful while MetricsMode is textfile or both
	TextfileIntervalSeconds           float64 `json:"textfile_interval_seconds"`         // configured PG_GUARD_TEXTFILE_INTERVAL
	BackupEnabled                     bool    `json:"backup_enabled"`                    // PG_GUARD_BACKUP_ENABLED -- the periodic scheduler; the on-demand command works regardless
	BackupInProgress                  bool    `json:"backup_in_progress"`
	LastBackupTimestampSeconds        float64 `json:"last_backup_timestamp_seconds"`         // unix epoch of the most recent SUCCESSFUL backup; 0 if none this run -- see Metrics section
	LastBackupDurationSeconds         float64 `json:"last_backup_duration_seconds"`          // 0 if no backup has completed this run
	LastBackupAttemptOK               bool    `json:"last_backup_attempt_ok"`                // whether the most recent ATTEMPT (not necessarily successful) succeeded; meaningless while LastBackupAttemptTimestampSeconds is 0
	LastBackupAttemptError            string  `json:"last_backup_attempt_error,omitempty"`   // error text if the most recent attempt failed; empty if it succeeded or none has happened
	LastBackupAttemptTimestampSeconds float64 `json:"last_backup_attempt_timestamp_seconds"` // unix epoch of the most recent attempt, success or failure; 0 if none this run -- unlike LastBackupTimestampSeconds, this advances on failed attempts too, so it's what tells you backups are *currently* broken vs. just not due yet
	// backups_total/backup_failures_total are deliberately /metrics-only
	// (like promotions_total/rejoins_total) -- /status reports current
	// state, not cumulative counters.
	PostgresRestartLimit              int     `json:"postgres_restart_limit"`                // configured PG_GUARD_POSTGRES_RESTART_LIMIT -- 0 means in-process crash-restart is disabled
	PostgresRestartWindowSeconds      float64 `json:"postgres_restart_window_seconds"`       // configured PG_GUARD_POSTGRES_RESTART_WINDOW
	LastPostgresCrashTimestampSeconds float64 `json:"last_postgres_crash_timestamp_seconds"` // unix epoch of the most recently detected unexpected postgres exit; 0 if none this run -- see Automatic Restart section
	StatsFile                         string  `json:"stats_file,omitempty"`                  // PG_GUARD_STATS_FILE -- unset means pg_guard_starts_total/postgres_restarts_total/last_postgres_crash_timestamp_seconds are in-memory only, reset every pg-guard restart
	// postgres_restarts_total/pg_guard_starts_total are deliberately
	// /metrics-only, same reasoning as backups_total above.
}

func (s *apiServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), statusQueryTimeout)
	defer cancel()

	resp := statusResponse{
		Version:                           version,
		Commit:                            commit,
		BuildDate:                         buildDate,
		Role:                              "unknown",
		PeerReachable:                     checkPeerReachable(s.cfg),
		PeerHost:                          s.cfg.PeerHost,
		ShutdownInProgress:                shutdownInProgress.Load(),
		SwitchoverInProgress:              switchoverInProgress.Load(),
		MaintenanceActive:                 maintenanceActive.Load(),
		ShutdownMode:                      s.cfg.ShutdownMode,
		FailoverMode:                      s.cfg.FailoverMode,
		FailoverTimeoutSeconds:            s.cfg.FailoverTimeout.Seconds(),
		FailoverCountdownActive:           failoverCountdownActive(),
		FailoverCountdownRemainingSeconds: failoverPromoteRemaining(),
		RebootGracePeriodSeconds:          s.cfg.RebootGracePeriod.Seconds(),
		RebootSuppressActive:              rebootSuppressActive(),
		RebootSuppressRemainingSeconds:    rebootSuppressRemaining(),
		TLSEnabled:                        s.cfg.TLSEnabled,
		MTLSRequired:                      s.cfg.MTLSRequire,
		MetricsMode:                       s.cfg.MetricsMode,
		TextfileDir:                       s.cfg.TextfileDir,
		TextfileIntervalSeconds:           s.cfg.TextfileInterval.Seconds(),
		BackupEnabled:                     s.cfg.BackupEnabled,
		BackupInProgress:                  backupInProgress.Load(),
		LastBackupTimestampSeconds:        lastBackupTimestampSeconds(),
		LastBackupDurationSeconds:         lastBackupDurationSeconds(),
		PostgresRestartLimit:              s.cfg.PostgresRestartLimit,
		PostgresRestartWindowSeconds:      s.cfg.PostgresRestartWindow.Seconds(),
		LastPostgresCrashTimestampSeconds: lastPostgresCrashTimestampSeconds(),
		StatsFile:                         s.cfg.StatsFile,
	}
	resp.LastBackupAttemptOK, resp.LastBackupAttemptError, resp.LastBackupAttemptTimestampSeconds = backupAttemptStatus()
	if resp.MaintenanceActive {
		if maintenanceWasPrimary.Load() {
			resp.RoleBeforeMaintenance = "primary"
		} else {
			resp.RoleBeforeMaintenance = "standby"
		}
	}

	if err := s.pool.Ping(ctx); err != nil {
		resp.PostgresReachable = false
		logDebug("status: pool ping failed: %v", err)
	} else {
		resp.PostgresReachable = true
		if inRecovery, err := isInRecovery(ctx, s.pool); err == nil {
			if inRecovery {
				resp.Role = "standby"
			} else {
				resp.Role = "primary"
			}
			if lag, err := replicationLagBytes(ctx, s.pool, inRecovery); err == nil {
				resp.ReplicationLagBytes = &lag
			}
		}
		if version, err := serverVersion(ctx, s.pool); err == nil {
			resp.ServerVersion = version
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *apiServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.MetricsEnabled {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), metricsCollectionTimeout)
	defer cancel()

	body := collectMetrics(ctx, s.pool, s.cfg)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}
