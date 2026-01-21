"""
Import prompt files from /prompts/{lang}/ into the database.

This script reads prompt text files and inserts them into the prompt_templates table,
enabling dynamic multilingual prompt loading for AI agents.

Usage:
    python maestro_backend/scripts/import_prompts_to_db.py [--language en] [--dry-run]

Arguments:
    --language: Language code to import (default: all languages in /prompts/)
    --dry-run: Preview import without making changes
    --force: Overwrite existing prompts
"""

import argparse
import sys
from pathlib import Path
from typing import List, Dict
import logging

# Add parent directory to path for imports
sys.path.insert(0, str(Path(__file__).parent.parent.parent))

from maestro_backend.database.database import SessionLocal
from maestro_backend.database.models import PromptTemplate, SupportedLanguage

logging.basicConfig(level=logging.INFO, format='%(levelname)s: %(message)s')
logger = logging.getLogger(__name__)

# Base paths
BASE_PATH = Path(__file__).parent.parent.parent
PROMPTS_PATH = BASE_PATH / "prompts"


def parse_filename(filename: str) -> Dict[str, str]:
    """
    Parse prompt filename to extract agent name and prompt key.

    Format: AgentName_prompt_key.txt
    Examples:
        - ResearchAgent_system_prompt.txt → {agent: ResearchAgent, key: system_prompt}
        - PlanningAgent_phase1.txt → {agent: PlanningAgent, key: phase1}

    Args:
        filename: Name of the prompt file

    Returns:
        Dictionary with 'agent_name' and 'prompt_key'
    """
    # Remove .txt extension
    name = filename.replace('.txt', '')

    # Split on first underscore to separate agent name from prompt key
    parts = name.split('_', 1)

    if len(parts) != 2:
        raise ValueError(f"Invalid filename format: {filename}. Expected: AgentName_prompt_key.txt")

    return {
        'agent_name': parts[0],
        'prompt_key': parts[1]
    }


def import_language_prompts(db: SessionLocal, language_code: str, force: bool = False, dry_run: bool = False) -> Dict[str, int]:
    """
    Import all prompts for a specific language.

    Args:
        db: Database session
        language_code: Language code (e.g., 'en', 'de')
        force: Overwrite existing prompts
        dry_run: Preview without making changes

    Returns:
        Statistics dictionary with counts
    """
    lang_path = PROMPTS_PATH / language_code
    stats = {'created': 0, 'updated': 0, 'skipped': 0, 'errors': 0}

    if not lang_path.exists():
        logger.warning(f"No prompts directory for language: {language_code} (path: {lang_path})")
        return stats

    # Verify language exists in database
    if not dry_run:
        supported = db.query(SupportedLanguage).filter(
            SupportedLanguage.code == language_code
        ).first()

        if not supported:
            logger.error(f"Language '{language_code}' not found in supported_languages table. Run migration first.")
            return stats

    # Process all .txt files
    prompt_files = list(lang_path.glob('*.txt'))

    if not prompt_files:
        logger.warning(f"No prompt files found in {lang_path}")
        return stats

    logger.info(f"Found {len(prompt_files)} prompt files for '{language_code}'")

    for prompt_file in prompt_files:
        try:
            # Parse filename
            file_info = parse_filename(prompt_file.name)
            agent_name = file_info['agent_name']
            prompt_key = file_info['prompt_key']

            # Read content
            with open(prompt_file, 'r', encoding='utf-8') as f:
                content = f.read().strip()

            if not content:
                logger.warning(f"  ⚠️  Empty file: {prompt_file.name}")
                stats['skipped'] += 1
                continue

            # Check if prompt already exists
            existing = None
            if not dry_run:
                existing = db.query(PromptTemplate).filter(
                    PromptTemplate.agent_name == agent_name,
                    PromptTemplate.prompt_key == prompt_key,
                    PromptTemplate.language_code == language_code
                ).first()

            if existing:
                if force:
                    if dry_run:
                        logger.info(f"  [DRY RUN] Would update {agent_name}.{prompt_key} ({len(content)} chars)")
                    else:
                        existing.content = content
                        existing.is_active = True
                        logger.info(f"  ✓ Updated {agent_name}.{prompt_key} ({len(content)} chars)")
                    stats['updated'] += 1
                else:
                    logger.info(f"  → Skipped {agent_name}.{prompt_key} (already exists, use --force to overwrite)")
                    stats['skipped'] += 1
            else:
                if dry_run:
                    logger.info(f"  [DRY RUN] Would create {agent_name}.{prompt_key} ({len(content)} chars)")
                else:
                    prompt = PromptTemplate(
                        agent_name=agent_name,
                        prompt_key=prompt_key,
                        language_code=language_code,
                        content=content,
                        is_active=True,
                        version=1
                    )
                    db.add(prompt)
                    logger.info(f"  ✓ Created {agent_name}.{prompt_key} ({len(content)} chars)")
                stats['created'] += 1

        except Exception as e:
            logger.error(f"  ✗ Error processing {prompt_file.name}: {e}")
            stats['errors'] += 1

    # Commit changes
    if not dry_run and stats['created'] + stats['updated'] > 0:
        db.commit()
        logger.info(f"Committed {stats['created'] + stats['updated']} changes to database")

    return stats


def main():
    """Main import function."""
    parser = argparse.ArgumentParser(description='Import prompts from files to database')
    parser.add_argument('--language', '-l', type=str, default=None,
                        help='Language code to import (default: all available)')
    parser.add_argument('--dry-run', '-d', action='store_true',
                        help='Preview import without making changes')
    parser.add_argument('--force', '-f', action='store_true',
                        help='Overwrite existing prompts')

    args = parser.parse_args()

    print("=" * 80)
    print("PROMPT IMPORT SCRIPT")
    print("=" * 80)
    print(f"Prompts path: {PROMPTS_PATH}")
    print(f"Dry run: {args.dry_run}")
    print(f"Force update: {args.force}")
    print()

    # Get database session
    db = SessionLocal()

    try:
        # Determine which languages to import
        if args.language:
            languages = [args.language]
        else:
            # Find all language directories
            languages = [d.name for d in PROMPTS_PATH.iterdir() if d.is_dir()]

        if not languages:
            logger.error(f"No language directories found in {PROMPTS_PATH}")
            return 1

        logger.info(f"Importing languages: {', '.join(languages)}")
        print()

        # Import each language
        total_stats = {'created': 0, 'updated': 0, 'skipped': 0, 'errors': 0}

        for lang_code in languages:
            print(f"\n{lang_code.upper()}")
            print("-" * 80)

            stats = import_language_prompts(db, lang_code, force=args.force, dry_run=args.dry_run)

            for key in total_stats:
                total_stats[key] += stats[key]

        # Print summary
        print("\n" + "=" * 80)
        print("IMPORT SUMMARY")
        print("=" * 80)
        print(f"Created: {total_stats['created']}")
        print(f"Updated: {total_stats['updated']}")
        print(f"Skipped: {total_stats['skipped']}")
        print(f"Errors:  {total_stats['errors']}")

        if args.dry_run:
            print("\n⚠️  DRY RUN MODE - No changes were made to the database")
        else:
            print(f"\n✅ Import completed successfully!")

        print("=" * 80)

        return 0 if total_stats['errors'] == 0 else 1

    except Exception as e:
        logger.error(f"Import failed: {e}", exc_info=True)
        return 1
    finally:
        db.close()


if __name__ == '__main__':
    exit(main())
