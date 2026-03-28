# Docker Installation

This guide provides instructions for installing and running AXIOM using Docker, the recommended deployment method.

## Prerequisites

### Required Software

- **Docker** (version 20.10 or higher)
  - [Installation guide for your platform](https://docs.docker.com/get-docker/)
- **Docker Compose** (version 2.0 or higher)
  - Usually included with Docker Desktop
  - [Standalone installation](https://docs.docker.com/compose/install/) if needed

### System Requirements

- **Operating System**: Linux, macOS, or Windows (with WSL2)
- **RAM**: Minimum 8GB (16GB recommended)
- **Storage**: At least 20GB free space
- **CPU**: 4+ cores recommended
- **Network**: Stable internet connection for downloading images

### Optional: GPU Support

For faster document processing (especially PDF conversion and embeddings):

- **NVIDIA GPU** with CUDA support
- **NVIDIA Container Toolkit** ([installation guide](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html))

Without GPU support, AXIOM will use CPU for processing (slower but fully functional).

## Quick Installation

### Step 1: Clone the Repository

```bash
git clone https://github.com/murtaza-nasir/axiom.git
cd axiom
```

### Step 2: Configure Environment

AXIOM provides two configuration methods:

#### Option A: Interactive Setup (Recommended)

```bash
./setup-env.sh
```

This script will guide you through:

- Setting up API keys for AI providers
- Configuring search providers
- Setting network parameters
- Generating secure database credentials

#### Option B: Manual Configuration

```bash
cp .env.example .env
```

Edit the `.env` file with your preferred text editor:

```bash
nano .env  # or vim, code, etc.
```

Key configurations to set:

```bash
# Main application port (nginx proxy — the only port most users need)
AXIOM_PORT=80

# Database credentials (CHANGE THESE for production!)
POSTGRES_USER=axiom_user
POSTGRES_PASSWORD=your_secure_password
POSTGRES_DB=axiom_db
```

!!! note
    AI provider API keys (OpenAI, DeepSeek, Z.AI, OpenRouter) are configured through the web UI after first login, **not** in the `.env` file. Only search provider keys (Tavily, LinkUp, Jina) are set in `.env`.

### Step 3: Start AXIOM

Choose the appropriate compose file for your hardware:

**CPU Only (default):**
```bash
docker compose up -d
```

**NVIDIA GPU:**
```bash
docker compose -f docker-compose.gpu.yml up -d
```

**macOS (Apple Silicon / Intel):**
```bash
docker compose -f docker-compose.macos.yml up -d
```

**External Database (bring your own PostgreSQL):**
```bash
docker compose -f docker-compose.external-db.yml up -d
```

This command will:

1. Download necessary Docker images
2. Build the AXIOM containers
3. Initialize the PostgreSQL database
4. Start all services (backend, frontend, nginx proxy, PostgreSQL)

### Step 4: Access the Application

Once running, access AXIOM through the nginx proxy at:

- **Web Interface**: `http://localhost` (or `http://localhost:<AXIOM_PORT>` if changed)

Default login credentials:

- **Username**: `admin`
- **Password**: The password from your `.env` file (the setup script generates a secure random password; if you used manual configuration, check your `AXIOM_ADMIN_PASSWORD` setting)

**Important**: Change the default password immediately after first login.

## Available Docker Compose Files

AXIOM ships with several compose files for different deployment scenarios:

| File | Use Case |
|------|----------|
| `docker-compose.yml` | Default CPU-only deployment |
| `docker-compose.gpu.yml` | NVIDIA GPU acceleration |
| `docker-compose.cpu.yml` | Explicit CPU-only (same as default) |
| `docker-compose.macos.yml` | Optimized for macOS (CPU mode in containers) |
| `docker-compose.external-db.yml` | Use an external PostgreSQL database |
| `docker-compose.gpu-external-db.yml` | GPU + external PostgreSQL |
| `docker-compose.override.yml` | Local overrides (not committed) |

## Managing AXIOM

### Starting and Stopping

Start AXIOM:
```bash
docker compose up -d  # -d runs in background
```

Stop AXIOM:
```bash
docker compose down
```

Stop and remove all data:
```bash
docker compose down -v  # Removes volumes (data loss!)
```

### Viewing Logs

View all logs:
```bash
docker compose logs
```

View specific service logs:
```bash
docker compose logs axiom-backend
docker compose logs axiom-frontend
docker compose logs axiom-postgres
```

Follow logs in real-time:
```bash
docker compose logs -f
```

### Updating AXIOM

To update to the latest version:

```bash
# Stop the current instance
docker compose down

# Pull latest changes
git pull

# Rebuild and start
docker compose up --build -d
```

The multi-stage Dockerfile caches dependency layers, so rebuilds only recompile the application code unless `requirements.txt` has changed.

### Database Management

Access PostgreSQL:
```bash
docker exec -it axiom-postgres psql -U axiom_user -d axiom_db
```

Backup database:
```bash
docker exec axiom-postgres pg_dump -U axiom_user axiom_db > backup.sql
```

Restore database:
```bash
docker exec -i axiom-postgres psql -U axiom_user axiom_db < backup.sql
```

## Deployment Scenarios

### Local Development

Standard configuration — use the defaults in `.env.example`:

```bash
AXIOM_PORT=80
```

All services are accessed through the nginx reverse proxy on a single port.

### Production Deployment

For production on a single server, configure HTTPS via an external reverse proxy (e.g., Caddy, Traefik, or nginx on the host) that terminates TLS and forwards to `AXIOM_PORT`:

```bash
AXIOM_PORT=8080  # Use a non-privileged port behind your reverse proxy
```

### Alternative Deployment Methods

For non-Docker deployments, see:

- [Proxmox LXC Deployment](../../deployment/proxmox-lxc.md)
- [Local LLM Deployment](../../deployment/local-llms.md) (vLLM / SGLang)
- [macOS Native Dev Stack](../../DEV_MACOS.md) (Apple Silicon with MPS GPU)

## Troubleshooting

### Container Won't Start

Check logs for errors:
```bash
docker compose logs axiom-backend
```

Common issues:

- Port already in use: Change `AXIOM_PORT` in `.env`
- Database connection failed: Check PostgreSQL is running with `docker compose ps`
- Backend crash on startup: Check for missing config variables with `docker compose logs axiom-backend | head -50`

### GPU Not Detected

Verify NVIDIA Container Toolkit:
```bash
nvidia-smi
docker run --rm --gpus all nvidia/cuda:11.8.0-base-ubuntu22.04 nvidia-smi
```

Make sure you are using the GPU compose file: `docker compose -f docker-compose.gpu.yml up -d`

### Memory Issues

Increase Docker memory allocation:

- **Docker Desktop**: Preferences → Resources → Memory (16GB+ recommended)
- **Linux**: Check system memory with `free -h`

### Permission Errors

Fix volume permissions:
```bash
sudo chown -R $USER:$USER ./data
chmod -R 755 ./data
```

## Security Considerations

### Production Checklist

- [ ] Change default admin password
- [ ] Use HTTPS via reverse proxy in production
- [ ] Use strong database credentials (setup script generates these)
- [ ] Limit network exposure with firewall rules
- [ ] Regular security updates (`git pull && docker compose up --build -d`)
- [ ] Regular database backups

## Next Steps

After successful installation:

1. **[First Login](../first-login.md)** - Set up your account
2. **[Configure AI Providers](../configuration/ai-providers.md)** - Set up language models (OpenAI, DeepSeek, Z.AI, OpenRouter, or local)
3. **[Upload Documents](../../user-guide/documents/uploading.md)** - Build your library
4. **[Quick Start Guide](../quickstart.md)** - Start using AXIOM

For additional help, see our [Troubleshooting Guide](../../troubleshooting/index.md) or visit the [Community Forum](https://github.com/murtaza-nasir/axiom/discussions).
