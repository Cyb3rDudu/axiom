# axiom deployment dev surface (#205).
# `make` is DEV ONLY — production installs come from GitHub releases via
# scripts/install_release.sh. Nothing here mutates /opt without
# `make install` (operator-confirmed, see scripts/install_dist.sh).

VERSION ?= $(shell git describe --tags --always --dirty)
COMMIT  ?= $(shell git rev-parse --short HEAD)
DIST    := dist
OS_ARCH := $(shell uname -s | tr '[:upper:]' '[:lower:]')-$(shell uname -m)
RAG_BIN := $(DIST)/axiom-ng-$(VERSION)-$(OS_ARCH)

LDFLAGS := -X github.com/Cyb3rDudu/axiom/axiom_ng/internal/version.Version=$(VERSION) -X github.com/Cyb3rDudu/axiom/axiom_ng/internal/version.Commit=$(COMMIT) -X github.com/Cyb3rDudu/axiom/axiom_ng/internal/version.BuildType=release

GO_SOURCES := $(wildcard axiom_ng/cmd/axiom-ng/*.go) $(wildcard axiom_ng/internal/*/*.go)

.PHONY: all build rag runner fixer clean install test checksums

all build: rag ## G1: only rag; runner/fixer land in G2

rag: $(RAG_BIN) ## Release build of the Go binary with version stamp

$(RAG_BIN): $(GO_SOURCES)
	@mkdir -p "$(DIST)"
	cd axiom_ng && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o '../$(RAG_BIN)' ./cmd/axiom-ng
	shasum -a 256 '$(RAG_BIN)' > '$(RAG_BIN).sha256'

runner:
	@echo "runner packaging: pending G2 (#205)"

fixer:
	@echo "fixer packaging: pending G2 (#205)"

checksums: ## shasum -a 256 sidecar for every dist/ artifact missing one
	@find "$(DIST)" -type f -name '*.sha256' -prune -o -type f -print0 | xargs -0 -I{} sh -c 'shasum -a 256 "$1" > "$1.sha256"' _ {}

clean:
	rm -rf "$(DIST)"

install: ## Operator-gated: dist/ artifacts -> /opt/axiom (asks first)
	./scripts/install_dist.sh

test: ## All suites: Go (vet+test), runner, fixer isolation+
	cd axiom_ng && go vet ./... && go test ./...
	cd axiom_ng_runner && .venv/bin/python -m pytest -q
	cd axiom_ng/tools/pdf_repair_agent && .venv/bin/python -m pytest -q
