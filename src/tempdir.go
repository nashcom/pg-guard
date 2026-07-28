// pg-guard -- tempdir.go -- one shared staging root for everything
// pg-guard itself writes to disk that isn't Postgres's own data: TLS
// cert/key/CA copies (tls.go's stageTLSFile) and the initdb password file
// (bootstrap.go's bootstrapAsPrimary). Each gets its own named
// subdirectory under a common pg-guard root (os.TempDir()/pg-guard/<name>,
// e.g. "tls", "bootstrap") instead of scattered, differently-named
// locations -- makes it obvious at a glance what's pg-guard's own vs.
// Postgres's, and means a single tmpfs mount at <TempDir>/pg-guard (or
// TempDir itself) covers all of it at once, so none of it needs to touch
// real disk if the deployment wants that.

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// pgGuardStagingDir returns os.TempDir()/pg-guard/<name>, creating it
// (and any missing parents) if it doesn't already exist.
func pgGuardStagingDir(name string) (string, error) {
	dir := filepath.Join(os.TempDir(), "pg-guard", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	return dir, nil
}
