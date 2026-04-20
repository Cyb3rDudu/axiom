-- Migration: Add Literature Portfolio storage on missions
-- Purpose: Persist the generated Literaturportfolio output as first-class JSONB
--          alongside the mission row. Enables analytics on source quality and
--          compliance traffic lights across all KMU-style missions without
--          having to parse the mission_context blob.
-- Reference: docs/plans/LITERATURE_PORTFOLIO_AGENT.md

ALTER TABLE missions
    ADD COLUMN IF NOT EXISTS literature_portfolio_output JSONB;

COMMENT ON COLUMN missions.literature_portfolio_output IS
    'PortfolioOutput JSON: entries[], compliance report, markdown_table. NULL if disabled or not yet generated.';

CREATE INDEX IF NOT EXISTS idx_missions_portfolio_traffic_light
    ON missions ((literature_portfolio_output->'compliance'->>'traffic_light'))
    WHERE literature_portfolio_output IS NOT NULL;

-- Verification:
-- SELECT column_name, data_type FROM information_schema.columns
--   WHERE table_name = 'missions' AND column_name = 'literature_portfolio_output';
