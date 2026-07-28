//go:build !linux

// reaper_stub.go: true zombie/orphan reaping is a Linux/PID-1-specific
// concept and doesn't apply here -- trackChild just starts cmd and waits on
// it directly in its own goroutine, since there's no competing unscoped
// Wait4(-1) to race against on this platform. This is deliberately a real
// functional equivalent rather than an error-returning stub -- the rest of
// the supervisor loop (start/signal/timeout/SIGKILL) should still run on
// non-Linux platforms so the whole binary stays buildable and runnable
// there for fast local iteration, even though it isn't PID 1 there and
// isn't the shipping deployment target.

package main

import "os/exec"

// startReaper is a no-op here -- see reaper_linux.go's version for what it
// does on the real PID-1 deployment target.
func startReaper() {}

// trackChild starts cmd and returns a channel that receives its exit code
// exactly once, via a plain cmd.Wait() in a dedicated goroutine.
func trackChild(cmd *exec.Cmd) (<-chan int, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := make(chan int, 1)
	go func() {
		code := 0
		if err := cmd.Wait(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				code = 1
			}
		}
		ch <- code
	}()
	return ch, nil
}
