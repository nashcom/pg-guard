//go:build linux

// detect_linux.go auto-detects the postgres binary when
// PG_GUARD_POSTGRES_BIN isn't set explicitly: the Debian/Ubuntu convention
// (/usr/lib/postgresql/<version>/bin/postgres -- confirmed against the
// official postgres:17 image) plus a PATH lookup, combined into one
// candidate set. Like detect_windows.go, this fails loud on ambiguity
// (multiple major versions installed) rather than silently guessing which
// one -- PG major versions are data-incompatible, so picking wrong is a
// real footgun, not a convenience.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func detectPostgresBin() (string, error) {
	seen := map[string]bool{}
	var found []string

	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		if _, err := os.Stat(path); err == nil {
			seen[path] = true
			found = append(found, path)
		}
	}

	matches, _ := filepath.Glob("/usr/lib/postgresql/*/bin/postgres")
	for _, m := range matches {
		add(m)
	}

	if p, err := exec.LookPath("postgres"); err == nil {
		add(p)
	}

	switch len(found) {
	case 0:
		return "", fmt.Errorf("no postgres binary found under /usr/lib/postgresql/*/bin or on PATH")
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("multiple PostgreSQL installations found (%v) -- set PG_GUARD_POSTGRES_BIN explicitly to disambiguate", found)
	}
}
