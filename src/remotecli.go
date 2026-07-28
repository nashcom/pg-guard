// pg-guard -- remotecli.go -- `pg-guard <command> [options]`: a thin CLI
// client for pg-guard's own local HTTP endpoints, so an operator (or a
// script) can run e.g. "docker compose exec pg-traveler-0 /pg-guard
// maintenance" instead of curling by hand. Always talks to 127.0.0.1 --
// this is a same-host/same-container convenience, never a remote call.
// health/ready/status/metrics hit 127.0.0.1:PG_GUARD_METRICS_PORT (no
// token, unauthenticated, plain HTTP always, matching that listener -- see
// metrics_server.go); the mutating commands hit
// 127.0.0.1:PG_GUARD_API_PORT with PG_GUARD_API_TOKEN attached if set (see
// api.go), switching to TLS the same way the listener itself does once
// PG_GUARD_SSL_CERT_FILE/PG_GUARD_SSL_KEY_FILE are set (see
// localAPITLSSettings) -- the API port serves TLS-or-plaintext exclusively
// (tls.go/config.go), never both, so this has to match or every request
// here just gets a connection refused. Prints the response as plain text
// (not raw JSON -- see printAPIResponse), and exits non-zero on a non-2xx
// response.

package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
)

// apiCLICommand maps a bare CLI command name to the HTTP request it
// performs. metricsPort selects which of the two local listeners it
// targets -- true for PG_GUARD_METRICS_PORT (health/ready/status/metrics),
// false for PG_GUARD_API_PORT (everything mutating). unboundedTimeout opts
// out of the default peerPromoteTimeout client timeout (see callLocalAPI)
// -- only "backup" sets it: a pg_basebackup archive of a real database has
// no fixed upper bound the way pg_promote() does, and runBackup (backup.go)
// itself is deliberately not context-bounded either, matching every other
// shelled-out Postgres tool in this codebase (pg_rewind, pg_basebackup-for-
// cloning, initdb).
type apiCLICommand struct {
	name             string
	method           string
	path             string
	metricsPort      bool
	unboundedTimeout bool
}

var apiCLICommands = []apiCLICommand{
	{"health", http.MethodGet, "/health", true, false},
	{"ready", http.MethodGet, "/ready", true, false},
	{"status", http.MethodGet, "/status", true, false},
	{"metrics", http.MethodGet, "/metrics", true, false},
	{"promote", http.MethodPost, "/api/promote", false, false},
	{"shutdown", http.MethodPost, "/api/shutdown", false, false},
	{"rejoin", http.MethodPost, "/api/rejoin", false, false},
	{"switchover", http.MethodPost, "/api/switchover", false, false},
	{"maintenance", http.MethodPost, "/api/maintenance", false, false},
	{"start", http.MethodPost, "/api/start", false, false},
	{"backup", http.MethodPost, "/api/backup", false, true},
}

// findAPICommand looks up name (main's os.Args[1]) against apiCLICommands.
func findAPICommand(name string) *apiCLICommand {
	for i := range apiCLICommands {
		if apiCLICommands[i].name == name {
			return &apiCLICommands[i]
		}
	}
	return nil
}

// runAPICommand performs cmd against the local API and exits -- called
// from main's command dispatch once cmd has already been matched. opts is
// everything after the command name; only -force (meaningful only
// alongside "promote", appending "?force=true" -- the same override
// "POST /api/promote?force=true" already supports) and -json (print the
// raw response instead of reformatting it -- see printAPIResponse) are
// recognized, so a typo'd option fails loudly rather than being ignored.
func runAPICommand(cmd apiCLICommand, opts []string) {
	force := false
	jsonOutput := false
	for _, o := range opts {
		switch o {
		case "-force":
			force = true
		case "-json":
			jsonOutput = true
		default:
			fmt.Fprintf(os.Stderr, "pg-guard: unknown option for %s: %s\n", cmd.name, o)
			os.Exit(1)
		}
	}
	if force && cmd.name != "promote" {
		fmt.Fprintf(os.Stderr, "pg-guard: -force only applies to promote\n")
		os.Exit(1)
	}

	// Deliberately not loadConfig(): that also auto-detects the postgres
	// binary, requires PGDATA, and logs both -- all irrelevant noise for a
	// call that only ever needs a local port (and, for API commands, the
	// token and TLS settings). A quick "pg-guard status" shouldn't fail (or
	// clutter its own output) over postgres-binary detection it has no use
	// for. localAPITLSSettings mirrors just the handful of TLS-relevant
	// fields loadConfig would otherwise derive.
	var port int
	var apiToken string
	var tlsEnabled bool
	var tlsCfg *tls.Config
	var err error
	if cmd.metricsPort {
		port, err = envInt("PG_GUARD_METRICS_PORT", 9100)
	} else {
		tlsEnabled, tlsCfg, err = localAPITLSSettings()
		if err == nil {
			defaultAPIPort := 8080
			if tlsEnabled {
				defaultAPIPort = 8443
			}
			port, err = envInt("PG_GUARD_API_PORT", defaultAPIPort)
		}
		apiToken = os.Getenv("PG_GUARD_API_TOKEN")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pg-guard: config error: %v\n", err)
		os.Exit(1)
	}

	path := cmd.path
	if force {
		path += "?force=true"
	}

	if err := callLocalAPI(port, apiToken, cmd.method, path, jsonOutput, cmd.unboundedTimeout, tlsEnabled, tlsCfg); err != nil {
		fmt.Fprintf(os.Stderr, "pg-guard: %v\n", err)
		os.Exit(1)
	}
}

// localAPITLSSettings reports whether the local API listener is running in
// TLS mode and, if so, the client-side TLS config to reach it with -- read
// directly from the raw PG_GUARD_SSL_*/PG_GUARD_MTLS_REQUIRE env vars
// (mirroring config.go's Config.TLSEnabled) rather than the full
// loadConfig(), per runAPICommand's doc comment above. Delegates the
// actual TLS config construction to buildClientTLSConfig (tls.go) -- the
// same function peer.go uses for outbound peer-to-peer calls, so this
// verifies against PG_GUARD_SSL_CA_FILE (if set) and presents this node's
// own cert as its client certificate under mTLS, identically.
func localAPITLSSettings() (bool, *tls.Config, error) {
	certFile := os.Getenv("PG_GUARD_SSL_CERT_FILE")
	keyFile := os.Getenv("PG_GUARD_SSL_KEY_FILE")
	if certFile == "" || keyFile == "" {
		return false, nil, nil
	}

	mtlsRequire, err := envBool("PG_GUARD_MTLS_REQUIRE", false)
	if err != nil {
		return false, nil, err
	}
	tlsCfg, err := buildClientTLSConfig(&Config{
		TLSEnabled:      true,
		TLSCertFile:     certFile,
		TLSKeyFile:      keyFile,
		TLSRootCertFile: os.Getenv("PG_GUARD_SSL_CA_FILE"),
		MTLSRequire:     mtlsRequire,
	})
	if err != nil {
		return false, nil, err
	}
	return true, tlsCfg, nil
}

// callLocalAPI performs one request against a local listener (API or
// metrics -- see runAPICommand) and prints the response (plain text by
// default, raw JSON if jsonOutput -- see printAPIResponse) to stdout. A
// 70s timeout matches peerPromoteHTTPClient (peer.go) -- pg_promote() can
// legitimately take up to 60s -- unless unbounded is set (only "backup",
// see apiCLICommand's doc comment), which uses no client timeout at all.
// Non-2xx is reported as an error, but the body (usually
// {"error": "..."}) is printed first either way.
func callLocalAPI(port int, apiToken, method, path string, jsonOutput, unbounded, tlsEnabled bool, tlsCfg *tls.Config) error {
	scheme := "http://"
	if tlsEnabled {
		scheme = "https://"
	}
	url := scheme + "127.0.0.1:" + strconv.Itoa(port) + path

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}

	timeout := peerPromoteTimeout
	if unbounded {
		timeout = 0
	}
	client := &http.Client{Timeout: timeout}
	if tlsEnabled {
		client.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if jsonOutput {
		printRaw(body)
	} else {
		printAPIResponse(body)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned %d", method, path, resp.StatusCode)
	}
	return nil
}

// printAPIResponse renders a response body for a human at a terminal, not
// a machine: every mutating endpoint and /status returns a small flat JSON
// object (writeJSON in api.go), which gets unmarshaled generically and
// printed as sorted "key=value" lines -- generic on purpose, so it needs no
// per-endpoint formatting logic and keeps working if a field is added
// later; unpadded so it's grep/cut-friendly. /health, /ready, and /metrics
// aren't JSON at all (plain text) -- json.Unmarshal failing on those is
// exactly the signal to fall back to printing the body as-is. -json (see
// runAPICommand) bypasses this entirely via printRaw, for scripts that
// want the actual API response instead (e.g. to pipe into jq).
func printAPIResponse(body []byte) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		printRaw(body)
		return
	}

	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%v\n", k, obj[k])
	}
}

func printRaw(body []byte) {
	os.Stdout.Write(body)
	if len(body) == 0 || body[len(body)-1] != '\n' {
		fmt.Println()
	}
}
