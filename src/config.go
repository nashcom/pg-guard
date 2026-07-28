// pg-guard -- config.go -- Runtime configuration loaded entirely from
// environment variables. No config file (see README's Design Goals).
//
// pg-guard always supervises the postgres binary directly -- there is no
// docker-entrypoint.sh dependency, on Linux or anywhere else. A real
// production container was observed running PID 1 as
// "postgres -c ssl=on -c ssl_cert_file=... -c ssl_key_file=..." directly,
// with no entrypoint script involved at all; PGDATA is always assumed to be
// already initialized by something else (see windows/init-traveler-db.cmd
// for the manual/dev equivalent) -- pg-guard never bootstraps a cluster.

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	LogLevel     string        // PG_GUARD_LOG_LEVEL
	LogFormat    string        // PG_GUARD_LOG_FORMAT
	LogFile      string        // PG_GUARD_LOG_FILE (optional; default is stderr)
	PostgresBin  string        // PG_GUARD_POSTGRES_BIN -- path to the postgres binary itself
	PostgresArgs []string      // always ["-D", PGData, <PG_GUARD_EXTRA_ARGS...>]
	PGData       string        // PGDATA (required)
	ShutdownWait time.Duration // PG_GUARD_SHUTDOWN_WAIT

	// Reused from Postgres/libpq -- used for pgx connectivity, not passed
	// as -c flags (postgres reads its own PGPORT etc. from the environment
	// directly; these are pg-guard's own copies for building a libpq
	// connection string).
	PGPort           int    // PGPORT
	PostgresUser     string // POSTGRES_USER
	PostgresPassword string // POSTGRES_PASSWORD
	PostgresDB       string // POSTGRES_DB (defaults to PostgresUser, matching Postgres's own convention)
	ReplUser         string // PG_GUARD_REPL_USER (defaults to PostgresUser)
	ReplPassword     string // PG_GUARD_REPL_PASSWORD (defaults to PostgresPassword)

	// HTTP API (mutating POST /api/* only -- see api.go) / metrics listener
	// (GET /health|/ready|/status|/metrics -- see metrics_server.go), two
	// separate listeners on separate ports so TLS/mTLS can eventually apply
	// to the API port alone without needing path-based exceptions (a single
	// net/http.Server is TLS-or-plaintext for all its routes, not per-route).
	APIPort     int    // PG_GUARD_API_PORT
	APIBind     string // PG_GUARD_API_BIND
	MetricsPort int    // PG_GUARD_METRICS_PORT -- shares APIBind
	APIToken    string // PG_GUARD_API_TOKEN -- gates POST /api/* only; empty disables (see auth.go)
	PeerVerify  string // PG_GUARD_PEER_VERIFY: off|ip|dns|both -- gates POST /api/* only (see auth.go)

	// MetricsMode picks which of the two output mechanisms are active --
	// in most deployments exactly one, not both (Kubernetes: "endpoint",
	// scraped directly; a Docker host with its own node_exporter:
	// "textfile"); "both" stays available for the less common case that
	// wants both at once. MetricsEnabled/TextfileEnabled below are
	// computed from this, not independently configurable -- see
	// loadConfig. /health|/ready|/status are always served regardless of
	// MetricsMode; only GET /metrics and the textfile writer (textfile.go)
	// are gated by it.
	MetricsMode     string // PG_GUARD_METRICS_MODE: endpoint|textfile|both
	MetricsEnabled  bool   // computed: true when MetricsMode is endpoint or both -- gates the GET /metrics route only
	TextfileEnabled bool   // computed: true when MetricsMode is textfile or both -- gates the textfile writer (see textfile.go)

	TextfileDir      string        // PG_GUARD_TEXTFILE_DIR -- destination directory; required when TextfileEnabled
	TextfileInterval time.Duration // PG_GUARD_TEXTFILE_INTERVAL

	// TLS for the API listener (PG_GUARD_API_PORT), peer-to-peer POST
	// /api/* calls, and (via composed -c ssl_cert_file=.../-c
	// ssl_key_file=... args -- see loadConfig) Postgres's own connections.
	// Named after Postgres's actual standard parameter names for this
	// (ssl_cert_file/ssl_key_file/ssl_ca_file, the GUCs -c sets) rather
	// than libpq's client-side PGSSLCERT/PGSSLKEY/PGSSLROOTCERT env vars --
	// those configure a *client's* own cert when connecting outbound, a
	// completely different thing from a server's own listening cert, even
	// though the names look similar. Deliberately NOT applied to the
	// metrics listener -- see api.go's top comment for why the two
	// listeners are separate ports in the first place. TLSEnabled is a
	// switch, not an addition: once a cert/key pair is present, the API
	// port serves TLS only, no plaintext fallback.
	TLSCertFile     string // PG_GUARD_SSL_CERT_FILE
	TLSKeyFile      string // PG_GUARD_SSL_KEY_FILE
	TLSRootCertFile string // PG_GUARD_SSL_CA_FILE -- CA used both to verify inbound client certs (mTLS) and to verify the peer's cert on outbound calls
	TLSEnabled      bool   // computed: true once TLSCertFile and TLSKeyFile are both set
	MTLSRequire     bool   // PG_GUARD_MTLS_REQUIRE -- require + verify a client cert on POST /api/*; requires TLSRootCertFile

	// Peer. PeerHost is empty if it couldn't be derived and wasn't set
	// explicitly -- see the loadConfig comment on why that's a warning, not
	// a fatal error, in this milestone.
	PeerHost        string // PG_GUARD_PEER_HOST
	PeerPort        int    // PG_GUARD_PEER_PORT -- peer's API port (POST /api/promote, /api/reboot-notice)
	PeerMetricsPort int    // PG_GUARD_PEER_METRICS_PORT -- peer's metrics port (GET /health, /status)

	// HA behavior
	ShutdownPolicy    string        // PG_GUARD_SHUTDOWN_POLICY: require-switchover|best-effort|force
	ShutdownMode      string        // PG_GUARD_SHUTDOWN_MODE: switchover|reboot -- see handover.go's coordinateReboot
	RebootGracePeriod time.Duration // PG_GUARD_REBOOT_GRACE_PERIOD -- how long a peer suppresses failover after a reboot notice
	FailoverTimeout   time.Duration // PG_GUARD_FAILOVER_TIMEOUT
	FailoverMode      string        // PG_GUARD_FAILOVER_MODE: automatic|manual

	// Bootstrap (first-run only, see bootstrap.go)
	BootstrapRole string // PG_GUARD_BOOTSTRAP_ROLE: auto|primary|standby

	// Backup (basic pg_basebackup-based orchestration, see backup.go).
	// Exactly one of BackupDir/BackupCommand is the actual destination --
	// enforced mutually exclusive in loadConfig, since a set of one implies
	// the other is meaningless (see runBackup vs. runBackupPipe).
	BackupEnabled  bool          // PG_GUARD_BACKUP_ENABLED -- gates the scheduler only; the on-demand CLI/API trigger always works
	BackupDir      string        // PG_GUARD_BACKUP_DIR -- disk destination, retention-pruned (PG_GUARD_BACKUP_RETAIN)
	BackupCommand  string        // PG_GUARD_BACKUP_COMMAND -- shell command pg_basebackup's tar stream is piped into instead (e.g. a borgbackup/restic/tar+upload invocation); no retention, pg-guard has no visibility into where the data ends up
	BackupInterval time.Duration // PG_GUARD_BACKUP_INTERVAL
	BackupRetain   int           // PG_GUARD_BACKUP_RETAIN -- keep this many newest backups, prune the rest (BackupDir mode only)
}

// varDef documents one environment variable: its name, default value, and
// purpose. It is the single source of truth reused by dumpConfig(),
// printHelp(), and in later milestones any --help-style listing.
type varDef struct {
	name string
	def  string
	desc string
}

var varDefs = []varDef{
	{"PG_GUARD_LOG_LEVEL", "info", "log verbosity: error|warn|info|debug"},
	{"PG_GUARD_LOG_FORMAT", "json", "log output format: json|text"},
	{"PG_GUARD_LOG_FILE", "(unset)", "log file path; default is stderr (a Windows Service has no console, so this is required there)"},
	{"PG_GUARD_POSTGRES_BIN", "(auto-detected)", "path to the postgres binary; auto-detected if unset (Windows: registry; Linux: /usr/lib/postgresql/*/bin, then PATH) -- set explicitly to override or to disambiguate multiple installs"},
	{"PGDATA", "(unset)", "Postgres data directory; required -- must already be initialized"},
	{"PG_GUARD_EXTRA_ARGS", "(unset)", "additional postgres arguments appended after -D <PGDATA>, e.g. \"-c ssl=on -c ssl_cert_file=...\""},
	{"PG_GUARD_SHUTDOWN_WAIT", "300s", "max wait for the child to exit before a forced kill"},
	{"PGPORT", "5432", "local Postgres port, used for pg-guard's own pgx connection"},
	{"POSTGRES_USER", "postgres", "role pg-guard connects as for health checks and metrics"},
	{"POSTGRES_PASSWORD", "(unset)", "password for the above"},
	{"POSTGRES_DB", "(= POSTGRES_USER)", "database pg-guard connects to"},
	{"PG_GUARD_API_PORT", "8080 (or 8443, if TLS is enabled)", "HTTP API port -- POST /api/* only (mutating HA control plane). 8080 always means plain HTTP, 8443 always means TLS -- never both at once, Tomcat-classic-connector style, not a same-port silent switch (see TLS)"},
	{"PG_GUARD_API_BIND", "0.0.0.0", "bind address for both the API and metrics listeners"},
	{"PG_GUARD_METRICS_PORT", "9100", "GET /health|/ready|/status|/metrics listener -- separate from the API port so TLS applies to PG_GUARD_API_PORT alone, never here (matches node_exporter's conventional port)"},
	{"PG_GUARD_METRICS_MODE", "endpoint", "endpoint|textfile|both -- which output mechanism(s) are active; /health|/ready|/status are always served regardless -- see Metrics"},
	{"PG_GUARD_TEXTFILE_DIR", "(unset)", "directory to periodically write pg-guard.prom into (node_exporter textfile-collector format); required when PG_GUARD_METRICS_MODE is textfile or both"},
	{"PG_GUARD_TEXTFILE_INTERVAL", "60s", "how often the textfile collector refreshes pg-guard.prom"},
	{"PG_GUARD_API_TOKEN", "(unset)", "bearer token required on POST /api/* (promote/shutdown/rejoin/switchover/maintenance/start) if set; the metrics listener never requires it"},
	{"PG_GUARD_PEER_VERIFY", "off", "off|ip|dns|both -- additionally require POST /api/* callers to resolve-match PG_GUARD_PEER_HOST; \"ip\" recommended (see auth.go -- \"dns\" is unreliable on Docker Compose networks)"},
	{"PG_GUARD_SSL_CERT_FILE", "(unset)", "path to the certificate; set together with PG_GUARD_SSL_KEY_FILE to switch the API listener, outbound peer calls, and Postgres's own connections from plain to TLS (see TLS). Named after Postgres's own ssl_cert_file GUC, which this also composes onto Postgres's args automatically"},
	{"PG_GUARD_SSL_KEY_FILE", "(unset)", "path to the private key, paired with PG_GUARD_SSL_CERT_FILE; composed onto Postgres's own ssl_key_file automatically"},
	{"PG_GUARD_SSL_CA_FILE", "(unset)", "CA used to verify inbound client certs (PG_GUARD_MTLS_REQUIRE) and the peer's cert on outbound calls; composed onto Postgres's own ssl_ca_file automatically if set"},
	{"PG_GUARD_MTLS_REQUIRE", "false", "require and verify a client certificate on POST /api/* (mutual TLS); requires PG_GUARD_SSL_CERT_FILE/PG_GUARD_SSL_KEY_FILE/PG_GUARD_SSL_CA_FILE all set"},
	{"PG_GUARD_PEER_HOST", "(derived)", "peer hostname; derived from own hostname's -0/-1 suffix if unset"},
	{"PG_GUARD_PEER_PORT", "(= PG_GUARD_API_PORT)", "peer's API port (POST /api/promote, /api/reboot-notice)"},
	{"PG_GUARD_PEER_METRICS_PORT", "(= PG_GUARD_METRICS_PORT)", "peer's metrics port (GET /health, /status)"},
	{"PG_GUARD_REPL_USER", "(= POSTGRES_USER)", "role used for pg_rewind/pg_basebackup against the peer"},
	{"PG_GUARD_REPL_PASSWORD", "(= POSTGRES_PASSWORD)", "password for the above"},
	{"PG_GUARD_SHUTDOWN_POLICY", "require-switchover", "require-switchover|best-effort|force -- see Coordinated Shutdown"},
	{"PG_GUARD_SHUTDOWN_MODE", "switchover", "switchover|reboot -- switchover promotes the peer (today's behavior); reboot notifies the peer to suppress failover for PG_GUARD_REBOOT_GRACE_PERIOD instead, for a planned reboot that comes back as the same role -- see Shutdown Modes"},
	{"PG_GUARD_REBOOT_GRACE_PERIOD", "180s", "how long a peer suppresses automatic failover after a PG_GUARD_SHUTDOWN_MODE=reboot notice, before falling back to normal PG_GUARD_FAILOVER_TIMEOUT-based promotion"},
	{"PG_GUARD_FAILOVER_TIMEOUT", "60s", "how long the peer must be continuously unhealthy before automatic failover promotes"},
	{"PG_GUARD_FAILOVER_MODE", "automatic", "automatic|manual -- manual disables the failover monitor entirely"},
	{"PG_GUARD_BOOTSTRAP_ROLE", "auto", "auto|primary|standby -- overrides the hostname-ordinal tiebreak for first-run bootstrap (see Bootstrap)"},
	{"PG_GUARD_BACKUP_ENABLED", "false", "enable the periodic backup scheduler (see Backup); the on-demand backup command always works regardless"},
	{"PG_GUARD_BACKUP_DIR", "(unset)", "directory to write pg_basebackup archives into, retention-pruned; mutually exclusive with PG_GUARD_BACKUP_COMMAND"},
	{"PG_GUARD_BACKUP_COMMAND", "(unset)", "shell command to pipe the pg_basebackup tar stream into instead of a directory (e.g. a borgbackup/restic/tar+upload invocation) -- no retention, mutually exclusive with PG_GUARD_BACKUP_DIR"},
	{"PG_GUARD_BACKUP_INTERVAL", "24h", "how often the backup scheduler runs, when PG_GUARD_BACKUP_ENABLED=true"},
	{"PG_GUARD_BACKUP_RETAIN", "7", "how many of the newest backups to keep; older ones are pruned after each successful backup (PG_GUARD_BACKUP_DIR mode only)"},
}

// envOrDefault returns the environment variable value or a default.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDuration parses a duration from an environment variable.
func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", key, v, err)
	}
	return d, nil
}

// envInt parses an integer from an environment variable.
func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q: %w", key, v, err)
	}
	return n, nil
}

// envBool parses a boolean from an environment variable.
func envBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: invalid boolean %q: %w", key, v, err)
	}
	return b, nil
}

// derivePeerHost implements the README's peer-derivation algorithm: flip a
// trailing "-0"/"-1" ordinal suffix (the Kubernetes StatefulSet pod-naming
// convention). Returns an error if hostname has no recognized suffix --
// callers decide what to do with that (see loadConfig).
func derivePeerHost(hostname string) (string, error) {
	switch {
	case strings.HasSuffix(hostname, "-0"):
		return strings.TrimSuffix(hostname, "-0") + "-1", nil
	case strings.HasSuffix(hostname, "-1"):
		return strings.TrimSuffix(hostname, "-1") + "-0", nil
	default:
		return "", fmt.Errorf("hostname %q has no recognized -0/-1 ordinal suffix", hostname)
	}
}

// loadConfig reads configuration from environment variables.
func loadConfig() (*Config, error) {
	cfg := &Config{
		LogLevel:  envOrDefault("PG_GUARD_LOG_LEVEL", "info"),
		LogFormat: envOrDefault("PG_GUARD_LOG_FORMAT", "json"),
		LogFile:   os.Getenv("PG_GUARD_LOG_FILE"),
	}

	cfg.PostgresBin = os.Getenv("PG_GUARD_POSTGRES_BIN")
	if cfg.PostgresBin == "" {
		detected, err := detectPostgresBin()
		if err != nil {
			return nil, fmt.Errorf("PG_GUARD_POSTGRES_BIN not set and auto-detection failed: %w", err)
		}
		logInfo("auto-detected postgres binary at %s (set PG_GUARD_POSTGRES_BIN to override)", detected)
		cfg.PostgresBin = detected
	}

	cfg.PGData = os.Getenv("PGDATA")
	if cfg.PGData == "" {
		return nil, fmt.Errorf("PGDATA must be set")
	}

	cfg.PostgresArgs = []string{"-D", cfg.PGData}
	if extra := os.Getenv("PG_GUARD_EXTRA_ARGS"); extra != "" {
		cfg.PostgresArgs = append(cfg.PostgresArgs, strings.Fields(extra)...)
	}

	shutdownWait, err := envDuration("PG_GUARD_SHUTDOWN_WAIT", 300*time.Second)
	if err != nil {
		return nil, err
	}
	cfg.ShutdownWait = shutdownWait
	if cfg.ShutdownWait <= 0 {
		return nil, fmt.Errorf("PG_GUARD_SHUTDOWN_WAIT must be a positive duration")
	}

	if cfg.PGPort, err = envInt("PGPORT", 5432); err != nil {
		return nil, err
	}
	cfg.PostgresUser = envOrDefault("POSTGRES_USER", "postgres")
	cfg.PostgresPassword = os.Getenv("POSTGRES_PASSWORD")
	cfg.PostgresDB = envOrDefault("POSTGRES_DB", cfg.PostgresUser)
	cfg.ReplUser = envOrDefault("PG_GUARD_REPL_USER", cfg.PostgresUser)
	cfg.ReplPassword = envOrDefault("PG_GUARD_REPL_PASSWORD", cfg.PostgresPassword)

	// TLS is parsed before PG_GUARD_API_PORT, not after: the API port's own
	// default depends on whether TLS is enabled (8080 plain / 8443 TLS --
	// see below), so TLSEnabled has to be known first.
	cfg.TLSCertFile = os.Getenv("PG_GUARD_SSL_CERT_FILE")
	cfg.TLSKeyFile = os.Getenv("PG_GUARD_SSL_KEY_FILE")
	cfg.TLSRootCertFile = os.Getenv("PG_GUARD_SSL_CA_FILE")
	cfg.TLSEnabled = cfg.TLSCertFile != "" && cfg.TLSKeyFile != ""
	if cfg.MTLSRequire, err = envBool("PG_GUARD_MTLS_REQUIRE", false); err != nil {
		return nil, err
	}
	if cfg.MTLSRequire && (!cfg.TLSEnabled || cfg.TLSRootCertFile == "") {
		return nil, fmt.Errorf("PG_GUARD_MTLS_REQUIRE=true requires PG_GUARD_SSL_CERT_FILE, PG_GUARD_SSL_KEY_FILE, and PG_GUARD_SSL_CA_FILE all set")
	}
	if cfg.TLSEnabled {
		// Stage private copies before anything reads these paths -- see
		// stageTLSFile's doc comment for why (Postgres's own ownership
		// requirement on ssl_key_file, confirmed as a real failure in
		// testing against a bind-mounted cert). 0600 for the key
		// specifically; the cert/CA aren't secret, just need to exist
		// readable in the same private location.
		if cfg.TLSCertFile, err = stageTLSFile(cfg.TLSCertFile, 0o644); err != nil {
			return nil, fmt.Errorf("staging PG_GUARD_SSL_CERT_FILE: %w", err)
		}
		if cfg.TLSKeyFile, err = stageTLSFile(cfg.TLSKeyFile, 0o600); err != nil {
			return nil, fmt.Errorf("staging PG_GUARD_SSL_KEY_FILE: %w", err)
		}
		if cfg.TLSRootCertFile != "" {
			if cfg.TLSRootCertFile, err = stageTLSFile(cfg.TLSRootCertFile, 0o644); err != nil {
				return nil, fmt.Errorf("staging PG_GUARD_SSL_CA_FILE: %w", err)
			}
		}
		if _, err := buildServerTLSConfig(cfg); err != nil {
			return nil, fmt.Errorf("invalid TLS configuration: %w", err)
		}
		// Composed here, not via PG_GUARD_EXTRA_ARGS -- that stays reserved
		// for genuinely arbitrary extra postgres args a deployment might
		// need, not overloaded with something pg-guard already knows how
		// to do transparently: PG_GUARD_SSL_CERT_FILE/_KEY_FILE/_CA_FILE
		// being set is enough on its own to also switch Postgres's own
		// connections to TLS, with no separate flag to remember to set.
		cfg.PostgresArgs = append(cfg.PostgresArgs,
			"-c", "ssl=on",
			"-c", "ssl_cert_file="+cfg.TLSCertFile,
			"-c", "ssl_key_file="+cfg.TLSKeyFile,
		)
		if cfg.TLSRootCertFile != "" {
			cfg.PostgresArgs = append(cfg.PostgresArgs, "-c", "ssl_ca_file="+cfg.TLSRootCertFile)
		}
	}

	// Explicit, Tomcat-classic-connector-style port split, not a silent
	// same-port switch: 8080 only ever means plain HTTP, 8443 only ever
	// means TLS. The two are never both listening at once -- leaving 8080
	// open as a permanent unauthenticated-transport bypass right next to
	// 8443 would undermine the whole point of turning TLS (and especially
	// mTLS) on in the first place. An explicit PG_GUARD_API_PORT still
	// overrides either default.
	defaultAPIPort := 8080
	if cfg.TLSEnabled {
		defaultAPIPort = 8443
	}
	if cfg.APIPort, err = envInt("PG_GUARD_API_PORT", defaultAPIPort); err != nil {
		return nil, err
	}
	cfg.APIBind = envOrDefault("PG_GUARD_API_BIND", "0.0.0.0")
	if cfg.MetricsPort, err = envInt("PG_GUARD_METRICS_PORT", 9100); err != nil {
		return nil, err
	}
	cfg.MetricsMode = envOrDefault("PG_GUARD_METRICS_MODE", "endpoint")
	switch cfg.MetricsMode {
	case "endpoint", "textfile", "both":
	default:
		return nil, fmt.Errorf("PG_GUARD_METRICS_MODE: invalid value %q (must be endpoint, textfile, or both)", cfg.MetricsMode)
	}
	cfg.MetricsEnabled = cfg.MetricsMode == "endpoint" || cfg.MetricsMode == "both"
	cfg.TextfileEnabled = cfg.MetricsMode == "textfile" || cfg.MetricsMode == "both"

	cfg.TextfileDir = os.Getenv("PG_GUARD_TEXTFILE_DIR")
	if cfg.TextfileEnabled && cfg.TextfileDir == "" {
		return nil, fmt.Errorf("PG_GUARD_METRICS_MODE=%s requires PG_GUARD_TEXTFILE_DIR to be set", cfg.MetricsMode)
	}
	if cfg.TextfileInterval, err = envDuration("PG_GUARD_TEXTFILE_INTERVAL", 60*time.Second); err != nil {
		return nil, err
	}
	if cfg.TextfileInterval <= 0 {
		return nil, fmt.Errorf("PG_GUARD_TEXTFILE_INTERVAL must be a positive duration")
	}
	// Both listeners bind on cfg.APIBind -- an identical port would fail at
	// Listen() time anyway ("address already in use"), but only after the
	// process has already gotten partway through startup, and with a
	// generic OS error that doesn't say *which* two settings collided.
	// Catching it here fails loud and specific, before anything else runs.
	if cfg.APIPort == cfg.MetricsPort {
		return nil, fmt.Errorf("PG_GUARD_API_PORT and PG_GUARD_METRICS_PORT must be different (both %d)", cfg.APIPort)
	}
	cfg.APIToken = os.Getenv("PG_GUARD_API_TOKEN")

	cfg.PeerVerify = envOrDefault("PG_GUARD_PEER_VERIFY", "off")
	switch cfg.PeerVerify {
	case "off", "ip", "dns", "both":
	default:
		return nil, fmt.Errorf("PG_GUARD_PEER_VERIFY: invalid value %q (must be off, ip, dns, or both)", cfg.PeerVerify)
	}

	if cfg.PeerPort, err = envInt("PG_GUARD_PEER_PORT", cfg.APIPort); err != nil {
		return nil, err
	}
	if cfg.PeerMetricsPort, err = envInt("PG_GUARD_PEER_METRICS_PORT", cfg.MetricsPort); err != nil {
		return nil, err
	}
	cfg.PeerHost = os.Getenv("PG_GUARD_PEER_HOST")
	if cfg.PeerHost == "" {
		hostname, herr := os.Hostname()
		if herr != nil {
			logWarn("could not look up local hostname to derive peer host: %v -- peer features disabled", herr)
		} else if derived, derr := derivePeerHost(hostname); derr != nil {
			// README describes this as a fail-fast condition for genuine
			// two-node HA deployments. But pg-guard also supports
			// legitimate single-node use (see docker-compose.yml, and
			// native Windows testing) with no peer at all and no "-0"/"-1"
			// in the hostname -- hard-failing startup there would break an
			// already-working, already-verified deployment shape. So this
			// milestone treats an undeducible peer host as "peer checks
			// disabled" (a warning), not fatal -- /status and the
			// postgres_ha_peer_reachable metric just report false/unknown.
			// Revisit if/when single-node vs. two-node becomes an explicit
			// mode rather than something inferred from the hostname.
			logWarn("PG_GUARD_PEER_HOST not set and %v -- peer features disabled", derr)
		} else {
			logInfo("derived peer host %s from local hostname %s (set PG_GUARD_PEER_HOST to override)", derived, hostname)
			cfg.PeerHost = derived
		}
	}

	cfg.ShutdownPolicy = envOrDefault("PG_GUARD_SHUTDOWN_POLICY", "require-switchover")
	switch cfg.ShutdownPolicy {
	case "require-switchover", "best-effort", "force":
	default:
		return nil, fmt.Errorf("PG_GUARD_SHUTDOWN_POLICY: invalid value %q (must be require-switchover, best-effort, or force)", cfg.ShutdownPolicy)
	}

	cfg.ShutdownMode = envOrDefault("PG_GUARD_SHUTDOWN_MODE", "switchover")
	switch cfg.ShutdownMode {
	case "switchover", "reboot":
	default:
		return nil, fmt.Errorf("PG_GUARD_SHUTDOWN_MODE: invalid value %q (must be switchover or reboot)", cfg.ShutdownMode)
	}
	if cfg.RebootGracePeriod, err = envDuration("PG_GUARD_REBOOT_GRACE_PERIOD", 180*time.Second); err != nil {
		return nil, err
	}

	if cfg.FailoverTimeout, err = envDuration("PG_GUARD_FAILOVER_TIMEOUT", 60*time.Second); err != nil {
		return nil, err
	}
	cfg.FailoverMode = envOrDefault("PG_GUARD_FAILOVER_MODE", "automatic")
	switch cfg.FailoverMode {
	case "automatic", "manual":
	default:
		return nil, fmt.Errorf("PG_GUARD_FAILOVER_MODE: invalid value %q (must be automatic or manual)", cfg.FailoverMode)
	}

	cfg.BootstrapRole = envOrDefault("PG_GUARD_BOOTSTRAP_ROLE", "auto")
	switch cfg.BootstrapRole {
	case "auto", "primary", "standby":
	default:
		return nil, fmt.Errorf("PG_GUARD_BOOTSTRAP_ROLE: invalid value %q (must be auto, primary, or standby)", cfg.BootstrapRole)
	}

	if cfg.BackupEnabled, err = envBool("PG_GUARD_BACKUP_ENABLED", false); err != nil {
		return nil, err
	}
	cfg.BackupDir = os.Getenv("PG_GUARD_BACKUP_DIR")
	cfg.BackupCommand = os.Getenv("PG_GUARD_BACKUP_COMMAND")
	if cfg.BackupDir != "" && cfg.BackupCommand != "" {
		return nil, fmt.Errorf("PG_GUARD_BACKUP_DIR and PG_GUARD_BACKUP_COMMAND are mutually exclusive -- set only one")
	}
	if cfg.BackupEnabled && cfg.BackupDir == "" && cfg.BackupCommand == "" {
		return nil, fmt.Errorf("PG_GUARD_BACKUP_ENABLED=true requires PG_GUARD_BACKUP_DIR or PG_GUARD_BACKUP_COMMAND to be set")
	}
	if cfg.BackupInterval, err = envDuration("PG_GUARD_BACKUP_INTERVAL", 24*time.Hour); err != nil {
		return nil, err
	}
	if cfg.BackupInterval <= 0 {
		return nil, fmt.Errorf("PG_GUARD_BACKUP_INTERVAL must be a positive duration")
	}
	if cfg.BackupRetain, err = envInt("PG_GUARD_BACKUP_RETAIN", 7); err != nil {
		return nil, err
	}
	if cfg.BackupRetain < 1 {
		return nil, fmt.Errorf("PG_GUARD_BACKUP_RETAIN must be at least 1")
	}

	return cfg, nil
}

// dumpConfig writes the active configuration to stdout as plain text,
// bypassing JSON log mode -- this is operator-facing startup output, not an
// operational log event. Secret-looking values (PASSWORD/TOKEN in the name)
// are redacted rather than printed -- this applies to PG_GUARD_API_TOKEN
// same as POSTGRES_PASSWORD/PG_GUARD_REPL_PASSWORD.
func dumpConfig(cfg *Config) {
	fmt.Println("pg-guard configuration:")
	for _, vd := range varDefs {
		current := os.Getenv(vd.name)
		switch {
		case current == "":
			current = vd.def + " (default)"
		case isSecretVar(vd.name):
			current = "(set, redacted)"
		}
		fmt.Printf("  %-22s %-28s %s\n", vd.name, current, vd.desc)
	}
}

func isSecretVar(name string) bool {
	return strings.Contains(name, "PASSWORD") || strings.Contains(name, "TOKEN")
}
