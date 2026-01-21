-- Migration: Add Multilingual Prompt Management Support
-- Purpose: Enable dynamic prompt loading in multiple languages for AI agents
-- Reference: GitHub Issue #2

-- ============================================================
-- PART 1: Create prompt templates table
-- ============================================================
CREATE TABLE IF NOT EXISTS prompt_templates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_name VARCHAR(100) NOT NULL,      -- 'PlanningAgent', 'ResearchAgent', etc.
    prompt_key VARCHAR(100) NOT NULL,      -- 'system_prompt', 'phase1', 'phase2', etc.
    language_code VARCHAR(10) NOT NULL,    -- 'en', 'de', 'fr', 'es', 'pt'
    content TEXT NOT NULL,
    version INTEGER DEFAULT 1,
    is_active BOOLEAN DEFAULT true,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    created_by_user_id INTEGER REFERENCES users(id),
    UNIQUE(agent_name, prompt_key, language_code, version)
);

-- Indexes for performance (fast lookups by agent + language)
CREATE INDEX IF NOT EXISTS idx_prompt_templates_agent_lang
    ON prompt_templates(agent_name, language_code, is_active);
CREATE INDEX IF NOT EXISTS idx_prompt_templates_active
    ON prompt_templates(is_active, language_code);

COMMENT ON TABLE prompt_templates IS 'Stores versioned prompt templates for AI agents in multiple languages';
COMMENT ON COLUMN prompt_templates.agent_name IS 'Agent class name (e.g., PlanningAgent, ResearchAgent)';
COMMENT ON COLUMN prompt_templates.prompt_key IS 'Prompt identifier within agent (e.g., system_prompt, phase1)';
COMMENT ON COLUMN prompt_templates.language_code IS 'ISO language code (e.g., en, de, fr)';
COMMENT ON COLUMN prompt_templates.version IS 'Prompt version number for tracking changes';
COMMENT ON COLUMN prompt_templates.is_active IS 'Whether this prompt version is currently active';

-- ============================================================
-- PART 2: Create supported languages table
-- ============================================================
CREATE TABLE IF NOT EXISTS supported_languages (
    code VARCHAR(10) PRIMARY KEY,
    name VARCHAR(100) NOT NULL,              -- English name
    native_name VARCHAR(100) NOT NULL,       -- Native name (e.g., 'Deutsch' for German)
    is_active BOOLEAN DEFAULT true,
    completion_percentage INTEGER DEFAULT 0,  -- Translation completion (0-100)
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

COMMENT ON TABLE supported_languages IS 'Languages supported by the application for prompts and UI';
COMMENT ON COLUMN supported_languages.completion_percentage IS 'Percentage of prompts translated (0-100)';

-- Seed supported languages (English and German validated in Mission ef1cab00)
INSERT INTO supported_languages (code, name, native_name, is_active, completion_percentage) VALUES
    ('en', 'English', 'English', true, 100),
    ('de', 'German', 'Deutsch', true, 100),  -- Already validated with excellent results
    ('fr', 'French', 'Français', true, 0),
    ('es', 'Spanish', 'Español', true, 0),
    ('pt', 'Portuguese', 'Português', true, 0)
ON CONFLICT (code) DO NOTHING;

-- ============================================================
-- PART 3: Add language_code to users (user preference)
-- ============================================================
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS language_code VARCHAR(10) DEFAULT 'en'
    REFERENCES supported_languages(code);

CREATE INDEX IF NOT EXISTS idx_users_language ON users(language_code);

COMMENT ON COLUMN users.language_code IS 'User default language preference for prompts and UI';

-- ============================================================
-- PART 4: Add language_code to missions (per-mission language)
-- ============================================================
ALTER TABLE missions
    ADD COLUMN IF NOT EXISTS language_code VARCHAR(10) DEFAULT 'en'
    REFERENCES supported_languages(code);

CREATE INDEX IF NOT EXISTS idx_missions_language ON missions(language_code);

COMMENT ON COLUMN missions.language_code IS 'Language used for this specific mission (can override user default)';

-- ============================================================
-- Verification Queries
-- ============================================================
-- Run these to verify the migration succeeded:
--
-- SELECT table_name FROM information_schema.tables
--     WHERE table_name IN ('prompt_templates', 'supported_languages');
--
-- SELECT * FROM supported_languages;
--
-- SELECT column_name, data_type FROM information_schema.columns
--     WHERE table_name = 'users' AND column_name = 'language_code';
--
-- SELECT column_name, data_type FROM information_schema.columns
--     WHERE table_name = 'missions' AND column_name = 'language_code';
