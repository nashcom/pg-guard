// pg-guard -- rewind.go -- rejoin orchestration, matching README's Rejoin
// diagram: try pg_rewind first, fall back to a full pg_basebackup re-clone.
// Both shell out to the official binaries (per README's Command Execution
// vs. Direct Connection table) -- never reimplemented.

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// pgTool derives a sibling binary's path from PostgresBin (all of
// postgres/pg_rewind/pg_basebackup/pg_ctl ship together in the same bin/
// directory), reusing whatever extension PostgresBin has (".exe" on
// Windows, none on Linux) -- same derivation stop_windows.go already uses
// for pg_ctl, generalized here since rewind.go is cross-platform.
func pgTool(cfg *Config, name string) string {
	return filepath.Join(filepath.Dir(cfg.PostgresBin), name+filepath.Ext(cfg.PostgresBin))
}

// rejoinAsStandby makes the local, currently-stopped PGDATA a standby of
// the peer: pg_rewind if possible, a full pg_basebackup re-clone if not.
// Called before the first supervisor.start() at startup (see main.go) when
// the peer is primary and we're not already a standby, and by
// POST /api/rejoin.
func rejoinAsStandby(cfg *Config) error {
	rejoinStart := time.Now()

	if err := runRewind(cfg); err != nil {
		logWarn("rejoin: pg_rewind failed (%v) -- falling back to pg_basebackup (full re-clone)", err)
		if err := runBasebackup(cfg); err != nil {
			return fmt.Errorf("pg_basebackup fallback also failed: %w", err)
		}
		logInfo("rejoin: pg_basebackup re-clone from %s succeeded", cfg.PeerHost)
		recordRejoinDuration(time.Since(rejoinStart))
		incrementRejoins()
		return nil // pg_basebackup -R already wrote standby.signal + primary_conninfo
	}

	logInfo("rejoin: pg_rewind against %s succeeded", cfg.PeerHost)
	// Unlike pg_basebackup -R, pg_rewind doesn't create standby.signal or
	// set primary_conninfo -- do that manually.
	if err := configureAsStandby(cfg); err != nil {
		return fmt.Errorf("configuring standby after pg_rewind: %w", err)
	}
	recordRejoinDuration(time.Since(rejoinStart))
	incrementRejoins()
	return nil
}

func runRewind(cfg *Config) error {
	source := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable connect_timeout=5",
		cfg.PeerHost, cfg.PGPort, cfg.ReplUser, cfg.ReplPassword, cfg.PostgresDB,
	)
	cmd := exec.Command(pgTool(cfg, "pg_rewind"),
		"--target-pgdata="+cfg.PGData, "--source-server="+source, "--progress")
	if out, err := runLoggedCommand("pg_rewind", cmd); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// runBasebackup re-clones PGData from the peer via pg_basebackup -- used
// both by rejoin's fallback (an existing, diverged PGData that pg_rewind
// couldn't fix) and by bootstrapAsStandby (bootstrap.go, PGData may not
// exist at all yet). refuseIfPostgresRunning guards the wipe below: this
// must never run against a directory a live postgres still has open.
func runBasebackup(cfg *Config) error {
	if err := refuseIfPostgresRunning(cfg); err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.PGData, 0o700); err != nil {
		return fmt.Errorf("creating PGDATA: %w", err)
	}
	// pg_basebackup refuses to write into a non-empty directory -- clear
	// anything left over from a previous, now-diverged install (a fresh
	// bootstrap target is already empty, so this is normally a no-op there).
	entries, err := os.ReadDir(cfg.PGData)
	if err != nil {
		return fmt.Errorf("reading PGDATA to clear it: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(cfg.PGData, e.Name())); err != nil {
			return fmt.Errorf("clearing PGDATA: %w", err)
		}
	}

	cmd := exec.Command(pgTool(cfg, "pg_basebackup"),
		"-h", cfg.PeerHost, "-p", strconv.Itoa(cfg.PGPort), "-U", cfg.ReplUser,
		"-D", cfg.PGData, "-P", "-R", "-X", "stream")
	cmd.Env = append(os.Environ(), "PGPASSWORD="+cfg.ReplPassword)
	if out, err := runLoggedCommand("pg_basebackup", cmd); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// refuseIfPostgresRunning is a safety net before ever wiping PGData: if
// something is still listening on the local Postgres port, refuse rather
// than risk deleting a live data directory out from under a running
// postgres, which self-terminates abruptly ("immediate shutdown because
// data directory lock file is invalid") instead of anything graceful.
func refuseIfPostgresRunning(cfg *Config) error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.PGPort)), 500*time.Millisecond)
	if err != nil {
		return nil // nothing listening -- safe to proceed
	}
	conn.Close()
	return fmt.Errorf("refusing to wipe PGDATA: something is still listening on 127.0.0.1:%d", cfg.PGPort)
}

// configureAsStandby writes standby.signal and appends primary_conninfo to
// postgresql.auto.conf -- what pg_basebackup -R does automatically, and
// pg_rewind doesn't.
func configureAsStandby(cfg *Config) error {
	signalPath := filepath.Join(cfg.PGData, "standby.signal")
	if err := os.WriteFile(signalPath, nil, 0o600); err != nil {
		return fmt.Errorf("creating standby.signal: %w", err)
	}

	connInfo := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		cfg.PeerHost, cfg.PGPort, cfg.ReplUser, cfg.ReplPassword, cfg.PostgresDB,
	)
	autoConfPath := filepath.Join(cfg.PGData, "postgresql.auto.conf")
	f, err := os.OpenFile(autoConfPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening postgresql.auto.conf: %w", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "\nprimary_conninfo = %s\n", dsnQuote(connInfo)); err != nil {
		return fmt.Errorf("writing primary_conninfo: %w", err)
	}
	return nil
}
