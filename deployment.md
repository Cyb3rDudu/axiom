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

- Proxmox VE 9.1+ with `buildah` installed
- External PostgreSQL with `pgvector` extension (CT 107)
- GPU passthrough configured (optional, for CUDA acceleration)

```bash
apt install -y buildah
```

## Step 1: Build Docker images locally

On a machine with Docker (or on the Proxmox host with buildah):

```bash
cd /path/to/axiom

# Build all images
buildah build -t axiom-backend -f axiom_backend/Dockerfile axiom_backend/
buildah build -t axiom-frontend -f axiom_frontend/Dockerfile axiom_frontend/
buildah build -t axiom-nginx -f nginx/Dockerfile nginx/
buildah build -t axiom-doc-processor -f axiom_backend/Dockerfile axiom_backend/
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
# From buildah
buildah push axiom-backend oci-archive:/var/lib/vz/template/cache/axiom-backend.tar
buildah push axiom-frontend oci-archive:/var/lib/vz/template/cache/axiom-frontend.tar
buildah push axiom-nginx oci-archive:/var/lib/vz/template/cache/axiom-nginx.tar
buildah push axiom-doc-processor oci-archive:/var/lib/vz/template/cache/axiom-doc-processor.tar
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

### Native OCI method

```bash
# Rebuild images
buildah build -t axiom-backend -f axiom_backend/Dockerfile axiom_backend/

# Re-export
buildah push axiom-backend oci-archive:/var/lib/vz/template/cache/axiom-backend.tar

# Recreate container (data volumes persist if using bind mounts)
pct stop 120
pct destroy 120
# Re-run pct create with same config
pct start 120
```

### Docker Compose method

```bash
pct exec 120 -- bash -c 'cd /opt/axiom && git pull && docker compose -f docker-compose.external-db.yml up -d --build'
```

## Alternative: Podman Quadlet on a privileged LXC

Podman Quadlet replaces Docker Compose with native systemd unit files — no Docker daemon, proper
process management, and automatic restart/logging via journald.

### Setup

```bash
# Create a privileged Debian LXC with nesting
pct create 120 local:vztmpl/debian-13-standard_13.0-1_amd64.tar.zst \
  --hostname axiom \
  --cores 8 \
  --memory 16384 \
  --net0 name=eth0,bridge=vmbr0,ip=10.36.0.120/22,gw=10.36.0.1 \
  --storage local-lvm \
  --rootfs local-lvm:32 \
  --unprivileged 0 \
  --features nesting=1,keyctl=1

pct start 120
pct exec 120 -- bash -c 'apt update && apt install -y podman'
```

### Convert Compose to Quadlet

Create unit files in `/etc/containers/systemd/` inside the LXC:

**axiom-network.network**
```ini
[Network]
NetworkName=axiom
```

**axiom-data.volume**
```ini
[Volume]
VolumeName=axiom-data
```

**axiom-backend.container**
```ini
[Unit]
Description=Axiom Backend
After=network-online.target

[Container]
Image=localhost/axiom-backend
ContainerName=axiom-backend
Network=axiom.network
PublishPort=8000:8000
Volume=axiom-data.volume:/app/ai_researcher/data
Environment=DATABASE_URL=postgresql://axiom:dsHXpLsy7qDyVIeI80FlZ7Yn/Gzht1N6pNRoiAvKPZ4=@10.36.0.107:5432/axiom
Environment=ADMIN_USERNAME=admin
Environment=ADMIN_PASSWORD=qExzeg-xowqo9-gycwyr
Environment=JWT_SECRET_KEY=change-me
Environment=LOG_LEVEL=ERROR
Environment=MAX_WORKER_THREADS=10
Environment=EMBEDDING_BATCH_SIZE=32
Environment=CORS_ALLOWED_ORIGINS=*

[Service]
Restart=always

[Install]
WantedBy=default.target
```

**axiom-frontend.container**
```ini
[Unit]
Description=Axiom Frontend
After=axiom-backend.service

[Container]
Image=localhost/axiom-frontend
ContainerName=axiom-frontend
Network=axiom.network

[Service]
Restart=always

[Install]
WantedBy=default.target
```

**axiom-doc-processor.container**
```ini
[Unit]
Description=Axiom Document Processor
After=axiom-backend.service

[Container]
Image=localhost/axiom-doc-processor
ContainerName=axiom-doc-processor
Network=axiom.network
Volume=axiom-data.volume:/app/ai_researcher/data
Exec=python -u services/background_document_processor.py
Environment=DATABASE_URL=postgresql://axiom:dsHXpLsy7qDyVIeI80FlZ7Yn/Gzht1N6pNRoiAvKPZ4=@10.36.0.107:5432/axiom
Environment=EMBEDDING_BATCH_SIZE=32

[Service]
Restart=always

[Install]
WantedBy=default.target
```

**axiom-nginx.container**
```ini
[Unit]
Description=Axiom Nginx Proxy
After=axiom-backend.service axiom-frontend.service

[Container]
Image=localhost/axiom-nginx
ContainerName=axiom-nginx
Network=axiom.network
PublishPort=80:80

[Service]
Restart=always

[Install]
WantedBy=default.target
```

### Build and start

```bash
# Build images with podman inside the LXC
pct exec 120 -- bash -c '
  cd /opt/axiom
  podman build -t axiom-backend -f axiom_backend/Dockerfile axiom_backend/
  podman build -t axiom-frontend -f axiom_frontend/Dockerfile axiom_frontend/
  podman build -t axiom-nginx -f nginx/Dockerfile nginx/
  podman build -t axiom-doc-processor -f axiom_backend/Dockerfile axiom_backend/
'

# Reload systemd to pick up quadlet files, then start
pct exec 120 -- bash -c '
  systemctl daemon-reload
  systemctl start axiom-backend axiom-frontend axiom-doc-processor axiom-nginx
  systemctl enable axiom-backend axiom-frontend axiom-doc-processor axiom-nginx
'
```

### Management

```bash
# Status
systemctl status axiom-backend
systemctl status axiom-frontend

# Logs
journalctl -u axiom-backend -f

# Restart a service
systemctl restart axiom-backend

# Update: rebuild image, then restart
podman build -t axiom-backend -f axiom_backend/Dockerfile axiom_backend/
systemctl restart axiom-backend
```

### Advantages over Docker Compose

- No Docker daemon (Podman is daemonless)
- Native systemd integration (ordering, restart, logging)
- Each container is a proper systemd service
- Works in unprivileged LXC with nesting
- `journalctl` for logs instead of `docker logs`

## Deployment method comparison

| Method | Maturity | Compose support | GPU | Best for |
|--------|----------|-----------------|-----|----------|
| Native OCI LXC | Tech preview | No (1 image = 1 LXC) | Via device passthrough | Single-image services |
| Docker Compose in LXC | Production | Full | Via nvidia-container-toolkit | Complex stacks, quick setup |
| Podman Quadlet in LXC | Production | Via unit files | Via CDI/device mount | Systemd-native, no daemon |

## Notes

- Native OCI containers are a Proxmox 9.1 tech preview — use Docker Compose or Podman Quadlet in LXC for production
- Always use static IPs for native OCI containers (minimal images lack dhclient)
- Use `pct exec <id> -- <command>` to access containers
- PostgreSQL runs on CT 107 (shared), not inside axiom containers
- The `docker-compose.external-db.yml` variant is designed for external DB setups
