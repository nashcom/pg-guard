//go:build linux

package main

import "os"

// stopGracefullyPlatform on Linux just forwards the received signal --
// os.Process.Signal() works directly, no shelling out needed.
func (s *supervisor) stopGracefullyPlatform(cfg *Config, sig os.Signal) error {
	return s.signalChild(sig)
}
