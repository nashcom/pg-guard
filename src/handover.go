// pg-guard -- handover.go -- the coordinated handover sequence shared by
// SIGTERM/SIGINT, POST /api/shutdown, and POST /api/switchover (via
// main.go's handleTerminationSignal / handleHandoverRequest, always on the
// main goroutine -- see state.go). They differ only in the exit code used
// afterward and, for shutdown, that it always means "don't come back."
// PG_GUARD_SHUTDOWN_MODE=reboot replaces coordinateHandover with
// coordinateReboot for both the SIGTERM and POST /api/shutdown paths --
// see its own doc comment below for how that differs.

package main

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// shutdownSignal is what stopLocal asks the child to stop with when there's
// no real OS signal driving the call (API-triggered shutdown/switchover, or
// the Windows Service Control Manager's Stop/Shutdown commands, which have
// no OS-signal equivalent to forward at all). SIGTERM is Postgres's "smart
// shutdown" -- the same graceful request an OS-signal-triggered shutdown
// would normally forward. On the Linux SIGTERM/SIGINT path, the actual
// received signal is threaded through instead -- see handleTerminationSignal
// in main.go -- since Postgres gives those two distinct smart-vs-fast
// shutdown meanings (README's Process Model) and always normalizing to
// SIGTERM would silently override that.
const shutdownSignal = syscall.SIGTERM

// coordinateHandover stops local postgres, per cfg.ShutdownPolicy:
//   - require-switchover (default): refuses (returns an error, does nothing)
//     unless the peer is reachable and healthy, then confirms the cluster
//     still has exactly one primary afterward.
//   - best-effort: same checks, but proceeds and only logs a warning if they fail.
//   - force: skips peer coordination entirely -- plain stop-and-wait.
//
// What "healthy peer" and "confirms" mean depends on the local role, checked
// fresh via isInRecovery on pool (not assumed from the caller): if we're
// currently primary, the peer must be a standby we hand primary duties to
// (request its promotion, wait for it to confirm). If we're currently
// standby, the peer must already be a healthy primary -- there's no
// promotion step, just a stop; requesting a promote here would target a
// peer that's already primary.
func coordinateHandover(sup *supervisor, cw *childWatcher, cfg *Config, pool *pgxpool.Pool, sig os.Signal, policy string) error {
	if policy == "force" {
		return stopLocal(sup, cw, cfg, pool, sig)
	}

	amPrimary := true
	ctx, cancel := context.WithTimeout(context.Background(), peerHealthCheckTimeout)
	if inRecovery, err := isInRecovery(ctx, pool); err == nil {
		amPrimary = !inRecovery
	} else {
		logWarn("coordinateHandover: could not determine local role (%v) -- assuming primary", err)
	}
	cancel()

	if !amPrimary {
		return coordinateStandbyHandover(sup, cw, cfg, pool, sig, policy)
	}

	status, err := fetchPeerStatus(cfg)
	if err != nil || status.Role != "standby" {
		msg := fmt.Sprintf("peer not available as a healthy standby (err=%v, status=%+v)", err, status)
		if policy == "require-switchover" {
			return fmt.Errorf("refusing handover: %s", msg)
		}
		logWarn("best-effort handover: %s -- proceeding anyway", msg)
	}

	if err := stopLocal(sup, cw, cfg, pool, sig); err != nil {
		return fmt.Errorf("stopping local postgres: %w", err)
	}

	if err := requestPeerPromote(cfg); err != nil {
		msg := fmt.Sprintf("peer promote request failed: %v", err)
		if policy == "require-switchover" {
			return fmt.Errorf("local postgres stopped but %s -- cluster may now have no primary", msg)
		}
		logWarn("best-effort handover: %s", msg)
		return nil
	}

	deadline := time.Now().Add(peerPromoteConfirmDeadline)
	for time.Now().Before(deadline) {
		if st, err := fetchPeerStatus(cfg); err == nil && st.Role == "primary" {
			logInfo("handover complete: peer %s confirmed primary", cfg.PeerHost)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	msg := "peer did not confirm primary role within 10s of promote request"
	if policy == "require-switchover" {
		return fmt.Errorf("local postgres stopped but %s -- cluster may now have no primary", msg)
	}
	logWarn("best-effort handover: %s", msg)
	return nil
}

// coordinateStandbyHandover is coordinateHandover's standby-shutdown path:
// just confirm the peer is a healthy primary (so the cluster keeps running
// after we stop), then stop -- no promotion request, since the peer is
// already primary.
func coordinateStandbyHandover(sup *supervisor, cw *childWatcher, cfg *Config, pool *pgxpool.Pool, sig os.Signal, policy string) error {
	status, err := fetchPeerStatus(cfg)
	if err != nil || status.Role != "primary" {
		msg := fmt.Sprintf("peer not available as a healthy primary (err=%v, status=%+v)", err, status)
		if policy == "require-switchover" {
			return fmt.Errorf("refusing standby shutdown: %s", msg)
		}
		logWarn("best-effort standby shutdown: %s -- proceeding anyway", msg)
	}

	if err := stopLocal(sup, cw, cfg, pool, sig); err != nil {
		return fmt.Errorf("stopping local postgres: %w", err)
	}
	return nil
}

// coordinateReboot is PG_GUARD_SHUTDOWN_MODE=reboot's stop path -- unlike
// coordinateHandover, it never requests the peer promote: the whole point
// is to come back as whatever role this node already was, not hand off.
// If currently primary, it sends the peer a best-effort "I'm rebooting"
// notice (notifyPeerReboot, peer.go) so the peer's automatic-failover
// monitor suppresses promotion for PG_GUARD_REBOOT_GRACE_PERIOD instead of
// promoting on the normal PG_GUARD_FAILOVER_TIMEOUT (failover.go,
// handleRebootNotice in api.go) -- if this node is back within that
// window, nothing else needed to happen at all; startPostgres's existing
// startup rejoin check (main.go) already does the right thing regardless:
// resumes as primary unchanged if the peer stayed standby, or rejoins as
// standby if the grace period lapsed and the peer promoted anyway.
//
// A standby reboot needs none of this -- only a primary's departure can
// trigger the peer's promotion -- so it just reuses
// coordinateStandbyHandover's existing peer-health safety check unchanged.
func coordinateReboot(sup *supervisor, cw *childWatcher, cfg *Config, pool *pgxpool.Pool, sig os.Signal) error {
	ctx, cancel := context.WithTimeout(context.Background(), peerHealthCheckTimeout)
	amPrimary := true
	if inRecovery, err := isInRecovery(ctx, pool); err == nil {
		amPrimary = !inRecovery
	} else {
		logWarn("coordinateReboot: could not determine local role (%v) -- assuming primary", err)
	}
	cancel()

	if !amPrimary {
		return coordinateStandbyHandover(sup, cw, cfg, pool, sig, cfg.ShutdownPolicy)
	}

	if err := notifyPeerReboot(cfg); err != nil {
		logWarn("reboot: notifying peer to suppress failover: %v -- proceeding anyway (peer falls back to its normal failover timeout instead)", err)
	}
	return stopLocal(sup, cw, cfg, pool, sig)
}

// stopLocal stops the supervised child using the same
// stopGracefully-then-wait-then-kill pattern the original plain shutdown
// used, just without the os.Exit call at the end. sig is what's forwarded
// to postgres on Linux (see shutdownSignal above for why it isn't always
// the same constant); ignored on Windows, which always uses pg_ctl.
//
// Resets pool before signaling postgres: Postgres's "smart" shutdown
// (SIGTERM) waits for EVERY connected client to disconnect voluntarily,
// including pg-guard's own health-check/API pool -- leaving it open turns
// every graceful shutdown into a guaranteed wait for the full
// PG_GUARD_SHUTDOWN_WAIT timeout followed by a forced kill, confirmed in
// testing (coordinated shutdown and switchover both took ~30-38s, right at
// the timeout, instead of completing in under a second against an idle
// database).
//
// Reset(), not Close(): pool is the single pgxpool.Pool created once in
// runInteractive/runService and shared for the whole process lifetime by
// the API server, metrics server, and failover monitor. Close() is
// terminal -- every later Ping()/query on it would fail forever, even
// after postgres comes back up via POST /api/start or a rejoin, since
// nothing ever re-creates the pool mid-process. Confirmed as a real bug in
// testing: after a maintenance stop, postgres's port was reachable again
// following POST /api/start, but /status stayed stuck reporting
// unreachable until the whole container was restarted (which built a
// fresh pool from scratch). Reset() drops the same idle/in-use connections
// -- so postgres's smart shutdown still doesn't wait on them -- without
// killing the pool's ability to serve future callers once postgres is
// reachable again.
//
// Clears currentChild/postgresRunning itself, right here, rather than
// leaving that to coordinateHandover's caller: a later step (requestPeerPromote,
// the peer-confirmation poll) can still fail and cause coordinateHandover to
// return an error even after this succeeds, and the caller only sees that
// one error -- it can't tell "postgres never stopped" from "postgres
// stopped fine but the peer handshake failed" apart. Postgres being stopped
// is a fact the instant this function confirms it, independent of whatever
// happens next, so the state update belongs here, not several call frames
// up gated behind a nil-error check that a partial failure would skip.
func stopLocal(sup *supervisor, cw *childWatcher, cfg *Config, pool *pgxpool.Pool, sig os.Signal) error {
	pool.Reset()
	logDebug("connection pool reset ahead of stopping postgres")

	if err := sup.stopGracefully(cfg, sig); err != nil {
		logWarn("failed to stop child gracefully: %v", err)
	}

	select {
	case <-cw.done:
		logInfo("child exited cleanly (code %d)", cw.code)
		currentChild.Store(nil)
		postgresRunning.Store(false)
		return nil
	case <-time.After(cfg.ShutdownWait):
		logWarn("child did not exit within %s -- forcing kill", cfg.ShutdownWait)
		if err := sup.killChild(); err != nil {
			return fmt.Errorf("failed to kill child: %w", err)
		}
		select {
		case <-cw.done:
		case <-time.After(handoverKillConfirmTimeout):
			logWarn("child did not confirm exit after kill within grace period")
		}
		currentChild.Store(nil)
		postgresRunning.Store(false)
		return nil
	}
}
