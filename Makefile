VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS  := -trimpath
LDFLAGS  := -ldflags "-X main.version=$(VERSION)"
CGO    := $(shell go env CGO_ENABLED)
RACE   := $(if $(filter 1,$(CGO)),-race,)

.PHONY: build test cover lint clean install docker docker-alpine docker-bookworm fmt fmt-check e2e e2e-mock e2e-mock-fast e2e-mock-matrix e2e-azure e2e-docker e2e-setup e2e-attach e2e-status e2e-clean e2e-grant e2e-ci e2e-janitor perf perf-mock perf-azure vulncheck bench bench-azure check-installable help

.DEFAULT_GOAL := help

build: ## Build the aztunnel binary
	go build $(GOFLAGS) $(LDFLAGS) -o bin/aztunnel ./cmd/aztunnel

check-installable: ## Assert root go.mod has no replace directives (required for `go install`)
	@if grep -nE '^replace[[:space:]]|^replace[[:space:]]*\(' go.mod >/dev/null 2>&1; then \
		echo "error: root go.mod contains replace directive(s):" >&2; \
		grep -nE '^replace[[:space:]]|^replace[[:space:]]*\(' go.mod >&2; \
		echo >&2; \
		echo "  \`go install github.com/philsphicas/aztunnel/cmd/aztunnel@<sha>\` will reject this module." >&2; \
		echo "  Move test-only imports of in-repo siblings into the e2e/ module instead." >&2; \
		exit 1; \
	fi
	@echo "ok: root go.mod has no replace directives"

test: ## Run tests (with -race if CGO is available)
ifneq ($(RACE),)
	go test -race ./...
	cd e2e && go test -race ./...
else
	@echo "warning: CGO disabled (no C compiler), running tests without -race"
	go test ./...
	cd e2e && go test ./...
endif

cover: ## Run tests with coverage report
ifneq ($(RACE),)
	go test -race -coverprofile=coverage.txt ./...
else
	@echo "warning: CGO disabled (no C compiler), running coverage without -race"
	go test -coverprofile=coverage.txt ./...
endif
	go tool cover -func=coverage.txt

lint: ## Run linters (go vet + golangci-lint) across root and e2e modules
	go vet ./...
	cd e2e && go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./... && \
		(cd e2e && golangci-lint run ./...); \
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

# `go test` buffers each test binary's combined stdout+stderr until
# that binary exits when multiple packages share one invocation,
# so `-v` does NOT stream output across packages. The Azure run is
# the slow one (~18-22 min with the per-shape echo-workload
# scenarios); keep these as separate invocations so each binary's
# output streams live. Each invocation runs unconditionally and the
# recipe exits with the combined non-zero status if any fails —
# mirroring the multi-package `go test` semantics so a failure in
# azrelay does not mask one in ./e2e/backends/azure/ (see also the
# e2e-docker target's status-capture pattern). The 50 m per-
# invocation timeout for the backend target is set below the 60 m
# GHA job-level `timeout-minutes` to leave headroom for `go test`
# to emit its own per-package goroutine dump on hang; the job-level
# timeout still covers the whole job (checkout, build, azrelay,
# backend) so the workflow envelope remains the outer cap if the
# preceding steps consume too much of it. See golang/go#24929.
e2e-mock: ## Run e2e scenarios against the in-process mock relay (both auth methods, default delay profile)
	cd e2e && go test -tags=e2e -timeout=20m -v ./backends/mock/...

e2e-mock-fast: ## Run mock e2e as fast as possible: zero delay profile, no synthetic token-acquisition cost (both auth methods)
	cd e2e && E2E_DELAY=zero go test -tags=e2e -timeout=20m -v ./backends/mock/...

e2e-mock-matrix: ## Run mock e2e over the full auth x delay-profile matrix (both auth methods x every registered profile)
	cd e2e && E2E_DELAY=all go test -tags=e2e -timeout=40m -v ./backends/mock/...

e2e-azure: build ## Run e2e scenarios against a real Azure Relay namespace (configure via `make e2e-setup`)
	@cd e2e && { \
		status=0; \
		go test -tags=e2e -timeout=20m -v ./azrelay/ || status=$$?; \
		go test -tags=e2e -timeout=50m -v ./backends/azure/... || status=$$?; \
		exit $$status; \
	}

# Run mock and Azure backends; safe to run with `make -j2` because
# the two targets share no infra (mock runs entirely in-process,
# Azure provisions per-test hycos). Serial invocation is also fine.
e2e: e2e-mock e2e-azure ## Run e2e tests against both backends (parallel-safe under make -j)

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
	cd e2e && go run golang.org/x/vuln/cmd/govulncheck@latest ./...

bench: ## Run mock e2e benchmarks once (override BENCH=, COUNT=, BENCHTIME=)
	cd e2e && go test -v -tags=e2e -run='^$$' -bench='$(or $(BENCH),.)' -benchmem \
		-count='$(or $(COUNT),1)' -benchtime='$(or $(BENCHTIME),1s)' \
		./backends/mock/...

# bench-azure runs the e2e benchmark suite against a real Azure
# Relay namespace. Default knobs (COUNT=3, BENCHTIME=10x) are tuned
# for the CI bench workflow's budget: each iteration is a real relay
# round-trip (~0.3-1.0s on Azure today, varies ~3x with namespace
# latency), so 10x per benchmark with 3 samples per cell yields ~2
# minutes of actual benchmark time across the registered sub-benches
# (see e2e/scenarios/bench.go's benchmarkCases — Azure runs the four
# serial ones; the mock-only ConcurrentConnect_N100 is skipped on
# this backend) while giving benchstat enough data without
# amplifying namespace cost disproportionately. Operators wanting
# tighter confidence intervals can run locally with COUNT=6
# BENCHTIME=20x and a longer timeout.
# BENCH defaults to BenchmarkE2E_Azure (the single-mode suite);
# override to run a specific benchmark.
#
# No `build` dep: BenchmarkE2E_Azure exercises real `aztunnel`
# subprocesses (the Azure backend driver in e2e/backends/azure
# matches what production users hit), but TestMain pre-builds
# cmd/aztunnel internally via helpers_test.go:buildAztunnelBinary
# before any benchmark runs, so re-building from this Makefile
# target would just duplicate work TestMain already does.
# `go test -v` is required so that BackendScope-driven b.Skipf
# emissions ("--- SKIP:" markers + the skip reason on the preceding
# log line) actually land in the captured output; without -v Go's
# bench harness suppresses both for skipped sub-benches and the
# scope-skip decision becomes invisible to the operator.
bench-azure: ## Run Azure-live e2e benchmarks (override BENCH=, COUNT=, BENCHTIME=, BENCH_TIMEOUT=)
	cd e2e && go test -v -tags=e2e \
		-run='^$$' -bench='$(or $(BENCH),BenchmarkE2E_Azure)' -benchmem \
		-count='$(or $(COUNT),3)' -benchtime='$(or $(BENCHTIME),10x)' \
		-timeout='$(or $(BENCH_TIMEOUT),30m)' \
		./backends/azure/...

# Performance characterization scenarios (e2e/scenarios/performance.go).
# Filters the shared e2e suite down to the performance scenarios via
# the scenario-name prefix regex so the Core/Topology/Reliability/
# Observability suites do not run. Each scenario emits a
# `workload-summary` log line; pipe to `grep workload-summary` to
# extract the per-shape distribution. Override the scenario filter
# with PERF=<regex>, e.g.
# `make perf-mock PERF=Parallel_ConnPrewarmedEcho_SOCKS5`.
PERF_SCENARIO_FILTER = $(or $(PERF),^(ConnectLatency|ShortSession|Serial_Conn|Parallel_Conn))

perf-mock: ## Run performance characterization scenarios against the in-process mock relay
	cd e2e && go test -tags=e2e -timeout=20m -v \
		-run='TestE2E_Mock/$(PERF_SCENARIO_FILTER)' \
		./backends/mock/

# Pass E2E_AUTH=entra (or sas) to pin one auth method and skip the
# other; default runs both. Configure infra with `make e2e-setup`
# first. Timeout matches `e2e-azure` to leave room for ARM
# provisioning + the Performance round budgets.
perf-azure: build ## Run performance characterization scenarios against a real Azure Relay namespace
	cd e2e && go test -tags=e2e -timeout=35m -v \
		-run='TestE2E_Azure/[^/]+/$(PERF_SCENARIO_FILTER)' \
		./backends/azure/

perf: perf-mock perf-azure ## Run performance scenarios against both backends

help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
