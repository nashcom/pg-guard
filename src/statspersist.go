// pg-guard -- statspersist.go -- optionally persists a small set of
// observability stats (pg-guard start count, cumulative postgres restart
// count, last crash timestamp) to PG_GUARD_STATS_FILE, so they survive a
// pg-guard restart instead of resetting to 0 like every other in-memory
// counter in this codebase. Deliberately does NOT persist the crash-loop
// rolling window itself (main.go's crashTimestamps) -- that enforcement
// stays in-memory and unchanged; only these display stats carry forward.
// This is diagnostic data, not correctness-critical: a missing or corrupt
// file just starts the counters at 0 again, never fails startup.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type persistedStats struct {
	PgGuardStarts     int64 `json:"pg_guard_starts"`
	PostgresRestarts  int64 `json:"postgres_restarts"`
	LastCrashUnixNano int64 `json:"last_crash_unix_nano"`
}

// initPersistentStats seeds the in-memory counters from cfg.StatsFile (if
// set), then records this startup as a new pg-guard start and writes that
// back immediately -- durably counted even if pg-guard crashes moments
// later. No-op if PG_GUARD_STATS_FILE is unset.
func initPersistentStats(cfg *Config) {
	if cfg.StatsFile == "" {
		return
	}

	loaded, err := loadPersistedStats(cfg.StatsFile)
	if err != nil {
		logWarn("stats file: %v -- starting from 0", err)
	}

	postgresRestartsCounter.Store(loaded.PostgresRestarts)
	lastPostgresCrashUnixNano.Store(loaded.LastCrashUnixNano)
	pgGuardStartsCounter.Store(loaded.PgGuardStarts)
	incrementPgGuardStarts()

	persistStats(cfg)
}

// loadPersistedStats reads and parses cfg.StatsFile. A missing file (fresh
// volume, first run) is not an error -- returns a zero-value persistedStats
// and nil. Any other error (unreadable, corrupt JSON) also returns a
// zero-value persistedStats, but with the error, for the caller to log.
func loadPersistedStats(path string) (persistedStats, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return persistedStats{}, nil
	}
	if err != nil {
		return persistedStats{}, err
	}

	var s persistedStats
	if err := json.Unmarshal(data, &s); err != nil {
		return persistedStats{}, err
	}
	return s, nil
}

// persistStats snapshots the current counters and writes them to
// cfg.StatsFile. No-op if unset. Write failures are logged and otherwise
// swallowed -- a failed stat write must never block or crash postgres
// supervision.
func persistStats(cfg *Config) {
	if cfg.StatsFile == "" {
		return
	}

	s := persistedStats{
		PgGuardStarts:     pgGuardStartsTotal(),
		PostgresRestarts:  postgresRestartsTotal(),
		LastCrashUnixNano: lastPostgresCrashUnixNano.Load(),
	}

	if err := savePersistedStats(cfg.StatsFile, s); err != nil {
		logWarn("stats file: %v", err)
	}
}

// savePersistedStats writes atomically: marshal, write to a temp file in
// the same directory, then rename over the target -- a crash mid-write
// (a real possibility here specifically, since this is often called in
// immediate response to one) can never leave a truncated/corrupt file in
// place of a good one. Creates the parent directory if missing, same as
// the fix textfile.go needed for the same reason (see its own history).
func savePersistedStats(path string, s persistedStats) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.Marshal(s)
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
