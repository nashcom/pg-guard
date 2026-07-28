// pg-guard -- failover.go -- automatic failover: a background monitor that
// promotes the local standby if the peer has been continuously unreachable
// for PG_GUARD_FAILOVER_TIMEOUT. Implements README's documented v1
// assumption: peer-unreachability is treated as server loss, not an
// arbitrary network partition -- a real, accepted risk, not new here.
// Only ever calls the local promote() (a pure DB operation) -- never
// touches *supervisor/*childWatcher, so it needs no coordination with the
// main loop's handoverRequests serialization (see state.go).

package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// startFailoverMonitor starts the background loop unless FailoverMode is
// "manual" or no peer is configured. Runs for the lifetime of the process.
func startFailoverMonitor(cfg *Config, pool *pgxpool.Pool) {
	if cfg.FailoverMode != "automatic" || cfg.PeerHost == "" {
		logInfo("automatic failover monitor disabled (mode=%s, peer configured=%v)", cfg.FailoverMode, cfg.PeerHost != "")
		return
	}

	go func() {
		var unhealthySince time.Time
		ticker := time.NewTicker(failoverCheckInterval)
		defer ticker.Stop()

		for range ticker.C {
			if !postgresRunning.Load() {
				unhealthySince = time.Time{}
				failoverPromoteDeadline.Store(0)
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), failoverReachabilityTimeout)
			inRecovery, err := isInRecovery(ctx, pool)
			cancel()
			if err != nil {
				unhealthySince = time.Time{}
				failoverPromoteDeadline.Store(0)
				continue
			}

			if !inRecovery {
				// We're primary -- nothing to fail over to, but keep the
				// peer's replication grant fresh. ensureReplicationHBA only
				// ever runs once, right after a promotion (handlePromote,
				// or this monitor's own automatic-failover branch below) --
				// and automatic failover promotes precisely because the
				// peer was unreachable at that moment, so resolving its
				// hostname to refresh pg_hba.conf fails right then almost
				// by definition. That failure is swallowed as a warning
				// (ensureRoleAndDatabase), so the one-shot call after
				// promote looks "successful" and never retries -- leaving
				// the peer permanently unable to rejoin later once it's
				// actually back, with no automatic recovery. Confirmed as a
				// real bug in testing: a node that came back after an
				// automatic failover was rejected by the new primary's
				// stale pg_hba.conf indefinitely, until something else
				// happened to re-run this. Retrying it on every tick here
				// -- cheap (a DNS lookup plus a couple of idempotent
				// existence checks) -- means the very next tick after the
				// peer becomes resolvable again fixes it with no operator
				// intervention needed. Skipped entirely while the peer is
				// already known unreachable -- it can't possibly be trying
				// to rejoin while down, so there's nothing to gain from
				// repeatedly resolving its hostname (which does nothing but
				// generate log noise against a down peer) until it's back.
				if checkPeerReachable(cfg) {
					refreshCtx, refreshCancel := context.WithTimeout(context.Background(), failoverHBARefreshTimeout)
					if err := ensureRoleAndDatabase(refreshCtx, cfg); err != nil {
						logDebug("periodic replication-grant refresh: %v", err)
					}
					refreshCancel()
				}
				unhealthySince = time.Time{}
				failoverPromoteDeadline.Store(0)
				continue
			}

			if peerHealthy(cfg) {
				unhealthySince = time.Time{}
				failoverPromoteDeadline.Store(0)
				// Clear a lingering reboot-failover-suppression window early
				// rather than letting it run out the clock: rebootSuppressUntil
				// is just a fixed deadline (state.go) with nothing else to
				// clear it, so once the peer that sent the notice is
				// confirmed healthy again, the notice has served its
				// purpose. Leaving it active for its full remaining window
				// is a real gap, not cosmetic -- if the peer went down again
				// for an unrelated reason during that window, this node
				// would wrongly keep withholding automatic failover for a
				// notice that no longer applies.
				if rebootSuppressUntil.Swap(0) != 0 {
					logInfo("peer %s confirmed healthy again -- clearing reboot failover-suppression early", cfg.PeerHost)
				}
				continue
			}

			if unhealthySince.IsZero() {
				unhealthySince = time.Now()
				failoverPromoteDeadline.Store(unhealthySince.Add(cfg.FailoverTimeout).UnixNano())
				logWarn("peer %s appears unreachable -- starting failover timeout (%s)", cfg.PeerHost, cfg.FailoverTimeout)
				continue
			}
			if time.Since(unhealthySince) < cfg.FailoverTimeout {
				continue
			}

			if rebootSuppressActive() {
				logInfo("peer %s has been unreachable beyond the normal failover timeout, but a planned-reboot notice (PG_GUARD_SHUTDOWN_MODE=reboot) is suppressing promotion until %s", cfg.PeerHost, time.Unix(0, rebootSuppressUntil.Load()).UTC().Format(time.RFC3339))
				continue
			}

			logError("peer %s has been unreachable for over %s -- promoting locally (automatic failover)", cfg.PeerHost, cfg.FailoverTimeout)
			promoteCtx, cancel := context.WithTimeout(context.Background(), promoteOperationTimeout)
			promoteStart := time.Now()
			err = promote(promoteCtx, pool)
			cancel()
			if err != nil {
				logError("automatic failover promote failed: %v -- will retry on next check", err)
				continue
			}
			recordPromotionDuration(time.Since(promoteStart))
			incrementPromotions()
			// Same reasoning as handlePromote (api.go): refresh the
			// pg_hba.conf replication grant now, not on some later restart
			// -- this node's /status starts reporting "primary" immediately,
			// and a peer rejoining against it needs the grant to already be
			// correct, not stale from whenever this node was last primary.
			ensureRoleAndDatabaseBlocking(cfg)
			logError("automatic failover promote succeeded -- this node is now primary")
			unhealthySince = time.Time{}
			failoverPromoteDeadline.Store(0)
		}
	}()
}

func peerHealthy(cfg *Config) bool {
	if !checkPeerReachable(cfg) {
		return false
	}
	_, err := fetchPeerStatus(cfg)
	return err == nil
}
