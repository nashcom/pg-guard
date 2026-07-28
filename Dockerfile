# pg-guard
#
# The official postgres image is never rebuilt -- pg-guard is a single
# static binary, mounted into an unmodified postgres container at runtime.
# For local dev/test (docker-compose.yml, docker/two-node/), the binary is
# built directly with the Go toolchain and bind-mounted -- this Dockerfile
# isn't part of that loop. It's for producing a standalone pg-guard image
# (e.g. for a registry), not something the compose files here use.
#
# Usage:
#   docker build -t pg-guard:latest .

FROM cgr.dev/chainguard/go:latest AS builder

WORKDIR /build

COPY src/go.mod .
COPY src/*.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o pg-guard .

FROM cgr.dev/chainguard/static:latest

COPY --from=builder /build/pg-guard /usr/local/bin/pg-guard

# Chainguard static runs as nonroot (uid 65532) by default
USER 65532

ENTRYPOINT ["/usr/local/bin/pg-guard"]
