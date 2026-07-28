// pg-guard -- bootstrap.go -- first-run cluster initialization, replacing
// the Docker-only init-container approach so native installs get the same
// treatment: initdb + pg_hba.conf for a fresh primary, pg_basebackup clone
// for a fresh standby. Runs once, before the very first sup.start() in the
// process's life (see main.go's startPostgres), same slot as the startup
// rejoin check.

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// needsBootstrap reports whether PGDATA has never been initialized.
func needsBootstrap(cfg *Config) bool {
	_, err := os.Stat(filepath.Join(cfg.PGData, "PG_VERSION"))
	return err != nil
}

// runBootstrap decides whether this node bootstraps as primary (initdb) or
// standby (pg_basebackup clone from the peer):
//   - PG_GUARD_BOOTSTRAP_ROLE=primary|standby overrides everything.
//   - Otherwise, if the peer is reachable and already reports primary,
//     bootstrap as standby -- regardless of hostname ordinal.
//   - Otherwise, the hostname -0/-1 ordinal breaks the tie (matching the
//     peer-derivation convention, config.go's derivePeerHost): "-0" (or no
//     peer configured at all -- single-node) briefly checks for a primary
//     peer (up to 15s) before bootstrapping as primary itself; "-1" waits
//     much longer, up to 120s, then fails loud rather than silently create
//     a second primary.
//
// "-0"'s brief wait, not an instant decision, matters for a concurrent
// two-node restart (both containers recreated together): the peer's
// listener may simply not be reachable yet on the very first check, which
// used to be treated as "no primary exists" rather than "can't tell yet."
// Confirmed as a real, repeatable split-brain in testing -- two freshly
// bootstrapping nodes started together each self-elected primary because
// neither could see the other yet. 15s, not "-1"'s full 120s: this is
// still fundamentally a tiebreak for the case where nothing else exists,
// so it shouldn't wait as long as the side that's deferring to a peer it
// already knows should exist.
func runBootstrap(cfg *Config) error {
	switch cfg.BootstrapRole {
	case "primary":
		logInfo("bootstrap: PG_GUARD_BOOTSTRAP_ROLE=primary -- bootstrapping as primary")
		return bootstrapAsPrimary(cfg)
	case "standby":
		logInfo("bootstrap: PG_GUARD_BOOTSTRAP_ROLE=standby -- bootstrapping as standby")
		return runBasebackup(cfg)
	}

	hostname, err := os.Hostname()
	isOrdinalZero := cfg.PeerHost == "" || err != nil || !strings.HasSuffix(hostname, "-1")

	if !isOrdinalZero {
		logInfo("bootstrap: waiting for peer %s to become primary before bootstrapping as standby (up to 120s)", cfg.PeerHost)
		return bootstrapAsStandbyWithRetry(cfg)
	}

	if cfg.PeerHost != "" {
		checkDeadline := time.Now().Add(bootstrapPrimaryTiebreakTimeout)
		for {
			status, err := fetchPeerStatus(cfg)
			if err == nil && status.Role == "primary" {
				logInfo("bootstrap: peer %s is primary -- bootstrapping as standby", cfg.PeerHost)
				return bootstrapAsStandbyWithRetry(cfg)
			}
			if time.Now().After(checkDeadline) {
				break
			}
			time.Sleep(bootstrapPollInterval)
		}
	}

	logInfo("bootstrap: no primary peer found -- bootstrapping as primary")
	return bootstrapAsPrimary(cfg)
}

// bootstrapAsStandbyWithRetry waits for the peer to report primary and
// clones from it, retrying the whole cycle -- not just the initial "is it
// primary yet" check -- for up to 120s. Even once the peer reports primary,
// its own post-promotion/post-initdb setup (ensureRoleAndDatabase, creating
// the replicator role and database) can still be a couple of seconds from
// done; a clone attempted in that narrow window fails with "role ... does
// not exist" and, before this, permanently aborted bootstrap instead of
// just trying again a moment later. Confirmed as a real bug in testing: a
// from-scratch two-node bootstrap lost this race against a freshly
// primary-elected peer, even though the whole point of the surrounding
// wait loop was to be resilient to exactly this kind of timing.
func bootstrapAsStandbyWithRetry(cfg *Config) error {
	deadline := time.Now().Add(bootstrapStandbyTiebreakTimeout)
	for {
		status, err := fetchPeerStatus(cfg)
		if err != nil || status.Role != "primary" {
			if time.Now().After(deadline) {
				return fmt.Errorf("peer %s never became primary within 120s -- check it, or set PG_GUARD_BOOTSTRAP_ROLE=primary explicitly if it's permanently gone", cfg.PeerHost)
			}
			time.Sleep(bootstrapRetryInterval)
			continue
		}
		if err := runBasebackup(cfg); err != nil {
			if time.Now().After(deadline) {
				return fmt.Errorf("cloning from peer %s: %w", cfg.PeerHost, err)
			}
			logWarn("bootstrap: clone from %s failed (%v) -- retrying", cfg.PeerHost, err)
			time.Sleep(bootstrapRetryInterval)
			continue
		}
		logInfo("bootstrap: peer %s is primary -- bootstrapping as standby succeeded", cfg.PeerHost)
		return nil
	}
}

// bootstrapAsPrimary runs initdb and prepares pg_hba.conf for replication +
// application connections. Database/role creation happens separately, once
// postgres is actually running -- see ensureRoleAndDatabase.
func bootstrapAsPrimary(cfg *Config) error {
	authMode := "trust"
	var pwfile string
	if cfg.PostgresPassword != "" {
		authMode = "scram-sha-256"
		dir, err := pgGuardStagingDir("bootstrap")
		if err != nil {
			return fmt.Errorf("staging initdb password file: %w", err)
		}
		f, err := os.CreateTemp(dir, "pwfile-*")
		if err != nil {
			return fmt.Errorf("creating initdb password file: %w", err)
		}
		pwfile = f.Name()
		defer os.Remove(pwfile)
		if _, err := f.WriteString(cfg.PostgresPassword); err != nil {
			f.Close()
			return fmt.Errorf("writing initdb password file: %w", err)
		}
		f.Close()
	}

	// --auth-local stays trust regardless of authMode: local (Unix-socket)
	// connections only happen from inside the same container/host (e.g.
	// "docker exec ... psql", verify.sh/roundtrip.sh), which is already a
	// trusted boundary -- only --auth-host (real network connections, what
	// Traveler's HA server and replication actually use) needs the password.
	args := []string{"-D", cfg.PGData, "-U", cfg.PostgresUser, "-E", "UTF8", "--auth-local=trust", "--auth-host=" + authMode}
	if pwfile != "" {
		args = append(args, "--pwfile="+pwfile)
	}
	cmd := exec.Command(pgTool(cfg, "initdb"), args...)
	if out, err := runLoggedCommand("initdb", cmd); err != nil {
		return fmt.Errorf("initdb failed: %w: %s", err, out)
	}
	logInfo("bootstrap: initdb complete at %s (auth=%s)", cfg.PGData, authMode)

	if err := writeBootstrapHBA(cfg, authMode); err != nil {
		return fmt.Errorf("configuring pg_hba.conf: %w", err)
	}
	if err := writeBootstrapPostgresConf(cfg); err != nil {
		return fmt.Errorf("configuring postgresql.conf: %w", err)
	}
	return nil
}

// writeBootstrapPostgresConf enables wal_log_hints -- required for
// pg_rewind (README's Rejoin/Resync sections) to run at all: without it
// (or --data-checksums, a heavier alternative this project doesn't use, as
// it also enables page-level checksum verification on every read/write),
// Postgres refuses to run pg_rewind entirely, and rejoinAsStandby silently
// falls back to a full pg_basebackup re-clone every time -- confirmed as
// what actually happened before this was added: the pg_rewind fast path in
// the Rejoin diagram was unreachable in every real test run this session,
// since nothing ever enabled its prerequisite.
//
// Written once, at first bootstrap -- pg_basebackup clones the whole data
// directory (postgresql.conf included), so every standby, and every former
// standby that's since been promoted, inherits it automatically. No need
// to set it again anywhere else.
func writeBootstrapPostgresConf(cfg *Config) error {
	confPath := filepath.Join(cfg.PGData, "postgresql.conf")
	f, err := os.OpenFile(confPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n# added by pg-guard bootstrap -- required for pg_rewind\nwal_log_hints = on\n")
	return err
}

// writeBootstrapHBA grants replication access to just the configured peer
// (not "all") plus a general entry for application connections.
//
// The peer is resolved to an IP and written as a /32, not left as a
// hostname: Postgres's hostname matching in pg_hba.conf works by
// reverse-resolving the *connecting client's* IP and comparing that name
// against the rule (then forward-confirming) -- it does not simply
// forward-resolve the rule's hostname. Docker's reverse DNS on a Compose
// network returns "<service>.<project>_default" (the network name embeds
// the Compose project name), which a plain "pg-traveler-1" entry never
// matches -- confirmed via a real "no pg_hba.conf entry ... forward lookup
// not checked" failure. Resolving to an IP up front sidesteps that
// mismatch entirely (and matches Postgres's own documented preference for
// IP-based rules over hostname-based ones).
func writeBootstrapHBA(cfg *Config, authMode string) error {
	hbaPath := filepath.Join(cfg.PGData, "pg_hba.conf")
	f, err := os.OpenFile(hbaPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	peerAddr := cfg.PeerHost
	if peerAddr == "" {
		peerAddr = "all" // single-node: no peer to scope to
	} else if ips, err := net.LookupHost(cfg.PeerHost); err == nil && len(ips) > 0 {
		peerAddr = ips[0] + "/32"
	} else {
		logWarn("writeBootstrapHBA: could not resolve peer host %s (%v) -- falling back to a hostname-based pg_hba.conf entry, which may not match Postgres's reverse-DNS-based hostname matching", cfg.PeerHost, err)
	}
	_, err = fmt.Fprintf(f,
		"\n# added by pg-guard bootstrap\nhost replication %s %s %s\nhost all all all %s\n",
		cfg.ReplUser, peerAddr, authMode, authMode,
	)
	return err
}

// hbaUpsertLine rewrites hbaFile: any existing line whose trimmed text
// starts with prefix is dropped (stale duplicates included), then
// desiredLine is appended if it wasn't already present verbatim. Reports
// whether the file actually changed, so callers only reload/log when
// something real happened -- most startups are no-ops here.
func hbaUpsertLine(hbaFile, prefix, desiredLine string) (bool, error) {
	raw, err := os.ReadFile(hbaFile)
	if err != nil {
		return false, fmt.Errorf("reading pg_hba.conf: %w", err)
	}

	lines := strings.Split(string(raw), "\n")
	var out []string
	upToDate := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			out = append(out, line)
			continue
		}
		if trimmed == desiredLine {
			upToDate = true
		}
		// Drop any existing line with this prefix -- stale or not, it's
		// superseded by desiredLine appended below.
	}
	if upToDate {
		return false, nil
	}
	out = append(out, desiredLine)

	if err := os.WriteFile(hbaFile, []byte(strings.Join(out, "\n")), 0o600); err != nil {
		return false, fmt.Errorf("writing pg_hba.conf: %w", err)
	}
	return true, nil
}

// ensureReplicationHBA keeps the primary's pg_hba.conf replication grant
// pointed at whichever host is presently configured as PG_GUARD_PEER_HOST,
// re-resolved to its current IP every time this runs (called on every
// primary startup, not just first bootstrap).
//
// Needed because the grant written by writeBootstrapHBA is IP-scoped and
// static: a standby cloned via pg_basebackup inherits its former primary's
// pg_hba.conf verbatim, including a rule scoped to THAT primary's peer at
// the time -- which, after a promotion, is the node's own former self, not
// its new standby. Every switchover/failover/rejoin after the very first
// one would otherwise inherit a stale, backwards-pointing rule and reject
// the real peer's replication connection ("no pg_hba.conf entry ...").
// Confirmed in testing: a rejoin failed this way after a coordinated
// shutdown handed primary to the other node.
//
// Idempotent (a no-op if the correct rule is already present) and applied
// via pg_reload_conf() -- pg_hba.conf changes don't need a restart.
func ensureReplicationHBA(ctx context.Context, conn *pgx.Conn, cfg *Config) error {
	if cfg.PeerHost == "" {
		return nil
	}
	// net.LookupHost has no timeout of its own -- a misbehaving resolver
	// (confirmed in testing: Docker's embedded DNS returns SERVFAIL for a
	// stopped peer container, and the lookup can then take most of a
	// caller's shared context budget) would otherwise starve every
	// subsequent step in ensureRoleAndDatabase, which all share ctx.
	// Resolving is capped independently so a slow/failing DNS lookup can
	// never eat time meant for the role/database checks or CHECKPOINT below.
	lookupCtx, lookupCancel := context.WithTimeout(ctx, peerDNSLookupTimeout)
	ips, err := net.DefaultResolver.LookupHost(lookupCtx, cfg.PeerHost)
	lookupCancel()
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("resolving peer host %s: %w", cfg.PeerHost, err)
	}
	peerAddr := ips[0] + "/32"

	var hbaFile string
	if err := conn.QueryRow(ctx, "SHOW hba_file").Scan(&hbaFile); err != nil {
		return fmt.Errorf("querying hba_file location: %w", err)
	}

	authMode := "trust"
	if cfg.PostgresPassword != "" {
		authMode = "scram-sha-256"
	}
	prefix := fmt.Sprintf("host replication %s ", cfg.ReplUser)
	desiredLine := fmt.Sprintf("%s%s %s", prefix, peerAddr, authMode)

	changed, err := hbaUpsertLine(hbaFile, prefix, desiredLine)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if _, err := conn.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		return fmt.Errorf("reloading config: %w", err)
	}
	logInfo("ensureReplicationHBA: updated pg_hba.conf replication grant for peer %s (%s)", cfg.PeerHost, peerAddr)
	return nil
}

// ensureAppHBA upgrades pg_hba.conf's general application-access line from
// trust to scram-sha-256. Called only by runSetPassword (setpassword.go)
// after successfully setting a password -- deliberately NOT wired into
// ensureRoleAndDatabase's automatic every-startup pass: tying a real
// security-mode change to "a password happens to be present in the
// environment on this particular restart" was more surprising than useful,
// and couldn't cleanly handle rotating an already-scram password anyway
// (the connection applying the change would itself need the new password
// to authenticate under the very mode it's changing). See
// docker/README.md's "Setting a password" section and setpassword.go.
//
// Deliberately one-way: never downgrades back to trust, which would be a
// silent security regression.
func ensureAppHBA(ctx context.Context, conn *pgx.Conn) error {
	var hbaFile string
	if err := conn.QueryRow(ctx, "SHOW hba_file").Scan(&hbaFile); err != nil {
		return fmt.Errorf("querying hba_file location: %w", err)
	}

	const prefix = "host all all all "
	desiredLine := prefix + "scram-sha-256"

	changed, err := hbaUpsertLine(hbaFile, prefix, desiredLine)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if _, err := conn.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		return fmt.Errorf("reloading config: %w", err)
	}
	logInfo("ensureAppHBA: upgraded pg_hba.conf application access to scram-sha-256")
	return nil
}

// ensureRoleAndDatabase is called once, shortly after startup. Idempotently
// creates the configured replication role and application database if they
// don't already exist, and refreshes the replication grant's peer address
// (see ensureReplicationHBA). Guarded by !inRecovery -- never attempted on
// a standby, where these would just fail (read-only).
//
// Deliberately does NOT reuse the app's own pool (db.go's newPool, which
// connects to dbname=cfg.PostgresDB): that pool can never connect until
// PostgresDB already exists, which is exactly the chicken-and-egg problem
// this function exists to resolve. Instead it opens its own short-lived
// connection to the always-present "postgres" maintenance database.
func ensureRoleAndDatabase(ctx context.Context, cfg *Config) error {
	connString := fmt.Sprintf(
		"host=127.0.0.1 port=%d user=%s password=%s dbname=postgres sslmode=disable connect_timeout=5",
		cfg.PGPort, dsnQuote(cfg.PostgresUser), dsnQuote(cfg.PostgresPassword),
	)
	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		return fmt.Errorf("connecting to maintenance database: %w", err)
	}
	defer conn.Close(ctx)

	var inRecovery bool
	if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		return fmt.Errorf("querying pg_is_in_recovery: %w", err)
	}
	if inRecovery {
		return nil
	}

	if err := ensureReplicationHBA(ctx, conn, cfg); err != nil {
		logWarn("ensureRoleAndDatabase: refreshing pg_hba.conf replication grant: %v", err)
	}

	if cfg.ReplUser != cfg.PostgresUser {
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", cfg.ReplUser).Scan(&exists); err != nil {
			logWarn("ensureRoleAndDatabase: checking role %s: %v", cfg.ReplUser, err)
		} else if !exists {
			sql := fmt.Sprintf("CREATE ROLE %s WITH REPLICATION LOGIN", pgIdent(cfg.ReplUser))
			if cfg.ReplPassword != "" {
				sql += " PASSWORD " + pgLiteral(cfg.ReplPassword)
			}
			if _, err := conn.Exec(ctx, sql); err != nil {
				logWarn("ensureRoleAndDatabase: creating role %s: %v", cfg.ReplUser, err)
			} else {
				logInfo("ensureRoleAndDatabase: created replication role %s", cfg.ReplUser)
			}
		}
	}

	if cfg.PostgresDB != "" && cfg.PostgresDB != "postgres" {
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", cfg.PostgresDB).Scan(&exists); err != nil {
			logWarn("ensureRoleAndDatabase: checking database %s: %v", cfg.PostgresDB, err)
		} else if !exists {
			sql := fmt.Sprintf("CREATE DATABASE %s WITH OWNER %s ENCODING 'UTF8'", pgIdent(cfg.PostgresDB), pgIdent(cfg.PostgresUser))
			if _, err := conn.Exec(ctx, sql); err != nil {
				logWarn("ensureRoleAndDatabase: creating database %s: %v", cfg.PostgresDB, err)
			} else {
				logInfo("ensureRoleAndDatabase: created database %s (owner %s)", cfg.PostgresDB, cfg.PostgresUser)
			}
		}
	}

	// CREATE DATABASE generates real WAL (it copies template1's files).
	// Force a checkpoint now rather than leaving it for pg_basebackup's own
	// forced checkpoint to discover -- pg-traveler-1 typically connects
	// within a second of this function returning (it's polling /status,
	// which only starts reporting "primary" once this same DB exists), so
	// without this, that first basebackup's checkpoint has fresh WAL to
	// flush that a truly idle cluster wouldn't have had.
	if _, err := conn.Exec(ctx, "CHECKPOINT"); err != nil {
		logWarn("ensureRoleAndDatabase: CHECKPOINT: %v", err)
	}
	return nil
}

func pgIdent(s string) string   { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func pgLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ensureRoleAndDatabaseBlocking runs ensureRoleAndDatabase, retrying for up
// to 60s since postgres may not have accepted connections yet right after
// startPostgres returns -- each attempt's own connect failure is the
// readiness probe.
//
// Deliberately blocking, called before startAPIServer (not a fire-and-
// forget background goroutine run alongside it): a peer's bootstrap-as-
// standby decides to run pg_basebackup the moment this node's /status
// reports "primary", using PG_GUARD_REPL_USER. If the API were already
// serving requests while this was still creating that very role in the
// background, a peer polling at exactly the wrong moment would see
// "primary" and attempt to connect as a role that doesn't exist yet --
// confirmed in testing (a real "role \"replicator\" does not exist"
// failure on the peer, landing in roughly a one-second window right after
// postgres itself became reachable but before this had finished). Nothing
// external can observe this node as usable until its own setup is done.
//
// Non-fatal on timeout -- idempotent, retried again next restart; skipped
// entirely if startPostgres left postgres stopped (bootstrap/rejoin
// failure).
func ensureRoleAndDatabaseBlocking(cfg *Config) {
	if !postgresRunning.Load() {
		return
	}
	deadline := time.Now().Add(ensureRoleAndDatabaseTimeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), ensureRoleAndDatabaseOpTimeout)
		err := ensureRoleAndDatabase(ctx, cfg)
		cancel()
		if err == nil {
			return
		}
		logDebug("ensureRoleAndDatabase: %v -- retrying", err)
		time.Sleep(bootstrapRetryInterval)
	}
	logWarn("ensureRoleAndDatabase: postgres never became reachable within 60s -- skipped")
}
