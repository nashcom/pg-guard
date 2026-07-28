#!/bin/bash

# Generates a small local CA (10 years) plus a leaf/server cert signed by
# it (2 years), for local TLS testing of the two-node stack in this
# directory. The CA is idempotent -- reused if it already exists, so
# re-running this only rotates the leaf cert, the same way you'd reissue a
# shorter-lived cert against a stable long-lived trust root in practice.
# The leaf covers both pg-traveler nodes + localhost, and both serverAuth
# and clientAuth (see ../README.md's TLS section -- the same cert is
# presented as this node's client certificate for outbound peer calls
# under PG_GUARD_MTLS_REQUIRE, not just served to inbound connections).
#
# Not something pg-guard itself does at runtime -- it only ever consumes an
# already-existing cert (same as it already does for Postgres's own),
# same as verify.sh/roundtrip.sh are separate dev-tooling from pg-guard's
# own code. For a real deployment, replace tls/ with a real CA-issued cert
# covering your actual hostnames instead of running this.
#
# Run: ./generate-certs.sh, then set PG_GUARD_SSL_CERT_FILE/PG_GUARD_SSL_KEY_FILE/PG_GUARD_SSL_CA_FILE in
# .env -- see .env.example -- before "docker compose up -d --force-recreate".
# pg-guard adds the matching ssl_cert_file/ssl_key_file/ssl_ca_file
# arguments to Postgres itself automatically once those are set -- nothing
# else to configure.

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  cat <<'EOF'
Usage: ./generate-certs.sh

Generates a local micro-CA (10y, idempotent -- reused if it already exists)
and a leaf/server cert signed by it (2y, regenerated every run) for local
TLS testing of this two-node stack, covering both pg-traveler nodes plus
localhost. Output: tls/ca.{crt,key}, tls/tls.{crt,key}. Takes no arguments.
Then set PG_GUARD_SSL_CERT_FILE/PG_GUARD_SSL_KEY_FILE/PG_GUARD_SSL_CA_FILE
in .env (see .env.example) before "docker compose up -d --force-recreate".
EOF
  exit 0
fi

set -euo pipefail
cd "$(dirname "$0")"

OUT_DIR="tls"
CA_KEY="${OUT_DIR}/ca.key"
CA_CERT="${OUT_DIR}/ca.crt"
LEAF_KEY="${OUT_DIR}/tls.key"
LEAF_CERT="${OUT_DIR}/tls.crt"
CA_DAYS=3650
LEAF_DAYS=730
SAN="subjectAltName=DNS:pg-traveler-0,DNS:pg-traveler-1,DNS:localhost,IP:127.0.0.1,IP:::1"

mkdir -p "$OUT_DIR"

# MSYS_NO_PATHCONV, scoped to just the two -subj calls below (not exported
# globally): Git Bash on Windows otherwise mangles "/CN=..." as if it were
# a Unix path to translate. A no-op on real Linux/macOS either way.

if [[ -f "$CA_KEY" && -f "$CA_CERT" ]]; then
  echo "Reusing existing CA (${CA_CERT}) -- delete it first to rotate the CA itself"
else
  echo "Generating micro CA (${CA_DAYS} days) -- ${CA_CERT}"
  openssl ecparam -name prime256v1 -genkey -noout -out "$CA_KEY"
  MSYS_NO_PATHCONV=1 openssl req -x509 -new -key "$CA_KEY" -out "$CA_CERT" -days "$CA_DAYS" \
    -subj "/CN=pg-traveler-ca" \
    -addext "basicConstraints=critical,CA:true" \
    -addext "keyUsage=critical,keyCertSign,cRLSign"
fi

echo "Generating leaf cert (${LEAF_DAYS} days), signed by the CA -- ${LEAF_CERT}"
openssl ecparam -name prime256v1 -genkey -noout -out "$LEAF_KEY"
MSYS_NO_PATHCONV=1 openssl req -new -key "$LEAF_KEY" -subj "/CN=pg-traveler" -out "${OUT_DIR}/tls.csr" \
  -addext "$SAN"

# A file inside OUT_DIR, not mktemp's /tmp -- keeps every path in this
# script relative, so nothing here depends on Unix-vs-Windows absolute
# path handling at all.
EXT_FILE="${OUT_DIR}/tls.ext"
printf "%s\nbasicConstraints=CA:false\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth,clientAuth\n" "$SAN" > "$EXT_FILE"

openssl x509 -req -in "${OUT_DIR}/tls.csr" -CA "$CA_CERT" -CAkey "$CA_KEY" \
  -CAcreateserial -out "$LEAF_CERT" -days "$LEAF_DAYS" -extfile "$EXT_FILE"
rm -f "${OUT_DIR}/tls.csr" "${OUT_DIR}/ca.srl" "$EXT_FILE"

chmod 600 "$CA_KEY" "$LEAF_KEY"
chmod 644 "$CA_CERT" "$LEAF_CERT"

echo
echo "${CA_CERT}   -- CA/trust root, use as PG_GUARD_SSL_CA_FILE"
echo "${LEAF_CERT}  -- server+client cert, use as PG_GUARD_SSL_CERT_FILE"
echo "${LEAF_KEY}  -- its key, use as PG_GUARD_SSL_KEY_FILE"
