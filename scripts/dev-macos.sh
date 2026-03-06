#!/usr/bin/env bash
# Hybrid dev stack for macOS / Apple Silicon.
# Infra (postgres, nginx, frontend) runs in containers.
# Backend + doc-processor run natively for Metal/MPS GPU access.
#
# Usage:
#   ./scripts/dev-macos.sh start   - start infra containers + native backend
#   ./scripts/dev-macos.sh stop    - stop everything
#   ./scripts/dev-macos.sh backend - start only the native backend (assumes infra is running)

set -euo pipefail
cd "$(dirname "$0")/.."  # repo root

# ── Helpers ──────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
info()  { echo -e "${GREEN}[axiom]${NC} $*"; }
warn()  { echo -e "${YELLOW}[axiom]${NC} $*"; }
error() { echo -e "${RED}[axiom]${NC} $*" >&2; }

# ── Load environment ─────────────────────────────────────────────────
load_env() {
    if [ ! -f .env.macos ]; then
        error ".env.macos not found. Copy from template: cp .env.macos.example .env.macos"
        exit 1
    fi
    set -a
    # shellcheck disable=SC1091
    source .env.macos
    set +a

    # Convert relative paths to absolute
    export AXIOM_DATA_PATH="$(cd "$AXIOM_DATA_PATH" 2>/dev/null && pwd || echo "$PWD/$AXIOM_DATA_PATH")"
    export AXIOM_AI_DATA_PATH="$(cd "$AXIOM_AI_DATA_PATH" 2>/dev/null && pwd || echo "$PWD/$AXIOM_AI_DATA_PATH")"
    export AXIOM_APP_PATH="$(cd "$AXIOM_APP_PATH" 2>/dev/null && pwd || echo "$PWD/$AXIOM_APP_PATH")"

    info "Loaded .env.macos  (device=$PREFERRED_DEVICE_TYPE)"
}

# ── Container runtime detection ──────────────────────────────────────
detect_compose() {
    if command -v docker &>/dev/null && docker compose version &>/dev/null; then
        echo "docker compose"
    elif command -v podman-compose &>/dev/null; then
        echo "podman-compose"
    elif command -v podman &>/dev/null && podman compose version &>/dev/null; then
        echo "podman compose"
    else
        error "No container runtime found. Install Docker, Podman, or OrbStack."
        exit 1
    fi
}

# ── Python venv ──────────────────────────────────────────────────────
ensure_venv() {
    if [ ! -d .venv ]; then
        info "Creating Python venv..."
        python3 -m venv .venv
    fi
    # shellcheck disable=SC1091
    source .venv/bin/activate

    # Quick check: if key deps are missing, install
    if ! python -c "import fastapi" &>/dev/null; then
        info "Installing Python dependencies..."
        pip install -r axiom_backend/requirements.txt -q
        pip install "transformers<5" -q
    fi
}

# ── Wait for postgres ────────────────────────────────────────────────
wait_for_postgres() {
    info "Waiting for postgres..."
    local retries=30
    while ! pg_isready -h localhost -p 5432 -U axiom_user -q 2>/dev/null; do
        retries=$((retries - 1))
        if [ $retries -le 0 ]; then
            error "Postgres did not become ready in time."
            exit 1
        fi
        sleep 1
    done
    info "Postgres is ready."
}

# ── Commands ──────────────────────────────────────────────────────────
cmd_start() {
    load_env
    local COMPOSE
    COMPOSE=$(detect_compose)

    info "Starting infra containers..."
    $COMPOSE -f docker-compose.macos.yml up -d

    wait_for_postgres
    ensure_venv

    # Ensure data directories exist
    mkdir -p "$AXIOM_DATA_PATH/raw_pdfs" \
             "$AXIOM_DATA_PATH/processed/markdown" \
             "$AXIOM_DATA_PATH/processed/metadata" \
             "$AXIOM_DATA_PATH/processed/images" \
             "$AXIOM_DATA_PATH/vector_store" \
             "$AXIOM_DATA_PATH/markdown_files"

    # Init database tables
    info "Initializing database..."
    (cd axiom_backend && python -m database.init_postgres 2>/dev/null || true)

    # Start doc-processor in background
    info "Starting background document processor..."
    (cd axiom_backend && python -u services/background_document_processor.py &)

    # Start backend (foreground)
    info "Starting backend on :8000  (MPS device)..."
    cd axiom_backend
    exec uvicorn main:app --host 0.0.0.0 --port 8000 --reload
}

cmd_backend() {
    load_env
    ensure_venv

    info "Starting backend on :8000  (MPS device)..."
    cd axiom_backend
    exec uvicorn main:app --host 0.0.0.0 --port 8000 --reload
}

cmd_stop() {
    local COMPOSE
    COMPOSE=$(detect_compose)

    info "Stopping infra containers..."
    $COMPOSE -f docker-compose.macos.yml down

    # Kill native processes
    info "Stopping native backend processes..."
    pkill -f "uvicorn main:app" 2>/dev/null || true
    pkill -f "background_document_processor" 2>/dev/null || true
    info "Done."
}

# ── Main ──────────────────────────────────────────────────────────────
case "${1:-start}" in
    start)   cmd_start   ;;
    stop)    cmd_stop    ;;
    backend) cmd_backend ;;
    *)
        echo "Usage: $0 {start|stop|backend}"
        exit 1
        ;;
esac
