# axiom-ng: Go Backend Migration Plan

**Status:** Draft for review
**Owner:** @jamu85
**Created:** 2026-04-18
**Target delivery:** 10–14 engineering weeks (one FTE) for Phase 1–5; Phase 6 (local distribution) adds 2–3 weeks.

---

## 1. Motivation

axiom today ships as three Python containers built from the same image:

- `axiom-backend` — FastAPI + uvicorn, owns the agent controller, RAG pipeline, REST/WebSocket API.
- `axiom-doc-processor` — loads the same Python code, but only runs `services/background_document_processor.py`.
- `axiom-cli` — loads the same code again for one-shot bulk ingests.

This split exists because Python's GIL made a single-process model painful under concurrent document ingestion + interactive requests + GPU work. Splitting by container papered over the concurrency problem but introduced new ones:

- **Duplicate RSS.** `torch` + `transformers` + `sentence-transformers` + `spacy` are loaded into two always-on containers. Measured duplication: **6.7–8.8 GB**.
- **Duplicate VRAM** (historical). Solved in April 2026 by the shared GPU-worker subprocess, but it took effort and the fix remains fragile under restart races.
- **Deployment complexity.** Three images, three compose services, mutual dependencies, inter-container IPC over `/tmp`.
- **Distribution cost.** PyInstaller bundles for a "local binary" come in around 2–3 GB — impractical for end-user distribution.

The hypothesis: a single statically-compiled Go binary with goroutines replaces backend + doc-processor + CLI, while Python continues to host **only** the ML inference code that has no viable Go replacement (Marker, mREBEL, GLiNER, BGE).

axiom-ng is not a feature rewrite. It is a **bit-for-bit drop-in for the existing React/TypeScript frontend and its REST/WebSocket contract**, plus the existing Postgres+pgvector and OpenSearch data stores. No schema changes. No frontend changes. Same HTTP API, same event names on WebSockets, same cookie-based auth.

---

## 2. Goals and non-goals

### In scope

- New Go binary `axiom-ng` exposing the full `/api/*` and `/ws/*` surface on port 8001 (or whatever `BACKEND_PORT` is).
- Pass the existing frontend against axiom-ng with no code changes.
- Keep the same `init-db` migrations, same Postgres schema, same OpenSearch indices.
- Keep the existing Python GPU worker and short-lived `pdf_worker` / `relation_worker` subprocesses as-is. Go calls them over the stable msgpack-over-unix-socket protocol that already exists.
- Single static binary (with required ML shared libraries shipped alongside) suitable for local non-Docker deployment.
- Deprecation path: run both stacks side-by-side during migration, cut over per-endpoint.

### Out of scope

- Frontend changes.
- Rewriting the ML sidecars in Go.
- Changing the Postgres schema, pgvector dimensions, or OpenSearch mappings.
- Replacing Marker, mREBEL, GLiNER, or BGE with non-Python equivalents (covered as "optional R&D" in Phase 6).
- New features. This is a migration, not a product iteration.

---

## 3. Target architecture

```
                        ┌──────────────────────────┐
                        │  React frontend (nginx)  │
                        └────────────┬─────────────┘
                                     │  REST + WS  (unchanged)
                                     ▼
        ┌──────────────────────────────────────────────────────┐
        │                    axiom-ng (Go)                     │
        │                                                      │
        │  chi router ──┬── REST handlers                      │
        │               ├── WebSocket hub (coder/websocket)    │
        │               ├── auth (jwt v5, bcrypt, CSRF)        │
        │               └── OpenAI-compat endpoint             │
        │                                                      │
        │  services ────┬── agent controller (research loop)   │
        │               ├── writing controller                 │
        │               ├── mission manager                    │
        │               ├── document ingest worker pool        │
        │               ├── RAG retriever (hybrid + RRF)       │
        │               ├── metadata enrichment                │
        │               └── citation / references              │
        │                                                      │
        │  data ────────┬── pgx/v5 + pgvector-go  ──► Postgres │
        │               └── opensearch-go v4      ──► OpenSearch│
        │                                                      │
        │  clients ─────┬── openai-go (LLM dispatcher)         │
        │               ├── Tavily / Linkup / SearXNG (HTTP)   │
        │               └── GPU-worker msgpack client          │
        └──────────────────────┬───────────────────────────────┘
                               │  msgpack over AF_UNIX
                               ▼
        ┌──────────────────────────────────────────────────────┐
        │   Python GPU worker (unchanged, long-lived)          │
        │   BGE-M3, BGE-Reranker, GLiNER, (optional: CLIP)     │
        └──────────────────────────────────────────────────────┘
              │                                │
              ▼                                ▼
        Marker subprocess              mREBEL subprocess
        (short-lived, per import)      (short-lived, per doc)
```

### Why one Go process, not three

- Backend, doc-processor, and CLI share ~90% of their Python code today. They are the same image with different entrypoints. Collapsing them is the whole point.
- Goroutines solve the concurrency case that originally motivated the split (GIL contention between request handling and background ingestion).
- Document ingest runs as an in-process worker pool (errgroup + buffered channel). Interactive requests and ingest share the process but do not share a mutex.

### Why the Python GPU worker stays exactly as it is

- **BGE-M3 / BGE-Reranker / GLiNER / mREBEL / Marker** have no production-grade Go equivalents. ONNX Runtime Go (`yalue/onnxruntime_go`) works for BGE and GLiNER-onnx but requires model export, tokenizer reimplementation (`daulet/tokenizers`), and introduces a cgo dependency chain we don't need on day one.
- The existing `ai_researcher/gpu_worker/` already speaks a stable, language-agnostic protocol: `[4-byte big-endian length][msgpack payload]` over AF_UNIX. Go has first-class msgpack (`github.com/vmihailenco/msgpack/v5`) and native Unix-socket support. The Go port of the client is a one-day task.
- Keeping Python for ML means we continue to benefit from HuggingFace releases, FlagEmbedding updates, Marker improvements without any Go work.

### What Go replaces directly

| Python                               | Go replacement                                    |
|--------------------------------------|---------------------------------------------------|
| FastAPI + uvicorn                    | chi + net/http                                    |
| SQLAlchemy ORM                       | sqlc (generated) + pgx/v5 + pgvector-go           |
| asyncpg / psycopg2                   | pgx/v5                                            |
| opensearch-py                        | opensearch-go v4                                  |
| python-jose (JWT)                    | golang-jwt/jwt v5                                 |
| passlib / bcrypt                     | golang.org/x/crypto/bcrypt                        |
| pydantic                             | struct + go-playground/validator v10 + JSON tags  |
| FastAPI WebSockets                   | coder/websocket                                   |
| `services/background_document_processor.py` | errgroup + buffered channel worker pool    |
| openai SDK                           | openai/openai-go (official)                       |
| BeautifulSoup4 + lxml                | PuerkitoBio/goquery + golang.org/x/net/html       |
| tavily-python, linkup-sdk            | stdlib net/http                                   |
| httpx, requests                      | stdlib net/http                                   |
| pypandoc                             | shell out to `pandoc` binary                      |
| logging                              | log/slog                                          |
| python-dotenv + os.getenv            | koanf                                             |

### What Go **does not** replace (stays Python)

| Python                               | Why it stays                                      |
|--------------------------------------|---------------------------------------------------|
| FlagEmbedding (BGE-M3, reranker)     | No ONNX exports for sparse output; PyTorch native |
| GLiNER                               | HuggingFace-native, no Go binding                 |
| mREBEL (transformers)                | Seq2Seq + beam search, Python-entangled           |
| Marker PDF                           | Bundles layout + OCR + vision; no Go equivalent   |
| sentence-transformers (CLIP)         | Stays in Python GPU worker (could move to ONNX-Go later) |
| PyMuPDF / pymupdf4llm                | Used by Marker and as pre-parser; stays Python    |
| spaCy (fallback NER)                 | Already CPU-only; small; no Go port               |
| python-docx                          | Called from `pdf_worker`; acceptable              |
| weasyprint                           | Not used in current code path; can drop           |

---

## 4. Frontend compatibility: the contract surface

The Go backend must serve every URL the frontend currently calls. Based on the audit of `axiom_backend/api/*.py`:

### REST routers (from `main.py`)

| Router                  | Prefix        | Endpoint count | Notes                                                          |
|-------------------------|---------------|----------------|----------------------------------------------------------------|
| `auth`                  | `/api/auth`   | 6              | JWT in HttpOnly cookie + CSRF double-submit                    |
| `missions`              | `/api`        | ~37            | Largest surface. Research mission lifecycle + streaming notes. |
| `system`                | `/api/system` | 5              | Health, GPU status, component reset.                           |
| `chat`                  | `/api`        | 5              | MessengerAgent entry, question refinement.                     |
| `chats`                 | `/api`        | 11             | Chat history CRUD.                                             |
| `documents`             | `/api`        | ~41            | Upload/list/delete/search + document groups + images.          |
| `settings`              | `/api`        | 9              | User settings, profile, language, appearance.                  |
| `dashboard`             | `/api/dashboard` | 1           | Aggregated stats.                                              |
| `writing`               | (no prefix)   | ~23            | Writing sessions + drafts + references.                        |
| `websockets`            | (no prefix)   | 3 WS endpoints | Documents, research, writing.                                  |
| `admin`                 | (router sets) | ~5             | Admin panel.                                                   |
| `research_reports`      | (no prefix)   | ~5             | Report versions.                                               |
| `languages`             | (no prefix)   | 2              | Supported languages.                                           |
| `rag`                   | (no prefix)   | 6              | Knowledge graph, chunks, entities.                             |
| `openai_compat`         | `/api`        | 4              | `/v1/chat/completions` + API key management.                   |

**Approximate total: ~160 REST endpoints + 3 WebSocket endpoints.**

### WebSocket event contract (must match byte-for-byte)

`/ws/documents/{user_id}`:
- `document_progress`, `active_jobs`, `processing_update`

`/api/ws/research`:
- `mission_started`, `mission_completed`, `mission_stopped`
- `plan_update`, `notes_update`, `draft_update`, `logs_update`, `context_update`
- `status_update`

`/ws/{session_id}` (writing):
- `writing_status`, `draft_content_update`, `writing_stats_update`, `chat_title_update`

Heartbeat: 30s ping/pong on research and writing.

### Data stores (unchanged)

- **Postgres** schema: 19 SQLAlchemy models → 19 tables. Primary keys are UUID except `User`, `SystemSetting`, `WritingSessionStats` (ints). `dense_embedding` is `vector` via pgvector; sparse embeddings are JSONB.
- **OpenSearch** indices: per-document BM25 fulltext; queried via bool+match+filter.

The schema stays verbatim. Go talks to the same database with the same column names. Migrations in `init-db/` continue to own DDL.

---

## 5. Expected savings (what we're buying)

Numbers are ranges from the Python→Go analysis in §9.

### RAM (idle, single host)

| Role              | Python (today) | axiom-ng (Go) | Saved       |
|-------------------|----------------|---------------|-------------|
| backend container | 1.2–1.5 GB     | 50–80 MB      | 1.1–1.5 GB  |
| doc-processor     | 1.0–1.2 GB     | 0 (merged)    | 1.0–1.2 GB  |
| cli (when running)| 1.0–1.2 GB     | 0 (merged)    | 1.0–1.2 GB  |
| **Combined idle** | **3.2–3.9 GB** | **50–80 MB**  | **~3.2 GB** |

### RAM (loaded, ~50 concurrent requests)

Python ≈ 5–7 GB → Go ≈ 200–300 MB. **~95% reduction.**

### VRAM

No change. The GPU worker holds the same models (~2.5–3.1 GB idle, up to 5–6 GB during mREBEL bursts).

### Concurrency

- Per-request Python footprint ≈ 50–100 MB.
- Per-goroutine Go footprint ≈ 3–4 KB.
- WebSocket capacity: Python uvicorn ~100 concurrent → Go `coder/websocket` 10k+ on the same box.

### Distribution

- Docker image: 2.5–3.0 GB (Python) → 150–200 MB (Go with `scratch`/distroless + bundled ORT not needed for Phase 1).
- Single-binary distribution for local use: 2–3 GB PyInstaller → **30–40 MB Go binary**.

### Startup

- Container cold start: 12–23 s → 2–3 s.
- Single-binary startup: 2–5 s → 50–100 ms.

These numbers are conservative. They assume the GPU worker keeps its current VRAM budget and we don't move CLIP / Marker off Python.

---

## 6. Target tech stack

All choices are the mainstream Go 2026 picks. One line each on why.

- **Web framework:** `go-chi/chi/v5` — stdlib-compatible, idiomatic middleware.
- **SQL:** `jackc/pgx/v5` + `pgvector/pgvector-go`, queries generated with `sqlc`, migrations with `pressly/goose` (or keep Python's existing `init-db/` runner if we want zero schema ownership change).
- **Search:** `opensearch-project/opensearch-go/v4`, wrapped in a small internal query builder.
- **WebSockets:** `coder/websocket` (maintained fork of nhooyr).
- **Validation:** `go-playground/validator/v10`.
- **Auth:** `golang-jwt/jwt/v5` + `golang.org/x/crypto/bcrypt`.
- **LLM:** `openai/openai-go` (official).
- **HTTP client:** stdlib `net/http` with tuned `http.Client`.
- **HTML parsing:** `PuerkitoBio/goquery`.
- **msgpack:** `vmihailenco/msgpack/v5`.
- **Logging:** stdlib `log/slog`.
- **Tracing/metrics:** `go.opentelemetry.io/otel` with otelchi, otelpgx autoinstrumentation.
- **Config:** `knadh/koanf` (env + yaml).
- **Packaging:** `goreleaser` + `embed.FS` for static assets and SQL migrations.
- **Worker pool:** stdlib `errgroup` + buffered channels for the document ingest queue.
- **Testing:** stdlib `testing` + `testify`, `dockertest` for integration with Postgres+OpenSearch.

---

## 7. Migration phases

Each phase ends in a merge-ready state. We run Python and Go side by side throughout, routing by endpoint via nginx.

### Phase 0 — Scaffolding (0.5 week)

- Create `axiom_backend_ng/` directory at repo root (Go module `github.com/Cyb3rDudu/axiom/axiom-ng`).
- `go.mod`, Makefile, Dockerfile (multi-stage, `FROM scratch` final image), pre-commit.
- CI: `go test ./...`, `golangci-lint`, build matrix (linux/amd64, linux/arm64, darwin/arm64).
- Add `nginx/` split-route config with `AXIOM_NG_ENABLED` toggle.
- Wire up `docker-compose.override.ng.yml` so contributors can start the Go backend against the existing Postgres+OpenSearch+GPU-worker.

**Exit criteria:** empty `axiom-ng` binary serves `/health` alongside the Python backend; CI green.

### Phase 1 — Data + auth foundation (1.5 weeks)

Goal: anything the frontend does that does not touch an agent or a GPU works against the Go binary.

- Port the Postgres schema to `sqlc` queries. Start with `User`, `Chat`, `Message`, `Document`, `DocumentGroup`, `DocumentChunk`, `DocumentImage`, `WritingSession`, `Draft`, `Reference`, `WritingSessionStats`, `SystemSetting`, `SupportedLanguage`, `PromptTemplate`, `Mission`, `MissionExecutionLog`, `DocumentProcessingJob`, `ResearchReport`.
- Implement auth: `/api/auth/login`, `/api/auth/logout`, `/api/auth/me`, `/api/auth/register`, `/api/auth/change-password`, `/api/auth/test-csrf`. Bcrypt verify, JWT sign, cookie set with identical attributes (`HttpOnly`, `SameSite=Lax`, `Secure` when TLS).
- Middleware: request-scoped user context, CSRF double-submit, CORS (mirror `main.py:get_cors_origins()`), activity tracker, slog access logging.
- Implement `/api/settings/*`, `/api/dashboard`, `/api/languages`, `/api/system/status`, `/api/system/config`.
- Implement `/api/chats/*` (history CRUD).

**Exit criteria:** log in from the React app against axiom-ng; settings + dashboard pages render; chat list/create/delete works.

### Phase 2 — Documents + groups + search (2 weeks)

- Port `/api/documents/*` read-only endpoints first: list, get, view, metadata, images, groups list/get.
- OpenSearch BM25 search via `opensearch-go v4`.
- Hybrid RAG retriever: dense query via pgvector (`<=>` operator), sparse via JSONB + Go-side cosine, RRF fusion. Matches `core_rag/retriever.py`.
- Implement Go msgpack client for the GPU worker. Protocol: `[4B big-endian len][msgpack]`. Methods: `embed_query`, `embed_chunks`, `rerank`, `extract_entities`, `health`, `shutdown`.
- Implement `/api/documents/search/fulltext` and `/api/search/`.
- Implement document group membership endpoints (add/remove/bulk).
- Implement `/api/rag/*` read endpoints: chunks, entities, graph.

**Exit criteria:** frontend document library page served entirely by axiom-ng; search returns identical top-k to Python path on the same corpus (validated by differential test).

### Phase 3 — Document ingest worker pool (1.5 weeks)

- Replace `services/background_document_processor.py` with an in-process Go worker pool.
- Workers claim `Document.processing_status='pending'` rows with `FOR UPDATE SKIP LOCKED`.
- Per-document pipeline (Go orchestration, Python sidecars for ML):
  1. Copy/move file, compute hash.
  2. Shell out to `python -m ai_researcher.pdf_worker` for PDF→markdown+images (existing JSON-over-stdio protocol).
  3. Chunk markdown (pure Go port of `core_rag/chunker.py` — section-aware, token-aware).
  4. Call GPU worker `embed_chunks` over msgpack.
  5. Upsert chunks into Postgres + OpenSearch.
  6. Call GPU worker `extract_entities` for knowledge graph.
  7. Optionally shell out to `python -m ai_researcher.relation_worker` for mREBEL (gated by config).
  8. Emit WebSocket `document_progress` events.
- Implement upload endpoint `POST /api/documents/upload`, bulk-delete, bulk-reprocess, bulk-enrich, bulk-reembed.
- Implement `/api/internal/document-progress` for sidecar callbacks.
- Retire `axiom-doc-processor` container in compose (still available but off by default once Phase 3 merges).

**Exit criteria:** full ingest of a 100-document test corpus via the Go binary produces byte-identical chunk/embedding counts as the Python path (tolerance: identical counts, embedding cosine ≥ 0.9999 since same model).

### Phase 4 — Chat, messenger, writing (2 weeks)

- Port chat entry: `/api/chat`, `/api/chat/generate-questions`, `/api/chat/refine-questions`, `/api/chat/approve-questions`, `/api/chat/status`.
- Port the MessengerAgent intent-detection loop. This is an LLM-driven classifier + delegator; translates cleanly because `model_dispatcher.py` is pure HTTP over OpenAI-compatible endpoints.
- Port `/api/openai-compat` (OpenAI-compatible `/v1/chat/completions` + API key management). Bearer auth via existing `User.api_key` column.
- Port writing sessions: `/sessions/*`, drafts, versions, references, stats.
- Port `/enhanced-chat-stream` using SSE.
- WebSocket `/ws/{session_id}` for writing: `draft_content_update`, `writing_status`, `writing_stats_update`, `chat_title_update`.

**Exit criteria:** writing flow end-to-end on the Go backend; OpenAI-compatible endpoint passes existing curl test suite.

### Phase 5 — Research missions + agent controller (3 weeks — the big one)

This is the biggest surface: ~8,900 LOC of Python controller code (`agent_controller`, `core_controller`, `research_manager`, `reflection_manager`, `writing_manager`, `report_generator`, `user_interaction`). The translation is mechanical — the code is mostly HTTP/LLM orchestration, not Python-specific — but it is a lot of HTTP/LLM orchestration.

- Port `MissionContext` as a Go struct backed by a single JSONB column (same column as today).
- Port the async execution loop: each mission runs in its own goroutine, with a `context.Context` for cancellation and a channel fan-out for WebSocket updates.
- Agents become Go structs implementing a small `Agent` interface (one method: `Run(ctx, input) (output, error)`). Each loads prompts from `PromptTemplate` (language fallback chain matches `prompt_loader.py`).
- Port PlanningAgent, ResearchAgent, ReflectionAgent, WritingAgent, EnhancedCollaborativeWritingAgent, WritingReflectionAgent, MessengerAgent, NoteAssignmentAgent.
- Port tools: web_search, jina_fetcher, web_page_fetcher, document_search, arxiv, calculator, reference_integration, writing_tools. Skip `python_tool.py` (no sandbox parity in Go; document as deferred).
- Port `/api/missions/*` endpoints.
- Port `/api/ws/research` event stream.
- Port `/api/research_reports/*` (versioned reports).

**Exit criteria:** kick off a 3-phase research mission in the frontend, watch notes/plan/draft stream live from axiom-ng, produce a completed report. Side-by-side run against the Python backend on the same question: agree on mission outline and final report sections ±1 section.

### Phase 6 — Cutover + local binary distribution (1.5 weeks)

- Flip nginx default upstream to axiom-ng.
- Keep the Python backend runnable behind a flag for rollback for 4 weeks.
- Add `goreleaser` config producing:
  - `axiom-ng_<version>_linux_amd64.tar.gz` (binary + `pdf_worker.tar.zst` + `gpu_worker.tar.zst` + install script that sets up a venv for the Python sidecars on first run).
  - Docker image (minimal debian-slim, includes `python3`, `pip install -r requirements-ml.txt`).
  - macOS and Linux Homebrew taps for the binary only (no GPU, CPU-only ML via ONNX later).
- Documentation: `docs/getting-started/local-binary.md`.

**Exit criteria:** `brew install axiom-ng && axiom-ng init && axiom-ng serve` works on a fresh Mac (CPU-only, no GPU worker).

### Phase 7 — Retire Python backend (0.5 week, after 4-week bake)

- Remove `axiom_backend/api/*`, `axiom_backend/services/*`, `axiom_backend/main.py`.
- Keep `axiom_backend/ai_researcher/gpu_worker/`, `pdf_worker/`, `relation_worker/` and their direct dependencies. These ship as a trimmed `ml-sidecars/` Python package.

---

## 8. Risks and mitigations

| Risk                                                         | Likelihood | Impact | Mitigation                                                                                                       |
|--------------------------------------------------------------|------------|--------|------------------------------------------------------------------------------------------------------------------|
| Subtle behavioral divergence from the agent controller       | High       | High   | Side-by-side run during Phase 5; golden-mission regression suite with recorded LLM responses.                    |
| pgvector query behavior differs between pgx and SQLAlchemy   | Low        | Med    | Identical SQL text from sqlc; differential test on top-k retrieval for 100 queries.                              |
| WebSocket event ordering/shape changes break the frontend    | Med        | High   | Contract-test recorded from the Python backend; replay against Go.                                               |
| GPU worker msgpack protocol drifts                           | Low        | High   | Pin protocol schema in `docs/ARCHITECTURE_GPU_WORKER.md`; add a protocol version handshake.                      |
| `pgvector-go` sparse vector handling                         | Med        | Med    | We already store sparse as JSONB, not pgvector's `sparsevec`. No change.                                         |
| Async behavior of background ingest under load               | Med        | Med    | Worker pool size defaults to 1 (same as today's single poller). Scale via config. Metric on queue depth + lag.   |
| Pandoc/Marker/mREBEL Python deps break on local install      | High       | Med    | Local binary ships with an install wizard that creates the ML venv; failure opens a GitHub issue with logs.      |
| Go team unfamiliar with SQLAlchemy loading semantics         | Med        | Low    | sqlc forces explicit joins — which is the correct behavior anyway. Audit existing N+1s as we go.                 |
| Effort estimate miss                                         | High       | Med    | Each phase has an independent exit criterion; we can stop at the end of any phase and still have a usable system.|

---

## 9. Effort estimate

| Phase | Title                                | Eng-weeks (1 FTE) | Cumulative |
|-------|--------------------------------------|-------------------|------------|
| 0     | Scaffolding                          | 0.5               | 0.5        |
| 1     | Data + auth foundation               | 1.5               | 2.0        |
| 2     | Documents + groups + search          | 2.0               | 4.0        |
| 3     | Document ingest worker pool          | 1.5               | 5.5        |
| 4     | Chat, messenger, writing             | 2.0               | 7.5        |
| 5     | Research missions + agent controller | 3.0               | 10.5       |
| 6     | Cutover + local binary distribution  | 1.5               | 12.0       |
| 7     | Retire Python backend                | 0.5               | 12.5       |

**Headline estimate: 12–14 engineering weeks with one focused engineer.** With two engineers in parallel (one on Phase 5, one on Phases 3/4), 8–10 weeks is realistic. Phase 5 is the long pole and does not parallelize well within itself.

---

## 10. Open questions (decide before Phase 1)

1. **Repository layout.** Sub-module `axiom-ng/` in the same repo, or a separate `axiom-ng` repo with a git submodule back-reference? Recommend same repo — simpler versioning, shared CI, shared issues.
2. **Migration runner.** Keep Python's `init-db/*.sql` + a Go migration runner, or port to goose? Recommend goose with the existing SQL files embedded via `embed.FS`.
3. **LLM provider support.** Drop the Z.AI / DeepSeek special-case branches and go pure OpenAI-compatible? Recommend yes where possible; they both expose OpenAI-compatible endpoints in 2026.
4. **Agent prompt storage.** Continue storing prompts in Postgres `PromptTemplate`, or move to embedded YAML? Recommend keep Postgres — admin UI already edits them.
5. **Do we ever rewrite Marker/BGE in Go via ONNX?** Not in this migration. Open R&D ticket for Phase 8+.
6. **Rollback window.** How long do we keep the Python backend runnable after cutover? Recommend 4 weeks, then archive the code in a `legacy-python-backend/` branch.

---

## 11. Deliverables checklist

- [ ] `axiom_backend_ng/` Go module (Phase 0)
- [ ] Auth + settings + chat history parity (Phase 1)
- [ ] Documents + search parity (Phase 2)
- [ ] Document ingest pool + doc-processor retirement (Phase 3)
- [ ] Writing + messenger + OpenAI-compat parity (Phase 4)
- [ ] Mission orchestrator + agent controller parity (Phase 5)
- [ ] goreleaser single-binary distribution (Phase 6)
- [ ] Python backend removal (Phase 7)
- [ ] Updated docs under `docs/architecture/axiom-ng/`
- [ ] Release notes in `CHANGELOG.md`

---

## Appendix A — Component inventory (raw material)

Line counts of the Python surface the Go port must cover (core code, excluding `.venv`):

| Area                                           | LOC     |
|------------------------------------------------|---------|
| `axiom_backend/api/`                           | 10,927  |
| `axiom_backend/services/`                      | 6,647   |
| `axiom_backend/database/`                      | 5,162   |
| `axiom_backend/ai_researcher/agentic_layer/`   | 5,341   |
| `axiom_backend/ai_researcher/core_rag/`        | 8,223   |
| `axiom_backend/ai_researcher/gpu_worker/`      | 307     |
| `axiom_backend/auth/`                          | ~160    |
| Other (config, logging, utils, scripts)        | ~4,500  |
| `axiom_backend/cli_*`                          | ~6,000  |
| **Relevant total (Python we replace with Go)** | ~37,000 |
| **Stays Python (GPU worker + sidecars + ML)**  | ~1,500  |

Not every Python line translates to a Go line. Expect the Go port to land at roughly 60–70% of the Python LOC (no Pydantic boilerplate, no SQLAlchemy verbosity, but more explicit error handling).

## Appendix B — Go dependency short list

```
github.com/go-chi/chi/v5
github.com/jackc/pgx/v5
github.com/pgvector/pgvector-go
github.com/opensearch-project/opensearch-go/v4
github.com/sqlc-dev/sqlc                 (dev-time)
github.com/pressly/goose/v3
github.com/coder/websocket
github.com/go-playground/validator/v10
github.com/golang-jwt/jwt/v5
golang.org/x/crypto
github.com/openai/openai-go
github.com/PuerkitoBio/goquery
github.com/vmihailenco/msgpack/v5
github.com/knadh/koanf/v2
go.opentelemetry.io/otel
github.com/stretchr/testify            (test)
github.com/ory/dockertest/v3           (test)
```
