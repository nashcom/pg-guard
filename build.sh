#!/bin/bash
# pg-guard -- build.sh -- builds the deployable binaries for both
# platforms into bin/, matching what windows/start-pg-guard.cmd
# (bin/pg-guard.exe) and docker/docker-compose.yml (bin/pg-guard-linux)
# expect.

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  cat <<'EOF'
Usage: ./build.sh

Builds bin/pg-guard-linux (linux/amd64) and bin/pg-guard.exe (windows/amd64)
from src/. Takes no arguments. Must be run from the repo root, not src/.
EOF
  exit 0
fi

set -euo pipefail
cd "$(dirname "$0")"

mkdir -p bin

# VERSION comes from "git describe" -- automatically "vX.Y.Z" once a tag
# exists (or "vX.Y.Z-N-gHASH" if N commits past the latest tag), "-dirty"
# appended if the working tree has uncommitted changes; falls back to
# "dev" if run outside a git checkout at all (e.g. a source tarball).
# Explicit -ldflags rather than Go's own -buildvcs stamping, since that's
# deliberately off below (see its own flag for why); this keeps working
# identically regardless.
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.buildDate=$BUILD_DATE"

echo "building bin/pg-guard-linux (linux/amd64)... [$VERSION]"
(cd src && GOOS=linux GOARCH=amd64 go build -buildvcs=false -ldflags="$LDFLAGS" -o ../bin/pg-guard-linux .)

echo "building bin/pg-guard.exe (windows/amd64)... [$VERSION]"
(cd src && GOOS=windows GOARCH=amd64 go build -buildvcs=false -ldflags="$LDFLAGS" -o ../bin/pg-guard.exe .)

echo "done."
ls -la bin/pg-guard.exe bin/pg-guard-linux
