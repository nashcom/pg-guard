//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// stopGracefullyPlatform on Windows shells out to pg_ctl stop -m fast.
// os.Process.Signal() only implements os.Kill on Windows, so there is no
// real SIGTERM/SIGINT delivery to an arbitrary child process here -- pg_ctl
// is the only correct way to stop postgres.exe cleanly. pg_ctl.exe is
// assumed to be a sibling of the supervised entrypoint (both always ship
// together in Postgres's bin/ directory).
func (s *supervisor) stopGracefullyPlatform(cfg *Config, sig os.Signal) error {
	if cfg.PGData == "" {
		return fmt.Errorf("cannot stop gracefully: PGDATA is not set")
	}

	pgCtl := filepath.Join(filepath.Dir(cfg.PostgresBin), "pg_ctl.exe")
	logInfo("stopping via %s -D %s -m fast", pgCtl, cfg.PGData)

	out, err := exec.Command(pgCtl, "stop", "-D", cfg.PGData, "-m", "fast").CombinedOutput()
	if err != nil {
		return fmt.Errorf("pg_ctl stop failed: %w (%s)", err, out)
	}
	return nil
}
