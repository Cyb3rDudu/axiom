# axiom deployment dev surface (#205).
# `make` is DEV ONLY — production installs come from GitHub releases via
# scripts/install_release.sh. Nothing here mutates /opt without
# `make install` (operator-confirmed, see scripts/install_dist.sh).

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD)
DIST    := dist
OS_ARCH := $(shell uname -s | tr '[:upper:]' '[:lower:]')-$(shell uname -m)
# F05 (#299): the canonical binary is `axiom`; `axiom-ng` stays a
# liefervorderfähig alias of the same build generation (same logic via
# internal/cli; the alias adds exactly the deprecation witness call).
AXIOM_BIN := $(DIST)/axiom-$(VERSION)-$(OS_ARCH)
RAG_BIN := $(DIST)/axiom-ng-$(VERSION)-$(OS_ARCH)
ZSTD_BIN := $(dir $(firstword $(wildcard /nix/store/*-zstd-*-bin/bin/zstd)))

LDFLAGS := -X github.com/Cyb3rDudu/axiom/axiom/internal/version.Version=$(VERSION) -X github.com/Cyb3rDudu/axiom/axiom/internal/version.Commit=$(COMMIT) -X github.com/Cyb3rDudu/axiom/axiom/internal/version.BuildType=release

GO_SOURCES := $(wildcard axiom/cmd/axiom/*.go) $(wildcard axiom/cmd/axiom-ng/*.go) $(wildcard axiom/internal/*/*.go) $(wildcard axiom/internal/db/schema/*.sql) axiom/go.mod axiom/go.sum

.PHONY: all build rag compute-worker runner fixer clean install test checksums golden-baseline

all build: rag ## G1: only rag; compute-worker/fixer land in G2

rag: $(AXIOM_BIN) $(RAG_BIN) ## Release builds (axiom + axiom-ng alias) with version stamp

$(AXIOM_BIN): $(GO_SOURCES)
	@mkdir -p "$(DIST)"
	cd axiom && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o '../$(AXIOM_BIN)' ./cmd/axiom
	(cd "$(DIST)" && shasum -a 256 '$(notdir $(AXIOM_BIN))' > '$(notdir $(AXIOM_BIN)).sha256')

$(RAG_BIN): $(GO_SOURCES)
	@mkdir -p "$(DIST)"
	cd axiom && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o '../$(RAG_BIN)' ./cmd/axiom-ng
	(cd "$(DIST)" && shasum -a 256 '$(notdir $(RAG_BIN))' > '$(notdir $(RAG_BIN)).sha256')

compute-worker: ## conda-pack style artifact (micromamba env, relocatable) -> dist/
	PATH="$(ZSTD_BIN):$$PATH" ./scripts/build_runner_artifact.sh $(VERSION)

runner: ## DEPRECATED alias of compute-worker (F10 #304, ADR 0001 §4)
	@echo "make: 'runner' is deprecated — use 'make compute-worker' (ADR 0001: docs/adr/0001-canonical-naming.md)" >&2
	PATH="$(ZSTD_BIN):$$PATH" ./scripts/build_runner_artifact.sh $(VERSION)

fixer: ## autarkic env/+app/ artifact (own venv) -> dist/
	PATH="$(ZSTD_BIN):$$PATH" ./scripts/build_fixer_artifact.sh $(VERSION)

checksums: ## shasum -a 256 sidecar for every dist/ artifact missing one
	@[ -d "$(DIST)" ] || exit 0; cd "$(DIST)" && find . -type f -name '*.sha256' -prune -o -type f -exec sh -c 'for f do f=$${f#./}; [ -f "$$f.sha256" ] || shasum -a 256 "$$f" > "$$f.sha256"; done' sh {} +

clean:
	rm -rf "$(DIST)"

install: ## Operator-gated: dist/ artifacts -> /opt/axiom (asks first)
	./scripts/install_dist.sh rag $(VERSION)

test: ## All suites: fix-convention, Go (vet+test), compute worker, fixer isolation+
	./scripts/test_fix_convention.sh
	cd axiom && go vet ./... && go test ./...
	@[ -x axiom-compute-worker/.venv/bin/python ] || { echo "compute-worker: venv missing — bootstrap first (axiom-compute-worker/.venv)"; exit 1; }
	cd axiom-compute-worker && .venv/bin/python -m pytest -q
	@[ -x axiom/tools/pdf_repair_agent/.venv/bin/python ] || { echo "fixer: venv missing — bootstrap first (axiom/tools/pdf_repair_agent: ./bootstrap.sh)"; exit 1; }
	cd axiom/tools/pdf_repair_agent && .venv/bin/python -m pytest -q

# --- 0.1.18 frozen compatibility baseline (#295) --------------------------

golden-baseline: ## Freeze-bit golden suite: needs the dev env in --release mode
	@bash scripts/dev/golden_baseline.sh
