// pg-guard -- statepersist.go -- optionally persists a small set of
// observability stats (pg-guard start count, cumulative postgres restart
// count, last crash timestamp, cumulative backup counts, last backup
// timestamp/duration) to PG_GUARD_STATE_FILE, so they survive a pg-guard
// restart instead of resetting to 0 like every other in-memory counter in
// this codebase. Deliberately does NOT persist the crash-loop rolling
// window itself (main.go's crashTimestamps) or the backup last-attempt
// status/error (backupLastAttempt* in metrics.go) -- both stay in-memory
// and unchanged: the former is enforcement logic, not a display stat; the
// latter is "is backup broken right now," which naturally refreshes on the
// next attempt regardless, and the error text could carry more detail than
// belongs in a plain file on disk. Only display stats carry forward. This
// is diagnostic data, not correctness-critical: a missing or corrupt file
// just starts the counters at 0 again, never fails startup.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type persistedState struct {
	PgGuardStarts          int64 `json:"pg_guard_starts"`
	PostgresRestarts       int64 `json:"postgres_restarts"`
	LastCrashUnixNano      int64 `json:"last_crash_unix_nano"`
	BackupsTotal           int64 `json:"backups_total"`
	BackupFailuresTotal    int64 `json:"backup_failures_total"`
	LastBackupUnixNano     int64 `json:"last_backup_unix_nano"`
	LastBackupDurationNano int64 `json:"last_backup_duration_nano"`
}

// initPersistentState seeds the in-memory counters from cfg.StateFile (if
// set), then records this startup as a new pg-guard start and writes that
// back immediately -- durably counted even if pg-guard crashes moments
// later. No-op if PG_GUARD_STATE_FILE is unset.
func initPersistentState(cfg *Config) {
	if cfg.StateFile == "" {
		return
	}

	loaded, err := loadPersistedState(cfg.StateFile)
	if err != nil {
		logWarn("state file: %v -- starting from 0", err)
	}

	postgresRestartsCounter.Store(loaded.PostgresRestarts)
	lastPostgresCrashUnixNano.Store(loaded.LastCrashUnixNano)
	pgGuardStartsCounter.Store(loaded.PgGuardStarts)
	incrementPgGuardStarts()
	backupsCounter.Store(loaded.BackupsTotal)
	backupFailuresCounter.Store(loaded.BackupFailuresTotal)
	lastBackupUnixNano.Store(loaded.LastBackupUnixNano)
	lastBackupDuration.Store(loaded.LastBackupDurationNano)

	persistState(cfg)
}

// loadPersistedState reads and parses cfg.StateFile. A missing file (fresh
// volume, first run) is not an error -- returns a zero-value persistedState
// and nil. Any other error (unreadable, corrupt JSON) also returns a
// zero-value persistedState, but with the error, for the caller to log.
func loadPersistedState(path string) (persistedState, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return persistedState{}, nil
	}
	if err != nil {
		return persistedState{}, err
	}

	var s persistedState
	if err := json.Unmarshal(data, &s); err != nil {
		return persistedState{}, err
	}
	return s, nil
}

// persistState snapshots the current counters and writes them to
// cfg.StateFile. No-op if unset. Write failures are logged and otherwise
// swallowed -- a failed state write must never block or crash postgres
// supervision.
func persistState(cfg *Config) {
	if cfg.StateFile == "" {
		return
	}

	s := persistedState{
		PgGuardStarts:          pgGuardStartsTotal(),
		PostgresRestarts:       postgresRestartsTotal(),
		LastCrashUnixNano:      lastPostgresCrashUnixNano.Load(),
		BackupsTotal:           backupsTotal(),
		BackupFailuresTotal:    backupFailuresTotal(),
		LastBackupUnixNano:     lastBackupUnixNano.Load(),
		LastBackupDurationNano: lastBackupDuration.Load(),
	}

	if err := savePersistedState(cfg.StateFile, s); err != nil {
		logWarn("state file: %v", err)
	}
}

// savePersistedState writes atomically: marshal, write to a temp file in
// the same directory, then rename over the target -- a crash mid-write
// (a real possibility here specifically, since this is often called in
// immediate response to one) can never leave a truncated/corrupt file in
// place of a good one. Creates the parent directory if missing, same as
// the fix textfile.go needed for the same reason (see its own history).
func savePersistedState(path string, s persistedState) error {
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
