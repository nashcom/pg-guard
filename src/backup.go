// pg-guard -- backup.go -- basic backup orchestration: pg_basebackup against
// the local instance, either into PG_GUARD_BACKUP_DIR (simple count-based
// retention, PG_GUARD_BACKUP_RETAIN) or piped into PG_GUARD_BACKUP_COMMAND
// (any external tool -- borgbackup, restic, a tar+upload script -- no
// retention, mutually exclusive with PG_GUARD_BACKUP_DIR), triggered on a
// schedule (PG_GUARD_BACKUP_ENABLED/PG_GUARD_BACKUP_INTERVAL) and/or on
// demand (POST /api/backup, "pg-guard backup"). Always runs against whichever node
// is currently primary -- no standby-preferred logic, no cross-node
// coordination, matching README's Design Goals ("leverage the standard
// tools, never reimplement Postgres functionality") and this project's
// general preference for the simplest thing that actually works.
//
// Deliberately does NOT do WAL archiving/continuous PITR or automated
// restore -- both are a different, much bigger class of problem (that's
// what dedicated tools like pgBackRest/WAL-G exist for). Restore stays a
// manual, documented procedure (see README's Backup section): untar into a
// fresh PGDATA and start pg-guard normally against it.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// errBackupNotPrimary/errBackupInProgress are sentinel errors distinguished
// by callers (handleBackup, api.go; the scheduler loop below) from a real
// backup failure -- neither is a fault worth logging as an error or
// counting in postgres_ha_backup_failures_total.
var (
	errBackupNotPrimary = errors.New("backup must be run on the primary")
	errBackupInProgress = errors.New("a backup is already in progress")
)

// backupTimestampFormat names each backup's destination subdirectory --
// lexically sortable (so pruneOldBackups can just sort.Strings), and
// pruneOldBackups only ever touches entries matching backupDirNamePattern,
// so anything else an operator drops into BackupDir is left alone.
const backupTimestampFormat = "20060102T150405Z"

var backupDirNamePattern = regexp.MustCompile(`^\d{8}T\d{6}Z$`)

// performBackup is the shared entry point for the on-demand trigger
// (handleBackup, api.go), the scheduler below, and the CLI's default
// "pg-guard backup" (via POST /api/backup, same as promote/rejoin/etc.):
// checks the local role (backups only ever run on primary -- see this
// file's top comment), then delegates to whichever destination is
// configured -- runBackup (PG_GUARD_BACKUP_DIR, retention-pruned
// afterward) or runBackupPipe (PG_GUARD_BACKUP_COMMAND, no retention --
// see Config's doc comment on why the two are mutually exclusive) --
// before recording metrics either way. Returns a human-readable
// description of where the backup went and how long it took.
func performBackup(cfg *Config, pool *pgxpool.Pool) (string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), roleCheckTimeout)
	inRecovery, err := isInRecovery(ctx, pool)
	cancel()
	if err != nil {
		return "", 0, fmt.Errorf("checking local role: %w", err)
	}
	if inRecovery {
		return "", 0, errBackupNotPrimary
	}

	start := time.Now()
	var dest string
	if cfg.BackupCommand != "" {
		err = runBackupPipe(cfg)
		dest = "(piped to PG_GUARD_BACKUP_COMMAND)"
	} else {
		dest, err = runBackup(cfg)
	}
	if err != nil {
		if !errors.Is(err, errBackupInProgress) {
			incrementBackupFailures()
			recordBackupAttempt(err)
		}
		return "", 0, err
	}
	duration := time.Since(start)
	recordBackupSuccess(duration)
	recordBackupAttempt(nil)
	incrementBackups()

	if cfg.BackupCommand == "" {
		if err := pruneOldBackups(cfg); err != nil {
			// Non-fatal -- the backup itself already succeeded and is
			// safely on disk; a pruning failure just means one more old
			// backup sticks around than intended, worth logging but not
			// worth reporting the whole operation as failed.
			logWarn("backup: pruning old backups: %v", err)
		}
	}
	return dest, duration, nil
}

// runBackup shells out to pg_basebackup (via the same pgTool sibling-binary
// derivation rewind.go uses) against the local instance, writing a
// compressed tar archive to a fresh timestamped subdirectory of
// cfg.BackupDir. Tar+gzip (-F tar -z), not plain directory format:
// self-contained per backup (a single os.RemoveAll prunes one cleanly) and
// far less disk than an uncompressed full copy -- a deliberate "basic"
// default, not configurable in this milestone.
//
// Guarded by backupInProgress (state.go) so a scheduled tick can never
// overlap an on-demand trigger, or itself.
func runBackup(cfg *Config) (string, error) {
	if !backupInProgress.CompareAndSwap(false, true) {
		return "", errBackupInProgress
	}
	defer backupInProgress.Store(false)

	if cfg.BackupDir == "" {
		return "", fmt.Errorf("PG_GUARD_BACKUP_DIR is not set")
	}
	if err := os.MkdirAll(cfg.BackupDir, 0o700); err != nil {
		return "", fmt.Errorf("creating backup directory: %w", err)
	}

	dest := filepath.Join(cfg.BackupDir, time.Now().UTC().Format(backupTimestampFormat))
	cmd := exec.Command(pgTool(cfg, "pg_basebackup"),
		"-h", "127.0.0.1", "-p", strconv.Itoa(cfg.PGPort), "-U", cfg.ReplUser,
		"-D", dest, "-F", "tar", "-z", "-X", "stream", "-c", "fast", "-P")
	cmd.Env = append(os.Environ(), "PGPASSWORD="+cfg.ReplPassword)
	if out, err := runLoggedCommand("pg_basebackup-backup", cmd); err != nil {
		return "", fmt.Errorf("%w: %s", err, out)
	}
	return dest, nil
}

// runBackupPipe is runBackup's counterpart for PG_GUARD_BACKUP_COMMAND:
// runs pg_basebackup with its tar stream (stdout) connected directly to
// cfg.BackupCommand's stdin -- the same shell-pipeline shape as
// "pg_basebackup ... | borg create ::archive -" or "... | gzip > file",
// except pg-guard runs both ends itself so it works uniformly from the
// scheduler and POST /api/backup, not just an interactive CLI. Run through
// a shell (sh -c / cmd /C, per platform) so the configured command can use
// ordinary shell syntax (redirection, further pipes, command substitution
// for a timestamped destination) -- cfg.BackupCommand is trusted admin
// configuration, the same trust level as PG_GUARD_EXTRA_ARGS.
//
// Guarded by backupInProgress, same as runBackup -- the two destinations
// are mutually exclusive per node (see Config's doc comment), but the
// guard still matters against two triggers of this same path overlapping.
func runBackupPipe(cfg *Config) error {
	if !backupInProgress.CompareAndSwap(false, true) {
		return errBackupInProgress
	}
	defer backupInProgress.Store(false)

	pgCmd := exec.Command(pgTool(cfg, "pg_basebackup"),
		"-h", "127.0.0.1", "-p", strconv.Itoa(cfg.PGPort), "-U", cfg.ReplUser,
		"-D", "-", "-F", "tar", "-z", "-X", "stream", "-c", "fast")
	pgCmd.Env = append(os.Environ(), "PGPASSWORD="+cfg.ReplPassword)

	shell, shellFlag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, shellFlag = "cmd", "/C"
	}
	destCmd := exec.Command(shell, shellFlag, cfg.BackupCommand)

	stdout, err := pgCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("wiring pg_basebackup output: %w", err)
	}
	destCmd.Stdin = stdout

	var pgErrBuf, destErrBuf bytes.Buffer
	pgCmd.Stderr = &pgErrBuf
	destCmd.Stderr = &destErrBuf

	if err := destCmd.Start(); err != nil {
		return fmt.Errorf("starting PG_GUARD_BACKUP_COMMAND (%q): %w", cfg.BackupCommand, err)
	}
	if err := pgCmd.Start(); err != nil {
		_ = destCmd.Process.Kill()
		_ = destCmd.Wait()
		return fmt.Errorf("starting pg_basebackup: %w", err)
	}

	pgErr := pgCmd.Wait()
	destErr := destCmd.Wait()

	if pgErr != nil {
		return fmt.Errorf("pg_basebackup: %w: %s", pgErr, pgErrBuf.String())
	}
	if destErr != nil {
		return fmt.Errorf("PG_GUARD_BACKUP_COMMAND (%q): %w: %s", cfg.BackupCommand, destErr, destErrBuf.String())
	}
	return nil
}

// pruneOldBackups keeps the cfg.BackupRetain newest entries under
// cfg.BackupDir matching backupDirNamePattern and removes the rest. Simple
// count-based retention, not time-tiered/GFS -- matches this feature's
// "basic" scope.
func pruneOldBackups(cfg *Config) error {
	entries, err := os.ReadDir(cfg.BackupDir)
	if err != nil {
		return fmt.Errorf("reading backup directory: %w", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() && backupDirNamePattern.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if len(names) <= cfg.BackupRetain {
		return nil
	}

	sort.Strings(names) // backupTimestampFormat sorts lexically == chronologically
	for _, name := range names[:len(names)-cfg.BackupRetain] {
		path := filepath.Join(cfg.BackupDir, name)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("removing old backup %s: %w", path, err)
		}
		logInfo("backup: pruned old backup %s (retaining %d newest)", path, cfg.BackupRetain)
	}
	return nil
}

// startBackupScheduler starts the periodic backup loop unless
// PG_GUARD_BACKUP_ENABLED is false -- the on-demand trigger (handleBackup,
// "pg-guard backup") works regardless of this setting. Runs for the
// lifetime of the process; each tick that finds the local node isn't
// primary right now just skips quietly (expected -- see this file's top
// comment on why backups never run against a standby), not logged as a
// failure.
func startBackupScheduler(cfg *Config, pool *pgxpool.Pool) {
	if !cfg.BackupEnabled {
		logInfo("backup scheduler disabled (PG_GUARD_BACKUP_ENABLED=false) -- on-demand backup still available")
		return
	}

	go func() {
		ticker := time.NewTicker(cfg.BackupInterval)
		defer ticker.Stop()

		for range ticker.C {
			if !postgresRunning.Load() {
				continue
			}
			dest, duration, err := performBackup(cfg, pool)
			switch {
			case errors.Is(err, errBackupNotPrimary):
				logDebug("backup: local node is not primary -- skipping scheduled backup")
			case errors.Is(err, errBackupInProgress):
				logWarn("backup: a backup was already running -- skipping this scheduled tick")
			case err != nil:
				logError("backup: scheduled backup failed: %v", err)
			default:
				logInfo("backup: scheduled backup complete (%s, took %s)", dest, duration.Round(time.Second))
			}
		}
	}()

	logInfo("backup scheduler started (interval=%s, retain=%d, dir=%s)", cfg.BackupInterval, cfg.BackupRetain, cfg.BackupDir)
}

// runBackupToStdout implements "pg-guard backup -stdout": runs
// pg_basebackup directly from this CLI invocation with its tar output wired
// straight to os.Stdout (pg_basebackup's own "-D -" convention), letting an
// operator pipe a backup to any destination pg-guard has no built-in
// knowledge of (object storage, a remote host over ssh, ...). Deliberately
// separate from performBackup/runBackup: this never touches
// PG_GUARD_BACKUP_DIR, isn't retention-pruned, and -- since it runs in its
// own short-lived process, not the long-running supervisor -- doesn't
// update postgres_ha_backup_* metrics or backupInProgress; those stay tied
// to the disk-based path (scheduled ticks and POST /api/backup), which is
// what "did a backup run recently" alerting should watch. Avoid running
// this at the same time as a scheduled/on-demand disk backup -- both hit
// the same live instance, and nothing here coordinates with the other.
//
// Same primary-only policy as the disk-based path (see this file's top
// comment): checked via a one-off connection since this process has no
// pool of its own.
func runBackupToStdout(cfg *Config) error {
	if err := requireLocalPrimary(cfg); err != nil {
		return err
	}

	cmd := exec.Command(pgTool(cfg, "pg_basebackup"),
		"-h", "127.0.0.1", "-p", strconv.Itoa(cfg.PGPort), "-U", cfg.ReplUser,
		"-D", "-", "-F", "tar", "-z", "-X", "stream", "-c", "fast")
	cmd.Env = append(os.Environ(), "PGPASSWORD="+cfg.ReplPassword)
	// Raw passthrough, not runLoggedCommand: stdout here is the actual tar
	// archive the operator is piping elsewhere, not log output -- it must
	// stay exactly what pg_basebackup writes, with nothing else mixed in.
	// pg_basebackup's own progress/diagnostic output already goes to
	// stderr (-P), kept separate.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_basebackup: %w", err)
	}
	return nil
}

// requireLocalPrimary opens a one-off connection (this file's callers all
// run outside the supervisor's own long-lived pool -- runBackupToStdout is
// CLI-only, performBackup is given a pool by its caller instead) and
// returns errBackupNotPrimary if the local node is currently a standby.
func requireLocalPrimary(cfg *Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), roleCheckTimeout)
	defer cancel()

	connString := fmt.Sprintf(
		"host=127.0.0.1 port=%d user=%s password=%s dbname=postgres sslmode=disable connect_timeout=5",
		cfg.PGPort, dsnQuote(cfg.PostgresUser), dsnQuote(cfg.PostgresPassword),
	)
	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		return fmt.Errorf("connecting to check local role: %w", err)
	}
	defer conn.Close(context.Background())

	var inRecovery bool
	if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		return fmt.Errorf("checking local role: %w", err)
	}
	if inRecovery {
		return errBackupNotPrimary
	}
	return nil
}
