// pg-guard -- peer.go -- peer communication: reachability, status, and the
// promote request used during a coordinated handover (see handover.go).
// /health and /status live on the peer's metrics listener
// (PG_GUARD_PEER_METRICS_PORT) and are always plain HTTP -- see
// config.go's comment on why the metrics listener never gets TLS.
// /api/promote and /api/reboot-notice live on its API listener
// (PG_GUARD_PEER_PORT), and switch to TLS the same way the local API
// listener does (see tls.go, api.go's startAPIServer) once
// PG_GUARD_SSL_CERT_FILE/PG_GUARD_SSL_KEY_FILE are configured -- both
// nodes are assumed to run the identical config (README: "both nodes run
// the identical binary"), so this node's own TLSEnabled/MTLSRequire
// settings are used for the outbound call too.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var peerHTTPClient = &http.Client{Timeout: peerReachabilityTimeout}

// peerPromoteHTTPClient is deliberately separate from peerHTTPClient, with
// a much longer timeout: pg_promote() (called server-side by the peer's
// own /api/promote handler) can legitimately take up to 60s to return,
// unlike the fast /health and /status reachability checks peerHTTPClient
// is tuned for. Reusing the 2s client here caused a real failure --
// the peer's promote genuinely succeeded, but the client gave up waiting
// for the response and coordinateHandover treated it as a failed handover.
var peerPromoteHTTPClient = &http.Client{Timeout: peerPromoteTimeout}

// initPeerHTTPClients rebuilds peerHTTPClient/peerPromoteHTTPClient with a
// TLS-aware transport if TLS is configured -- called once at startup
// (after loadConfig, before anything might call the peer), leaving the
// package-level clients unchanged (and their timeouts intact) for every
// caller in this file. A no-op, plain-HTTP transport if TLS isn't
// configured, matching today's behavior exactly.
func initPeerHTTPClients(cfg *Config) error {
	tlsCfg, err := buildClientTLSConfig(cfg)
	if err != nil {
		return err
	}
	if tlsCfg == nil {
		return nil
	}
	transport := &http.Transport{TLSClientConfig: tlsCfg}
	peerHTTPClient.Transport = transport
	peerPromoteHTTPClient.Transport = transport
	return nil
}

var (
	peerMu       sync.Mutex
	lastPeerSeen time.Time
)

// peerAPIURL builds a URL against the peer's API listener (PG_GUARD_PEER_PORT)
// -- POST /api/* only. https:// once TLS is configured (see tls.go), since
// the peer's own API listener switches the same way this node's does.
func peerAPIURL(cfg *Config, path string) string {
	scheme := "http://"
	if cfg.TLSEnabled {
		scheme = "https://"
	}
	return scheme + net.JoinHostPort(cfg.PeerHost, strconv.Itoa(cfg.PeerPort)) + path
}

// peerMetricsURL builds a URL against the peer's metrics listener
// (PG_GUARD_PEER_METRICS_PORT) -- GET /health|/ready|/status|/metrics only.
func peerMetricsURL(cfg *Config, path string) string {
	return "http://" + net.JoinHostPort(cfg.PeerHost, strconv.Itoa(cfg.PeerMetricsPort)) + path
}

// checkPeerReachable reports whether the peer's own /health endpoint
// responds right now. Returns false immediately, with no network call, if
// no peer host is configured (see config.go's loadConfig -- single-node
// deployments have no peer at all).
func checkPeerReachable(cfg *Config) bool {
	if cfg.PeerHost == "" {
		return false
	}

	resp, err := peerHTTPClient.Get(peerMetricsURL(cfg, "/health"))
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	reachable := resp.StatusCode == http.StatusOK
	if reachable {
		markPeerSeen()
	}
	return reachable
}

// fetchPeerStatus GETs the peer's own /status and decodes it. Used by the
// promote safety guard, coordinateHandover, the startup rejoin check, and
// the automatic-failover monitor -- anywhere pg-guard needs to know what
// the peer currently believes about itself, not just whether it's up.
func fetchPeerStatus(cfg *Config) (*statusResponse, error) {
	if cfg.PeerHost == "" {
		return nil, fmt.Errorf("no peer configured")
	}

	resp, err := peerHTTPClient.Get(peerMetricsURL(cfg, "/status"))
	if err != nil {
		return nil, fmt.Errorf("fetching peer status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer /status returned %d", resp.StatusCode)
	}

	var status statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decoding peer status: %w", err)
	}

	markPeerSeen()
	return &status, nil
}

// requestPeerPromote asks the peer to promote itself, during a
// coordinateHandover sequence. force=true is legitimate here (unlike an
// operator hitting /api/promote directly): coordinateHandover has
// already verified the peer is a healthy standby before calling this, and
// the peer's own safety guard would otherwise see *this* node still
// reporting as primary for the brief window before it finishes stopping.
func requestPeerPromote(cfg *Config) error {
	if cfg.PeerHost == "" {
		return fmt.Errorf("no peer configured")
	}

	req, err := http.NewRequest(http.MethodPost, peerAPIURL(cfg, "/api/promote?force=true"), nil)
	if err != nil {
		return fmt.Errorf("building peer promote request: %w", err)
	}
	// The peer's own PG_GUARD_API_TOKEN, if set -- both nodes share the same
	// value (see README's Configuration table), same as PG_GUARD_REPL_USER.
	if cfg.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	}

	resp, err := peerPromoteHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("requesting peer promote: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("peer promote returned %d: %s", resp.StatusCode, body)
	}
	return nil
}

// notifyPeerReboot tells the peer a planned reboot is starting
// (PG_GUARD_SHUTDOWN_MODE=reboot -- see handover.go's coordinateReboot),
// so its automatic-failover monitor suppresses promotion for
// PG_GUARD_REBOOT_GRACE_PERIOD instead of promoting on the normal
// PG_GUARD_FAILOVER_TIMEOUT (failover.go, api.go's handleRebootNotice).
// Best-effort by design -- coordinateReboot proceeds with the local stop
// either way; a peer that never got the notice just falls back to normal
// failover timing, which is still correct, just without the extended
// grace window.
func notifyPeerReboot(cfg *Config) error {
	if cfg.PeerHost == "" {
		return fmt.Errorf("no peer configured")
	}

	req, err := http.NewRequest(http.MethodPost, peerAPIURL(cfg, "/api/reboot-notice"), nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if cfg.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	}

	resp, err := peerHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("notifying peer: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("peer reboot-notice returned %d: %s", resp.StatusCode, body)
	}
	return nil
}

func markPeerSeen() {
	peerMu.Lock()
	lastPeerSeen = time.Now()
	peerMu.Unlock()
}

// secondsSincePeerSeen returns how long ago the peer was last confirmed
// reachable, or -1 if it has never been seen reachable this run.
func secondsSincePeerSeen() float64 {
	peerMu.Lock()
	defer peerMu.Unlock()
	if lastPeerSeen.IsZero() {
		return -1
	}
	return time.Since(lastPeerSeen).Seconds()
}
