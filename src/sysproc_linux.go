//go:build linux

package main

import "syscall"

// newSysProcAttr sets the child into its own process group for defensive
// process-group hygiene as PID 1. Shutdown signals are still targeted at
// the child's own PID, not the group -- see supervisor.go.
func newSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
