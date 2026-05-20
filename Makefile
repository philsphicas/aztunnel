VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS  := -trimpath
LDFLAGS  := -ldflags "-X main.version=$(VERSION)"
CGO    := $(shell go env CGO_ENABLED)
RACE   := $(if $(filter 1,$(CGO)),-race,)

.PHONY: build test cover lint clean install docker docker-alpine docker-bookworm fmt fmt-check e2e e2e-docker e2e-setup e2e-attach e2e-status e2e-clean e2e-grant e2e-ci e2e-janitor vulncheck bench bench-compare help

.DEFAULT_GOAL := help

build: ## Build the aztunnel binary
	go build $(GOFLAGS) $(LDFLAGS) -o bin/aztunnel ./cmd/aztunnel

test: ## Run tests (with -race if CGO is available)
ifneq ($(RACE),)
	go test -race ./...
else
	@echo "warning: CGO disabled (no C compiler), running tests without -race"
	go test ./...
endif

cover: ## Run tests with coverage report
ifneq ($(RACE),)
	go test -race -coverprofile=coverage.txt ./...
else
	@echo "warning: CGO disabled (no C compiler), running coverage without -race"
	go test -coverprofile=coverage.txt ./...
endif
	go tool cover -func=coverage.txt

lint: ## Run linters (go vet + golangci-lint)
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "warning: golangci-lint not found, skipping (install: https://golangci-lint.run/welcome/install/)"; \
	fi

clean: ## Remove build artifacts
	rm -rf bin/ coverage.txt

install: ## Install to $$GOPATH/bin
	go install $(GOFLAGS) $(LDFLAGS) ./cmd/aztunnel

docker: ## Build Docker image (scratch)
	docker build --build-arg VERSION=$(VERSION) -t aztunnel .

docker-alpine: ## Build Docker image (alpine)
	docker build --build-arg VERSION=$(VERSION) \
		--build-arg BUILDER_IMAGE=golang:1-alpine \
		--build-arg RUNTIME_IMAGE=alpine \
		-t aztunnel:alpine .

docker-bookworm: ## Build Docker image (bookworm)
	docker build --build-arg VERSION=$(VERSION) \
		--build-arg BUILDER_IMAGE=golang:1-bookworm \
		--build-arg RUNTIME_IMAGE=debian:bookworm-slim \
		-t aztunnel:bookworm .

fmt: ## Format markdown and YAML with prettier
	npx --yes prettier --write .

fmt-check: ## Check formatting (same as CI)
	npx --yes prettier --check .

e2e: build ## Run end-to-end tests (configure once via `make e2e-setup`)
	go test -tags=e2e -timeout=40m -v ./e2e/azrelay/ ./e2e/

e2e-docker: ## Run container-to-container e2e tests
	@status=0; \
	docker compose -f docker-compose.e2e.yml up --build --abort-on-container-exit --exit-code-from test-runner || status=$$?; \
	docker compose -f docker-compose.e2e.yml down; \
	exit $$status

e2e-setup: ## Provision per-developer e2e infra and write e2e/.local.json
	cd e2e/infra && go run ./cmd/e2e-infra setup

e2e-attach: ## Record an existing e2e RG/namespace in e2e/.local.json
	@if [ -z "$(RESOURCE_GROUP)" ]; then \
		echo "error: RESOURCE_GROUP is required (example: make e2e-attach RESOURCE_GROUP=aztunnel-e2e)" >&2; \
		exit 2; \
	fi
	cd e2e/infra && RESOURCE_GROUP="$(RESOURCE_GROUP)" RELAY_NAME="$(RELAY_NAME)" go run ./cmd/e2e-infra attach

e2e-status: ## Show persisted config and Azure-side health checks
	cd e2e/infra && go run ./cmd/e2e-infra status

e2e-clean: ## Delete your e2e RG (pass CLEAN_ARGS="--yes [--force]")
	cd e2e/infra && go run ./cmd/e2e-infra clean $(CLEAN_ARGS)

e2e-grant: ## Grant Azure Relay Owner to another principal
	@if [ -z "$(ASSIGNEE)" ]; then \
		echo "error: ASSIGNEE is required (example: make e2e-grant ASSIGNEE=alice@contoso.com)" >&2; \
		exit 2; \
	fi
	cd e2e/infra && go run ./cmd/e2e-infra grant --assignee "$(ASSIGNEE)"

e2e-ci: ## Configure the shared CI e2e infrastructure and secrets
	cd e2e/infra && RESOURCE_GROUP="$(or $(RESOURCE_GROUP),aztunnel-e2e)" go run ./cmd/e2e-infra ci

e2e-janitor: ## Delete orphaned per-invocation hybrid connections older than 4h
	cd e2e/infra && RESOURCE_GROUP="$(or $(RESOURCE_GROUP),aztunnel-e2e)" go run ./cmd/e2e-infra janitor

vulncheck: ## Check Go dependencies for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

bench: ## Run mock e2e benchmarks once (override BENCH=, COUNT=, BENCHTIME=)
	cd mockrelay && go test -run='^$$' -bench='$(or $(BENCH),.)' -benchmem \
		-count='$(or $(COUNT),1)' -benchtime='$(or $(BENCHTIME),1s)' \
		./testharness/mockbackend/...

bench-compare: ## Compare e2e benchmarks across two refs: BASE=<sha> [HEAD=<sha>]
	@if [ -z "$(BASE)" ]; then \
		echo "usage: make bench-compare BASE=<sha> [HEAD=<sha>] [BACKEND=mock|azure] [COUNT=N] [BENCHTIME=...]" >&2; \
		exit 2; \
	fi
	BACKEND='$(or $(BACKEND),mock)' COUNT='$(or $(COUNT),5)' BENCHTIME='$(BENCHTIME)' \
		scripts/bench-compare.sh '$(BASE)' '$(HEAD)'

help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
