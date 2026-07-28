// pg-guard -- textfile.go -- optional textfile-collector mode: periodically
// writes the exact same output collectMetrics (metrics.go) already
// produces for GET /metrics to a file (pg-guard.prom) in
// PG_GUARD_TEXTFILE_DIR, in node_exporter's own textfile-collector format,
// for node_exporter's --collector.textfile.directory to pick up and merge
// into its own scrape. Useful on a plain Docker host running a single
// OS-level node_exporter that should combine host and Postgres metrics
// into one scrape target -- an alternative, not a replacement, to scraping
// GET /metrics directly (the natural fit for Kubernetes, where each pod is
// already its own scrape target). Which of the two is active is one
// setting, PG_GUARD_METRICS_MODE (endpoint|textfile|both, see config.go) --
// most deployments want exactly one, not both; "both" stays available for
// the less common case that wants it. Either way, both are backed by the
// one collectMetrics call; this file adds no second metrics
// implementation, only a periodic writer around the one that already
// exists.
//
// Written via a temp file + rename, not a direct write: node_exporter (or
// anything else) reading pg-guard.prom mid-write would otherwise risk
// seeing a truncated/partial scrape. os.Rename is atomic within the same
// directory on both platforms this project supports (a same-volume
// rename -- POSIX rename(2) on Linux, MoveFileEx with
// MOVEFILE_REPLACE_EXISTING on Windows), so a reader only ever sees the
// previous complete file or the new complete file, never something
// in-between.

package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	textfileName     = "pg-guard.prom"
	textfileTempName = "pg-guard.temp"
)

// startTextfileWriter starts the periodic textfile-collector writer unless
// PG_GUARD_METRICS_MODE is "endpoint" (the default) -- loadConfig has
// already validated PG_GUARD_TEXTFILE_DIR is set whenever TextfileEnabled
// is true, so no further check is needed here. Runs for the lifetime of
// the process.
func startTextfileWriter(cfg *Config, pool *pgxpool.Pool) {
	if !cfg.TextfileEnabled {
		return
	}

	go func() {
		// Written once immediately, not just on the first tick -- a
		// monitoring file with no content at all until the first interval
		// elapses would otherwise leave node_exporter merging in nothing
		// for that whole window right after startup.
		writeTextfile(cfg, pool)

		ticker := time.NewTicker(cfg.TextfileInterval)
		defer ticker.Stop()
		for range ticker.C {
			writeTextfile(cfg, pool)
		}
	}()

	logInfo("textfile collector started (dir=%s, interval=%s)", cfg.TextfileDir, cfg.TextfileInterval)
}

// writeTextfile runs the same collectMetrics call GET /metrics uses and
// writes it to PG_GUARD_TEXTFILE_DIR/pg-guard.prom via a temp file +
// rename. Errors are logged, not fatal -- a write failure (directory
// vanished, permissions, disk full) shouldn't affect Postgres supervision;
// the next tick just tries again.
func writeTextfile(cfg *Config, pool *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(context.Background(), metricsCollectionTimeout)
	body := collectMetrics(ctx, pool, cfg)
	cancel()

	// os.WriteFile does not create parent directories -- unlike
	// PG_GUARD_BACKUP_DIR (runBackup, backup.go), which an operator might
	// reasonably pre-create for permissions/ownership reasons before ever
	// triggering a backup, this directory needs to exist before the very
	// first tick with nothing else having had a chance to create it.
	// World-readable/-traversable (0o755), not backup's 0o700 -- the whole
	// point is an external process (node_exporter, likely a different OS
	// user) reading this file.
	if err := os.MkdirAll(cfg.TextfileDir, 0o755); err != nil {
		logWarn("textfile collector: creating %s: %v", cfg.TextfileDir, err)
		return
	}

	tempPath := filepath.Join(cfg.TextfileDir, textfileTempName)
	finalPath := filepath.Join(cfg.TextfileDir, textfileName)

	if err := os.WriteFile(tempPath, []byte(body), 0o644); err != nil {
		logWarn("textfile collector: writing %s: %v", tempPath, err)
		return
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		logWarn("textfile collector: renaming %s to %s: %v", tempPath, finalPath, err)
	}
}
