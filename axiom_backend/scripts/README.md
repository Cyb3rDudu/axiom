# Prompt Management Scripts

This directory contains scripts for managing multilingual prompts in the AI research system.

## Overview

The multilingual prompt system uses a two-step process:
1. **Extract** hardcoded prompts from agent Python files → text files
2. **Import** text files → database for dynamic loading

## Scripts

### 1. Extract Prompts (`extract_prompts_to_files.py`)

Extracts hardcoded prompt strings from agent files and saves them as individual text files.

**Usage:**
```bash
cd /path/to/axiom-private
python axiom_backend/scripts/extract_prompts_to_files.py
```

**What it does:**
- Scans agent files in `axiom_backend/ai_researcher/agentic_layer/agents/`
- Extracts prompts from `_default_system_prompt()` methods
- Special handling for PlanningAgent's 6 phase prompts
- Saves to `/prompts/en/` directory

**Output files:**
```
prompts/en/
├── ResearchAgent_system_prompt.txt
├── WritingAgent_system_prompt.txt
├── MessengerAgent_system_prompt.txt
├── ReflectionAgent_system_prompt.txt
├── WritingReflectionAgent_system_prompt.txt
├── NoteAssignmentAgent_system_prompt.txt
├── PlanningAgent_phase1.txt
├── PlanningAgent_phase2.txt
├── PlanningAgent_phase3.txt
├── PlanningAgent_phase3a_structural.txt
├── PlanningAgent_phase3b_subsection.txt
└── PlanningAgent_phase3c_redistribution.txt
```

### 2. Import Prompts (`import_prompts_to_db.py`)

Imports prompt text files into the database for dynamic loading.

**Usage:**
```bash
cd /path/to/axiom-private

# Import all languages (dry run)
python axiom_backend/scripts/import_prompts_to_db.py --dry-run

# Import all languages (actual import)
python axiom_backend/scripts/import_prompts_to_db.py

# Import specific language
python axiom_backend/scripts/import_prompts_to_db.py --language en

# Force overwrite existing prompts
python axiom_backend/scripts/import_prompts_to_db.py --force
```

**Options:**
- `--language <code>` / `-l <code>`: Import only specified language (default: all)
- `--dry-run` / `-d`: Preview import without making changes
- `--force` / `-f`: Overwrite existing prompts in database

**What it does:**
- Reads all `.txt` files from `/prompts/{language}/`
- Parses filename to extract agent name and prompt key
- Inserts/updates prompts in `prompt_templates` table
- Validates language exists in `supported_languages` table

## Workflow

### Initial Setup (First Time)

1. **Run database migration** (creates tables):
   ```bash
   psql -U axiom_user -d axiom_db -f axiom_backend/database/migrations/add_multilingual_support.sql
   ```

2. **Extract English prompts from code**:
   ```bash
   python axiom_backend/scripts/extract_prompts_to_files.py
   ```

3. **Import English prompts to database**:
   ```bash
   python axiom_backend/scripts/import_prompts_to_db.py --language en
   ```

### Adding a New Language

1. **Create language directory**:
   ```bash
   mkdir -p prompts/de  # For German
   ```

2. **Translate prompts**:
   - Copy English prompts: `cp prompts/en/*.txt prompts/de/`
   - Translate each `.txt` file to target language
   - Keep filenames unchanged (e.g., `ResearchAgent_system_prompt.txt`)

3. **Import translated prompts**:
   ```bash
   python axiom_backend/scripts/import_prompts_to_db.py --language de
   ```

4. **Verify import**:
   ```bash
   psql -U axiom_user -d axiom_db -c "
       SELECT agent_name, prompt_key, language_code, LENGTH(content)
       FROM prompt_templates
       WHERE language_code = 'de'
       ORDER BY agent_name, prompt_key;"
   ```

### Updating Prompts

To update prompts after code changes:

1. **Re-extract from code**:
   ```bash
   python axiom_backend/scripts/extract_prompts_to_files.py
   ```

2. **Review changes**:
   ```bash
   git diff prompts/en/
   ```

3. **Re-import to database**:
   ```bash
   python axiom_backend/scripts/import_prompts_to_db.py --language en --force
   ```

4. **Clear PromptLoader cache** (optional, happens automatically on app restart):
   - Restart the application
   - Or call `PromptLoader.clear_cache()` programmatically

## File Naming Convention

Prompt files must follow this naming pattern:
```
{AgentName}_{prompt_key}.txt
```

Examples:
- `ResearchAgent_system_prompt.txt` → agent: ResearchAgent, key: system_prompt
- `PlanningAgent_phase1.txt` → agent: PlanningAgent, key: phase1
- `WritingAgent_system_prompt.txt` → agent: WritingAgent, key: system_prompt

**Important:**
- Agent name must match the Python class name exactly
- Prompt key must match the database prompt_key field
- Use `.txt` extension
- One prompt per file

## Database Schema

```sql
-- Prompts table
CREATE TABLE prompt_templates (
    id UUID PRIMARY KEY,
    agent_name VARCHAR(100),          -- e.g., 'PlanningAgent'
    prompt_key VARCHAR(100),          -- e.g., 'system_prompt', 'phase1'
    language_code VARCHAR(10),        -- e.g., 'en', 'de', 'fr'
    content TEXT,                     -- The actual prompt
    version INTEGER,                  -- Version tracking
    is_active BOOLEAN,                -- Active/inactive flag
    created_at TIMESTAMP,
    updated_at TIMESTAMP,
    UNIQUE(agent_name, prompt_key, language_code, version)
);

-- Languages table
CREATE TABLE supported_languages (
    code VARCHAR(10) PRIMARY KEY,     -- e.g., 'en', 'de'
    name VARCHAR(100),                -- e.g., 'English', 'German'
    native_name VARCHAR(100),         -- e.g., 'English', 'Deutsch'
    is_active BOOLEAN,                -- Enable/disable language
    completion_percentage INTEGER,     -- Translation progress (0-100)
    created_at TIMESTAMP
);
```

## Verification

After import, verify prompts are loaded correctly:

```sql
-- Check all prompts for a language
SELECT agent_name, prompt_key, LENGTH(content) as chars, is_active
FROM prompt_templates
WHERE language_code = 'en'
ORDER BY agent_name, prompt_key;

-- Check completion percentage
SELECT l.code, l.name, l.completion_percentage,
       COUNT(pt.id) as prompt_count
FROM supported_languages l
LEFT JOIN prompt_templates pt ON l.code = pt.language_code
GROUP BY l.code, l.name, l.completion_percentage
ORDER BY l.code;
```

## Troubleshooting

### "Language not found in supported_languages table"
**Solution:** Run the database migration first to create and seed the supported_languages table.

### "No prompt files found"
**Solution:** Run the extract script first to create the prompt files from agent code.

### "Failed to extract prompt"
**Solution:** Check that the agent file exists and has the expected method signature:
```python
def _default_system_prompt(self) -> str:
    """Docstring"""
    prompt = """Your prompt here"""
    return prompt
```

### Prompts not loading in application
**Solution:**
1. Check PromptLoader is initialized: Look for "PromptLoader initialized" in logs
2. Verify prompts exist: Query database for prompts
3. Check cache: PromptLoader caches aggressively, restart app to clear
4. Enable debug logging: Set `LOG_LEVEL=DEBUG` to see prompt loading details

## Performance

- **Extraction**: ~1-2 seconds for all agents
- **Import**: ~100ms per language (12 prompts)
- **Database queries**: <10ms per prompt (first load)
- **Cached lookups**: <1ms (99%+ hit rate after warm-up)

## Related Files

- Database migration: `/axiom_backend/database/migrations/add_multilingual_support.sql`
- PromptLoader service: `/axiom_backend/ai_researcher/agentic_layer/services/prompt_loader.py`
- Database models: `/axiom_backend/database/models.py`
- Agent files: `/axiom_backend/ai_researcher/agentic_layer/agents/`

## Reference

For more details, see:
- Implementation progress: `/IMPLEMENTATION_PROGRESS.md`
- GitHub Issue: #2 - Multilingual Prompt Management System
- Proof of concept: Mission ef1cab00 (German prompts validated)
