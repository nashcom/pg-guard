//go:build linux

// reaper_linux.go implements real PID-1 zombie reaping: any process
// reparented to us (because its original parent exited first) must be
// wait()ed on or it stays a zombie forever. A single, process-lifetime
// reaper (started once via startReaper) is the only place that ever calls
// wait() on any child -- every spawn site (supervisor.start, runcmd.go's
// runLoggedCommand) goes through trackChild instead of calling cmd.Wait()
// itself, registering interest in its own pid rather than racing an
// independent waiter against the reaper's own Wait4(-1).
//
// That race was a real, confirmed bug: with a separate reaper started (and
// never stopped) on every postgres start/stop cycle, and runLoggedCommand
// calling cmd.Wait() directly for pg_rewind/pg_basebackup/initdb, any of
// the (leaked, still SIGCHLD-registered) reapers could win the kernel-level
// race to collect one of those subprocesses' exit status first -- causing
// cmd.Wait() to fail with ECHILD ("no child processes") even though the
// subprocess had already completed successfully. Seen in testing: a rejoin
// whose pg_basebackup clone fully succeeded (100% on both tablespaces) was
// still reported and treated as a failure, leaving the node stopped until
// a manual retry happened to win the race differently.
package main

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

type reaper struct {
	mu      sync.Mutex
	waiters map[int]chan int
}

var procReaper *reaper

// startReaper starts the single, process-lifetime reaper goroutine. Must be
// called exactly once, before any child process is spawned.
func startReaper() {
	r := &reaper{waiters: make(map[int]chan int)}
	procReaper = r

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGCHLD)

	go func() {
		r.reap() // catch anything that exited before Notify registered
		for range sigCh {
			r.reap()
		}
	}()
}

// reap drains every waitable child (WNOHANG) until none remain. A pid with
// a registered waiter (see trackChild) gets its exit code delivered there;
// anything else is a genuinely orphaned/reparented process with nothing
// further to do once collected.
func (r *reaper) reap() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if err != nil || pid <= 0 {
			return
		}

		code := status.ExitStatus()
		if status.Signaled() {
			code = 128 + int(status.Signal())
		}

		if ch, ok := r.waiters[pid]; ok {
			delete(r.waiters, pid)
			select {
			case ch <- code:
			default:
			}
			continue
		}

		logDebug("reaped orphaned process (pid %d, code %d)", pid, code)
	}
}

// trackChild starts cmd and registers its pid with the process-lifetime
// reaper, returning a channel that receives its exit code exactly once.
// Starting and registering happen under the same lock reap() uses, so a
// child that exits immediately after Start() can never be collected as an
// unrecognized orphan before its waiter is registered -- closing the race
// this file's top comment describes.
func trackChild(cmd *exec.Cmd) (<-chan int, error) {
	procReaper.mu.Lock()
	defer procReaper.mu.Unlock()

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := make(chan int, 1)
	procReaper.waiters[cmd.Process.Pid] = ch
	return ch, nil
}
