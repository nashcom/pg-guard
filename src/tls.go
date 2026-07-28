// pg-guard -- tls.go -- TLS for the API listener (PG_GUARD_API_PORT) and
// peer-to-peer POST /api/* calls, named after Postgres's own standard
// ssl_cert_file/ssl_key_file/ssl_ca_file GUCs (PG_GUARD_SSL_CERT_FILE/
// PG_GUARD_SSL_KEY_FILE/PG_GUARD_SSL_CA_FILE) -- see config.go's
// Config.TLSEnabled for why those, not libpq's client-side PGSSLCERT/
// PGSSLKEY/PGSSLROOTCERT, which configure something different. Deliberately
// not applied to the metrics listener (PG_GUARD_METRICS_PORT) -- that's the
// whole reason the API and metrics listeners are separate ports, not
// routes on one server (see api.go's top comment).

package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
)

// buildServerTLSConfig loads the cert/key pair the API listener will serve,
// and -- if PG_GUARD_MTLS_REQUIRE is set -- the CA pool used to require and
// verify a client certificate on every inbound POST /api/* connection.
// Called both from loadConfig (to fail loud on a bad cert at startup,
// before anything else has a chance to run with a half-broken config) and
// from startAPIServer (to actually build the listener).
func buildServerTLSConfig(cfg *Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading PG_GUARD_SSL_CERT_FILE/PG_GUARD_SSL_KEY_FILE: %w", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	if cfg.MTLSRequire {
		pool, err := loadCertPool(cfg.TLSRootCertFile)
		if err != nil {
			return nil, fmt.Errorf("loading PG_GUARD_SSL_CA_FILE: %w", err)
		}
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		tlsCfg.ClientCAs = pool
	}

	return tlsCfg, nil
}

// buildClientTLSConfig is the outbound-call counterpart, used for
// peer-to-peer POST /api/* requests (peer.go's peerPromoteHTTPClient/
// peerAPIURL): verifies the peer's server cert against PG_GUARD_SSL_CA_FILE
// if it's set, and -- if mTLS is required -- presents this node's own
// cert/key as its client certificate, since both nodes reuse the identical
// cert setup (matching README's "both nodes run the identical binary").
// Returns nil, nil if TLS isn't configured -- callers fall back to a plain
// http.Client in that case (see peer.go's initPeerHTTPClients).
//
// Deliberately does *not* fall back to the OS trust store when no CA is
// given: for a self-signed/internal cert (this project's realistic case --
// see docker/generate-certs.sh) that would just fail every peer call with a
// confusing "unknown authority" error. Verification only happens when a CA
// is explicitly provided; otherwise the connection is still encrypted, just
// without verifying the peer's identity (InsecureSkipVerify) -- an
// explicit choice, not an accidental gap.
func buildClientTLSConfig(cfg *Config) (*tls.Config, error) {
	if !cfg.TLSEnabled {
		return nil, nil
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if cfg.TLSRootCertFile != "" {
		pool, err := loadCertPool(cfg.TLSRootCertFile)
		if err != nil {
			return nil, fmt.Errorf("loading PG_GUARD_SSL_CA_FILE: %w", err)
		}
		tlsCfg.RootCAs = pool
	} else {
		tlsCfg.InsecureSkipVerify = true
	}

	if cfg.MTLSRequire {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading PG_GUARD_SSL_CERT_FILE/PG_GUARD_SSL_KEY_FILE for outbound mTLS: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	return tlsCfg, nil
}

// stageTLSFile copies src into pg-guard's own private "tls" staging
// directory (pgGuardStagingDir, tempdir.go) with the given permissions,
// and returns the new path. Necessary because Postgres refuses to start
// with "ssl=on" if its private key file isn't owned by its own runtime
// user or root (confirmed in testing: "private key file ... must be owned
// by the database user or root") -- a bind-mounted cert/key
// (docker-compose's ./tls mount, or any other external-provisioning path)
// is owned by whatever created it outside the container, which
// essentially never matches. pg-guard itself already runs as Postgres's
// own user (see README's Deployment/Non-root section), so a copy it makes
// is automatically owned correctly -- no host-side chown/permission
// wrangling needed, on any platform. Re-copied fresh on every startup, so
// it's never stale relative to the configured source.
func stageTLSFile(src string, perm os.FileMode) (string, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", src, err)
	}
	dir, err := pgGuardStagingDir("tls")
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, filepath.Base(src))
	if err := os.WriteFile(dst, data, perm); err != nil {
		return "", fmt.Errorf("writing %s: %w", dst, err)
	}
	return dst, nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no valid certificates found in %s", path)
	}
	return pool, nil
}
