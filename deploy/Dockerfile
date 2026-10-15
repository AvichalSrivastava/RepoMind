# syntax=docker/dockerfile:1.7

# -------- build stage --------
# Pin to the same Go toolchain the module requires (see go.mod).
# Alpine variant is fine because CGO is disabled (pure-Go binary).
FROM golang:1.25-alpine AS builder

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOFLAGS=-trimpath

WORKDIR /src

# --- Corporate TLS interception (Netskope / Zscaler / etc.) ---
# Some corporate networks MITM outbound HTTPS to inspect traffic. The
# Alpine build container doesn't trust the corporate root CA, so any
# outbound TLS call (proxy.golang.org, sum.golang.org, etc.) fails
# with `x509: certificate signed by unknown authority`.
#
# The fix: COPY in a developer-exported CA bundle and append it to
# the system trust BEFORE go mod download.
#
# The file `secrets/corporate-ca-bundle.crt` MUST exist (gitignored
# via secrets/*). It can be:
#   * Populated with corporate root CAs (see scripts/export-corporate-ca.ps1
#     or the project README) — required behind Netskope / Zscaler / etc.
#   * Empty — for devs on networks without TLS interception (the
#     update-ca-certificates step harmlessly ignores empty files).
#
# Create an empty one with: `New-Item secrets\corporate-ca-bundle.crt`
COPY secrets/corporate-ca-bundle.crt /usr/local/share/ca-certificates/corporate-ca-bundle.crt
RUN apk add --no-cache ca-certificates && update-ca-certificates

# Cache go modules separately for fast incremental builds
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source
COPY . .

ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/repomind \
        ./cmd/repomind

# Backfill utility (cmd/backfill-sprint) — shares the same module,
# reuses the same corporate-CA and go-mod caches. Bundled into the
# image so operators can run one-off re-ingestions via
# `docker compose run --rm --entrypoint /app/backfill-sprint repomind ...`.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/backfill-sprint \
        ./cmd/backfill-sprint

# One-shot master-page reset (cmd/clean-master-page) — overwrites the
# configured Confluence page with an empty table shell. Bundled for
# operator use via
# `docker compose run --rm --entrypoint /app/clean-master-page repomind ...`.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/clean-master-page \
        ./cmd/clean-master-page

# -------- runtime stage --------
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.source="https://github.com/repomind/repomind" \
      org.opencontainers.image.title="RepoMind" \
      org.opencontainers.image.description="Engineering Intelligence Platform"

USER nonroot:nonroot
WORKDIR /app

COPY --from=builder /out/repomind /app/repomind
COPY --from=builder /out/backfill-sprint /app/backfill-sprint
COPY --from=builder /out/clean-master-page /app/clean-master-page
# Migrations are shipped alongside the binary so an operator can run
# `migrate -path /app/migrations -database ... up` against any DB.
# Prompts are embedded as Go string literals inside each analyzer
# package, so there is no /prompts directory to copy.
COPY --from=builder /src/migrations /app/migrations

# Configs (committed, non-secret): repo-policy.yaml + example envs.
# Only repo-policy.yaml is consumed at runtime by the binary; the
# others live here for operator reference in case the container is
# inspected.
COPY --from=builder /src/configs /app/configs

# Corporate-CA trust for RUNTIME TLS calls (Jira / GitHub / OpenAI /
# Confluence). The builder stage already merged Mozilla + corporate
# CAs into the system bundle via `update-ca-certificates`; we copy
# that merged file in so the running binary's outbound HTTPS calls
# pass through the corporate MITM proxy (Netskope / Zscaler) without
# `x509: certificate signed by unknown authority`. distroless ships
# Mozilla CAs at this same path; we overwrite with the superset.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

EXPOSE 8080
ENTRYPOINT ["/app/repomind"]
