// pg-guard -- auth.go -- optional bearer-token and peer-origin checks for
// the mutating HA API endpoints (POST /api/promote|shutdown|rejoin|
// switchover|maintenance|start|reboot-notice). GET /health, /ready,
// /status, and /metrics never require either -- they're
// liveness/readiness/monitoring surfaces polled by more than just the
// peer (health checks, Prometheus, operators), not the HA control plane.
// Both checks are opt-in and off by default, matching the rest of
// pg-guard's "secure by configuration, not by default friction" pattern
// (PG_GUARD_MTLS_REQUIRE).

package main

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// requireHAAuth wraps a POST /api/* handler with the bearer-token and
// peer-origin checks, in that order (token first -- cheap, no DNS lookups).
func (s *apiServer) requireHAAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := checkAPIToken(s.cfg, r); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}
		if err := checkPeerOrigin(s.cfg, r); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		next(w, r)
	}
}

// checkAPIToken requires "Authorization: Bearer <PG_GUARD_API_TOKEN>" if
// PG_GUARD_API_TOKEN is set; a no-op otherwise. Constant-time comparison so
// a mismatched token can't be brute-forced via response-timing.
func checkAPIToken(cfg *Config, r *http.Request) error {
	if cfg.APIToken == "" {
		return nil
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return fmt.Errorf("missing or malformed Authorization header")
	}
	supplied := strings.TrimPrefix(h, prefix)
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(cfg.APIToken)) != 1 {
		return fmt.Errorf("invalid token")
	}
	return nil
}

// checkPeerOrigin requires the caller's address to resolve-match
// PG_GUARD_PEER_HOST if PG_GUARD_PEER_VERIFY is set to something other than
// "off"; a no-op otherwise.
//
// "ip" forward-resolves PeerHost and compares against the caller's IP -- the
// same pattern already used for pg_hba.conf's replication grant (see
// bootstrap.go's writeBootstrapHBA/ensureReplicationHBA), and the
// recommended mode for Docker Compose: Compose's reverse DNS returns
// "<service>.<project>_default", not a plain service name, so "dns" mode's
// reverse lookup will NOT match PeerHost there -- the exact same failure
// that motivated resolving pg_hba.conf's grant by IP instead of hostname in
// the first place. "dns" is offered for environments with real reverse DNS
// (e.g. native installs on a proper internal DNS zone). "both" requires
// both checks to pass.
func checkPeerOrigin(cfg *Config, r *http.Request) error {
	if cfg.PeerVerify == "off" {
		return nil
	}
	if cfg.PeerHost == "" {
		return fmt.Errorf("peer verification enabled but no peer is configured")
	}

	remoteIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		remoteIP = host
	}

	var ipOK, dnsOK bool
	if cfg.PeerVerify == "ip" || cfg.PeerVerify == "both" {
		ipOK = remoteMatchesPeerIP(cfg.PeerHost, remoteIP)
	}
	if cfg.PeerVerify == "dns" || cfg.PeerVerify == "both" {
		dnsOK = remoteMatchesPeerDNS(cfg.PeerHost, remoteIP)
	}

	switch cfg.PeerVerify {
	case "ip":
		if !ipOK {
			return fmt.Errorf("caller %s does not resolve-match peer %s", remoteIP, cfg.PeerHost)
		}
	case "dns":
		if !dnsOK {
			return fmt.Errorf("caller %s does not reverse-DNS-match peer %s", remoteIP, cfg.PeerHost)
		}
	case "both":
		if !ipOK || !dnsOK {
			return fmt.Errorf("caller %s failed peer verification (ip match=%v, dns match=%v)", remoteIP, ipOK, dnsOK)
		}
	}
	return nil
}

func remoteMatchesPeerIP(peerHost, remoteIP string) bool {
	ips, err := net.LookupHost(peerHost)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if ip == remoteIP {
			return true
		}
	}
	return false
}

func remoteMatchesPeerDNS(peerHost, remoteIP string) bool {
	names, err := net.LookupAddr(remoteIP)
	if err != nil {
		return false
	}
	for _, n := range names {
		n = strings.TrimSuffix(n, ".")
		if n == peerHost || strings.HasPrefix(n, peerHost+".") {
			return true
		}
	}
	return false
}
