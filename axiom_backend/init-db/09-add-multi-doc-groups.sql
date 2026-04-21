-- Multi-doc-group support for writing sessions, mission chats, and research
-- missions.
--
-- The chat/writing panels used to lock users into a single document group.
-- We now allow an array of group IDs so users can e.g. pair the
-- auto-created web-sources group from a research run with their curated
-- VWL library when continuing in writing mode. The existing singular
-- `document_group_id` column is kept for backward compatibility with older
-- clients; new code reads `document_group_ids` when present and falls back
-- to wrapping the singular value in a one-element list.

-- writing_sessions: array of doc group UUIDs (JSONB, nullable)
ALTER TABLE writing_sessions
    ADD COLUMN IF NOT EXISTS document_group_ids JSONB;

-- chats (used by both research missions and writing sessions): same idea
ALTER TABLE chats
    ADD COLUMN IF NOT EXISTS document_group_ids JSONB;

-- missions: research mission DB row; mission_context also mirrors this but
-- having it on the missions row matters for queries/exports
ALTER TABLE missions
    ADD COLUMN IF NOT EXISTS document_group_ids JSONB;
