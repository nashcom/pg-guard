#!/bin/bash
# pg-guard -- bin/download-postgresql-jdbc.sh -- fetches the pgJDBC driver
# used by ../traveler/PgTravelerProbe.java and ../hatest/HaTest.java into
# this directory (alongside pg-guard's own built binaries -- this is the
# one file here that's a downloaded dependency rather than pg-guard's own
# build output, but keeping every binary artifact in one place beats a
# separate directory for a single file), verified against a pinned SHA-256
# so a corrupted or tampered download fails loudly instead of silently
# landing on the classpath. Idempotent -- skips the download if a file
# already sitting here already matches the expected checksum, so it's safe
# to just always run this before building either Java tool.
#
# The verified download keeps its real, versioned filename (VERSIONED_JAR)
# -- that's the file the checksum actually pins. STABLE_JAR is a symlink
# to it with a fixed, version-less name; everything that references the
# classpath (both READMEs, check.sh) uses STABLE_JAR and never needs to
# change when VERSION is bumped here -- only this script does. Falls back
# to a plain copy if symlink creation fails (e.g. Windows without
# Developer Mode enabled), so this still works either way, just without
# the auto-follow-if-VERSION-changes property in the fallback case.
#
# Usage: ./download-postgresql-jdbc.sh

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  echo "Usage: ./download-postgresql-jdbc.sh"
  echo "Fetches/verifies pgJDBC into this directory. Takes no arguments."
  exit 0
fi

set -euo pipefail
cd "$(dirname "$0")"

VERSION="42.7.8"
VERSIONED_JAR="postgresql-${VERSION}.jar"
STABLE_JAR="postgresql-jdbc.jar"
URL="https://jdbc.postgresql.org/download/postgresql-${VERSION}.jar"
SHA256="2a32a9dcbc42d67a50ad3a0de5efd102c8d2be46720045f2cbd6689f160ab7c7"

sha256_of()
{
  sha256sum "$1" | cut -d' ' -f1
}

if [ -f "$VERSIONED_JAR" ] && [ "$(sha256_of "$VERSIONED_JAR")" = "$SHA256" ]; then
  echo "$VERSIONED_JAR already present and verified (sha256 matches) -- skipping download."
else
  echo "downloading pgJDBC $VERSION from $URL..."
  curl -fsSL -o "$VERSIONED_JAR.tmp" "$URL"

  actual="$(sha256_of "$VERSIONED_JAR.tmp")"
  if [ "$actual" != "$SHA256" ]; then
    echo "FATAL: sha256 mismatch for pgJDBC $VERSION -- refusing to use it" >&2
    echo "  expected: $SHA256" >&2
    echo "  actual:   $actual" >&2
    rm -f "$VERSIONED_JAR.tmp"
    exit 1
  fi

  mv "$VERSIONED_JAR.tmp" "$VERSIONED_JAR"
  echo "$VERSIONED_JAR downloaded and verified (sha256 matches)."
fi

rm -f "$STABLE_JAR"
ln -s "$VERSIONED_JAR" "$STABLE_JAR" 2>/dev/null || true
# ln -s can report success (exit 0) on Windows/Git-Bash without Developer
# Mode enabled while actually just copying the file instead of creating a
# real symlink -- confirmed happening in practice, so its exit status
# alone can't be trusted here. "-L" is the actual symlink test.
if [ -L "$STABLE_JAR" ]; then
  echo "$STABLE_JAR -> $VERSIONED_JAR (symlink)"
else
  echo "WARNING: $STABLE_JAR is a plain copy, not a symlink -- ln -s silently" >&2
  echo "  falls back to copying on Windows without Developer Mode enabled" >&2
  echo "  (Settings -> Privacy & security -> For developers) or without" >&2
  echo "  running elevated. Works the same either way, it just won't" >&2
  echo "  auto-update if VERSION above is bumped later without re-running" >&2
  echo "  this script." >&2
  rm -f "$STABLE_JAR"
  cp "$VERSIONED_JAR" "$STABLE_JAR"
fi
