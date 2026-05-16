VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS := -trimpath
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
CGO     := $(shell go env CGO_ENABLED)
RACE    := $(if $(filter 1,$(CGO)),-race,)

.PHONY: build test vulncheck clean docker help

.DEFAULT_GOAL := help

build: ## Build the aztunnel-relay binary
	go build $(GOFLAGS) $(LDFLAGS) -o bin/aztunnel-relay ./cmd/aztunnel-relay

test: ## Run tests (with -race if CGO is available)
ifneq ($(RACE),)
	go test -race ./...
else
	@echo "warning: CGO disabled, running tests without -race"
	go test ./...
endif

clean: ## Remove build artifacts
	rm -rf bin/

docker: ## Build the aztunnel-relay Docker image
	docker build --build-arg VERSION=$(VERSION) -t aztunnel-relay -f Dockerfile ..

vulncheck: ## Check Go dependencies for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
