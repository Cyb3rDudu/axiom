-- Migration: Writing-mode Literaturportfolio persistence
-- Purpose: Persist PortfolioOutput JSON per draft so the writing-mode
--          portfolio (Epic #61) survives restarts, powers the DOCX export
--          path, and is versioned naturally with each new draft revision.
-- Reference: Epic #61, sub-issue #64; design: docs/plans/WRITING_MODE_LITERATURE_PORTFOLIO.md

ALTER TABLE drafts
    ADD COLUMN IF NOT EXISTS portfolio_output JSONB;

COMMENT ON COLUMN drafts.portfolio_output IS
    'PortfolioOutput JSON (entries[], compliance report, markdown_table). NULL until the user triggers generation or the session-close hook fires. Per-draft, so new draft versions start null without clobbering the previous one.';

CREATE INDEX IF NOT EXISTS idx_drafts_portfolio_traffic_light
    ON drafts ((portfolio_output->'compliance'->>'traffic_light'))
    WHERE portfolio_output IS NOT NULL;

-- Verification:
-- SELECT column_name, data_type FROM information_schema.columns
--   WHERE table_name = 'drafts' AND column_name = 'portfolio_output';
