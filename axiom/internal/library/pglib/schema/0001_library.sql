-- 0001_library.sql
-- Library component tables (F06, #300). Own migration set, own ledger
-- (library_schema_migrations) on the SAME physical database as the core
-- schema — the physical split is F12/DM05. Additive only: no core table
-- is touched, so the F01 baseline fingerprint (derived from the core
-- migration set alone) stays untouched.
--
-- No BLOBs anywhere: rendition bytes live hashed under the staging root
-- (see staging.go); these tables carry descriptors only.

CREATE TABLE IF NOT EXISTS library_imports (
  import_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  idempotency_key TEXT NOT NULL UNIQUE,
  payload_hash TEXT NOT NULL,               -- sha256 over canonical request JSON + content
  record_type TEXT NOT NULL,
  request_json JSONB NOT NULL,              -- the frozen F03 ImportRequest
  status TEXT NOT NULL,                     -- F03 ImportStatus vocabulary
  -- staging descriptor (the file lives under <artifact-root>/library_staging/<sha256>)
  staging_sha256 TEXT NOT NULL,
  staging_size BIGINT NOT NULL,
  media_type TEXT NOT NULL,                 -- derived from magic bytes ONLY
  -- outcome bookkeeping (filled as the saga advances)
  failure_code TEXT,
  failure_message TEXT,
  record_provider_id TEXT,
  rendition_provider_id TEXT,
  collection_provider_id TEXT,
  record_id TEXT,                           -- Library identity once committed
  rendition_id TEXT,
  revision_id BIGINT,                       -- published library_source_revisions row
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS library_import_events (
  import_id UUID NOT NULL REFERENCES library_imports(import_id) ON DELETE CASCADE,
  seq BIGINT NOT NULL,
  kind TEXT NOT NULL,                       -- state_entered | decision_offered | decision_resolved | provider_write | provenance | retry | terminal
  detail JSONB NOT NULL DEFAULT '{}',
  at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (import_id, seq)
);

-- Provider-Zwischenstände der Saga: the repairable part-states. A step row
-- records the dedup anchor (provider ref) the moment it is known; crash
-- between provider write and bookkeeping is absorbed by the provider-side
-- idempotency keyed on that anchor. Teilzustände bleiben stehen — kein
-- automatisches DELETE als Kompensation.
CREATE TABLE IF NOT EXISTS library_import_steps (
  import_id UUID NOT NULL REFERENCES library_imports(import_id) ON DELETE CASCADE,
  step TEXT NOT NULL,                       -- inspecting | resolving_metadata | ensuring_collections | creating_record | uploading_rendition | verifying
  state TEXT NOT NULL,                      -- in_progress | done
  provider_ref TEXT,                        -- dedup anchor / provider-assigned id
  detail JSONB NOT NULL DEFAULT '{}',
  attempts INT NOT NULL DEFAULT 1,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (import_id, step)
);

CREATE TABLE IF NOT EXISTS library_external_identifiers (
  kind TEXT NOT NULL CHECK (kind IN ('doi','isbn')),
  normalized_value TEXT NOT NULL,
  record_id TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- one record per identifier value: two records claiming the same DOI is
  -- a data error the constraint makes loud, not silent.
  UNIQUE (kind, normalized_value)
);

-- Field provenance of the metadata verify ladder: append-only. The
-- effective provenance of a field is its latest applied=true row; weaker
-- attempts land as applied=false rows — the audit trail that documents a
-- rejected overwrite (DoD sonde).
CREATE TABLE IF NOT EXISTS library_metadata_provenance (
  id BIGSERIAL PRIMARY KEY,
  import_id UUID NOT NULL REFERENCES library_imports(import_id) ON DELETE CASCADE,
  field TEXT NOT NULL,
  source TEXT NOT NULL,                     -- document | identifier | crossref | open_library | provider_existing | user
  resolver_version TEXT NOT NULL DEFAULT '',
  confidence DOUBLE PRECISION NOT NULL DEFAULT 1.0,
  applied BOOLEAN NOT NULL,
  value TEXT NOT NULL DEFAULT '',
  at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS library_metadata_provenance_import
  ON library_metadata_provenance (import_id, field);

-- Published source revisions — the Library→Store bridge artifacts (F09
-- turns the Store onto revision intake). revision_id is monotonic per
-- (source_id, record_id). Origins: import | sync | heal (the Mits-Schrieb
-- points of F06).
CREATE TABLE IF NOT EXISTS library_source_revisions (
  source_id TEXT NOT NULL,
  record_id TEXT NOT NULL,
  rendition_id TEXT NOT NULL DEFAULT '',
  revision_id BIGINT NOT NULL,
  content_hash TEXT NOT NULL,
  media_type TEXT NOT NULL,
  bibliography JSONB NOT NULL,
  locator_capabilities JSONB NOT NULL,
  content_ticket TEXT NOT NULL,
  origin TEXT NOT NULL DEFAULT 'import',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (source_id, record_id, rendition_id, revision_id)
);
CREATE INDEX IF NOT EXISTS library_source_revisions_latest
  ON library_source_revisions (source_id, record_id, rendition_id, revision_id DESC);
