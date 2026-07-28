// pg-guard -- runcmd.go -- shared helper for shelling out to the official
// Postgres binaries (initdb, pg_rewind, pg_basebackup, ...) with their
// stdout/stderr flowing through pg-guard's own structured logger instead of
// being captured silently and dumped as one blob after the fact.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// runLoggedCommand runs cmd, streaming each line of combined stdout/stderr
// through logCmdOutput(source, line) in real time (visible with
// PG_GUARD_LOG_LEVEL=debug), while also collecting the same output into the
// returned []byte so a failure can still produce a complete error message
// regardless of log level.
//
// Waits via trackChild rather than cmd.Wait() directly: on Linux, a raw
// cmd.Wait() here would race the process-lifetime reaper's own Wait4(-1)
// call over the same pid -- confirmed as a real bug where the reaper won
// that race and stole a pg_basebackup process's exit status, so cmd.Wait()
// returned ECHILD ("no child processes") even though the clone had already
// completed successfully. trackChild registers this pid with the reaper
// atomically with starting it, so there's exactly one waiter, never two.
func runLoggedCommand(source string, cmd *exec.Cmd) ([]byte, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)

	stream := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text()
			logCmdOutput(source, line)
			mu.Lock()
			buf.WriteString(line)
			buf.WriteByte('\n')
			mu.Unlock()
		}
	}

	exited, err := trackChild(cmd)
	if err != nil {
		return nil, err
	}
	go stream(stdout)
	go stream(stderr)
	wg.Wait()

	code := <-exited
	if code != 0 {
		return buf.Bytes(), fmt.Errorf("exit status %d", code)
	}
	return buf.Bytes(), nil
}
