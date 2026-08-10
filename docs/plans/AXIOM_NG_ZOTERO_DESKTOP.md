# axiom-ng RAG: Zotero-Orchestrator + Processor Contract

**Status:** Draft for implementation review
**Owner:** @dudu
**Scope:** Neuer axiom-ng Orchestrator fuer ein Zotero-gehostetes wissenschaftliches RAG. Der alte Go-Port in `axiom_backend_ng/` ist kein fachlicher Ausgangspunkt; der Name `axiom-ng` bleibt, die Architektur wird neu gezogen.

## 1. Zielbild

axiom-ng wird die saubere Schnittstelle zwischen Zotero, Dokumentverarbeitung, Retrieval und spaeteren Research-Missions.

```
┌─────────────────────────────┐
│ Zotero Desktop auf macOS    │
│ - Local API :23119          │
│ - lokale PDFs/EPUBs         │
│ - Collections/Tags/Metadata │
└──────────────┬──────────────┘
               │ Zotero Local API, read-only fuer Indexing
               ▼
┌─────────────────────────────┐
│ axiom-ng Orchestrator       │
│ - ZoteroSource              │
│ - Sync-State + Job-Queue    │
│ - Processor Dispatcher      │
│ - RAG/Search/Chat API       │
│ - Workspace/Collection API  │
└──────────────┬──────────────┘
               │ Processor API Contract
               ▼
┌─────────────────────────────┐
│ Document Processor v1       │
│ Python, lokal               │
│ - Marker / PDF / EPUB       │
│ - Chunking                  │
│ - Embeddings                │
│ - Entities / Graph          │
│ - OpenSearch indexing       │
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│ Stores                      │
│ - Postgres + pgvector       │
│ - Knowledge Graph tables    │
│ - OpenSearch                │
└─────────────────────────────┘
```

Kernentscheidungen:

- Zotero bleibt Source of Truth fuer Dokumente, Metadaten, Tags und Collections.
- axiom-ng ist die einzige Zotero-Schnittstelle. Processor reden nicht mit Zotero.
- Die erste Processor-Implementierung bleibt Python, weil Marker, lokale Hardware/GPU/ML-Libs und die bestehende RAG-Qualitaet dort besser abgesichert sind.
- Die Processor-Schnittstelle wird aber jetzt stabil definiert, damit spaeter andere Processor gebaut werden koennen.
- Research Missions sind eine dritte Komponente und kommen erst, wenn Zotero -> Index -> Retrieval/Chat stabil funktioniert.

## 2. Phase-0-Befund: Zotero Local API

Lokaler Zugriff wurde am Mac verifiziert.

```
Base URL:              http://localhost:23119/api
Zotero API Version:    3
Zotero Schema Version: 44
Zotero Server ID:      ZQpTgJQ5H9O0
Last-Modified-Version: 181
Zotero Version:        10.0-beta.23+14fd49985
Collections:           20
Items total:           39
Book items:            16
Attachments:           23
```

Read-only Endpoints fuer axiom-ng:

```http
GET /users/0/collections
GET /users/0/items?limit=100
GET /users/0/items?since=<last_modified_version>&limit=100
GET /users/0/items/{itemKey}
GET /users/0/items/{attachmentKey}/file/view/url
GET /itemTypes
GET /itemTypeFields?itemType=book
```

Wichtiger Befund:

- `/users/0/items/{attachmentKey}/file/view/url` liefert robust einen `file://`-URI, z.B. `file:///Users/dudu/Zotero/storage/...pdf`.
- `GET /file` wird nicht als Primärweg verwendet, weil der lokale Test keinen brauchbaren Redirect-Header geliefert hat.
- Lesen braucht lokal keinen API-Key. Schreibzugriffe auf Zotero sind fuer das RAG-Indexing nicht Teil von Phase 1.

## 3. Zotero Data Model in axiom-ng

axiom-ng normalisiert Zotero-Items in eigene, stabile Records. Der Zotero-Key allein reicht nicht, weil Parent-Items, Attachments, Versionen, mehrere Dateiformate und geloeschte Items sauber getrennt werden muessen.

### `zotero_sources`

Eine lokale Zotero-Bibliothek.

```sql
CREATE TABLE zotero_sources (
  id UUID PRIMARY KEY,
  base_url TEXT NOT NULL,
  library_id TEXT NOT NULL DEFAULT 'users/0',
  server_id TEXT,
  schema_version INTEGER,
  last_modified_version BIGINT NOT NULL DEFAULT 0,
  last_sync_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (base_url, library_id)
);
```

### `zotero_documents`

Ein wissenschaftliches Parent-Item, z.B. ein Buch.

```sql
CREATE TABLE zotero_documents (
  id UUID PRIMARY KEY,
  source_id UUID NOT NULL REFERENCES zotero_sources(id),
  zotero_key TEXT NOT NULL,
  zotero_version BIGINT NOT NULL,
  item_type TEXT NOT NULL,
  title TEXT NOT NULL,
  creators JSONB NOT NULL DEFAULT '[]',
  abstract_note TEXT,
  publication_year INTEGER,
  publisher TEXT,
  isbn TEXT,
  doi TEXT,
  url TEXT,
  language TEXT,
  metadata JSONB NOT NULL DEFAULT '{}',
  tags JSONB NOT NULL DEFAULT '[]',
  collections JSONB NOT NULL DEFAULT '[]',
  deleted BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_id, zotero_key)
);
```

### `zotero_attachments`

Ein konkreter PDF-/EPUB-Anhang zu einem Parent-Item.

```sql
CREATE TABLE zotero_attachments (
  id UUID PRIMARY KEY,
  source_id UUID NOT NULL REFERENCES zotero_sources(id),
  document_id UUID NOT NULL REFERENCES zotero_documents(id),
  zotero_key TEXT NOT NULL,
  zotero_version BIGINT NOT NULL,
  parent_zotero_key TEXT NOT NULL,
  link_mode TEXT NOT NULL,
  content_type TEXT NOT NULL,
  filename TEXT NOT NULL,
  file_uri TEXT,
  local_path TEXT,
  content_hash TEXT,
  file_size BIGINT,
  mtime_ms BIGINT,
  preferred BOOLEAN NOT NULL DEFAULT false,
  deleted BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_id, zotero_key)
);
```

Preferred attachment rule for v1:

1. Prefer PDF over EPUB when both exist for the same parent.
2. Use EPUB only when no PDF exists.
3. Store all attachments, but enqueue only the preferred one by default.

## 4. Processor Contract

Der wichtigste Teil des Plans ist ein stabiler Vertrag. Der erste Processor ist Python; spaeter koennen andere Implementierungen denselben Vertrag bedienen.

### Transport v1

Lokal zuerst:

- HTTP auf `127.0.0.1:<processor_port>`.
- axiom-ng uebergibt lokale Pfade, weil Orchestrator und Processor auf demselben Mac laufen.
- Remote-Processor sind explizit out of scope, bis Content Delivery definiert ist.

### Request

```json
{
  "job_id": "uuid",
  "source": {
    "type": "zotero",
    "source_id": "uuid",
    "server_id": "ZQpTgJQ5H9O0"
  },
  "document": {
    "document_id": "uuid",
    "zotero_key": "5J6XFMNP",
    "item_type": "book",
    "title": "Ganzheitliches Life Cycle Management",
    "creators": [],
    "publication_year": 2024,
    "publisher": null,
    "isbn": null,
    "doi": null,
    "tags": [],
    "collections": ["ORG", "Nachhaltigkeit"],
    "metadata": {}
  },
  "attachment": {
    "attachment_id": "uuid",
    "zotero_key": "NU8SS6HG",
    "content_type": "application/pdf",
    "filename": "ganzheitliches-life-cycle-management.pdf",
    "local_path": "/Users/dudu/Zotero/storage/NU8SS6HG/file.pdf",
    "content_hash": "sha256:..."
  },
  "processing": {
    "mode": "full",
    "force_rebuild": false,
    "build_knowledge_graph": true,
    "index_opensearch": true,
    "extract_images": true,
    "language_hint": "de"
  }
}
```

### Response

```json
{
  "job_id": "uuid",
  "status": "completed",
  "document_id": "uuid",
  "attachment_id": "uuid",
  "content_hash": "sha256:...",
  "stats": {
    "pages": 312,
    "chunks": 684,
    "images": 12,
    "entities": 4479,
    "entity_relationships": 1694,
    "chunk_relationships": 683,
    "opensearch_indexed_chunks": 684
  },
  "outputs": {
    "markdown_path": "/path/to/processed.md",
    "manifest_path": "/path/to/manifest.json"
  },
  "warnings": []
}
```

### Error Response

```json
{
  "job_id": "uuid",
  "status": "failed",
  "error": {
    "code": "PDF_CONVERSION_FAILED",
    "message": "marker failed with exit code 1",
    "retryable": false,
    "stage": "convert"
  }
}
```

### Required Processor Endpoints

```http
GET  /health
GET  /capabilities
POST /process
GET  /jobs/{job_id}
POST /jobs/{job_id}/cancel
```

`/capabilities` example:

```json
{
  "name": "axiom-python-processor",
  "version": "0.1.0",
  "formats": ["application/pdf", "application/epub+zip"],
  "features": {
    "marker": true,
    "chunking": true,
    "dense_embeddings": true,
    "sparse_embeddings": true,
    "gliner_entities": true,
    "mrebel_relationships": true,
    "knowledge_graph": true,
    "opensearch": true
  }
}
```

## 5. Ingest Queue

axiom-ng bekommt eine eigene Queue, weil Zotero-Attachments nicht identisch mit den alten `documents.processing_status`-Rows sind. Die Queue ist die Wahrheit fuer Zotero-Ingest.

```sql
CREATE TYPE ingest_job_status AS ENUM (
  'pending',
  'claimed',
  'processing',
  'completed',
  'failed',
  'cancelled',
  'skipped'
);

CREATE TABLE ingest_jobs (
  id UUID PRIMARY KEY,
  source_id UUID NOT NULL REFERENCES zotero_sources(id),
  document_id UUID NOT NULL REFERENCES zotero_documents(id),
  attachment_id UUID NOT NULL REFERENCES zotero_attachments(id),
  status ingest_job_status NOT NULL DEFAULT 'pending',
  content_hash TEXT,
  force_rebuild BOOLEAN NOT NULL DEFAULT false,
  claimed_by TEXT,
  lease_until TIMESTAMPTZ,
  attempt INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 3,
  error_code TEXT,
  error_message TEXT,
  processor_name TEXT,
  processor_version TEXT,
  result JSONB NOT NULL DEFAULT '{}',
  enqueued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  started_at TIMESTAMPTZ,
  completed_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX ingest_jobs_idempotency_idx
ON ingest_jobs (attachment_id, content_hash)
WHERE force_rebuild = false;
```

Claim SQL:

```sql
WITH next_job AS (
  SELECT id
  FROM ingest_jobs
  WHERE status = 'pending'
     OR (status IN ('claimed', 'processing') AND lease_until < now())
  ORDER BY enqueued_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
UPDATE ingest_jobs j
SET status = 'claimed',
    claimed_by = :worker_id,
    lease_until = now() + (:lease_seconds || ' seconds')::interval,
    attempt = attempt + 1,
    started_at = COALESCE(started_at, now()),
    updated_at = now()
FROM next_job
WHERE j.id = next_job.id
RETURNING j.*;
```

Idempotenz:

- Same `attachment_id + content_hash` and no `force_rebuild` -> skip.
- Changed Zotero version but same file hash -> metadata update only, no full reprocess.
- Changed file hash -> enqueue full process.
- Missing local file -> job failed with `FILE_NOT_FOUND`, retryable false.

## 6. axiom-ng REST API

Clients reden nur mit axiom-ng, nie direkt mit Zotero.

```http
POST /api/zotero/sync
GET  /api/zotero/status
GET  /api/zotero/collections
GET  /api/zotero/documents
GET  /api/zotero/documents/{id}

GET  /api/ingest/jobs
GET  /api/ingest/jobs/{id}
POST /api/ingest/jobs/{id}/retry
POST /api/ingest/jobs/{id}/cancel

POST /api/rag/search
POST /api/rag/chat
GET  /api/rag/sources/{chunk_id}
GET  /api/rag/graph
GET  /api/rag/entities

POST /api/research/missions
GET  /api/research/missions/{id}
```

Phase-1 API-Scope:

- `/api/zotero/*`
- `/api/ingest/*`
- `/api/rag/search`

Nicht Phase 1:

- Schreibzugriffe in Zotero.
- Remote Processor.
- Research Missions.
- Neues Frontend.

## 7. Retrieval Requirements

Der Smoke-Test aus dem bestehenden Axiom bleibt das Qualitaetsgate:

Query:

> Ist das St. Galler Management-Modell bei Umweltanalysen hilfreich und warum? Finde 5 Quellen.

Pass-Kriterien:

- `POST /api/rag/search` liefert mindestens 5 relevante Passagen.
- Jede Passage hat `document_id`, `zotero_key`, `attachment_key`, Titel, Autoren, Jahr, Chunk-ID, Textauszug und nach Moeglichkeit Seite/Section.
- Search nutzt Hybrid Retrieval: dense + sparse/BM25 + rerank.
- Wenn Knowledge Graph aktiviert ist: Logs/Response-Debug zeigen Query-Entity-Lookup und Graph Expansion.
- `POST /api/rag/chat` darf nur auf Basis der gelieferten Passagen antworten und muss strukturierte Sources zurueckgeben.

Wichtig: Ein Chat-Response ohne strukturierte `sources` gilt nicht als bestanden, selbst wenn die Antwort plausibel klingt.

## 8. Phasen

### Phase 0 - Local API Spike (done, weiter haerten)

- Zotero Local API erreichbar.
- Collections/Items/Attachments gezaehlt.
- Ein Attachment ueber `/file/view/url` zu lokalem Pfad aufgeloest.
- Naechster Schritt: alle 23 Attachments pruefen: Pfad existiert, Hash berechnen, preferred attachment pro Parent bestimmen.

### Phase 1 - ZoteroSource + Queue

- `ZoteroSource` implementieren.
- `zotero_sources`, `zotero_documents`, `zotero_attachments`, `ingest_jobs` Migration.
- Full sync + incremental sync via `since=<last_modified_version>`.
- Preferred attachment selection.
- Idempotenz ueber content hash.
- Keine Dokumentverarbeitung in dieser Phase ausser Hashing/metadata normalization.

### Phase 2 - Python Processor Adapter

- Bestehende Python-Pipeline als lokaler HTTP-Processor kapseln.
- `/health`, `/capabilities`, `/process`, `/jobs/{id}` implementieren.
- axiom-ng Dispatcher ruft Processor mit lokalem Pfad + normalisierten Metadaten.
- Processor schreibt in Stores: Postgres chunks/KG, pgvector, OpenSearch.
- E2E: ein Zotero-PDF wird verarbeitet und ist suchbar.

### Phase 3 - RAG Search API

- `POST /api/rag/search` implementieren.
- User-/Workspace-Scoping ueber Zotero Collections und spaeter Axiom-Workspaces.
- Sources-Response stabilisieren.
- Smoke-Test mit 5 Quellen bestehen.

### Phase 4 - Chat API

- `POST /api/rag/chat` als grounded chat ueber Search API.
- Keine freie Antwort ohne Sources.
- Prompt/response contract fuer wissenschaftliche Arbeit: Quellenpflicht, Unsicherheiten, direkte Textstellen.

### Phase 5 - Research Missions

- Erst nach funktionierendem RAG.
- Mission Agent nutzt ausschliesslich axiom-ng APIs: search, chat, sources, collections.
- Mission Agent schreibt Notizen/Outlines/Reports, aber verarbeitet keine Dokumente und spricht nicht mit Zotero.

### Phase 6 - Remote Processor

- Erst wenn lokale Pipeline stabil ist.
- Processor Request bekommt dann keinen lokalen Pfad mehr, sondern `content_uri` oder `blob_ref`.
- Content Delivery Design erforderlich: signed download endpoint, object store oder upload bundle.

## 9. Definition of Done fuer den ersten Index

- Alle Zotero-Parent-Items und Attachments sind in axiom-ng gespiegelt.
- Fuer jedes Parent-Item ist ein preferred Attachment bestimmt.
- Alle preferred Attachments haben lokalen Pfad, Hash und Job-Status.
- Alle Jobs laufen durch den Python Processor.
- RAG Search findet relevante Passagen aus dem neuen Zotero-Index.
- Der St.-Galler-Smoke-Test liefert 5 strukturierte Quellen.
- Chat gibt keine Antwort ohne Sources.
- Zotero bleibt Source of Truth; Processor hat keinen Zotero-Zugriff.

