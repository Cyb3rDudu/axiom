"""mREBEL relation extraction in a short-lived subprocess.

Usage:
    python -m ai_researcher.relation_worker <chunks_json> <triples_json_out>

- ``chunks_json`` — JSON file (a list of chunk dicts with ``text`` and
  ``metadata.chunk_id`` keys, as the doc-processor already produces).
- ``triples_json_out`` — path where the resulting list of triple dicts
  will be written as JSON.

A final single-line JSON summary is printed to stdout on success:
    {"ok": true, "triples_path": "...", "count": N}

On failure, a JSON error is written to stderr and the process exits non-zero.
"""

from __future__ import annotations

import json
import logging
import os
import sys
import traceback
from pathlib import Path
from typing import Any, Dict


def _stderr_err(payload: Dict[str, Any]) -> None:
    print(json.dumps(payload), file=sys.stderr, flush=True)


def main() -> int:
    logging.basicConfig(
        level=os.getenv("LOG_LEVEL", "INFO"),
        format="%(asctime)s [relation-worker] %(levelname)s %(name)s: %(message)s",
    )
    logger = logging.getLogger(__name__)

    if len(sys.argv) < 3:
        _stderr_err(
            {
                "ok": False,
                "error": (
                    "usage: python -m ai_researcher.relation_worker "
                    "<chunks_json> <triples_json_out>"
                ),
            }
        )
        return 2

    chunks_path = Path(sys.argv[1])
    triples_path = Path(sys.argv[2])

    if not chunks_path.exists():
        _stderr_err({"ok": False, "error": f"chunks file not found: {chunks_path}"})
        return 2

    try:
        with open(chunks_path, "r", encoding="utf-8") as f:
            chunks = json.load(f)
        if not isinstance(chunks, list):
            _stderr_err({"ok": False, "error": "chunks JSON must be a list of dicts"})
            return 2

        logger.info(f"Loading mREBEL and extracting relations from {len(chunks)} chunks...")
        from ai_researcher.core_rag.relation_extractor import extract_relations_from_chunks

        triples = extract_relations_from_chunks(chunks)
        logger.info(f"Extracted {len(triples)} triples")

        triples_path.parent.mkdir(parents=True, exist_ok=True)
        with open(triples_path, "w", encoding="utf-8") as f:
            json.dump(triples, f, ensure_ascii=False)

        print(
            json.dumps(
                {
                    "ok": True,
                    "triples_path": str(triples_path),
                    "count": len(triples),
                }
            ),
            flush=True,
        )
        return 0

    except Exception as exc:
        _stderr_err(
            {
                "ok": False,
                "error": str(exc),
                "traceback": traceback.format_exc(),
            }
        )
        return 1


if __name__ == "__main__":
    sys.exit(main())
