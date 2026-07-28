// pg-guard -- log.go -- Centralised logging with optional JSON output and log levels.
//
// Text mode (PG_GUARD_LOG_FORMAT=text):
//   2026-07-25T10:00:00Z   INFO: started postgres (pid 7)
//
// JSON mode (default, PG_GUARD_LOG_FORMAT=json):
//   {"ts":"2026-07-25T10:00:00Z","level":"info","msg":"started postgres (pid 7)"}
//
// Log level is controlled by PG_GUARD_LOG_LEVEL (error|warn|info|debug). Default is "info".

package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

type LogLevel int

const (
	LOG_ERROR LogLevel = iota
	LOG_WARN
	LOG_INFO
	LOG_DEBUG
)

func (l LogLevel) String() string {
	switch l {
	case LOG_ERROR:
		return "ERROR"
	case LOG_WARN:
		return "WARN"
	case LOG_INFO:
		return "INFO"
	case LOG_DEBUG:
		return "DEBUG"
	default:
		return "UNKNOWN"
	}
}

func ParseLogLevel(s string) (LogLevel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return LOG_ERROR, nil
	case "warn":
		return LOG_WARN, nil
	case "info":
		return LOG_INFO, nil
	case "debug":
		return LOG_DEBUG, nil
	}
	return LOG_INFO, fmt.Errorf("invalid log level: %s", s)
}

func ParseLogFormat(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "json":
		return true, nil
	case "text":
		return false, nil
	}
	return true, fmt.Errorf("invalid log format: %s", s)
}

// -----------------------------------------------------------------------------
// Package-level state
// -----------------------------------------------------------------------------

// gLogLevel is the active log level; set from PG_GUARD_LOG_LEVEL at startup.
var gLogLevel = LOG_INFO

// gLogJSON switches all logXxx output to single-line JSON objects.
var gLogJSON = true

// -----------------------------------------------------------------------------
// Core emitters
// -----------------------------------------------------------------------------

// logLine is the single path through which all of pg-guard's own operational
// log events flow. It is the only place that branches on gLogJSON for
// pg-guard's own messages.
func logLine(level LogLevel, msg string) {
	if level > gLogLevel {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	if gLogJSON {
		log.Printf(`{"ts":%q,"level":%q,"msg":%q,"source":"pg-guard"}`, ts, strings.ToLower(level.String()), msg)
		return
	}
	log.Printf("%s   %s: %s", ts, level.String(), msg)
}

// logSourced is logLine's counterpart for lines that didn't originate from
// pg-guard itself: wrapped subprocess output (runcmd.go's
// runLoggedCommand -- initdb/pg_rewind/pg_basebackup) and the supervised
// postgres child's own stdout/stderr (supervisor.go). Text mode always
// shows the source in brackets (matching how pg-guard's own lines are
// already the implicit "no bracket" case); JSON mode always carries a
// "source" field, same shape as logLine's.
func logSourced(level LogLevel, source, line string) {
	if level > gLogLevel {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	if gLogJSON {
		log.Printf(`{"ts":%q,"level":%q,"msg":%q,"source":%q}`, ts, strings.ToLower(level.String()), line, source)
		return
	}
	log.Printf("%s   %s [%s]: %s", ts, level.String(), source, line)
}

// logCmdOutput logs one line of a wrapped subprocess's stdout/stderr (see
// runcmd.go) at debug level -- the caller still surfaces a clear
// error-level summary on failure; this is for live troubleshooting
// visibility. See logPostgresOutput (supervisor.go) for the supervised
// postgres child's own output, which is level-classified instead of always
// debug.
func logCmdOutput(source, line string) {
	logSourced(LOG_DEBUG, source, line)
}

func logMsg(level LogLevel, format string, args ...any) {
	if level > gLogLevel {
		return
	}
	logLine(level, fmt.Sprintf(format, args...))
}

func logError(format string, args ...any) { logMsg(LOG_ERROR, format, args...) }
func logWarn(format string, args ...any)  { logMsg(LOG_WARN, format, args...) }
func logInfo(format string, args ...any)  { logMsg(LOG_INFO, format, args...) }
func logDebug(format string, args ...any) { logMsg(LOG_DEBUG, format, args...) }

// logFatal logs at ERROR level then exits with status 1.
func logFatal(format string, args ...any) {
	logMsg(LOG_ERROR, format, args...)
	os.Exit(1)
}

// -----------------------------------------------------------------------------
// Initialisation
// -----------------------------------------------------------------------------

// initLogging reads PG_GUARD_LOG_LEVEL and PG_GUARD_LOG_FORMAT from the environment
// and configures the package globals. It must be called once at the very
// start of main(), before any log output is produced, and is deliberately
// independent of loadConfig() so logging works even if config validation
// later fails. It also suppresses the standard log package's own timestamp
// prefix since logLine formats its own.
func initLogging() {
	log.SetFlags(0)
	log.SetPrefix("")

	if v := os.Getenv("PG_GUARD_LOG_LEVEL"); v != "" {
		level, err := ParseLogLevel(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pg-guard: %v\n", err)
		} else {
			gLogLevel = level
		}
	}

	if v := os.Getenv("PG_GUARD_LOG_FORMAT"); v != "" {
		json, err := ParseLogFormat(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pg-guard: %v\n", err)
		} else {
			gLogJSON = json
		}
	}

	// PG_GUARD_LOG_FILE redirects log output from stderr to a file. This is
	// required in Windows Service mode, which has no console for stderr to
	// go to; it's optional everywhere else (default stays stderr).
	if path := os.Getenv("PG_GUARD_LOG_FILE"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pg-guard: failed to open PG_GUARD_LOG_FILE %q: %v\n", path, err)
		} else {
			log.SetOutput(f)
		}
	}
}
