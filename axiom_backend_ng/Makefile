BINARY      := axiom-ng
PKG         := github.com/Cyb3rDudu/axiom/axiom-ng
VERSION_PKG := $(PKG)/internal/version
VERSION     ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Commit=$(COMMIT) \
	-X $(VERSION_PKG).Date=$(DATE)

GO      ?= go
GOFLAGS ?= -trimpath
BIN_DIR := bin
DIST    := dist

.DEFAULT_GOAL := help

.PHONY: help
help: ## List targets
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_%-]+:.*?## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: fmt
fmt: ## gofmt
	$(GO) fmt ./...

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## golangci-lint run (requires golangci-lint installed)
	golangci-lint run ./...

.PHONY: test
test: ## Run unit tests
	$(GO) test $(GOFLAGS) -race -count=1 ./...

.PHONY: cover
cover: ## Run tests with coverage summary
	$(GO) test $(GOFLAGS) -race -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: build
build: ## Build host-platform binary into bin/
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/$(BINARY)

.PHONY: run
run: ## Run the binary with host env
	$(GO) run $(GOFLAGS) -ldflags "$(LDFLAGS)" ./cmd/$(BINARY)

.PHONY: cross
cross: ## Cross-compile for linux/amd64, linux/arm64, darwin/arm64
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)_linux_amd64   ./cmd/$(BINARY)
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)_linux_arm64   ./cmd/$(BINARY)
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)_darwin_arm64  ./cmd/$(BINARY)

.PHONY: docker-build
docker-build: ## Build container image
	docker build -t axiom-ng:$(VERSION) -f Dockerfile \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		.

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) $(DIST) coverage.out
