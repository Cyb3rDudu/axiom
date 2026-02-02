"""
Extract hardcoded prompts from agent files and save to /prompts/en/ directory.

This script extracts prompt strings from agent _default_system_prompt() and phase prompt
methods, saving them as individual text files for translation and database import.

Run once to create initial English prompt files before migration to database.
"""

import os
import ast
import re
from pathlib import Path
from typing import Dict, List, Optional

# Base path for the project
BASE_PATH = Path(__file__).parent.parent.parent
AGENTS_PATH = BASE_PATH / "axiom_backend" / "ai_researcher" / "agentic_layer" / "agents"
OUTPUT_PATH = BASE_PATH / "prompts" / "en"

# Agent files and their prompt methods to extract
AGENT_PROMPTS = {
    'ResearchAgent': {
        'file': 'research_agent.py',
        'methods': ['_default_system_prompt'],
        'keys': ['system_prompt']
    },
    'WritingAgent': {
        'file': 'writing_agent.py',
        'methods': ['_default_system_prompt'],
        'keys': ['system_prompt']
    },
    'MessengerAgent': {
        'file': 'messenger_agent.py',
        'methods': ['_default_system_prompt'],
        'keys': ['system_prompt']
    },
    'ReflectionAgent': {
        'file': 'reflection_agent.py',
        'methods': ['_default_system_prompt'],
        'keys': ['system_prompt']
    },
    'WritingReflectionAgent': {
        'file': 'writing_reflection_agent.py',
        'methods': ['_default_system_prompt'],
        'keys': ['system_prompt']
    },
    'NoteAssignmentAgent': {
        'file': 'note_assignment_agent.py',
        'methods': ['_default_system_prompt'],
        'keys': ['system_prompt']
    },
    'PlanningAgent': {
        'file': 'planning_agent.py',
        'methods': [
            '_phase1_system_prompt',
            '_phase2_system_prompt',
            '_phase3_system_prompt',
            '_phase3a_structural_prompt',
            '_phase3b_subsection_prompt',
            '_phase3c_note_redistribution_prompt'
        ],
        'keys': [
            'phase1',
            'phase2',
            'phase3',
            'phase3a_structural',
            'phase3b_subsection',
            'phase3c_redistribution'
        ]
    }
}


def extract_prompt_from_method(file_path: Path, method_name: str) -> Optional[str]:
    """
    Extract the prompt string returned by a method using AST parsing.

    Args:
        file_path: Path to the Python file
        method_name: Name of the method to extract from

    Returns:
        Extracted prompt string or None if not found
    """
    try:
        with open(file_path, 'r', encoding='utf-8') as f:
            content = f.read()

        # Parse the file into an AST
        tree = ast.parse(content)

        # Find the method in the AST
        for node in ast.walk(tree):
            if isinstance(node, ast.FunctionDef) and node.name == method_name:
                # Look for return statements
                for stmt in node.body:
                    if isinstance(stmt, ast.Return) and stmt.value:
                        # Handle f-strings and regular strings
                        if isinstance(stmt.value, ast.JoinedStr):
                            # This is an f-string, need to reconstruct it
                            return extract_fstring_content(stmt.value, content)
                        elif isinstance(stmt.value, ast.Constant):
                            return stmt.value.value
                        elif isinstance(stmt.value, ast.Str):  # Python 3.7 compatibility
                            return stmt.value.s

        # Fallback to regex if AST doesn't work
        return extract_prompt_regex(content, method_name)

    except Exception as e:
        print(f"Error extracting {method_name} from {file_path}: {e}")
        return extract_prompt_regex(file_path.read_text(), method_name)


def extract_fstring_content(fstring_node: ast.JoinedStr, source_code: str) -> str:
    """
    Extract content from an f-string AST node, preserving placeholders.

    For prompts with dynamic variables like {max_depth}, we preserve them as-is.
    """
    # Get the source code segment for this f-string
    # This is complex, so let's use regex as fallback
    return None


def extract_prompt_regex(content: str, method_name: str) -> Optional[str]:
    """
    Fallback: Extract prompt using regex pattern matching.

    Looks for the method definition and extracts the returned string literal.
    """
    # Pattern to match the method and its return statement
    # Handle both triple-quoted strings and f-strings
    patterns = [
        # f-string pattern
        rf'def {method_name}\(self\).*?return\s+f"""(.*?)"""\s*$',
        # Regular string pattern
        rf'def {method_name}\(self\).*?return\s+"""(.*?)"""\s*$',
        # Single line return
        rf'def {method_name}\(self\).*?return\s+[rf]?"([^"]+)"',
    ]

    for pattern in patterns:
        match = re.search(pattern, content, re.DOTALL | re.MULTILINE)
        if match:
            prompt = match.group(1).strip()
            # Unescape if needed
            return prompt

    return None


def extract_planning_agent_prompts(file_path: Path) -> Dict[str, str]:
    """
    Special handling for PlanningAgent which has fallback patterns.

    Extract the hardcoded fallback prompts (the ones after "# Fallback to hardcoded prompt")
    """
    prompts = {}

    with open(file_path, 'r', encoding='utf-8') as f:
        content = f.read()

    # For each phase method, find the fallback prompt
    phase_methods = [
        ('_phase1_system_prompt', 'phase1'),
        ('_phase2_system_prompt', 'phase2'),
        ('_phase3_system_prompt', 'phase3'),
        ('_phase3a_structural_prompt', 'phase3a_structural'),
        ('_phase3b_subsection_prompt', 'phase3b_subsection'),
        ('_phase3c_note_redistribution_prompt', 'phase3c_redistribution'),
    ]

    for method_name, key in phase_methods:
        # Find the method
        method_pattern = rf'def {method_name}\(self\).*?(?=\n    def |\Z)'
        method_match = re.search(method_pattern, content, re.DOTALL)

        if method_match:
            method_content = method_match.group(0)

            # Look for the fallback prompt (after "# Fallback to hardcoded prompt")
            fallback_pattern = r'# Fallback to hardcoded prompt\s+prompt = f?"""(.*?)"""\s+return prompt'
            fallback_match = re.search(fallback_pattern, method_content, re.DOTALL)

            if fallback_match:
                prompt = fallback_match.group(1).strip()
                prompts[key] = prompt
                print(f"  ✓ Extracted {method_name} (fallback)")
            else:
                print(f"  ✗ Could not find fallback in {method_name}")

    return prompts


def main():
    """Extract all prompts and save to files."""
    print("=" * 70)
    print("PROMPT EXTRACTION SCRIPT")
    print("=" * 70)
    print(f"Agents path: {AGENTS_PATH}")
    print(f"Output path: {OUTPUT_PATH}")
    print()

    # Create output directory
    OUTPUT_PATH.mkdir(parents=True, exist_ok=True)

    extracted_count = 0
    failed_count = 0

    for agent_name, config in AGENT_PROMPTS.items():
        print(f"\n{agent_name} ({config['file']}):")
        print("-" * 70)

        file_path = AGENTS_PATH / config['file']

        if not file_path.exists():
            print(f"  ✗ File not found: {file_path}")
            failed_count += len(config['methods'])
            continue

        # Special handling for PlanningAgent
        if agent_name == 'PlanningAgent':
            prompts = extract_planning_agent_prompts(file_path)

            for key, content in prompts.items():
                if content:
                    output_file = OUTPUT_PATH / f"{agent_name}_{key}.txt"
                    output_file.write_text(content, encoding='utf-8')
                    print(f"  ✓ Saved {agent_name}_{key}.txt ({len(content)} chars)")
                    extracted_count += 1
                else:
                    print(f"  ✗ Failed to extract {key}")
                    failed_count += 1
        else:
            # Extract from simple agents
            for method_name, prompt_key in zip(config['methods'], config['keys']):
                prompt = extract_prompt_from_method(file_path, method_name)

                if prompt:
                    output_file = OUTPUT_PATH / f"{agent_name}_{prompt_key}.txt"
                    output_file.write_text(prompt, encoding='utf-8')
                    print(f"  ✓ Saved {agent_name}_{prompt_key}.txt ({len(prompt)} chars)")
                    extracted_count += 1
                else:
                    print(f"  ✗ Failed to extract {method_name}")
                    failed_count += 1

    print("\n" + "=" * 70)
    print(f"SUMMARY: {extracted_count} prompts extracted, {failed_count} failed")
    print(f"Output directory: {OUTPUT_PATH}")
    print("=" * 70)

    if failed_count > 0:
        print(f"\n⚠️  Some prompts failed to extract. Check the agents manually.")
        return 1

    return 0


if __name__ == '__main__':
    exit(main())
