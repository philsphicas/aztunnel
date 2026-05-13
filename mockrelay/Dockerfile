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
# Copy the workspace + both modules so module resolution works.
COPY go.work ./
COPY go.mod go.sum ./
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
