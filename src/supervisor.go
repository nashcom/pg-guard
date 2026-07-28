// pg-guard -- supervisor.go -- launches and controls the single supervised
// child process (the postgres binary itself). Never calls cmd.Wait()
// itself -- see reaper_linux.go for why.

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type supervisor struct {
	cfg    *Config
	cmd    *exec.Cmd
	exited <-chan int
}

func newSupervisor(cfg *Config) *supervisor {
	return &supervisor{cfg: cfg}
}

// start launches the child process and returns once it has been started
// (not once it has exited). postgres's own stdout/stderr are piped through
// logPostgresOutput rather than passed through raw, so its output flows
// through pg-guard's own structured logger (tagged source="postgres") and
// gets level-classified -- see classifyPostgresLine. Spawns via trackChild
// (reaper_linux.go/reaper_stub.go) rather than cmd.Start() directly, so the
// process-lifetime reaper's registration happens atomically with startup --
// see trackChild's doc comment for why that matters.
func (s *supervisor) start() error {
	s.cmd = exec.Command(s.cfg.PostgresBin, s.cfg.PostgresArgs...)
	stdout, err := s.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("piping stdout: %w", err)
	}
	stderr, err := s.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("piping stderr: %w", err)
	}
	s.cmd.SysProcAttr = newSysProcAttr()

	exited, err := trackChild(s.cmd)
	if err != nil {
		return fmt.Errorf("starting %s: %w", s.cfg.PostgresBin, err)
	}
	s.exited = exited

	go streamPostgresOutput(stdout)
	go streamPostgresOutput(stderr)

	logInfo("started %s %v (pid %d)", s.cfg.PostgresBin, s.cfg.PostgresArgs, s.cmd.Process.Pid)
	return nil
}

func streamPostgresOutput(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		logSourced(classifyPostgresLine(line), "postgres", line)
	}
}

// postgresLogLevel extracts postgres's own severity token (LOG, FATAL,
// ERROR, WARNING, NOTICE, PANIC, DEBUG1-5, or a continuation-line label
// like DETAIL/HINT/STATEMENT/CONTEXT) from one of its raw log lines, e.g.
// "2026-... UTC [50] FATAL:  terminating connection ...". Returns "" if the
// line doesn't match that shape (a wrapped continuation of the previous
// message, or genuinely unstructured output).
func postgresLogLevel(line string) string {
	idx := strings.Index(line, "] ")
	if idx < 0 {
		return ""
	}
	rest := line[idx+2:]
	colon := strings.Index(rest, ":")
	if colon <= 0 {
		return ""
	}
	token := rest[:colon]
	if token != "" && token == strings.ToUpper(token) && !strings.ContainsAny(token, " \t") {
		return token
	}
	return ""
}

// expectedShutdownNoise is substrings of postgres's own FATAL/ERROR lines
// that are normal, harmless byproducts of pg-guard's own coordinated
// shutdown/switchover (handover.go) -- postgres logs these at FATAL from
// the terminated connection's own perspective regardless of whether the
// termination was requested by an administrator (us) or is a genuine
// problem, so pg-guard is the only place with the context to tell them
// apart.
var expectedShutdownNoise = []string{
	"terminating connection due to administrator command",
	"the database system is shutting down",
}

// classifyPostgresLine decides what level to re-log one of postgres's own
// output lines at. Downgrades known-expected shutdown noise to info only
// while pg-guard itself has a coordinated shutdown/switchover in progress
// (shutdownInProgress/switchoverInProgress, state.go) -- the same line
// outside that window (e.g. an unexpected crash) still logs at error,
// unchanged.
func classifyPostgresLine(line string) LogLevel {
	switch postgresLogLevel(line) {
	case "FATAL", "PANIC", "ERROR":
		if shutdownInProgress.Load() || switchoverInProgress.Load() {
			for _, noise := range expectedShutdownNoise {
				if strings.Contains(line, noise) {
					return LOG_INFO
				}
			}
		}
		return LOG_ERROR
	case "WARNING":
		return LOG_WARN
	case "":
		return LOG_INFO
	default: // LOG, NOTICE, DEBUG1..5, STATEMENT, DETAIL, HINT, CONTEXT
		return LOG_INFO
	}
}

// signalChild forwards sig to the child. On Windows, os.Process.Signal only
// supports os.Kill -- anything else returns an error, which the caller
// (shutdown() in main.go) tolerates by falling through to the timeout and
// killChild(). This is a known Windows limitation, acceptable since native
// process control isn't a milestone-1 target there.
func (s *supervisor) signalChild(sig os.Signal) error {
	if s.cmd == nil || s.cmd.Process == nil {
		return fmt.Errorf("no child process running")
	}
	logInfo("forwarding %s to pid %d", sig, s.cmd.Process.Pid)
	return s.cmd.Process.Signal(sig)
}

// stopGracefully asks the child to shut down cleanly. What that means is
// platform-specific -- see stop_linux.go / stop_windows.go -- because
// os.Process.Signal() only implements os.Kill on Windows; there's no real
// SIGTERM/SIGINT delivery to an arbitrary child process there.
func (s *supervisor) stopGracefully(cfg *Config, sig os.Signal) error {
	return s.stopGracefullyPlatform(cfg, sig)
}

func (s *supervisor) killChild() error {
	if s.cmd == nil || s.cmd.Process == nil {
		return fmt.Errorf("no child process running")
	}
	logWarn("sending SIGKILL to pid %d", s.cmd.Process.Pid)
	return s.cmd.Process.Kill()
}

func (s *supervisor) pid() int {
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}
