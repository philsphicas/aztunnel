VERSION ?= dev
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
CGO    := $(shell go env CGO_ENABLED)
RACE   := $(if $(filter 1,$(CGO)),-race,)

.PHONY: build test cover lint clean install docker docker-alpine docker-bookworm help

.DEFAULT_GOAL := help

build: ## Build the aztunnel binary
	go build $(LDFLAGS) -o bin/aztunnel ./cmd/aztunnel

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
	go install $(LDFLAGS) ./cmd/aztunnel

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

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
