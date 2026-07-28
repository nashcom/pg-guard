//go:build !windows

// service_stub.go: Windows Service registration/hosting is a Windows-only
// concept with no meaningful equivalent elsewhere (unlike reaper_stub.go,
// which provides a real functional substitute) -- these are deliberate
// hard-error stubs.

package main

import "fmt"

func isWindowsService() bool { return false }

func installService() error {
	return fmt.Errorf("Windows Service installation is only supported on Windows")
}

func uninstallService() error {
	return fmt.Errorf("Windows Service installation is only supported on Windows")
}

func runService(cfg *Config) error {
	return fmt.Errorf("Windows Service mode is only supported on Windows")
}
