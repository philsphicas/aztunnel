VERSION ?= dev
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test cover lint clean install docker docker-alpine docker-bookworm

build:
	go build $(LDFLAGS) -o bin/aztunnel ./cmd/aztunnel

test:
	go test -race ./...

cover:
	go test -coverprofile=coverage.txt ./...
	go tool cover -func=coverage.txt

lint:
	go vet ./...
	golangci-lint run ./...

clean:
	rm -rf bin/ coverage.txt

install:
	go install $(LDFLAGS) ./cmd/aztunnel

docker:
	docker build --build-arg VERSION=$(VERSION) -t aztunnel .

docker-alpine:
	docker build --build-arg VERSION=$(VERSION) \
		--build-arg BUILDER_IMAGE=golang:1.24-alpine \
		--build-arg RUNTIME_IMAGE=alpine \
		-t aztunnel:alpine .

docker-bookworm:
	docker build --build-arg VERSION=$(VERSION) \
		--build-arg BUILDER_IMAGE=golang:1.24-bookworm \
		--build-arg RUNTIME_IMAGE=debian:bookworm-slim \
		-t aztunnel:bookworm .
