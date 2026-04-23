-- Migration: Structured bibliography as first-class data
-- Purpose: Extend draft_references with structured citation fields + add
--          citation_entries link table so the bibliography can be managed
--          as data instead of inline Markdown. Feature-flag gated in the
--          application layer; legacy drafts keep the citation_text path.
-- Reference: Epic #51, sub-issue #52

-- Structured fields on draft_references. All nullable so the migration is
-- backwards-compatible: legacy rows keep working through citation_text.
ALTER TABLE draft_references
    ADD COLUMN IF NOT EXISTS authors JSONB,
    ADD COLUMN IF NOT EXISTS year INTEGER,
    ADD COLUMN IF NOT EXISTS title TEXT,
    ADD COLUMN IF NOT EXISTS container_title TEXT,
    ADD COLUMN IF NOT EXISTS publisher TEXT,
    ADD COLUMN IF NOT EXISTS pages TEXT,
    ADD COLUMN IF NOT EXISTS url TEXT,
    ADD COLUMN IF NOT EXISTS accessed_at DATE,
    ADD COLUMN IF NOT EXISTS doi TEXT,
    ADD COLUMN IF NOT EXISTS entry_key TEXT,
    ADD COLUMN IF NOT EXISTS source_fingerprint TEXT;

COMMENT ON COLUMN draft_references.authors IS
    'JSONB array of {family, given} objects. NULL for legacy entries that only have citation_text.';
COMMENT ON COLUMN draft_references.entry_key IS
    'Stable per-draft slug (e.g. "destatis-2024"). Used by in-text citations to link to this entry.';
COMMENT ON COLUMN draft_references.source_fingerprint IS
    'Hash of document_id+page or normalized URL. Used by the dedup check at entry creation time.';

-- entry_key is unique per draft so in-text citations can resolve unambiguously.
-- Partial index: skips legacy rows where entry_key is NULL.
CREATE UNIQUE INDEX IF NOT EXISTS uq_draft_reference_entry_key
    ON draft_references (draft_id, entry_key)
    WHERE entry_key IS NOT NULL;

-- Link table: one row per in-text citation occurrence in the draft body.
-- Enables orphan-citation / dead-entry diagnostics and live sync when the
-- user edits the draft.
CREATE TABLE IF NOT EXISTS citation_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    draft_id UUID NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
    reference_id UUID NOT NULL REFERENCES draft_references(id) ON DELETE CASCADE,
    in_text_marker TEXT NOT NULL,
    paragraph_index INTEGER,
    char_offset_start INTEGER,
    char_offset_end INTEGER,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_citation_entries_draft_id
    ON citation_entries (draft_id);
CREATE INDEX IF NOT EXISTS idx_citation_entries_reference_id
    ON citation_entries (reference_id);

COMMENT ON TABLE citation_entries IS
    'In-text citation occurrences linked to draft_references entries. Populated by the citation-sync parser; drives orphan/dead-entry diagnostics.';

-- Verification:
-- SELECT column_name, data_type FROM information_schema.columns
--   WHERE table_name = 'draft_references';
-- SELECT COUNT(*) FROM citation_entries;
