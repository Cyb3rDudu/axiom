# Production Deployment: nerdctl + containerd on Proxmox LXC

This documents the production Axiom deployment on LXC 120 with nerdctl,
containerd, buildkit, and an NVIDIA A3000 GPU.

## Architecture

```
MacBook (dev) ──git push + deploy.sh──> LXC 120 (192.168.1.120)
                                         ├── axiom-backend   (nerdctl, GPU, :8000)
                                         │   └── GPU worker subprocess (shared socket)
                                         ├── axiom-doc-processor (nerdctl, GPU)
                                         │   └── connects to backend's GPU worker
                                         ├── axiom-frontend   (nerdctl, :3000)
                                         └── NO nginx (NPM proxies directly)
                                               │
                                         LXC 107 (192.168.1.107)
                                         └── PostgreSQL + pgvector + OpenSearch
```

The backend spawns a GPU worker subprocess that serves the embedder, reranker, and GLiNER over a Unix socket. The doc-processor connects to this same worker via a shared bind-mount. See [GPU Worker Architecture](../architecture/gpu-worker.md) for details.

Nginx Proxy Manager (on carrier) routes `axiom.i.catdev.io` to the LXC.

## LXC Requirements

- Debian 13+ with containerd, nerdctl, and buildkit (buildkitd with `--containerd-worker`)
- NVIDIA GPU passed through with nvidia-container-toolkit
- LXC config: `lxc.apparmor.profile: unconfined`, `lxc.cap.drop:` (empty), `lxc.mount.auto: proc:rw sys:rw`
- LXC cgroup2 device allows (see [GPU Driver & Container Runtime](#gpu-driver--container-runtime) for the full list and rationale):
  - `c 195:<minor> rwm` — the specific nvidia device minor that's passed through (e.g. `195:1` for an A3000 on PCIe slot 2)
  - `c 195:255 rwm` — `nvidiactl`
  - `c 504:* rwm` — **required:** `nvidia-uvm` (without this, container CUDA init fails with `CUDA_ERROR_UNKNOWN` / EPERM on `/dev/nvidia-uvm`)
  - `c 509:* rwm` — `nvidia-caps` (driver 570+)
  - `c 10:200 rwm` — `/dev/net/tun`
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
sudo nerdctl build -t axiom-backend:local -f axiom_backend/Dockerfile axiom_backend/
sudo nerdctl build -t axiom-frontend:local -f axiom_frontend/Dockerfile axiom_frontend/
```

Usually you don't invoke these directly — `./deploy.sh` from the MacBook
handles `git push` + remote build + systemd restart.

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

Either pre-seed the cache from another host or let containers download on
first run. The cache lives at `~/ai-models/` on the LXC and is bind-mounted
into `/root/.cache/huggingface` inside the backend container.

```bash
# Pre-seed from another host (faster, ~15GB)
rsync -avP <other-host>:~/AI-Models/huggingface/ ~/ai-models/

# Or set HF_HUB_OFFLINE=0 in .env so models download on first start
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

## GPU Worker Shared Socket

The backend and doc-processor share a single GPU worker subprocess. This requires a shared directory bind-mounted into both containers so they can access the same Unix socket.

### Required Volume Mount

Both containers need the same bind-mount in `start-pod.sh`:

```bash
# In start-pod.sh, for both axiom-backend and axiom-doc-processor:
-v /tmp/axiom-gpu:/tmp/axiom-gpu
```

### Required Environment Variables

```bash
# In .env (applies to both containers)
AXIOM_GPU_WORKER_SOCKET=/tmp/axiom-gpu/axiom-gpu.sock

# Override for doc-processor only (via -e flag in start-pod.sh)
# AXIOM_GPU_WORKER_CLIENT_MODE=true
```

The doc-processor container should pass `-e AXIOM_GPU_WORKER_CLIENT_MODE=true` so it connects to the backend's worker rather than spawning its own.

### Verifying the Worker

After starting the pod, trigger any search or chat query to spawn the GPU worker, then verify:

```bash
# Check the socket exists
ls -la /tmp/axiom-gpu/axiom-gpu.sock

# Check the worker is running inside the backend container
nerdctl exec axiom-backend pgrep -f "gpu_worker.server"

# Check the doc-processor can reach the socket
nerdctl exec axiom-doc-processor ls -la /tmp/axiom-gpu/axiom-gpu.sock
```

### Idle Behavior

The GPU worker automatically exits after 15 minutes of inactivity (`AXIOM_GPU_WORKER_IDLE_SEC=900`). This frees all VRAM. The next GPU request transparently respawns the worker. Adjust the idle threshold in `.env` if needed:

```bash
# Kill worker after 5 minutes of idle (shorter for memory-constrained GPUs)
AXIOM_GPU_WORKER_IDLE_SEC=300
```

## GPU Driver & Container Runtime

Running CUDA in nerdctl containers inside an LXC requires the right combination of:

1. Proxmox LXC passthrough config (`/etc/pve/lxc/<id>.conf`)
2. nvidia-container-toolkit runtime config (`/etc/nvidia-container-runtime/config.toml`)
3. Backend image CUDA base version (`axiom_backend/Dockerfile`)
4. `start-pod.sh` `--device` flags for non-auto-injected nodes

With NVIDIA driver 595.58.03 + nvidia-container-toolkit 1.19.0, the combination
below is known to work. The critical discovery is that in nested containers
(LXC → containerd), the runtime's default cgroup management conflicts with the
LXC's already-configured device cgroup. The fix is `no-cgroups = true` plus
explicit LXC cgroup allows for the NVIDIA majors.

### Proxmox LXC config

Add to `/etc/pve/lxc/<id>.conf` on the Proxmox host:

```ini
# Restrict to the passed-through GPU (example: A3000 is /dev/nvidia1 on host)
lxc.cgroup2.devices.allow: c 195:1 rwm     # the GPU minor you want
lxc.cgroup2.devices.allow: c 195:255 rwm   # nvidiactl (shared)
lxc.cgroup2.devices.allow: c 504:* rwm     # nvidia-uvm  (REQUIRED, see below)
lxc.cgroup2.devices.allow: c 509:* rwm     # nvidia-caps (driver 570+)

lxc.mount.entry: /dev/nvidia1          dev/nvidia1          none bind,optional,create=file
lxc.mount.entry: /dev/nvidiactl        dev/nvidiactl        none bind,optional,create=file
lxc.mount.entry: /dev/nvidia-uvm       dev/nvidia-uvm       none bind,optional,create=file
lxc.mount.entry: /dev/nvidia-uvm-tools dev/nvidia-uvm-tools none bind,optional,create=file
lxc.mount.entry: /dev/nvidia-caps      dev/nvidia-caps      none bind,optional,create=dir
lxc.mount.entry: /usr/lib/x86_64-linux-gnu usr/lib/x86_64-linux-gnu/nvidia none bind,optional,create=dir,ro
lxc.mount.entry: /usr/bin/nvidia-modprobe  usr/bin/nvidia-modprobe  none bind,optional,create=file
lxc.mount.entry: /usr/bin/nvidia-smi       usr/bin/nvidia-smi       none bind,optional,create=file
```

If you want *only* one specific GPU visible to the LXC (e.g. a multi-GPU host
where axiom should not see all of them), pass through only that GPU's minor
and omit the others from `lxc.mount.entry`.

Reboot the LXC after changing `/etc/pve/lxc/<id>.conf`:

```bash
ssh root@<proxmox-host> pct reboot <lxc-id>
```

### nvidia-container-runtime config

On the LXC (not the Proxmox host), set `no-cgroups = true`:

```bash
sudo nvidia-ctk config --set nvidia-container-cli.no-cgroups -i
```

This writes `no-cgroups = true` into `/etc/nvidia-container-runtime/config.toml`.
The runtime will then handle library injection and device node creation inside
the container, but **skip** cgroup device management — which is already managed
at the LXC level by the `lxc.cgroup2.devices.allow` lines above. Without this,
the runtime tries to write to `/sys/fs/cgroup/devices.allow` inside the nested
container and fails with `operation not permitted`, ultimately surfacing as
`CUDA_ERROR_UNKNOWN (999)` when PyTorch calls `cuInit()`.

### start-pod.sh GPU flags

For every container that uses the GPU:

```bash
nerdctl run \
  --gpus "device=GPU-<uuid-of-the-A3000>" \
  --device /dev/nvidia-caps/nvidia-cap1 \
  --device /dev/nvidia-caps/nvidia-cap2 \
  -v "/usr/bin/nvidia-smi:/usr/bin/nvidia-smi:ro" \
  -v "/usr/bin/nvidia-smi:/usr/local/bin/nvidia-smi:ro" \
  ...
  axiom-backend:local
```

Why each flag:

- `--gpus "device=GPU-<uuid>"` — invokes nvidia-container-runtime to inject the
  matching host libcuda.so, libcudart, libnvidia-ml etc. into the container.
  Use UUID (not device index) so the container sticks to the intended GPU
  across host reboots (PCI enumeration may re-order).
- `--device /dev/nvidia-caps/nvidia-cap1/cap2` — not auto-injected by the
  runtime, but required by driver 570+ for CUDA init.
- Two `-v` binds of host `/usr/bin/nvidia-smi` — override the image's own
  `nvidia-smi` binary (both `/usr/bin` and `/usr/local/bin` are on `$PATH`).
  The image-bundled binary is linked against the CUDA-version the image was
  built with (12.9 / 13.0) and fails with `exec format error` against driver
  595 libraries. Mounting the host binary (which matches the driver exactly)
  fixes all `nvidia-smi` calls from inside the container.

### Backend image CUDA base

`axiom_backend/Dockerfile`:

```dockerfile
FROM nvidia/cuda:13.0.1-base-ubuntu24.04 AS deps
```

Driver 595 is beyond the forward-compat range of CUDA 12.9 (which caps at
driver 570). Use a CUDA 13+ base image so the forward-compat libraries cover
driver 595. PyTorch 2.11.0+cu130 is already built against CUDA 13 — no
`requirements.txt` change needed.

### Verifying the setup

```bash
# From the LXC
BID=$(sudo nerdctl ps -q -f name=axiom-backend)

# Host binary should match driver
sudo nerdctl exec $BID nvidia-smi --query-gpu=name,driver_version --format=csv

# torch should detect the GPU
sudo nerdctl exec $BID python3 -c "
import torch
assert torch.cuda.is_available(), 'CUDA not available'
print(torch.cuda.get_device_name(0))
x = torch.randn(1024, 1024, device='cuda'); print('matmul OK:', (x @ x).sum().item())
"
```

Expected: driver matches host (e.g. `595.58.03`), GPU name is the passed-through
device, matmul completes.

### Fallback: hardware_detection.py OSError catch

`axiom_backend/ai_researcher/hardware_detection.py` catches `OSError` from
`subprocess.run(["nvidia-smi", ...])` so that a misconfigured environment falls
back to CPU rather than crashing the GPU worker. When properly configured (see
above), this fallback is never triggered. If it *does* trigger, log lines like
`No GPU detected, falling back to CPU mode` appear on worker startup — that's
the signal to recheck this section.

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

**GPU not visible in container / `cuda_available: False` / `CUDA_ERROR_UNKNOWN (999)`:**

First check that all prerequisites in [GPU Driver & Container Runtime](#gpu-driver--container-runtime) are met. Then strace cuInit to see which device returns EPERM:

```bash
sudo nerdctl exec <backend-cid> strace -f -e trace=openat python3 -c \
  "import ctypes; c=ctypes.CDLL('/usr/lib/x86_64-linux-gnu/libcuda.so.1'); c.cuInit(0)" \
  2>&1 | grep '/dev/nvidia'
```

- `EPERM (Operation not permitted)` on `/dev/nvidia-uvm` → LXC cgroup2 is
  missing `c 504:* rwm`. Add it to `/etc/pve/lxc/<id>.conf` and `pct reboot`.
- `ENOENT` on `/dev/nvidia-uvm` → device not mounted in LXC or was re-created
  on host after the LXC started. Fix: `pct reboot <id>` to refresh bind-mounts.
- `EACCES` on `/dev/nvidia0` / `/dev/nvidia1` → wrong minor in cgroup allow;
  check `ls -la /dev/nvidia*` on the LXC vs the `c 195:<minor> rwm` line.
- `exec format error` on `nvidia-smi` → image's binary is ABI-incompatible
  with the host driver. Ensure `-v /usr/bin/nvidia-smi:/usr/local/bin/nvidia-smi:ro`
  is in `start-pod.sh`.

If strace shows no device errors but cuInit still returns 999, check that
`/etc/nvidia-container-runtime/config.toml` has `no-cgroups = true` (required
for nested containers).

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
