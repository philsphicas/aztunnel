# Dockerfile for aztunnel-relay (mock/dev relay).
# Build context must be the parent aztunnel repo root so the workspace
# resolves the module replace correctly:
#
#   docker build -f mockrelay/Dockerfile -t aztunnel-relay .
#
# (The mockrelay/Makefile `docker` target does this automatically.)
ARG BUILDER_IMAGE=golang:1-bookworm
ARG RUNTIME_IMAGE=debian:bookworm-slim
ARG CGO_ENABLED=0

FROM ${BUILDER_IMAGE} AS builder
ARG CGO_ENABLED
WORKDIR /src
# Pre-stage the workspace file and every workspace member's go.mod /
# go.sum so `go mod download` can resolve the full module graph before
# the rest of the source tree is copied. Order matches go.work's
# `use` directive (., ./e2e, ./e2e/infra, ./mockrelay).
COPY go.work ./
COPY go.mod go.sum ./
COPY e2e/go.mod e2e/go.sum ./e2e/
COPY e2e/infra/go.mod e2e/infra/go.sum ./e2e/infra/
COPY mockrelay/go.mod mockrelay/go.sum ./mockrelay/
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=${CGO_ENABLED} go build -trimpath \
    -ldflags "-X main.version=${VERSION}" \
    -o /aztunnel-relay ./mockrelay/cmd/aztunnel-relay

FROM ${RUNTIME_IMAGE}
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /aztunnel-relay /usr/local/bin/aztunnel-relay
ENTRYPOINT ["aztunnel-relay"]
