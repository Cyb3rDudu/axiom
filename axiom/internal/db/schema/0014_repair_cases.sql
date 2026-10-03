-- #184 Fix-Service: repair queue (state machine) + Zotero write audit.
-- The RAG is the ONLY gateway to Zotero; every mutation it applies on the
-- fix-service's behalf is audited here.

CREATE TYPE repair_status AS ENUM (
    'rejected',      -- preflight rejected the document (entry state)
    'queued',        -- analysis attached, waiting for the fix-service
    'in_repair',     -- fix-service claimed the case
    'healed',        -- repair applied, awaiting/confirmed by preflight GREEN
    'failed',        -- verification failed or repair did not heal
    'blocked_for_dudu' -- ambiguous verdict (e.g. 8-jump class): human decision
);

CREATE TABLE repair_cases (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    attachment_id uuid REFERENCES zotero_attachments(id),
    document_id uuid REFERENCES zotero_documents(id),
    status repair_status NOT NULL DEFAULT 'rejected',
    attempts int NOT NULL DEFAULT 0,          -- loop guard: max 2 per attachment
    suspicion_class text NOT NULL DEFAULT '', -- 🔴 reparierbar / 🔴 unpaginiert / …
    analysis jsonb NOT NULL DEFAULT '{}',     -- analyze report (labels, folio runs, versatz)
    plan jsonb NOT NULL DEFAULT '{}',         -- judge's label plan (versioned JSON)
    plan_version int NOT NULL DEFAULT 0,
    verify_score numeric NOT NULL DEFAULT 0,  -- footer-verification coverage 0..1
    verify_contradictions int NOT NULL DEFAULT 0,
    verdict text NOT NULL DEFAULT '',         -- auto_apply | blocked | failed(+reason)
    blocked_reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One OPEN case (rejected/queued/in_repair) per attachment: the unique
-- re-entry point of the state machine; closed cases never block a new one
-- after a fresh rejection, but the attempts counter lives on the attachment.
CREATE UNIQUE INDEX repair_cases_one_open_per_attachment
    ON repair_cases (attachment_id)
    WHERE status IN ('rejected', 'queued', 'in_repair');

CREATE INDEX repair_cases_status_idx ON repair_cases (status, created_at);

-- Loop guard: attempts are counted per ATTACHMENT across all cases — the
-- third repair attempt for the same attachment key is impossible by check.
ALTER TABLE zotero_attachments
    ADD COLUMN repair_attempts int NOT NULL DEFAULT 0;

-- Audit trail for every Zotero mutation the RAG performs (Was/Wann/Warum).
CREATE TABLE zotero_write_audit (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    case_id uuid REFERENCES repair_cases(id),
    attachment_id uuid REFERENCES zotero_attachments(id),
    action text NOT NULL,                     -- quarantine | delete_attachment | create_attachment
    detail jsonb NOT NULL DEFAULT '{}',       -- what/warum: filename schema, plan version, quarantine path
    created_at timestamptz NOT NULL DEFAULT now()
);
