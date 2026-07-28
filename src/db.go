// pg-guard -- db.go -- pgx connectivity for live queries against the
// locally-supervised, running postgres instance. Per README's "Command
// Execution vs. Direct Connection": this is for anything that's a live
// query against a running server (role detection, replication lag,
// promotion, metrics); one-shot operations on the data directory itself
// (pg_rewind, pg_basebackup, pg_ctl stop on Windows) shell out instead --
// see stop_windows.go, detect_linux.go/detect_windows.go.

package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newPool creates a pgx connection pool against the locally-supervised
// postgres instance over TCP to 127.0.0.1 (not a Unix socket -- keeps this
// identical on Linux and Windows). pgxpool.New doesn't require the target
// to be reachable yet; it connects lazily on first use, which matters
// because this is called concurrently with the supervisor starting
// postgres, not after it's confirmed ready.
//
// Deliberately connects to the "postgres" maintenance database, not
// cfg.PostgresDB: every query this pool ever runs (role detection,
// replication lag, pg_promote(), connection counts, database size via
// pg_database_size($1) -- which works cross-database, targeting any name)
// is instance-level, not specific to the application database. Connecting
// to "postgres" means this pool works from the moment initdb completes,
// well before cfg.PostgresDB exists -- avoiding a startup race where
// /status polling (from the peer, or the local failover monitor) would
// otherwise hit "FATAL: database ... does not exist" on every attempt
// until bootstrap's ensureRoleAndDatabase creates it.
func newPool(cfg *Config) (*pgxpool.Pool, error) {
	connString := fmt.Sprintf(
		"host=127.0.0.1 port=%d user=%s password=%s dbname=postgres sslmode=disable connect_timeout=5",
		cfg.PGPort, dsnQuote(cfg.PostgresUser), dsnQuote(cfg.PostgresPassword),
	)
	pool, err := pgxpool.New(context.Background(), connString)
	if err != nil {
		return nil, fmt.Errorf("creating pgx pool: %w", err)
	}
	return pool, nil
}

// dsnQuote quotes a value for libpq's keyword/value connection string
// format, so a password containing spaces or quotes doesn't break parsing.
func dsnQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

func isInRecovery(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var inRecovery bool
	if err := pool.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		return false, fmt.Errorf("querying pg_is_in_recovery: %w", err)
	}
	return inRecovery, nil
}

func serverVersion(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var version string
	if err := pool.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		return "", fmt.Errorf("querying server_version: %w", err)
	}
	return version, nil
}

// replicationLagBytes: on a primary, the largest lag across all connected
// standbys (0 if none connected); on a standby, how far behind the primary
// it currently is.
func replicationLagBytes(ctx context.Context, pool *pgxpool.Pool, inRecovery bool) (int64, error) {
	sql := `SELECT COALESCE(MAX(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)), 0)::bigint FROM pg_stat_replication`
	if inRecovery {
		sql = `SELECT COALESCE(pg_wal_lsn_diff(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn()), 0)::bigint`
	}
	var lag int64
	if err := pool.QueryRow(ctx, sql).Scan(&lag); err != nil {
		return 0, fmt.Errorf("querying replication lag: %w", err)
	}
	if lag < 0 {
		// Briefly possible right after a standby starts: pg_last_wal_receive_lsn()
		// doesn't update until the first streamed chunk arrives, while
		// pg_last_wal_replay_lsn() can already reflect WAL replayed locally
		// from the base backup/rewind's copied segments -- not real lag.
		return 0, nil
	}
	return lag, nil
}

// replicationConnectedCount is how many standbys are currently streaming
// from this node (0 on a standby, or on a primary with none connected).
func replicationConnectedCount(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_replication").Scan(&count); err != nil {
		return 0, fmt.Errorf("querying pg_stat_replication: %w", err)
	}
	return count, nil
}

// replicationConnected reports whether replication is currently active.
// pg_stat_replication is only populated on a primary (rows = connected
// standbys); pg_stat_wal_receiver is only populated on a standby (one row
// if actively receiving WAL) -- inRecovery picks which side to check.
func replicationConnected(ctx context.Context, pool *pgxpool.Pool, inRecovery bool) (bool, error) {
	if inRecovery {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_wal_receiver").Scan(&count); err != nil {
			return false, fmt.Errorf("querying pg_stat_wal_receiver: %w", err)
		}
		return count > 0, nil
	}
	count, err := replicationConnectedCount(ctx, pool)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func connectionCount(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity").Scan(&count); err != nil {
		return 0, fmt.Errorf("querying pg_stat_activity: %w", err)
	}
	return count, nil
}

func databaseSizeBytes(ctx context.Context, pool *pgxpool.Pool, dbName string) (int64, error) {
	var size int64
	if err := pool.QueryRow(ctx, "SELECT pg_database_size($1)", dbName).Scan(&size); err != nil {
		return 0, fmt.Errorf("querying database size: %w", err)
	}
	return size, nil
}

// promote calls Postgres's own pg_promote() (PG12+) -- a SQL function, no
// pg_ctl promote needed. Postgres itself no-ops safely (returns false) if
// already primary; this deliberately doesn't duplicate that check.
func promote(ctx context.Context, pool *pgxpool.Pool) error {
	var promoted bool
	if err := pool.QueryRow(ctx, "SELECT pg_promote()").Scan(&promoted); err != nil {
		return fmt.Errorf("calling pg_promote: %w", err)
	}
	if !promoted {
		return fmt.Errorf("pg_promote() returned false (already primary, or promotion did not complete)")
	}
	return nil
}
