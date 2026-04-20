# Deployment Methods Comparison

AXIOM can be deployed in several ways depending on your infrastructure, hardware, and workflow requirements. This page provides an overview of each method and guidance on choosing the right one.

## At a Glance

| Method | GPU Support | Complexity | Best For | Maturity |
|--------|-------------|------------|----------|----------|
| Docker Compose | NVIDIA (native), MPS (macOS variant) | Low | Production servers, quick setup | Stable |
| Proxmox LXC + nerdctl | NVIDIA passthrough | Medium | Homelab / virtualized environments, current production setup | Stable |
| macOS Native Dev | Apple MPS (Metal) | Medium | Development with Apple Silicon GPUs | Experimental |

## Docker Compose

The default and most straightforward deployment method. AXIOM provides several Compose file variants for different environments:

| Compose File | Use Case |
|--------------|----------|
| `docker-compose.yml` | Standard deployment (CPU or NVIDIA GPU auto-detected) |
| `docker-compose.gpu.yml` | Explicit NVIDIA GPU support |
| `docker-compose.cpu.yml` | CPU-only, no GPU requirements |
| `docker-compose.macos.yml` | macOS with Docker Desktop |
| `docker-compose.external-db.yml` | External PostgreSQL database |
| `docker-compose.gpu-external-db.yml` | NVIDIA GPU + external PostgreSQL |

**Pros:**

- Simplest setup -- single command to start all services.
- Well-tested across Linux and macOS.
- Supports pre-built Docker images for fast deployment.

**Cons:**

- Requires Docker daemon running.
- macOS variant does not support GPU passthrough for ML workloads (use the native dev stack instead).

!!! tip
    For most production deployments on Linux with an NVIDIA GPU, start with `docker-compose.gpu.yml`. See the [Installation Guide](../getting-started/installation/cli-commands.md) for step-by-step instructions.

## Proxmox LXC + nerdctl

Run AXIOM inside a Proxmox LXC container with nerdctl + containerd + buildkit.
This is the current production setup (LXC 120).

**Pros:**

- Lightweight virtualization with near-native performance.
- NVIDIA GPU passthrough supported via Proxmox device mapping.
- Good isolation without full VM overhead.
- systemd-managed container lifecycle (single `axiom.service` starts the full pod).

**Cons:**

- Requires Proxmox host configuration for GPU passthrough.
- Nested container runtime (containerd-in-LXC) adds some complexity.
- Storage and network configuration needs careful planning.

!!! note
    See the [Proxmox LXC Guide](proxmox-lxc.md) for detailed setup
    instructions including GPU passthrough configuration, and the
    [LXC + nerdctl Production Guide](lxc-nerdctl-prod.md) for the
    specific runtime, systemd unit, and GPU driver tuning details.

## macOS Native Dev Stack

A hybrid approach designed for development on Apple Silicon Macs. Infrastructure services (PostgreSQL, Redis, Qdrant) run in containers, while the Python ML backend runs natively to access Metal Performance Shaders (MPS) GPU acceleration.

**Pros:**

- Full Apple Silicon GPU acceleration for embeddings and reranking.
- Fast development iteration -- no container rebuild for backend changes.
- Native debugging and profiling tools available.

**Cons:**

- Development only -- not suitable for production.
- Requires manual Python environment setup (pyenv, virtualenv).
- Mixed container/native architecture is more complex to maintain.

!!! note
    See the [macOS Development Guide](../DEV_MACOS.md) for complete setup instructions.

## Choosing a Method

Use this decision tree to pick the right deployment method:

1. **Are you developing on macOS with Apple Silicon?**
    - Yes, and you need MPS GPU for ML: **macOS Native Dev Stack**
    - Yes, but CPU-only is fine: **Docker Compose** (`docker-compose.macos.yml`)

2. **Are you deploying to a Proxmox host?**
    - Yes: **Proxmox LXC + nerdctl**

3. **Everything else:**
    - **Docker Compose** -- choose the variant matching your hardware.
