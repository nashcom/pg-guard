// pg-guard -- timeouts.go -- every timeout/interval/deadline duration used
// across the supervisor, grouped by what they bound and named for what
// they mean. Extracted from what were previously inline literals scattered
// across peer.go, api.go, metrics_server.go, bootstrap.go, main.go,
// handover.go, failover.go, setpassword.go, and remotecli.go -- collecting
// them here doesn't change any behavior (every value below is identical to
// the literal it replaces), it just puts the reasoning for each one in one
// place instead of requiring a grep across the whole package to find it.
//
// Deliberately NOT collapsed into fewer, shared constants purely because
// two sites happen to use the same duration today -- most of the values
// below stay separate even where they coincide numerically, since they
// bound conceptually different operations and merging them would mean
// changing one for a good reason later silently changes the other too.
// The two constants that ARE shared across files (promoteOperationTimeout,
// peerPromoteTimeout) are shared because they bound the literal same
// underlying call, not just a matching number -- see each one's comment.

package main

import "time"

// -- Peer HTTP calls (peer.go) --
const (
	// peerReachabilityTimeout bounds the fast /health and /status
	// reachability checks -- not tuned for anything that does real work
	// server-side.
	peerReachabilityTimeout = 2 * time.Second

	// peerPromoteTimeout is deliberately much longer than
	// peerReachabilityTimeout: pg_promote() (called server-side by the
	// peer's own /api/promote handler) can legitimately take up to 60s to
	// return. Reusing the short reachability timeout here once caused a
	// real failure -- the peer's promote genuinely succeeded, but the
	// client gave up waiting for the response and coordinateHandover
	// treated it as a failed handover. Also used by remotecli.go's local
	// client, which calls the same /api/promote endpoint.
	peerPromoteTimeout = 70 * time.Second
)

// -- pg_promote() itself, called locally via pgx (api.go, failover.go) --
const (
	// promoteOperationTimeout bounds the local SELECT pg_promote() call --
	// pg_promote() defaults to waiting up to 60s for promotion to
	// complete. Shared between handlePromote (api.go, the manual/API
	// trigger) and the automatic-failover monitor (failover.go): both
	// call the same underlying promote(ctx, pool), just from different
	// trigger paths.
	promoteOperationTimeout = 65 * time.Second
)

// -- REST API handlers (api.go) --
const (
	// roleCheckTimeout bounds a quick pg_is_in_recovery()-style query
	// against the local pool (handleMaintenance/handleSwitchover).
	roleCheckTimeout = 3 * time.Second

	// serverShutdownTimeout bounds the graceful HTTP server Shutdown()
	// call at process exit (stopServers).
	serverShutdownTimeout = 5 * time.Second
)

// -- Metrics/status handlers (metrics_server.go) --
const (
	// statusQueryTimeout bounds the DB queries behind GET /ready and
	// GET /status.
	statusQueryTimeout = 3 * time.Second

	// metricsCollectionTimeout bounds the DB queries behind GET /metrics.
	metricsCollectionTimeout = 5 * time.Second
)

// -- Bootstrap (bootstrap.go) --
const (
	// bootstrapPrimaryTiebreakTimeout is how long the "-0" node (or a
	// single, peer-less node) waits to see whether the peer is already
	// primary before bootstrapping as primary itself -- short, since this
	// is fundamentally the "nothing else exists yet" fallback, not a real
	// wait.
	bootstrapPrimaryTiebreakTimeout = 15 * time.Second

	// bootstrapStandbyTiebreakTimeout is how long the "-1" node waits for
	// the peer to report primary before giving up -- long, since it's
	// deferring to a peer it already expects to exist.
	bootstrapStandbyTiebreakTimeout = 120 * time.Second

	// bootstrapPollInterval is the retry cadence for the -0 tiebreak's
	// brief peer check above.
	bootstrapPollInterval = 1 * time.Second

	// bootstrapRetryInterval is the retry cadence for the -1 tiebreak's
	// wait and for the basebackup-clone retry loop, both within the
	// bootstrapStandbyTiebreakTimeout budget.
	bootstrapRetryInterval = 2 * time.Second

	// peerDNSLookupTimeout bounds resolving the peer's hostname to an IP,
	// for the pg_hba.conf replication grant.
	peerDNSLookupTimeout = 2 * time.Second

	// ensureRoleAndDatabaseTimeout is the overall retry budget for
	// creating the replication role/database after bootstrap.
	ensureRoleAndDatabaseTimeout = 60 * time.Second

	// ensureRoleAndDatabaseOpTimeout bounds each individual attempt within
	// that budget.
	ensureRoleAndDatabaseOpTimeout = 5 * time.Second
)

// -- Startup rejoin check (main.go) --
const (
	// startupRejoinDecisionTimeout is the overall retry budget for
	// deciding whether a rejoin is needed at startup.
	startupRejoinDecisionTimeout = 30 * time.Second

	// startupRejoinPollInterval is the retry cadence while the peer's
	// status is inconclusive (unreachable/erroring).
	startupRejoinPollInterval = 1 * time.Second

	// startupRejoinRetryInterval is the retry cadence between failed
	// rejoin attempts once the peer is confirmed primary.
	startupRejoinRetryInterval = 2 * time.Second
)

// -- Coordinated handover (handover.go) --
const (
	// peerHealthCheckTimeout bounds a quick local role check
	// (coordinateReboot) or similar short, ad hoc context in the handover
	// path.
	peerHealthCheckTimeout = 3 * time.Second

	// peerPromoteConfirmDeadline is how long coordinateHandover polls
	// GET /status waiting to see the peer report itself primary after a
	// promote request.
	peerPromoteConfirmDeadline = 10 * time.Second

	// handoverKillConfirmTimeout is the grace period stopLocal waits for
	// the child to confirm its exit after being force-killed.
	handoverKillConfirmTimeout = 5 * time.Second
)

// -- Automatic failover monitor (failover.go) --
const (
	// failoverCheckInterval is how often the monitor loop ticks.
	failoverCheckInterval = 5 * time.Second

	// failoverReachabilityTimeout bounds each local role check the
	// monitor performs before evaluating peer health.
	failoverReachabilityTimeout = 3 * time.Second

	// failoverHBARefreshTimeout bounds the periodic pg_hba.conf
	// replication-grant refresh the monitor performs while primary.
	failoverHBARefreshTimeout = 5 * time.Second
)

// -- set-password CLI (setpassword.go) --
const (
	// setPasswordOperationTimeout bounds each individual SQL statement
	// (ALTER ROLE, pg_hba.conf reload, etc.) once connected.
	setPasswordOperationTimeout = 10 * time.Second

	// setPasswordSocketDialTimeout bounds each attempt to connect over
	// one of the candidate local Unix socket directories.
	setPasswordSocketDialTimeout = 5 * time.Second
)

// -- Windows Service (service_windows.go) --
const (
	// serviceStopPendingInterval is how often the Windows Service reports
	// StopPending back to the Service Control Manager during a long
	// shutdown, so SCM doesn't decide the service is hung.
	serviceStopPendingInterval = 2 * time.Second
)
