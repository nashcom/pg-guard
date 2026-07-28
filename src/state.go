// pg-guard -- state.go -- shared HA runtime state and the channels that let
// API handlers (running on HTTP goroutines) request supervisor actions
// without ever touching *supervisor/*childWatcher themselves. All actual
// mutation happens on the single main-loop goroutine (see main.go's
// handleHandoverRequest) so coordinateHandover never runs concurrently
// with itself or races the crash-detection path over the same child.

package main

import (
	"sync/atomic"
	"time"
)

var (
	shutdownInProgress   atomic.Bool
	switchoverInProgress atomic.Bool
	postgresRunning      atomic.Bool

	// backupInProgress guards runBackup (backup.go) against overlapping
	// itself -- a scheduled tick firing while an on-demand backup (or a
	// previous, still-running tick) is in flight, or two manual triggers
	// racing. Unlike shutdown/switchoverInProgress, nothing else in the
	// handover state machine needs to know about this: backup never touches
	// *supervisor/*childWatcher, only shells out against the already-running
	// instance, so it doesn't need to go through handoverRequests.
	backupInProgress atomic.Bool

	// maintenanceActive is persistent (unlike shutdownInProgress, which
	// flips back to false the instant handleMaintenance's own request
	// returns) -- set true once POST /api/maintenance succeeds, cleared
	// once postgres is actually running again (start/rejoin/a normal
	// startup). Without this, "postgres_reachable: false" on /status looks
	// identical whether a node is deliberately in maintenance, crashed, or
	// left stopped after a failed bootstrap/rejoin -- confirmed as a real
	// gap while testing maintenance mode: nothing distinguished "this was
	// on purpose, resume with /api/start" from "something's actually wrong."
	maintenanceActive atomic.Bool

	// maintenanceWasPrimary records the role this node had right before a
	// successful POST /api/maintenance stopped it -- postgres itself can't
	// be queried while stopped, so /status's live "role" field is
	// necessarily "unknown" during maintenance; this fills that gap with
	// what pg-guard already knows from the moment it decided whether to
	// request a peer promotion. Only meaningful while maintenanceActive is
	// true (see handleMaintenance, api.go).
	maintenanceWasPrimary atomic.Bool

	// rebootSuppressUntil is a UnixNano deadline (0 = no active
	// suppression): set by handleRebootNotice (api.go) when the peer
	// reports a planned reboot (PG_GUARD_SHUTDOWN_MODE=reboot), read by
	// failover.go's monitor to withhold automatic promotion until it
	// passes, even if PG_GUARD_FAILOVER_TIMEOUT has already elapsed.
	rebootSuppressUntil atomic.Int64

	// failoverPromoteDeadline is a UnixNano deadline (0 = not currently
	// counting down): set by failover.go's monitor the moment it first
	// observes the peer unreachable, cleared the moment the peer is seen
	// healthy again (or this node stops being a standby). Lets /status and
	// /metrics show a live countdown to automatic promotion -- added after
	// a real testing gap where the countdown was invisible between "peer
	// went down" and "60s later, promoted," making a working failover look
	// like nothing was happening.
	failoverPromoteDeadline atomic.Int64

	// currentChild is nil whenever postgres isn't running under the
	// supervisor right now (during a handover, or after a startup rejoin
	// failure left it deliberately stopped).
	currentChild atomic.Pointer[childWatcher]

	// exitRequested carries the process exit code an API handler wants
	// after it has already written its HTTP response. Buffered so the
	// handler's send never blocks on the main loop being ready.
	exitRequested = make(chan int, 1)

	// handoverRequests serializes shutdown/switchover/rejoin onto the main
	// loop goroutine.
	handoverRequests = make(chan handoverRequest)
)

// exitCodeSwitchoverRestart is a non-zero sentinel exit code: under
// "restart: on-failure", it tells the container to come straight back up,
// where the startup rejoin check (main.go) detects the peer is now primary
// and rejoins as standby automatically.
const exitCodeSwitchoverRestart = 78

type handoverRequest struct {
	kind   string // "shutdown" | "switchover" | "rejoin" | "maintenance" | "start" | "reboot"
	result chan error
}

// rebootSuppressRemaining returns how many seconds remain in an active
// planned-reboot failover-suppression window, or 0 if none is active --
// the single source of truth rebootSuppressActive, failover.go's monitor,
// and metrics.go/api.go's reported fields all derive from.
func rebootSuppressRemaining() float64 {
	until := rebootSuppressUntil.Load()
	if until == 0 {
		return 0
	}
	remaining := time.Until(time.Unix(0, until)).Seconds()
	if remaining < 0 {
		return 0
	}
	return remaining
}

// rebootSuppressActive reports whether a peer's planned-reboot notice is
// currently withholding automatic failover promotion.
func rebootSuppressActive() bool {
	return rebootSuppressRemaining() > 0
}

// failoverPromoteRemaining returns how many seconds remain before the
// failover monitor promotes this node, or 0 if the peer isn't currently
// being tracked as unreachable.
func failoverPromoteRemaining() float64 {
	deadline := failoverPromoteDeadline.Load()
	if deadline == 0 {
		return 0
	}
	remaining := time.Until(time.Unix(0, deadline)).Seconds()
	if remaining < 0 {
		return 0
	}
	return remaining
}

// failoverCountdownActive reports whether the failover monitor is
// currently counting down toward an automatic promotion.
func failoverCountdownActive() bool {
	return failoverPromoteRemaining() > 0
}
