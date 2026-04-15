# Production Deployment: nerdctl + containerd on Proxmox LXC

This documents the production Axiom deployment on LXC 120 with nerdctl, containerd, buildkit, and an NVIDIA A3000 GPU.

## Architecture

```
MacBook (dev) ──git push + deploy.sh──> LXC 120 (192.168.1.120)
                                         ├── axiom-backend   (nerdctl, GPU, :8000)
                                         ├── axiom-doc-processor (nerdctl, GPU)
                                         ├── axiom-frontend   (nerdctl, :3000)
                                         └── NO nginx (NPM proxies directly)
                                               │
                                         LXC 107 (192.168.1.107)
                                         └── PostgreSQL + pgvector + OpenSearch
```

Nginx Proxy Manager (on carrier) routes `axiom.i.catdev.io` to the LXC.

## LXC Requirements

- Debian 13+ with containerd, nerdctl, and buildkit (buildkitd with `--containerd-worker`)
- NVIDIA GPU passed through with nvidia-container-toolkit
- LXC config: `lxc.apparmor.profile: unconfined`, `lxc.cap.drop:` (empty), `lxc.mount.auto: proc:rw sys:rw`
- LXC cgroup2 device allow: `c 195:* rwm` (nvidia), `c 508:* rwm` (nvidia-uvm), `c 511:* rwm` (nvidia-caps), `c 10:200 rwm` (tun)
- TUN/TAP device bind-mounted from host
- Network access to PostgreSQL (LXC 107) and SearXNG

## Directory Layout on LXC

```
/home/dudu/
├── axiom-src/          # Git clone of the repo (source for builds)
├── axiom/
│   ├── .env            # Production environment variables
│   ├── start-pod.sh    # Pod creation script
│   ├── data/           # Backend data directory
│   └── reports/        # Generated reports
└── ai-models/          # HuggingFace model cache
```

## Initial Setup

### 1. Clone the repo

```bash
git clone git@github.com:Cyb3rDudu/axiom.git ~/axiom-src
```

### 2. Build images

```bash
cd ~/axiom-src
nerdctl build bud -t localhost/axiom-backend:local -f axiom_backend/Dockerfile axiom_backend/
nerdctl build bud -t localhost/axiom-frontend:local -f axiom_frontend/Dockerfile axiom_frontend/
```

### 3. Configure environment

```bash
cp ~/axiom-src/.env.example ~/axiom/.env
# Edit ~/axiom/.env with production values:
# - DATABASE_URL pointing to LXC 107
# - API keys (DeepSeek, OpenAI, etc.)
# - JWT_SECRET_KEY
# - ADMIN_PASSWORD
```

### 4. Model cache

Either copy from nimbus or let containers download on first run:

```bash
# Copy from nimbus (faster, ~15GB)
rsync -avP nimbus:~/AI-Models/huggingface/ ~/ai-models/

# Or set HF_HUB_OFFLINE=0 in .env to download on first start
```

### 5. Start the service

```bash
sudo systemctl start axiom
sudo systemctl status axiom
```

## Systemd Service

Located at `/etc/systemd/system/axiom.service`. Controls the full pod lifecycle:

```bash
sudo systemctl start axiom      # Start all containers
sudo systemctl stop axiom       # Stop all containers
sudo systemctl restart axiom    # Full restart
sudo systemctl status axiom     # Check status
journalctl -u axiom -f          # Follow logs
```

## Deploying Updates

From the MacBook (project root):

```bash
# Full deploy (push, build both images, restart)
./deploy.sh

# Backend only (skip frontend build)
./deploy.sh --backend-only

# Frontend only
./deploy.sh --frontend-only

# Just restart (no build)
./deploy.sh --restart-only
```

## Monitoring

```bash
# Container status
nerdctl pod ps
nerdctl ps --pod

# Logs
nerdctl logs axiom-backend -f
nerdctl logs axiom-doc-processor -f
nerdctl logs axiom-frontend -f

# GPU usage
nvidia-smi

# Health check
nerdctl exec axiom-backend python3 -c "
import urllib.request
r = urllib.request.urlopen('http://127.0.0.1:8000/health')
print(r.read().decode())
"
```

## NPM (Nginx Proxy Manager) Configuration

The proxy host for `maestro.i.catdev.io` needs custom Nginx config for WebSocket support and large uploads:

```nginx
# API + WebSocket
location /api/ {
    proxy_pass http://192.168.1.120:8000;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Cookie $http_cookie;
    proxy_pass_header Set-Cookie;
    proxy_read_timeout 600s;
    proxy_send_timeout 600s;
    client_max_body_size 2000M;
}

# WebSocket (research updates)
location /api/ws {
    proxy_pass http://192.168.1.120:8000;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_read_timeout 7d;
    proxy_send_timeout 7d;
}

# Frontend
location / {
    proxy_pass http://192.168.1.120:3000;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
}

# Health check
location /health {
    proxy_pass http://192.168.1.120:8000/health;
}
```

## GPU Configuration

The A3000 has 12GB VRAM. Per-model device config via environment variables:

```ini
# All on GPU (default for 12GB)
DEVICE_EMBEDDER=auto
DEVICE_RERANKER=auto
DEVICE_GLINER=auto
DEVICE_MREBEL=auto
DEVICE_MARKER=auto
DEVICE_VISION=auto

# If VRAM is tight, move lightweight models to CPU:
# DEVICE_RERANKER=cpu    # saves 0.8GB
# DEVICE_GLINER=cpu      # saves 0.5GB
# DEVICE_VISION=cpu      # saves 0.35GB
```

## Troubleshooting

**Pod won't start:**
```bash
nerdctl pod rm -f axiom  # Force remove stuck pod
sudo systemctl start axiom
```

**GPU not visible in container:**
```bash
nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml
nerdctl run --rm --device nvidia.com/gpu=all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi
```

**Database connection failed:**
```bash
# Test from LXC
nerdctl exec axiom-backend python3 -c "
from database.database import test_connection
print(test_connection())
"
```

**Models not found (HF_HUB_OFFLINE=1):**
```bash
# Check model cache
ls ~/ai-models/hub/models--BAAI--bge-m3/
# If missing, set HF_HUB_OFFLINE=0 temporarily or rsync from nimbus
```
