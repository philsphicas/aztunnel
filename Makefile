VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS  := -trimpath
LDFLAGS  := -ldflags "-X main.version=$(VERSION)"
CGO    := $(shell go env CGO_ENABLED)
RACE   := $(if $(filter 1,$(CGO)),-race,)

.PHONY: build test cover lint clean install docker docker-alpine docker-bookworm fmt fmt-check e2e e2e-mock e2e-mock-fast e2e-mock-matrix e2e-azure e2e-docker e2e-setup e2e-attach e2e-status e2e-clean e2e-grant e2e-ci e2e-janitor perf perf-mock perf-azure perf-matrix perf-placement perf-placement-azure perf-axes-mock perf-table perf-grid perf-history perf-compare perf-clean-history perf-gate vulncheck check-installable help

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

e2e-mock-matrix: ## Run mock e2e over the full auth x delay-profile matrix (both auth methods x the curated functional delay set: zero + default)
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

# Performance characterization scenarios (e2e/scenarios/performance.go).
# Filters the shared e2e suite down to the performance scenarios via
# the scenario-name prefix regex so the Core/Topology/Reliability/
# Observability suites do not run. Each scenario emits a
# `workload-summary` log line; pipe to `grep workload-summary` to
# extract the per-shape distribution. Override the scenario filter
# with PERF=<regex>, e.g.
# `make perf-mock PERF=Parallel_ConnPrewarmedEcho_SOCKS5`.
#
# The perf -run patterns must skip the axis sub-test layer(s) that the
# backends insert as t.Run wrappers above the scenario name. The two
# backends differ:
#   - Mock OMITS an axis layer for any dimension pinned to one value:
#     auth is a layer only when E2E_AUTH is unset (both sas+entra run),
#     and delay is a layer only when E2E_DELAY names more than one
#     profile (a comma list, or the `all` token, which expands to the
#     functional matrix). So mock needs 0, 1, or 2 layers — derived
#     here as PERF_AXIS_PREFIX_MOCK.
#   - Azure ALWAYS wraps the auth dimension in exactly one t.Run layer
#     (even when E2E_AUTH pins a single method the layer remains) and
#     has no delay axis, so its prefix is a fixed single `[^/]+/`.
# Deriving the mock prefix from the env keeps `-run` matched to the
# real sub-test depth, so `make perf-mock E2E_AUTH=sas` (or
# `E2E_DELAY=all`) selects the intended scenarios instead of skipping
# the filter.
# PERF_ALL_SCENARIOS is the default scenario filter: every perf scenario
# family. PERF=<regex> overrides it; PERF=all (in perf-placement) expands
# back to this set.
comma := ,
empty :=
space := $(empty) $(empty)
# Mock omits a t.Run layer for any dimension pinned to a single value.
# Auth: a layer only when E2E_AUTH is unset. Delay: a layer only when
# more than one DISTINCT profile runs — the `all` token (expands to the
# functional matrix) or a comma list with >1 distinct name. $(sort)
# dedupes like delayProfileNamesFromEnv, so degenerate lists such as
# `default,default` collapse to one word and add no layer.
PERF_MOCK_AUTH_LAYER = $(if $(strip $(E2E_AUTH)),,[^/]+/)
PERF_MOCK_DELAY_LAYER = $(if $(filter all,$(E2E_DELAY)),[^/]+/,$(if $(word 2,$(sort $(subst $(comma),$(space),$(E2E_DELAY)))),[^/]+/,))
PERF_AXIS_PREFIX_MOCK = $(PERF_MOCK_AUTH_LAYER)$(PERF_MOCK_DELAY_LAYER)
PERF_ALL_SCENARIOS = ^(ConnectLatency|ShortSession|Serial_Conn|Parallel_Conn)
PERF_SCENARIO_FILTER = $(or $(PERF),$(PERF_ALL_SCENARIOS))

perf-mock: ## Run performance characterization scenarios against the in-process mock relay
	cd e2e && PERF_MATRIX_BACKEND=mock \
		PERF_MATRIX_GIT_SHA="$$(git rev-parse --short HEAD 2>/dev/null)" \
		go test -tags=e2e -count=1 -timeout=20m -v \
		-run='TestE2E_Mock/$(PERF_AXIS_PREFIX_MOCK)$(PERF_SCENARIO_FILTER)' \
		./backends/mock/

# Pass E2E_AUTH=entra (or sas) to pin one auth method and skip the
# other; default runs both. Configure infra with `make e2e-setup`
# first. Timeout matches `e2e-azure` to leave room for ARM
# provisioning + the Performance round budgets.
perf-azure: build ## Run performance characterization scenarios against a real Azure Relay namespace
	cd e2e && PERF_MATRIX_BACKEND=azure \
		PERF_MATRIX_GIT_SHA="$$(git rev-parse --short HEAD 2>/dev/null)" \
		go test -tags=e2e -count=1 -timeout=35m -v \
		-run='TestE2E_Azure/[^/]+/$(PERF_SCENARIO_FILTER)' \
		./backends/azure/

perf: perf-mock perf-azure ## Run performance scenarios against both backends

# perf-matrix runs the mock perf scenarios and, after the usual
# verbose output streams, re-prints just the rendered PERF MATRIX
# block for convenience. Override the scenario filter with PERF=<regex>
# (same as perf-mock), e.g. `make perf-matrix PERF=Parallel_Conn`.
perf-matrix: SHELL := /bin/bash
perf-matrix: .SHELLFLAGS := -u -o pipefail -c
perf-matrix: ## Run mock perf scenarios and surface the human-readable PERF MATRIX
	@cd e2e && \
		out="$$(mktemp)"; \
		PERF_MATRIX_BACKEND=mock \
		PERF_MATRIX_GIT_SHA="$$(git rev-parse --short HEAD 2>/dev/null)" \
		go test -tags=e2e -count=1 -timeout=20m -v \
			-run='TestE2E_Mock/$(PERF_AXIS_PREFIX_MOCK)$(PERF_SCENARIO_FILTER)' \
			./backends/mock/ | tee "$$out"; \
		status=$$?; \
		echo; \
		awk '/PERF MATRIX/{p=1} p{sub(/^[[:space:]]*[^:]*:[0-9]+: /,""); print} /END PERF MATRIX/{p=0}' "$$out"; \
		rm -f "$$out"; \
		exit $$status

# perf-placement sweeps the PERF MATRIX over the full sender×listener
# placement grid (nine cells: each side near/mid/far from the relay) so
# the cold_rtt / est columns vary by placement. It pins E2E_AUTH=sas
# (placement, not auth, is the axis we want to see vary; placement
# profiles model the SAS path) and runs E2E_DELAY over the grid. By
# default it runs a single est-bearing scenario (Parallel_ConnReusedEcho
# — the only scenario that fills cold, warm, and est in one row) so the
# nine-cell sweep stays fast and the footer can show the 3x3 grid.
# Override with PERF=<regex> for a specific scenario, or PERF=all to sweep
# the FULL perf scenario set across the grid (slower; the footer then
# shows the flat table since a grid needs a single scenario — use
# `make perf-table` / `make perf-grid SCENARIO=<name>` to drill in).
# Subset the grid with PERF_PLACEMENT=<csv> (e.g.
# `make perf-placement PERF_PLACEMENT=nn,nf,fn,ff`). Keep at least two
# placement values: the `[^/]+/` run filter assumes exactly one axis
# layer (the delay axis), which only exists when the delay dimension
# varies — same caveat as perf-mock/perf-azure.
#
# There is a single artifact store: the always-on per-run history dir.
# Every run writes perf-artifacts/history/<backend>-<run id>.jsonl (tagged
# with PERF_MATRIX_BACKEND) — no separate per-target file. Each generation
# target pins a fresh run id, then its footer renders just the file it
# produced; perf-table/perf-grid merge the whole history into one
# comparison view. perf-placement-azure runs the same scenarios against a
# real namespace; Azure has real latency (not a synthetic grid), so its
# axis is the auth cell, not a placement code.
PERF_PLACEMENT ?= nn,nm,nf,mn,mm,mf,fn,fm,ff
PERF_PLACEMENT_SCENARIO = $(if $(filter all,$(PERF)),$(PERF_ALL_SCENARIOS),$(or $(PERF),Parallel_ConnReusedEcho))
PERF_ARTIFACT_DIR ?= perf-artifacts
PERF_HISTORY_DIR ?= $(PERF_ARTIFACT_DIR)/history

# perf_run_id prints a fresh run id in the harness's native format (a
# sortable UTC millisecond timestamp plus a short random suffix), so a
# generation target can pin PERF_MATRIX_RUN_ID and then locate the exact
# history file the run wrote: $(PERF_HISTORY_DIR)/<backend>-<id>.jsonl.
perf_run_id = $$(date -u +%Y%m%dT%H%M%S.%3NZ)-$$(printf '%04x%04x' $$RANDOM $$RANDOM)

perf-placement: SHELL := /bin/bash
perf-placement: .SHELLFLAGS := -u -o pipefail -c
perf-placement: ## Sweep the PERF MATRIX across the 3x3 mock sender/listener placement grid
	@cd e2e && \
		rid="$(perf_run_id)"; \
		hist="$(PERF_HISTORY_DIR)/mock-$$rid.jsonl"; \
		absdir="$(PERF_HISTORY_DIR)"; case "$$absdir" in /*) ;; *) absdir="$$PWD/$$absdir";; esac; \
		PERF_MATRIX_RUN_ID="$$rid" \
		PERF_MATRIX_HISTORY_DIR="$$absdir" \
		PERF_MATRIX_BACKEND=mock \
		PERF_MATRIX_GIT_SHA="$$(git rev-parse --short HEAD 2>/dev/null)" \
		E2E_AUTH=sas E2E_DELAY=$(PERF_PLACEMENT) go test -tags=e2e -count=1 -timeout=20m \
			-run='TestE2E_Mock/[^/]+/$(PERF_PLACEMENT_SCENARIO)' \
			./backends/mock/; \
		status=$$?; \
		echo; \
		if [ $$status -eq 0 ] && [ ! -s "$$hist" ]; then \
			echo "perf-placement: test passed but wrote no history at e2e/$$hist" >&2; \
			status=1; \
		fi; \
		if [ -s "$$hist" ]; then \
			go run ./cmd/perfreport --format auto --mode PortForward "$$hist" || \
				{ rc=$$?; [ $$status -eq 0 ] && status=$$rc; }; \
			echo; \
			echo "history:    e2e/$$hist  (jq-friendly JSON Lines)"; \
			echo "more views: make perf-table   |   make perf-grid METRIC=est|warm|cold MODE=SOCKS5 SCENARIO=<name>"; \
		fi; \
		exit $$status

perf-placement-azure: SHELL := /bin/bash
perf-placement-azure: .SHELLFLAGS := -u -o pipefail -c
perf-placement-azure: build ## Run the perf scenarios against a real Azure Relay namespace (one real placement)
	@cd e2e && \
		rid="$(perf_run_id)"; \
		hist="$(PERF_HISTORY_DIR)/azure-$$rid.jsonl"; \
		absdir="$(PERF_HISTORY_DIR)"; case "$$absdir" in /*) ;; *) absdir="$$PWD/$$absdir";; esac; \
		PERF_MATRIX_RUN_ID="$$rid" \
		PERF_MATRIX_HISTORY_DIR="$$absdir" \
		PERF_MATRIX_BACKEND=azure \
		PERF_MATRIX_GIT_SHA="$$(git rev-parse --short HEAD 2>/dev/null)" \
		go test -tags=e2e -count=1 -timeout=35m \
			-run='TestE2E_Azure/[^/]+/$(PERF_SCENARIO_FILTER)' \
			./backends/azure/; \
		status=$$?; \
		echo; \
		if [ $$status -eq 0 ] && [ ! -s "$$hist" ]; then \
			echo "perf-placement-azure: test passed but wrote no history at e2e/$$hist" >&2; \
			status=1; \
		fi; \
		if [ -s "$$hist" ]; then \
			go run ./cmd/perfreport --format table "$$hist" || \
				{ rc=$$?; [ $$status -eq 0 ] && status=$$rc; }; \
			echo; \
			echo "history:  e2e/$$hist  (jq-friendly JSON Lines)"; \
			echo "compare:  make perf-table   (merges mock + azure history)"; \
		fi; \
		exit $$status

# perf-axes-mock runs the mock backend UNPINNED so every live axis (auth ×
# delay) varies and each row carries the full named-axis set
# (axes={auth,delay}). The reporter then renders auth and delay as their
# own columns, so you can pivot/compare on any single dimension — e.g.
# `--filter delay=ff` to compare entra vs sas at one placement. This is the
# "produce JSON, compare whatever" path; placement runs pin auth and only
# vary delay. Subset with PERF_AXES_DELAY=<csv> and PERF_AXES_SCENARIO. The
# `[^/]+/[^/]+/` run filter assumes exactly two live axis layers (auth and
# delay), so keep both varying.
PERF_AXES_DELAY ?= nn,ff
PERF_AXES_SCENARIO ?= Parallel_ConnReusedEcho
perf-axes-mock: SHELL := /bin/bash
perf-axes-mock: .SHELLFLAGS := -u -o pipefail -c
perf-axes-mock: build ## Sweep mock UNPINNED over auth x delay; render named-axis columns (PERF_AXES_DELAY=<csv> PERF_AXES_SCENARIO=<name>)
	@cd e2e && \
		rid="$(perf_run_id)"; \
		hist="$(PERF_HISTORY_DIR)/mock-$$rid.jsonl"; \
		absdir="$(PERF_HISTORY_DIR)"; case "$$absdir" in /*) ;; *) absdir="$$PWD/$$absdir";; esac; \
		PERF_MATRIX_RUN_ID="$$rid" \
		PERF_MATRIX_HISTORY_DIR="$$absdir" \
		PERF_MATRIX_BACKEND=mock \
		PERF_MATRIX_GIT_SHA="$$(git rev-parse --short HEAD 2>/dev/null)" \
		E2E_DELAY=$(PERF_AXES_DELAY) go test -tags=e2e -count=1 -timeout=20m \
			-run='TestE2E_Mock/[^/]+/[^/]+/$(PERF_AXES_SCENARIO)' \
			./backends/mock/; \
		status=$$?; \
		echo; \
		if [ $$status -eq 0 ] && [ ! -s "$$hist" ]; then \
			echo "perf-axes-mock: test passed but wrote no history at e2e/$$hist" >&2; \
			status=1; \
		fi; \
		if [ -s "$$hist" ]; then \
			go run ./cmd/perfreport --format table "$$hist" || \
				{ rc=$$?; [ $$status -eq 0 ] && status=$$rc; }; \
			echo; \
			echo "history:  e2e/$$hist  (jq-friendly JSON Lines)"; \
			echo "pivot:    go run ./cmd/perfreport --filter delay=ff --mode PortForward $$hist"; \
		fi; \
		exit $$status

# perf-table and perf-grid re-render the accumulated history in
# PERF_HISTORY_DIR without re-running anything — cheap, so iterating on
# views costs nothing. They MERGE every run present and collapse to the
# newest row per cell, so once both perf-placement and perf-placement-azure
# have run, the table shows mock and azure side by side (backend column).
# METRIC selects the grid cell metric (all=composite cold/warm/est); MODE
# narrows to one transport (empty = all modes, the default for table/compare;
# the grid, which needs a single mode, falls back to PortForward when MODE is
# unset); SCENARIO/BACKEND narrow the merged history down to the single
# scenario/backend a grid needs.
METRIC ?= all
MODE ?=
SCENARIO ?=
BACKEND ?=

perf-table: SHELL := /bin/bash
perf-table: .SHELLFLAGS := -u -o pipefail -c
perf-table: ## Re-render the merged run history as the flat matrix table (SCENARIO=<name> BACKEND=mock|azure)
	@cd e2e && shopt -s nullglob && \
		files=($(PERF_HISTORY_DIR)/*.jsonl); \
		[ $${#files[@]} -gt 0 ] || { echo "no history in e2e/$(PERF_HISTORY_DIR); run a perf target first" >&2; exit 1; }; \
		go run ./cmd/perfreport --format table $(if $(SCENARIO),--scenario $(SCENARIO),) $(if $(BACKEND),--backend $(BACKEND),) "$${files[@]}"

perf-grid: SHELL := /bin/bash
perf-grid: .SHELLFLAGS := -u -o pipefail -c
perf-grid: ## Re-render a single mock backend as a 3x3 grid (METRIC=all|est|warm|cold MODE=PortForward|SOCKS5 SCENARIO=<name> BACKEND=mock)
	@cd e2e && shopt -s nullglob && \
		files=($(PERF_HISTORY_DIR)/*.jsonl); \
		[ $${#files[@]} -gt 0 ] || { echo "no history in e2e/$(PERF_HISTORY_DIR); run a perf target first" >&2; exit 1; }; \
		go run ./cmd/perfreport --format grid --metric $(METRIC) --mode $(or $(MODE),PortForward) $(if $(SCENARIO),--scenario $(SCENARIO),) $(if $(BACKEND),--backend $(BACKEND),) "$${files[@]}"

# All perf views read the same store: every perf (and functional) e2e run
# appends a timestamped per-run file under perf-artifacts/history (always-on
# emission), so runs accumulate instead of overwriting. perf-history lists
# what's there; perf-compare diffs two runs (BASE/CAND default to the two
# newest, so re-run a scenario before and after a change then just
# `make perf-compare`). BASE/CAND accept a run id, an unambiguous id prefix,
# or the words latest/previous.
#
# To compare along a different dimension instead of runs, set DIM to an axis
# name (e.g. auth, delay) or backend/scenario/mode, and give BASE/CAND the two
# values (e.g. `make perf-compare DIM=auth BASE=sas CAND=entra`). Non-run
# comparisons match within a single run, so BASE/CAND=previous/latest only make
# sense for the default DIM=run.
BASE ?= previous
CAND ?= latest
DIM ?= run

perf-history: SHELL := /bin/bash
perf-history: .SHELLFLAGS := -u -o pipefail -c
perf-history: ## List every run captured in perf-artifacts/history (one row per cell per run)
	@cd e2e && shopt -s nullglob && \
		files=($(PERF_HISTORY_DIR)/*.jsonl); \
		[ $${#files[@]} -gt 0 ] || { echo "no history in e2e/$(PERF_HISTORY_DIR); run any e2e/perf target first" >&2; exit 1; }; \
		go run ./cmd/perfreport --all-runs $(if $(SCENARIO),--scenario $(SCENARIO),) $(if $(BACKEND),--backend $(BACKEND),) "$${files[@]}"

perf-compare: SHELL := /bin/bash
perf-compare: .SHELLFLAGS := -u -o pipefail -c
perf-compare: ## Diff two runs (BASE=previous CAND=latest); or DIM=auth|delay|backend|... BASE=x CAND=y to compare within a run
	@cd e2e && shopt -s nullglob && \
		files=($(PERF_HISTORY_DIR)/*.jsonl); \
		[ $${#files[@]} -gt 0 ] || { echo "no history in e2e/$(PERF_HISTORY_DIR); run any e2e/perf target first" >&2; exit 1; }; \
		go run ./cmd/perfreport --compare "$(BASE)..$(CAND)" $(if $(filter-out run,$(DIM)),--compare-by $(DIM),) $(if $(SCENARIO),--scenario $(SCENARIO),) $(if $(MODE),--mode $(MODE),) $(if $(BACKEND),--backend $(BACKEND),) "$${files[@]}"

perf-clean-history: SHELL := /bin/bash
perf-clean-history: .SHELLFLAGS := -u -o pipefail -c
perf-clean-history: ## Delete the accumulated per-run history files
	@cd e2e && rm -f $(PERF_HISTORY_DIR)/*.jsonl

# perf-gate is the CI tripwire: diff the two newest runs and exit non-zero
# only if a warm/cold p50 cell regressed by more than FAIL_OVER percent AND
# more than FAIL_MIN_ABS (so a big percent on a tiny baseline can't flake
# it). A single run present (no baseline yet) is a skip, not a failure.
FAIL_OVER ?= 30
FAIL_MIN_ABS ?= 20ms
perf-gate: SHELL := /bin/bash
perf-gate: .SHELLFLAGS := -u -o pipefail -c
perf-gate: ## Fail only on a gross perf regression between the two newest runs (FAIL_OVER=<pct> FAIL_MIN_ABS=<dur> BACKEND=mock|azure)
	@cd e2e && shopt -s nullglob && \
		files=($(PERF_HISTORY_DIR)/*.jsonl); \
		[ $${#files[@]} -gt 0 ] || { echo "no history in e2e/$(PERF_HISTORY_DIR); run any e2e/perf target first" >&2; exit 1; }; \
		go run ./cmd/perfreport --compare "$(BASE)..$(CAND)" --fail-over $(FAIL_OVER) --fail-min-abs $(FAIL_MIN_ABS) \
			$(if $(SCENARIO),--scenario $(SCENARIO),) $(if $(MODE),--mode $(MODE),) $(if $(BACKEND),--backend $(BACKEND),) "$${files[@]}"

help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
