"""
Central configuration for all file paths used in the Axiom application.
Configurable via environment variables for native macOS / non-Docker execution.
Defaults preserve Docker container layout (/app/...).
"""

import os
from pathlib import Path

# Base paths - configurable via environment variable
DATA_BASE_PATH = Path(os.getenv("AXIOM_DATA_PATH", "/app/data"))
AI_DATA_BASE_PATH = Path(os.getenv("AXIOM_AI_DATA_PATH", "/app/ai_researcher/data"))
APP_ROOT = Path(os.getenv("AXIOM_APP_PATH", "/app"))

# Vector store - single source of truth
VECTOR_STORE_PATH = DATA_BASE_PATH / "vector_store"

# Document storage paths
RAW_FILES_PATH = DATA_BASE_PATH / "raw_pdfs"
MARKDOWN_PATH = DATA_BASE_PATH / "processed" / "markdown"
METADATA_PATH = DATA_BASE_PATH / "processed" / "metadata"
METADATA_DB_PATH = DATA_BASE_PATH / "processed" / "metadata.db"
IMAGES_PATH = DATA_BASE_PATH / "processed" / "images"

# Legacy paths for backward compatibility
LEGACY_RAW_FILES_PATH = DATA_BASE_PATH / "raw_files"
LEGACY_MARKDOWN_PATH = DATA_BASE_PATH / "markdown_files"

# Reference doc for pandoc DOCX conversion
REFERENCE_DOC_PATH = APP_ROOT / "reference.docx"

# Ensure critical directories exist
def ensure_directories():
    """Create necessary directories if they don't exist."""
    for path in [VECTOR_STORE_PATH, RAW_FILES_PATH, MARKDOWN_PATH, METADATA_PATH, IMAGES_PATH]:
        path.mkdir(parents=True, exist_ok=True)
