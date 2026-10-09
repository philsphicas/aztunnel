ARG BUILDER_IMAGE=golang:1.27.1-bookworm
ARG RUNTIME_IMAGE=debian:bookworm-slim
ARG CGO_ENABLED=0

FROM ${BUILDER_IMAGE} AS builder
ARG CGO_ENABLED
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Fail instead of silently compiling with a builder Go that differs from go.work.
# Plain shell only: builder images do not all ship awk.
RUN expected=""; \
    while read -r directive value _; do \
      if [ "$directive" = toolchain ]; then expected="${value%"$(printf '\r')"}"; break; fi; \
    done < go.work; \
    actual="$(go env GOVERSION)"; \
    if [ -z "$expected" ] || [ "$actual" != "$expected" ]; then \
      echo "error: builder Go $actual does not match go.work toolchain '$expected'" >&2; \
      exit 1; \
    fi
ARG VERSION=dev
RUN CGO_ENABLED=${CGO_ENABLED} go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /aztunnel ./cmd/aztunnel

FROM ${RUNTIME_IMAGE}
# Copy CA certificates from the builder for runtime images that may lack them
# (e.g., scratch). For images that already include them, this is a harmless
# overwrite.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /aztunnel /usr/local/bin/aztunnel
ENTRYPOINT ["aztunnel"]
