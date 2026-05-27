#!/bin/bash
# Deploy Axiom to LXC 107 (production) from MacBook
# Usage: ./deploy.sh [--backend-only] [--frontend-only] [--restart-only]
set -e

TARGET="dudu@192.168.1.107"
REMOTE_SRC="/home/dudu/axiom-src"
NERDCTL="sudo nerdctl"
COMPOSE_FILE="/home/dudu/axiom/docker-compose.prod.yml"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log() { echo -e "${GREEN}[$(date +%H:%M:%S)]${NC} $1"; }
warn() { echo -e "${YELLOW}[$(date +%H:%M:%S)]${NC} $1"; }
err() { echo -e "${RED}[$(date +%H:%M:%S)]${NC} $1"; }

BUILD_BACKEND=true
BUILD_FRONTEND=true
RESTART=true

case "${1:-}" in
    --backend-only)  BUILD_FRONTEND=false ;;
    --frontend-only) BUILD_BACKEND=false ;;
    --restart-only)  BUILD_BACKEND=false; BUILD_FRONTEND=false ;;
esac

# Step 1: Push to GitHub
log "Pushing to GitHub..."
git push 2>/dev/null || warn "git push failed or nothing to push"

# Step 2: Pull on LXC
log "Pulling latest code on LXC..."
ssh "$TARGET" "cd $REMOTE_SRC && git pull"

# Step 3: Build images with nerdctl
# Use CACHEBUST to force the app COPY layer to rebuild
CACHEBUST=$(date +%s)

if [ "$BUILD_BACKEND" = true ]; then
    log "Building backend image..."
    ssh "$TARGET" "cd $REMOTE_SRC && $NERDCTL build --build-arg CACHEBUST=$CACHEBUST -t axiom-backend:local -f axiom_backend/Dockerfile axiom_backend/"
fi

if [ "$BUILD_FRONTEND" = true ]; then
    log "Building frontend image..."
    ssh "$TARGET" "cd $REMOTE_SRC && $NERDCTL build --build-arg CACHEBUST=$CACHEBUST -t axiom-frontend:local -f axiom_frontend/Dockerfile axiom_frontend/"
fi

# Step 4: Restart service
if [ "$RESTART" = true ]; then
    log "Restarting axiom service..."
    ssh "$TARGET" "sudo systemctl restart axiom"

    log "Waiting for backend health check..."
    sleep 15
    HEALTH=$(ssh "$TARGET" "curl -sf http://127.0.0.1:8000/health" 2>/dev/null || echo "FAILED")

    if echo "$HEALTH" | grep -q "healthy"; then
        log "Backend is healthy!"
    else
        err "Health check failed: $HEALTH"
        warn "Check logs: ssh $TARGET '$NERDCTL compose -f $COMPOSE_FILE logs --tail 50'"
        exit 1
    fi
fi

log "Deploy complete."
