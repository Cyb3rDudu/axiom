# Axiom Deployment on Proxmox VE (Native LXC from Docker Images)

Proxmox VE 9.1+ can run OCI/Docker images as native LXC containers without a Docker daemon.
This guide deploys Axiom's Docker images directly as Proxmox containers.

## Architecture

Axiom consists of 4 services deployed as separate LXC containers on the same bridge network:

| Container | Image | Port | Purpose |
|-----------|-------|------|---------|
| axiom-backend | axiom-backend | 8000 | FastAPI backend + AI research engine |
| axiom-frontend | axiom-frontend | 3000 | React frontend (static files via nginx) |
| axiom-doc-processor | axiom-doc-processor | - | Background document processor |
| axiom-nginx | axiom-nginx | 80 | Reverse proxy (entry point) |

PostgreSQL runs on the shared database server (CT 107, 10.36.0.107).

## Prerequisites

- Proxmox VE 9.1+ with `containerd` and `nerdctl` installed
- External PostgreSQL with `pgvector` extension (CT 107)
- GPU passthrough configured (optional, for CUDA acceleration)

```bash
apt install -y containerd
# Install nerdctl from GitHub releases
curl -sL https://github.com/containerd/nerdctl/releases/latest/download/nerdctl-full-linux-amd64.tar.gz | tar xz -C /usr/local
```

## Step 1: Build Docker images locally

On a machine with Docker or nerdctl:

```bash
cd /path/to/axiom

# Build all images
nerdctl build -t axiom-backend -f axiom_backend/Dockerfile axiom_backend/
nerdctl build -t axiom-frontend -f axiom_frontend/Dockerfile axiom_frontend/
nerdctl build -t axiom-nginx -f nginx/Dockerfile nginx/
nerdctl build -t axiom-doc-processor -f axiom_backend/Dockerfile axiom_backend/
```

Or with Docker on another machine, then export:

```bash
docker compose build
docker save axiom-backend | gzip > axiom-backend.tar.gz
docker save axiom-frontend | gzip > axiom-frontend.tar.gz
docker save axiom-nginx | gzip > axiom-nginx.tar.gz
# Transfer to Proxmox host
```

## Step 2: Export to Proxmox template storage

```bash
# From nerdctl (export as OCI archive for Proxmox native LXC)
nerdctl save axiom-backend -o /var/lib/vz/template/cache/axiom-backend.tar
nerdctl save axiom-frontend -o /var/lib/vz/template/cache/axiom-frontend.tar
nerdctl save axiom-nginx -o /var/lib/vz/template/cache/axiom-nginx.tar
nerdctl save axiom-doc-processor -o /var/lib/vz/template/cache/axiom-doc-processor.tar
```

Or pull pre-built images from a registry:

```bash
pvesh create /nodes/carrier/storage/local/oci-registry-pull \
  --reference your-registry.com/axiom-backend:latest
```

## Step 3: Create containers

### Backend (CT 120)

```bash
pct create 120 local:vztmpl/axiom-backend.tar \
  --hostname axiom-backend \
  --cores 4 \
  --memory 8192 \
  --net0 name=eth0,bridge=vmbr0,ip=10.36.0.120/22,gw=10.36.0.1 \
  --storage local-lvm \
  --rootfs local-lvm:16 \
  --unprivileged 1

# Set environment variables
pct set 120 --env "DATABASE_URL=postgresql://axiom:dsHXpLsy7qDyVIeI80FlZ7Yn/Gzht1N6pNRoiAvKPZ4=@10.36.0.107:5432/axiom"
pct set 120 --env "ADMIN_USERNAME=admin"
pct set 120 --env "ADMIN_PASSWORD=qExzeg-xowqo9-gycwyr"
pct set 120 --env "JWT_SECRET_KEY=$(openssl rand -hex 32)"
pct set 120 --env "TZ=UTC"
pct set 120 --env "LOG_LEVEL=ERROR"
pct set 120 --env "MAX_WORKER_THREADS=10"
pct set 120 --env "EMBEDDING_BATCH_SIZE=32"
pct set 120 --env "CORS_ALLOWED_ORIGINS=*"
pct set 120 --env "ALLOW_CORS_WILDCARD=true"
```

### Frontend (CT 121)

```bash
pct create 121 local:vztmpl/axiom-frontend.tar \
  --hostname axiom-frontend \
  --cores 1 \
  --memory 512 \
  --net0 name=eth0,bridge=vmbr0,ip=10.36.0.121/22,gw=10.36.0.1 \
  --storage local-lvm \
  --rootfs local-lvm:2 \
  --unprivileged 1
```

### Doc Processor (CT 122)

```bash
pct create 122 local:vztmpl/axiom-doc-processor.tar \
  --hostname axiom-doc-processor \
  --cores 2 \
  --memory 4096 \
  --net0 name=eth0,bridge=vmbr0,ip=10.36.0.122/22,gw=10.36.0.1 \
  --storage local-lvm \
  --rootfs local-lvm:8 \
  --unprivileged 1

# Same database env as backend
pct set 122 --env "DATABASE_URL=postgresql://axiom:dsHXpLsy7qDyVIeI80FlZ7Yn/Gzht1N6pNRoiAvKPZ4=@10.36.0.107:5432/axiom"
pct set 122 --env "EMBEDDING_BATCH_SIZE=32"
```

### Nginx Proxy (CT 123)

```bash
pct create 123 local:vztmpl/axiom-nginx.tar \
  --hostname axiom-nginx \
  --cores 1 \
  --memory 256 \
  --net0 name=eth0,bridge=vmbr0,ip=10.36.0.123/22,gw=10.36.0.1 \
  --storage local-lvm \
  --rootfs local-lvm:1 \
  --unprivileged 1
```

The nginx config must route to the other containers by IP:
- Backend: `10.36.0.120:8000`
- Frontend: `10.36.0.121:3000`

## Step 4: Start all containers

```bash
pct start 120  # backend
pct start 121  # frontend
pct start 122  # doc-processor
pct start 123  # nginx
```

Access Axiom at `http://10.36.0.123` (or configure via Nginx Proxy Manager on CT 100).

## Step 5: GPU passthrough (optional)

For CUDA acceleration on backend/doc-processor, the containers need GPU access.
This requires privileged containers with device passthrough:

```bash
# Modify container config
pct set 120 --unprivileged 0
# Add to /etc/pve/lxc/120.conf:
#   lxc.cgroup2.devices.allow: c 195:* rwm
#   lxc.cgroup2.devices.allow: c 506:* rwm
#   lxc.mount.entry: /dev/nvidia0 dev/nvidia0 none bind,optional,create=file
#   lxc.mount.entry: /dev/nvidiactl dev/nvidiactl none bind,optional,create=file
#   lxc.mount.entry: /dev/nvidia-uvm dev/nvidia-uvm none bind,optional,create=file
```

## Alternative: Docker Compose on a privileged LXC

If the native OCI approach has issues (entrypoint problems, networking), use the traditional method:

```bash
# Create a privileged Debian LXC
pct create 120 local:vztmpl/debian-13-standard_13.0-1_amd64.tar.zst \
  --hostname axiom \
  --cores 8 \
  --memory 16384 \
  --net0 name=eth0,bridge=vmbr0,ip=10.36.0.120/22,gw=10.36.0.1 \
  --storage local-lvm \
  --rootfs local-lvm:32 \
  --unprivileged 0 \
  --features nesting=1,keyctl=1

# Install Docker inside the LXC
pct start 120
pct exec 120 -- bash -c 'apt update && apt install -y docker.io docker-compose-v2'

# Clone axiom and deploy with docker compose
pct exec 120 -- bash -c 'cd /opt && git clone https://github.com/Cyb3rDudu/axiom.git'
pct exec 120 -- bash -c 'cd /opt/axiom && cp env.example .env'
# Edit .env with external DB settings, then:
pct exec 120 -- bash -c 'cd /opt/axiom && docker compose -f docker-compose.external-db.yml up -d'
```

## Updating

### containerd + nerdctl method

```bash
ssh dudu@192.168.1.120
cd /opt/axiom && git pull
nerdctl compose -f docker-compose.external-db.yml down
nerdctl build -t axiom-backend -f axiom_backend/Dockerfile axiom_backend/
nerdctl compose -f docker-compose.external-db.yml up -d
```

## Recommended: containerd + nerdctl on a privileged LXC

containerd with nerdctl provides Docker-compatible CLI with native GPU support via NVIDIA CDI,
running inside a Proxmox LXC with GPU passthrough.

### LXC Setup (CT 120)

```bash
# Create privileged LXC from custom template
pct create 120 local:vztmpl/debian-13-dudu-gpu-podman.tar.zst \
  --hostname axiom \
  --cores 8 \
  --memory 16384 \
  --net0 name=eth0,bridge=vmbr0,ip=192.168.1.120/24,gw=192.168.1.1 \
  --storage local-lvm \
  --rootfs local-lvm:32 \
  --unprivileged 0 \
  --features nesting=1,keyctl=1,fuse=1
```

Add to `/etc/pve/lxc/120.conf`:
```conf
lxc.apparmor.profile: unconfined
lxc.cap.drop:
lxc.mount.auto: proc:rw sys:rw
lxc.mount.entry: /dev/net/tun dev/net/tun none bind,create=file

# NVIDIA GPU passthrough
lxc.cgroup2.devices.allow: c 195:* rwm
lxc.cgroup2.devices.allow: c 506:* rwm
lxc.mount.entry: /dev/nvidia0 dev/nvidia0 none bind,optional,create=file
lxc.mount.entry: /dev/nvidiactl dev/nvidiactl none bind,optional,create=file
lxc.mount.entry: /dev/nvidia-uvm dev/nvidia-uvm none bind,optional,create=file
lxc.mount.entry: /dev/nvidia-uvm-tools dev/nvidia-uvm-tools none bind,optional,create=file

# NVIDIA libs from host
lxc.mount.entry: /opt/nvidia-libs opt/nvidia-libs none bind,optional,create=dir,ro
```

### Install containerd + nerdctl inside LXC

```bash
# Install containerd
apt install -y containerd

# Install nerdctl (Docker-compatible CLI for containerd)
curl -sL https://github.com/containerd/nerdctl/releases/latest/download/nerdctl-full-linux-amd64.tar.gz | tar xz -C /usr/local

# Install nvidia-container-toolkit for CDI
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | \
  sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
  > /etc/apt/sources.list.d/nvidia-container-toolkit.list
apt update && apt install -y nvidia-container-toolkit

# Generate CDI spec
nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml

# Configure containerd for NVIDIA
nvidia-ctk runtime configure --runtime=containerd
systemctl restart containerd
```

### Deploy axiom

```bash
# Clone repo
cd /opt && git clone https://github.com/Cyb3rDudu/axiom.git
cd axiom && cp env.example .env
# Edit .env with external DB settings (192.168.1.107)

# Build images
nerdctl build -t axiom-backend -f axiom_backend/Dockerfile axiom_backend/
nerdctl build -t axiom-frontend -f axiom_frontend/Dockerfile axiom_frontend/
nerdctl build -t axiom-nginx -f nginx/Dockerfile nginx/

# Run with compose
nerdctl compose -f docker-compose.external-db.yml up -d
```

### GPU access in containers

```bash
# Run with GPU (CDI)
nerdctl run --rm --gpus all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi
```

### Management

```bash
# Status
nerdctl compose -f docker-compose.external-db.yml ps

# Logs
nerdctl compose -f docker-compose.external-db.yml logs -f backend

# Restart
nerdctl compose -f docker-compose.external-db.yml restart backend

# Update
nerdctl compose -f docker-compose.external-db.yml down
nerdctl build -t axiom-backend -f axiom_backend/Dockerfile axiom_backend/
nerdctl compose -f docker-compose.external-db.yml up -d
```

### Advantages

- Docker Compose compatible (uses the same `docker-compose.yml` files)
- Native GPU support via NVIDIA CDI
- No Docker daemon (containerd is lighter)
- `nerdctl` CLI is drop-in replacement for `docker`
- Works in privileged LXC with full GPU passthrough

## Deployment method comparison

| Method | Maturity | Compose support | GPU | Best for |
|--------|----------|-----------------|-----|----------|
| Native OCI LXC | Tech preview | No (1 image = 1 LXC) | Via device passthrough | Single-image services |
| Docker Compose in LXC | Production | Full | Via nvidia-container-toolkit | Quick Docker migration |
| **containerd + nerdctl in LXC** | **Production** | **Full (nerdctl compose)** | **Via NVIDIA CDI** | **Current setup** |

## Notes

- containerd + nerdctl replaced podman/buildah for better Docker Compose compatibility
- Always use static IPs for native OCI containers (minimal images lack dhclient)
- Use `pct exec <id> -- <command>` to access containers
- PostgreSQL runs on CT 107 (192.168.1.107, shared), not inside axiom containers
- The `docker-compose.external-db.yml` variant is designed for this setup (no embedded postgres)
- GPU is shared between host and LXC — multiple containers can use the A3000 simultaneously
