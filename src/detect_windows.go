//go:build windows

// detect_windows.go auto-detects the postgres binary when
// PG_GUARD_POSTGRES_BIN isn't set explicitly, via the registry key the
// official Windows installer writes for every installation
// (HKLM\SOFTWARE\PostgreSQL\Installations\<name>, "Base Directory" value --
// confirmed present on a real PG18 install: "C:\Program Files\PostgreSQL\18").
// Mirrors service_install_windows.go's "reuse the platform's own mechanism"
// principle -- no guessing, no hardcoded version-specific path.

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

const installationsKey = `SOFTWARE\PostgreSQL\Installations`

func detectPostgresBin() (string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, installationsKey, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return "", fmt.Errorf("no PostgreSQL installation found (HKLM\\%s not present): %w", installationsKey, err)
	}
	defer key.Close()

	names, err := key.ReadSubKeyNames(-1)
	if err != nil || len(names) == 0 {
		return "", fmt.Errorf("no PostgreSQL installations registered under HKLM\\%s", installationsKey)
	}

	var found []string
	for _, name := range names {
		sub, err := registry.OpenKey(registry.LOCAL_MACHINE, installationsKey+`\`+name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		base, _, err := sub.GetStringValue("Base Directory")
		sub.Close()
		if err != nil || base == "" {
			continue
		}
		bin := filepath.Join(base, "bin", "postgres.exe")
		if _, err := os.Stat(bin); err == nil {
			found = append(found, bin)
		}
	}

	switch len(found) {
	case 0:
		return "", fmt.Errorf("no usable postgres.exe found under any registered installation's Base Directory")
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("multiple PostgreSQL installations found (%v) -- set PG_GUARD_POSTGRES_BIN explicitly to disambiguate", found)
	}
}
