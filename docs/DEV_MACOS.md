# macOS / Apple Silicon Dev Stack

Hybrid architecture: infrastructure runs in containers, ML services run natively for Metal/MPS GPU access.

## Prerequisites

- **macOS** with Apple Silicon (M1/M2/M3/M4)
- **Apple Container** (`brew install container`) or Docker/Podman/OrbStack
- **Python 3.12** (`brew install python@3.12`)
- **Node.js 20+** (`brew install node`)
- **PostgreSQL client** (`brew install libpq`) — for `pg_isready` and `psql`

## Architecture

```
┌─────────────────────────────────────────────┐
│  Containers (Apple Container / Docker)       │
│  ┌──────────┐  ┌───────────┐  ┌───────────┐│
│  │ Postgres │  │OpenSearch │  │  Nginx*   ││
│  │ pgvector │  │  2.18.0   │  │  (opt.)   ││
│  │ :5432    │  │  :9200    │  │  :80      ││
│  └──────────┘  └───────────┘  └───────────┘│
└─────────────────────────────────────────────┘
         │              │              │
         ▼              ▼              ▼
┌─────────────────────────────────────────────┐
│  Native macOS (Metal/MPS GPU)                │
│  ┌──────────────────┐  ┌──────────────────┐ │
│  │ FastAPI Backend  │  │ Vite Frontend   │ │
│  │ uvicorn :8000    │  │ dev server :3000│ │
│  │ BGE-M3 on MPS   │  │ HMR enabled     │ │
│  └──────────────────┘  └──────────────────┘ │
└─────────────────────────────────────────────┘
```

*Nginx is optional for dev — access frontend directly on :3000.

## Quick Start

### 1. Start the container runtime

```bash
# Apple Container
container system start

# Or Docker
# open -a Docker
```

### 2. Start infrastructure containers

```bash
# Create network
container network create axiom-network

# PostgreSQL with pgvector
container run \
  --name axiom-postgres \
  -d \
  -e POSTGRES_DB=axiom_db \
  -e POSTGRES_USER=axiom_user \
  -e POSTGRES_PASSWORD=axiom_password \
  -e PGDATA=/var/lib/postgresql/data/pgdata \
  -v postgres-data:/var/lib/postgresql/data \
  -p 5432:5432 \
  --network axiom-network \
  pgvector/pgvector:pg15

# OpenSearch (fulltext search)
container run \
  --name axiom-opensearch \
  -d \
  -e "discovery.type=single-node" \
  -e "DISABLE_SECURITY_PLUGIN=true" \
  -e "OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx512m" \
  -v opensearch-data:/usr/share/opensearch/data \
  -p 9200:9200 \
  --network axiom-network \
  opensearchproject/opensearch:2.18.0
```

Wait for postgres to be ready:

```bash
# Wait until this returns "accepting connections"
pg_isready -h localhost -p 5432 -U axiom_user
```

### 3. Initialize the database

```bash
PGPASSWORD=axiom_password psql -h localhost -U axiom_user -d axiom_db \
  -f init-db/00-extensions.sql \
  -f init-db/01-schema.sql \
  -f init-db/04-pgvector.sql
```

Then sync any columns the SQLAlchemy models added after the schema was written:

```bash
source .venv/bin/activate
set -a; source .env.macos; set +a
export AXIOM_DATA_PATH="$(cd $AXIOM_DATA_PATH && pwd)"
export AXIOM_AI_DATA_PATH="$(cd $AXIOM_AI_DATA_PATH && pwd)"
export AXIOM_APP_PATH="$(cd $AXIOM_APP_PATH && pwd)"
cd axiom_backend
python -c "from database.database import engine; from database.models import Base; Base.metadata.create_all(bind=engine)"
```

### 4. Set up Python environment

```bash
python3.12 -m venv .venv
source .venv/bin/activate
pip install --upgrade pip
pip install -r axiom_backend/requirements.txt
pip install greenlet  # needed for async SQLAlchemy
```

Verify MPS works:

```bash
python -c "import torch; print(f'MPS: {torch.backends.mps.is_available()}')"
# Should print: MPS: True
```

### 5. Start the backend

```bash
source .venv/bin/activate
set -a; source .env.macos; set +a
export AXIOM_DATA_PATH="$(cd $AXIOM_DATA_PATH && pwd)"
export AXIOM_AI_DATA_PATH="$(cd $AXIOM_AI_DATA_PATH && pwd)"
export AXIOM_APP_PATH="$(cd $AXIOM_APP_PATH && pwd)"

# Create data directories
mkdir -p axiom_backend/data/{raw_pdfs,processed/markdown,processed/metadata,processed/images,vector_store,markdown_files}
mkdir -p axiom_backend/ai_researcher/data/{vector_store,raw_pdfs,processed/markdown,processed/metadata}

cd axiom_backend
uvicorn main:app --host 0.0.0.0 --port 8000 --reload
```

Verify:

```bash
curl http://localhost:8000/health
# {"status":"healthy"}
```

### 6. Start the frontend

In a separate terminal:

```bash
cd axiom_frontend
VITE_API_TARGET=http://localhost:8000 VITE_WS_TARGET=ws://localhost:8000 npx vite --host 0.0.0.0 --port 3000
```

Open http://localhost:3000 and login with `admin` / `admin123`.

## Environment Configuration

All config is in `.env.macos` at the repo root. Key variables:

| Variable | Default | Purpose |
|----------|---------|---------|
| `DATABASE_URL` | `postgresql://axiom_user:axiom_password@localhost:5432/axiom_db` | Postgres connection |
| `PREFERRED_DEVICE_TYPE` | `mps` | Use Apple Metal GPU |
| `PYTORCH_ENABLE_MPS_FALLBACK` | `1` | Fall back to CPU for unsupported MPS ops |
| `AXIOM_DATA_PATH` | `./axiom_backend/data` | Document storage |
| `AXIOM_AI_DATA_PATH` | `./axiom_backend/ai_researcher/data` | AI/vector store data |
| `AXIOM_APP_PATH` | `./axiom_backend` | App root (for reference.docx etc.) |
| `EMBEDDING_BATCH_SIZE` | `16` | Batch size tuned for Apple Silicon |
| `OPENSEARCH_HOST` | `localhost` | OpenSearch host |
| `OPENSEARCH_PORT` | `9200` | OpenSearch port |

## Stopping Everything

```bash
# Backend
pkill -f "uvicorn main:app"

# Frontend
pkill -f "vite"

# Containers
container stop axiom-postgres axiom-opensearch

# Or remove them entirely
container rm axiom-postgres axiom-opensearch
container volume rm postgres-data opensearch-data
```

## Automated Script

`scripts/dev-macos.sh` automates the above steps (uses docker compose by default, adapt for Apple Container):

```bash
./scripts/dev-macos.sh start    # start infra + backend
./scripts/dev-macos.sh stop     # stop everything
./scripts/dev-macos.sh backend  # backend only (assumes infra running)
```

## Troubleshooting

### Python 3.14 fails to install dependencies
Use Python 3.12 instead — spacy/blis don't support 3.14 yet:
```bash
python3.12 -m venv .venv
```

### OpenSearch times out on startup
First startup takes ~15s to initialize. The backend will show warnings during this time but will connect once OpenSearch is ready.

### Missing database columns
The init SQL scripts may lag behind the SQLAlchemy models. Run:
```bash
python -c "from database.database import engine; from database.models import Base; Base.metadata.create_all(bind=engine)"
```

### `greenlet` import error
Async SQLAlchemy requires greenlet:
```bash
pip install greenlet
```

### MPS fallback warnings
Some PyTorch ops aren't implemented for MPS yet. `PYTORCH_ENABLE_MPS_FALLBACK=1` silently falls back to CPU for those ops. This is expected and correct.
