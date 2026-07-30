// pg-guard -- metrics.go -- Prometheus text exposition, hand-rolled rather
// than pulling in the official client library: the metric set is small and
// static (40 gauges/counters, no histograms), matching this project's
// minimal-dependency stance. Exactly the metrics documented in README's
// Metrics section.
//
// HA metrics that depend on state this milestone doesn't build yet
// (coordinated shutdown, switchover, rejoin) are emitted as a static 0
// rather than omitted -- keeps the metric names stable for dashboards
// wired up early, matching Prometheus's own convention of always exposing
// a metric even when its value is currently a no-op. postgres_ha_role and
// postgres_ha_promotions_total ARE real: role detection and promote() are
// both implemented in this milestone.

package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var promotionsCounter atomic.Int64
var rejoinsCounter atomic.Int64

func incrementPromotions()   { promotionsCounter.Add(1) }
func promotionsTotal() int64 { return promotionsCounter.Load() }

func incrementRejoins()   { rejoinsCounter.Add(1) }
func rejoinsTotal() int64 { return rejoinsCounter.Load() }

// lastPromotionDuration/lastRejoinDuration/lastBootstrapDuration record how
// long the most recently completed operation of each kind took (as
// nanoseconds -- atomic.Int64 has no atomic float, and a duration doesn't
// need fractional-nanosecond precision anyway). "Why was failover slow" is
// a real question once customers are running this, and knowing whether
// pg_promote() itself was slow vs. something else nearby in the handover
// is otherwise invisible. Only the most recent value is kept -- a gauge,
// not a histogram -- matching this project's hand-rolled, minimal-
// dependency metrics stance: these events are rare enough that "how long
// did the last one take" answers the actual question operators ask,
// without histogram bucket infrastructure.
var (
	lastPromotionDuration atomic.Int64
	lastRejoinDuration    atomic.Int64
	lastBootstrapDuration atomic.Int64
)

func recordPromotionDuration(d time.Duration) { lastPromotionDuration.Store(int64(d)) }
func recordRejoinDuration(d time.Duration)    { lastRejoinDuration.Store(int64(d)) }
func recordBootstrapDuration(d time.Duration) { lastBootstrapDuration.Store(int64(d)) }

// postgresRestartsCounter/lastPostgresCrashUnixNano back the crash-restart
// metrics below -- see main.go's childDone handling, the only writer.
// lastPostgresCrashUnixNano is set the moment a crash is *detected*
// (before it's known whether pg-guard will restart it or give up), not
// when a restart succeeds -- same "no automatic backup in N hours" style
// alerting rationale as lastBackupUnixNano, just for the opposite kind of
// event (something to alert on happening, not something failing to
// happen).
var (
	postgresRestartsCounter   atomic.Int64
	lastPostgresCrashUnixNano atomic.Int64
)

func incrementPostgresRestarts()   { postgresRestartsCounter.Add(1) }
func postgresRestartsTotal() int64 { return postgresRestartsCounter.Load() }

func recordPostgresCrash() { lastPostgresCrashUnixNano.Store(time.Now().UnixNano()) }

// pgGuardStartsCounter counts pg-guard process starts (any reason: crash-
// restart budget exhausted -> container restarted, manual restart,
// redeploy, host reboot) -- "how many times has this node come back from
// scratch." Only ever incremented once, at startup (see statepersist.go's
// initPersistentState); persisted like postgresRestartsCounter/
// lastPostgresCrashUnixNano above when PG_GUARD_STATE_FILE is set, so it
// keeps counting across restarts instead of resetting to 0 every time.
var pgGuardStartsCounter atomic.Int64

func incrementPgGuardStarts()   { pgGuardStartsCounter.Add(1) }
func pgGuardStartsTotal() int64 { return pgGuardStartsCounter.Load() }

// lastPostgresCrashTimestampSeconds returns the unix timestamp of the most
// recently detected unexpected postgres exit, or 0 if none this run.
func lastPostgresCrashTimestampSeconds() float64 {
	ns := lastPostgresCrashUnixNano.Load()
	if ns == 0 {
		return 0
	}
	return float64(ns) / float64(time.Second)
}

func lastPromotionDurationSeconds() float64 {
	return time.Duration(lastPromotionDuration.Load()).Seconds()
}
func lastRejoinDurationSeconds() float64 { return time.Duration(lastRejoinDuration.Load()).Seconds() }
func lastBootstrapDurationSeconds() float64 {
	return time.Duration(lastBootstrapDuration.Load()).Seconds()
}

// backupsCounter/backupFailuresCounter/lastBackupDuration/lastBackupUnixNano
// back the backup metrics below -- see backup.go's runBackup, the only
// writer. lastBackupUnixNano (not a Duration) records *when* the last
// successful backup finished, not how long it took -- what
// postgres_ha_last_backup_timestamp_seconds needs to let an alerting rule
// catch "no backup has run in N hours" directly, the actual failure mode
// worth watching for in unattended scheduled backups.
var (
	backupsCounter        atomic.Int64
	backupFailuresCounter atomic.Int64
	lastBackupDuration    atomic.Int64
	lastBackupUnixNano    atomic.Int64
)

func incrementBackups()          { backupsCounter.Add(1) }
func backupsTotal() int64        { return backupsCounter.Load() }
func incrementBackupFailures()   { backupFailuresCounter.Add(1) }
func backupFailuresTotal() int64 { return backupFailuresCounter.Load() }

func recordBackupSuccess(d time.Duration) {
	lastBackupDuration.Store(int64(d))
	lastBackupUnixNano.Store(time.Now().UnixNano())
}

func lastBackupDurationSeconds() float64 {
	return time.Duration(lastBackupDuration.Load()).Seconds()
}

// lastBackupTimestampSeconds returns the unix timestamp of the last
// successful backup, or 0 if none has completed this run.
func lastBackupTimestampSeconds() float64 {
	ns := lastBackupUnixNano.Load()
	if ns == 0 {
		return 0
	}
	return float64(ns) / float64(time.Second)
}

// backupAttempt* track the outcome of the most recent backup *attempt*
// (success or failure), separately from lastBackup* above (success only).
// backup_failures_total climbing tells you failures have happened
// somewhere in this run's history, but not whether backups are *currently*
// broken -- if the last several scheduled attempts all failed,
// lastBackupTimestampSeconds just sits frozen at the last success, with
// nothing distinguishing "healthy, hasn't run again yet" from "broken,
// been failing for hours." A plain mutex, not atomics, since the error
// text needs to move together with its own timestamp and success flag as
// one consistent snapshot -- same reasoning as peer.go's
// lastPeerSeen/markPeerSeen.
var (
	backupAttemptMu           sync.Mutex
	backupLastAttemptOK       bool
	backupLastAttemptErr      string
	backupLastAttemptUnixNano int64
)

// recordBackupAttempt is called by performBackup (backup.go) for every
// real attempt -- success or a genuine failure -- but deliberately not for
// errBackupNotPrimary (the scheduler's expected, silent per-tick skip on a
// standby, not an attempt at all) or errBackupInProgress (another attempt
// was already running; this one never actually ran).
func recordBackupAttempt(err error) {
	backupAttemptMu.Lock()
	defer backupAttemptMu.Unlock()
	backupLastAttemptUnixNano = time.Now().UnixNano()
	if err != nil {
		backupLastAttemptOK = false
		backupLastAttemptErr = err.Error()
	} else {
		backupLastAttemptOK = true
		backupLastAttemptErr = ""
	}
}

// backupAttemptStatus returns whether the most recent attempt succeeded,
// its error text (empty if it succeeded or none has happened yet), and
// when it happened (unix seconds, 0 if no attempt this run -- callers
// should treat ok's default (true) as meaningless until this is nonzero).
func backupAttemptStatus() (ok bool, errMsg string, attemptSeconds float64) {
	backupAttemptMu.Lock()
	defer backupAttemptMu.Unlock()
	if backupLastAttemptUnixNano == 0 {
		return true, "", 0
	}
	return backupLastAttemptOK, backupLastAttemptErr, float64(backupLastAttemptUnixNano) / float64(time.Second)
}

func collectMetrics(ctx context.Context, pool *pgxpool.Pool, cfg *Config) string {
	var b strings.Builder

	pingStart := time.Now()
	up := pool.Ping(ctx) == nil
	pingDuration := time.Since(pingStart)
	writeGauge(&b, "postgres_up", "Whether the local Postgres instance is reachable.", boolToFloat(up))
	writeGauge(&b, "postgres_ping_duration_seconds", "How long the most recent connectivity check against the local Postgres instance took, whether it succeeded or not -- a slow-but-up Postgres is a different problem than a down one, and postgres_up alone can't tell them apart.", pingDuration.Seconds())

	if up {
		if inRecovery, err := isInRecovery(ctx, pool); err != nil {
			logWarn("metrics: querying role: %v", err)
		} else {
			writeGauge(&b, "postgres_ha_role", "1 if this node is currently primary, 0 if standby.", boolToFloat(!inRecovery))

			if conns, err := connectionCount(ctx, pool); err != nil {
				logWarn("metrics: %v", err)
			} else {
				writeGauge(&b, "postgres_connections", "Current number of connections to the local Postgres instance.", float64(conns))
			}

			if size, err := databaseSizeBytes(ctx, pool, cfg.PostgresDB); err != nil {
				logWarn("metrics: %v", err)
			} else {
				writeGauge(&b, "postgres_database_size_bytes", "Size in bytes of the configured database.", float64(size))
			}

			if connected, err := replicationConnected(ctx, pool, inRecovery); err != nil {
				logWarn("metrics: %v", err)
			} else {
				writeGauge(&b, "postgres_replication_connected", "Whether replication is currently connected/streaming.", boolToFloat(connected))
			}

			if lag, err := replicationLagBytes(ctx, pool, inRecovery); err != nil {
				logWarn("metrics: %v", err)
			} else {
				writeGauge(&b, "postgres_replication_lag_bytes", "Replication lag in bytes (standby: behind primary; primary: furthest-behind connected standby).", float64(lag))
			}
		}
	}

	peerReachable := checkPeerReachable(cfg)
	writeGauge(&b, "postgres_ha_peer_reachable", "Whether the peer pg-guard API is currently reachable.", boolToFloat(peerReachable))
	writeGauge(&b, "postgres_ha_peer_last_seen_seconds", "Seconds since the peer was last confirmed reachable; -1 if never seen this run.", secondsSincePeerSeen())

	writeGauge(&b, "postgres_ha_switchover_in_progress", "Whether a coordinated switchover is currently in progress.", boolToFloat(switchoverInProgress.Load()))
	writeGauge(&b, "postgres_ha_shutdown_requested", "Whether a coordinated shutdown is currently in progress.", boolToFloat(shutdownInProgress.Load()))
	writeGauge(&b, "postgres_ha_maintenance_active", "Whether postgres is deliberately stopped for maintenance (POST /api/maintenance) -- persists until POST /api/start, unlike shutdown_requested.", boolToFloat(maintenanceActive.Load()))
	writeGauge(&b, "postgres_ha_maintenance_role_primary", "Whether this node was primary (1) or standby (0) right before its most recent maintenance stop; only meaningful while postgres_ha_maintenance_active is 1.", boolToFloat(maintenanceWasPrimary.Load()))
	writeGauge(&b, "postgres_ha_shutdown_deferred", "Bidirectional shutdown-cancellation negotiation is out of scope -- always 0.", 0)
	writeGauge(&b, "postgres_ha_shutdown_mode_reboot", "Whether PG_GUARD_SHUTDOWN_MODE is reboot (1) rather than switchover (0).", boolToFloat(cfg.ShutdownMode == "reboot"))
	writeGauge(&b, "postgres_ha_failover_mode_automatic", "Whether PG_GUARD_FAILOVER_MODE is automatic (1) rather than manual (0).", boolToFloat(cfg.FailoverMode == "automatic"))
	writeGauge(&b, "postgres_ha_failover_timeout_seconds", "Configured PG_GUARD_FAILOVER_TIMEOUT in seconds -- how long the peer must be continuously unhealthy before automatic failover promotes (irrelevant when failover_mode_automatic is 0).", cfg.FailoverTimeout.Seconds())
	writeGauge(&b, "postgres_ha_failover_countdown_active", "Whether the peer is currently being tracked as unreachable, counting toward automatic promotion.", boolToFloat(failoverCountdownActive()))
	writeGauge(&b, "postgres_ha_failover_countdown_remaining_seconds", "Seconds remaining before automatic failover promotes this node; 0 if failover_countdown_active is 0.", failoverPromoteRemaining())
	writeGauge(&b, "postgres_ha_tls_enabled", "Whether the API listener and outbound peer calls use TLS (PG_GUARD_SSL_CERT_FILE/PG_GUARD_SSL_KEY_FILE set) instead of plain HTTP; never applies to the metrics listener.", boolToFloat(cfg.TLSEnabled))
	writeGauge(&b, "postgres_ha_mtls_required", "Whether PG_GUARD_MTLS_REQUIRE is set -- client certificates are required on POST /api/*; only meaningful when postgres_ha_tls_enabled is 1.", boolToFloat(cfg.MTLSRequire))
	writeGauge(&b, "postgres_ha_reboot_grace_period_seconds", "Configured PG_GUARD_REBOOT_GRACE_PERIOD in seconds.", cfg.RebootGracePeriod.Seconds())
	writeGauge(&b, "postgres_ha_reboot_suppress_active", "Whether automatic failover promotion is currently suppressed by a peer's planned-reboot notice (PG_GUARD_SHUTDOWN_MODE=reboot).", boolToFloat(rebootSuppressActive()))
	writeGauge(&b, "postgres_ha_reboot_suppress_remaining_seconds", "Seconds remaining in an active planned-reboot failover-suppression window; 0 if none is active.", rebootSuppressRemaining())
	writeCounter(&b, "postgres_ha_promotions_total", "Successful promotions (manual or automatic failover).", float64(promotionsTotal()))
	writeCounter(&b, "postgres_ha_rejoins_total", "Successful rejoins (pg_rewind or pg_basebackup) as a standby.", float64(rejoinsTotal()))
	writeGauge(&b, "postgres_ha_last_promotion_duration_seconds", "How long the most recent successful pg_promote() call took; 0 if none has happened this run.", lastPromotionDurationSeconds())
	writeGauge(&b, "postgres_ha_last_rejoin_duration_seconds", "How long the most recent successful rejoin (pg_rewind or pg_basebackup fallback) took; 0 if none has happened this run.", lastRejoinDurationSeconds())
	writeGauge(&b, "postgres_ha_last_bootstrap_duration_seconds", "How long the most recent first-run bootstrap took; 0 if this node didn't bootstrap this run (already-initialized PGDATA).", lastBootstrapDurationSeconds())
	writeCounter(&b, "postgres_ha_postgres_restarts_total", "Automatic in-process restarts of postgres after an unexpected exit (see PG_GUARD_POSTGRES_RESTART_LIMIT). Does not include deliberate stop/start via the API.", float64(postgresRestartsTotal()))
	writeCounter(&b, "postgres_ha_pg_guard_starts_total", "How many times this pg-guard process itself has started (any reason -- container restart, redeploy, host reboot). Persisted across restarts when PG_GUARD_STATE_FILE is set; otherwise always 1, since nothing carries it forward.", float64(pgGuardStartsTotal()))
	writeGauge(&b, "postgres_ha_postgres_last_crash_timestamp_seconds", "Unix timestamp of the most recently detected unexpected postgres exit; 0 if none this run.", lastPostgresCrashTimestampSeconds())
	writeGauge(&b, "postgres_ha_backup_enabled", "Whether PG_GUARD_BACKUP_ENABLED is true -- the periodic backup scheduler is running.", boolToFloat(cfg.BackupEnabled))
	writeGauge(&b, "postgres_ha_backup_in_progress", "Whether a backup (scheduled or on-demand) is currently running.", boolToFloat(backupInProgress.Load()))
	writeCounter(&b, "postgres_ha_backups_total", "Successful backups (scheduled or on-demand via POST /api/backup).", float64(backupsTotal()))
	writeCounter(&b, "postgres_ha_backup_failures_total", "Failed backup attempts (scheduled or on-demand).", float64(backupFailuresTotal()))
	writeGauge(&b, "postgres_ha_last_backup_duration_seconds", "How long the most recent successful backup took; 0 if none has completed this run.", lastBackupDurationSeconds())
	writeGauge(&b, "postgres_ha_last_backup_timestamp_seconds", "Unix timestamp of the most recent successful backup; 0 if none has completed this run -- alert if this stops advancing.", lastBackupTimestampSeconds())
	backupAttemptOK, _, backupAttemptSeconds := backupAttemptStatus()
	writeGauge(&b, "postgres_ha_backup_last_attempt_ok", "Whether the most recent backup attempt (scheduled or on-demand) succeeded; meaningless if postgres_ha_last_backup_attempt_timestamp_seconds is 0 (no attempt yet). Differs from the last-success fields above: this can be 0 while a real backup is still sitting on disk from an earlier success.", boolToFloat(backupAttemptOK))
	writeGauge(&b, "postgres_ha_last_backup_attempt_timestamp_seconds", "Unix timestamp of the most recent backup attempt, success or failure; 0 if none this run. The error text itself is only on GET /status, not here -- Prometheus metrics in this project carry no labels.", backupAttemptSeconds)

	return b.String()
}

func writeGauge(b *strings.Builder, name, help string, value float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", name, help, name, name, formatFloat(value))
}

func writeCounter(b *strings.Builder, name, help string, value float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n%s %s\n", name, help, name, name, formatFloat(value))
}

// formatFloat formats at full (shortest-roundtrip) precision -- same exact
// value 'g'/-1 would have produced -- but with the 'f' verb, which never
// switches to scientific notation the way 'g' does for small/large
// magnitudes. No precision lost, nanosecond-level accuracy included where
// it exists (e.g. postgres_ha_peer_last_seen_seconds, derived from
// time.Since(...).Seconds()); only the presentation changes:
// postgres_ha_peer_last_seen_seconds now prints as "0.000075847" instead
// of "7.5847e-05", postgres_ha_last_backup_timestamp_seconds (a Unix
// timestamp) as "1785187872.3960674" instead of "1.7851878723960674e+09" --
// both exact, just not in an exponent a human has to mentally parse.
// Whole numbers/booleans are unaffected either way (e.g. "60", "1").
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
