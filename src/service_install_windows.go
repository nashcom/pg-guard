//go:build windows

// service_install_windows.go registers/unregisters pg-guard with the
// Windows Service Control Manager. Runs as NT AUTHORITY\NetworkService --
// the same non-admin built-in account the official Postgres installer's
// own service already uses -- so its child postgres.exe inherits a non-admin
// token automatically, satisfying Postgres's "must not run as an
// administrator" restriction with no privilege-drop code in pg-guard
// itself. This is the one operation that legitimately needs an elevated
// prompt: registering with SCM is inherently privileged.

package main

import (
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceName = "pg-guard"

// serviceEnvVars are copied from the current process's environment into
// the service's own private environment at install time (see
// writeServiceEnvironment) -- a Windows Service does not inherit a logged-in
// user's session environment variables the way an interactive shell does.
var serviceEnvVars = []string{
	"PG_GUARD_POSTGRES_BIN", "PGDATA", "PG_GUARD_EXTRA_ARGS",
	"PG_GUARD_LOG_LEVEL", "PG_GUARD_LOG_FORMAT", "PG_GUARD_LOG_FILE",
	"PG_GUARD_SHUTDOWN_WAIT",
	"POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB", "PGPORT",
}

func installService() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connecting to service control manager: %w", err)
	}
	defer m.Disconnect()

	if existing, err := m.OpenService(serviceName); err == nil {
		existing.Close()
		return fmt.Errorf("service %q is already installed -- run -uninstall-service first", serviceName)
	}

	s, err := m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName:      "pg-guard PostgreSQL HA Supervisor",
		Description:      "Supervises a local PostgreSQL instance for pg-guard HA. See README.md.",
		StartType:        mgr.StartAutomatic,
		ServiceStartName: "NT AUTHORITY\\NetworkService",
	})
	if err != nil {
		return fmt.Errorf("creating service: %w", err)
	}
	defer s.Close()

	if err := writeServiceEnvironment(); err != nil {
		return fmt.Errorf("writing service environment: %w", err)
	}

	if pgData := os.Getenv("PGDATA"); pgData != "" {
		if err := grantNetworkServiceAccess(pgData); err != nil {
			return fmt.Errorf("granting NetworkService access to %s: %w", pgData, err)
		}
		fmt.Printf("Granted NT AUTHORITY\\NETWORK SERVICE access to %s\n", pgData)
	} else {
		fmt.Println("PGDATA is not set -- skipping the NetworkService filesystem grant; set PGDATA and re-run, or grant access manually before starting the service.")
	}

	return nil
}

// writeServiceEnvironment persists the current process's PG_GUARD_*/POSTGRES_*
// values into the service's own registry Environment (REG_MULTI_SZ) --
// the documented, correct way to give one specific service its own
// environment without mutating machine-wide variables.
func writeServiceEnvironment() error {
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Services\`+serviceName, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()

	var envLines []string
	for _, name := range serviceEnvVars {
		if v := os.Getenv(name); v != "" {
			envLines = append(envLines, name+"="+v)
		}
	}
	if len(envLines) == 0 {
		return nil
	}
	return key.SetStringsValue("Environment", envLines)
}

// grantNetworkServiceAccess grants NetworkService modify rights on a
// manually-initdb'd data directory, which is owned by whichever user ran
// initdb and has no such grant by default -- the service would otherwise
// fail with a permissions error the first time it starts postgres.exe.
func grantNetworkServiceAccess(path string) error {
	out, err := exec.Command("icacls.exe", path,
		"/grant", "NT AUTHORITY\\NETWORK SERVICE:(OI)(CI)M", "/T").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connecting to service control manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %q is not installed: %w", serviceName, err)
	}
	defer s.Close()

	if status, err := s.Query(); err == nil && status.State != svc.Stopped {
		fmt.Println("Service is still running -- stop it first (Stop-Service pg-guard) for a clean shutdown before uninstalling.")
	}

	if err := s.Delete(); err != nil {
		return fmt.Errorf("deleting service: %w", err)
	}
	return nil
}
