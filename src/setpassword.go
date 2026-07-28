// pg-guard -- setpassword.go -- `pg-guard -set-password`: an explicit,
// immediately-applied way to set or rotate POSTGRES_PASSWORD without
// restarting the container. See ensureAppHBA's doc comment (bootstrap.go)
// for why this replaced an earlier env-var-plus-restart approach.

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
)

// localSocketDirs are tried in order to reach the local postgres over its
// Unix socket -- always trust under bootstrapAsPrimary's --auth-local=trust
// split, regardless of the current network-facing auth mode, so this works
// whether a password has never been set or is being rotated to a new value.
// "/var/run/postgresql" is the official postgres:18 image's default (see
// docker/README.md); "/tmp" covers a plain upstream build.
var localSocketDirs = []string{"/var/run/postgresql", "/tmp"}

// runSetPassword reads a new password from stdin (not argv -- avoids it
// showing up in "ps"), connects to the local postgres over its Unix socket,
// and:
//   - on a primary: ALTER ROLEs the postgres user and (if distinct)
//     replication role to the new password. This replicates to the
//     standby's catalog automatically -- no need to run this part there.
//   - on any node, primary or standby: upgrades pg_hba.conf's general
//     application-access line to scram-sha-256 and reloads. pg_hba.conf is
//     a local file, not replicated, so this half must run on *each* node
//     to actually take effect there.
//
// Run it against every node in the cluster for full effect -- e.g.
// "echo -n newpass | docker compose exec -T pg-traveler-0 /pg-guard -set-password",
// repeated for pg-traveler-1.
func runSetPassword(cfg *Config) error {
	password, err := readPasswordFromStdin()
	if err != nil {
		return fmt.Errorf("reading new password from stdin: %w", err)
	}
	if password == "" {
		return fmt.Errorf("empty password")
	}

	conn, err := connectLocalSocket(cfg)
	if err != nil {
		return fmt.Errorf("connecting to local postgres over its Unix socket: %w", err)
	}
	defer conn.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), setPasswordOperationTimeout)
	defer cancel()

	var inRecovery bool
	if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		return fmt.Errorf("querying pg_is_in_recovery: %w", err)
	}

	if inRecovery {
		logInfo("set-password: local node is a standby -- role password change replicates from the primary automatically, skipping ALTER ROLE here")
	} else {
		if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER ROLE %s PASSWORD %s", pgIdent(cfg.PostgresUser), pgLiteral(password))); err != nil {
			return fmt.Errorf("setting password for role %s: %w", cfg.PostgresUser, err)
		}
		logInfo("set-password: updated password for role %s", cfg.PostgresUser)

		if cfg.ReplUser != cfg.PostgresUser {
			if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER ROLE %s PASSWORD %s", pgIdent(cfg.ReplUser), pgLiteral(password))); err != nil {
				return fmt.Errorf("setting password for role %s: %w", cfg.ReplUser, err)
			}
			logInfo("set-password: updated password for role %s", cfg.ReplUser)
		}
	}

	if err := ensureAppHBA(ctx, conn); err != nil {
		return fmt.Errorf("upgrading pg_hba.conf application access: %w", err)
	}
	return nil
}

func connectLocalSocket(cfg *Config) (*pgx.Conn, error) {
	var lastErr error
	for _, dir := range localSocketDirs {
		connString := fmt.Sprintf(
			"host=%s port=%d user=%s dbname=postgres sslmode=disable connect_timeout=5",
			dir, cfg.PGPort, dsnQuote(cfg.PostgresUser),
		)
		ctx, cancel := context.WithTimeout(context.Background(), setPasswordSocketDialTimeout)
		conn, err := pgx.Connect(ctx, connString)
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("no local Unix socket found in %v: %w", localSocketDirs, lastErr)
}

func readPasswordFromStdin() (string, error) {
	fmt.Fprint(os.Stderr, "New password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
