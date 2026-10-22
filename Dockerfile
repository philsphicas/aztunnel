# make and the release workflow pass the exact go.work version as GO_VERSION.
ARG GO_VERSION=1
ARG GO_VARIANT=bookworm
ARG RUNTIME_IMAGE=debian:bookworm-slim
ARG CGO_ENABLED=0

FROM golang:${GO_VERSION}-${GO_VARIANT} AS builder
ARG CGO_ENABLED
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=${CGO_ENABLED} go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /aztunnel ./cmd/aztunnel && \
    CGO_ENABLED=${CGO_ENABLED} go test -c -o /trust.test ./internal/trustcheck

FROM ${RUNTIME_IMAGE} AS runtime
# Copy CA certificates from the builder for runtime images that may lack them
# (e.g., scratch). For images that already include them, this is a harmless
# overwrite.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

FROM runtime AS checked
COPY --from=builder /aztunnel /aztunnel
COPY --from=builder /trust.test /trust.test
RUN ["/trust.test", "-test.v", "-ca-bundle=/etc/ssl/certs/ca-certificates.crt", "-artifact=/aztunnel"]

FROM runtime
COPY --from=checked /aztunnel /usr/local/bin/aztunnel
ENTRYPOINT ["aztunnel"]
