"""#257 — caption dense backfill engine (operational CLI).

Pure computation surface of the caption-backfill one-shot tool (cmd/
caption-backfill): re-embeds EXISTING captioned chunks of active snapshots
so the dense arm sees their caption text. No re-captioning (the hash gate
and stored captions are untouched), no new snapshot, no chunk mutation —
only the dense vector of a captioned chunk is recomputed from
text + labeled captions, exactly like the ingest-time re-embed (#230/#257).

Protocol (#233 pattern): stdin = JSON array of
  {"chunk_id", "text", "section_titles": [...],
   "image_captions": {...}, "figure_captions": {...}}
stdout = JSON array of {"chunk_id", "model", "dimensions", "values": [...]}
Any failure exits non-zero BEFORE a single vector is emitted — the caller
must never apply a partial plan.
"""

from __future__ import annotations

import json
import sys
from typing import Any

from axiom_ng_runner.runner import _caption_augmentation

# The real engine only (the CLI loads the actual TextEmbedder) — its dense
# vectors are BAAI/bge-m3, whatever the runner's reference-stub constant says.
DENSE_MODEL = "BAAI/bge-m3"


def build_inputs(chunks: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Embedder input for every chunk that carries captions; empty chunks
    (no captions) are skipped — they need no backfill."""
    out: list[dict[str, Any]] = []
    for c in chunks:
        aug = _caption_augmentation(
            {
                "metadata": {
                    "image_captions": c.get("image_captions") or {},
                    "figure_captions": c.get("figure_captions") or {},
                }
            }
        )
        if not aug:
            continue
        out.append(
            {
                "chunk_id": c["chunk_id"],
                "text": c["text"] + aug,
                "metadata": {"section_titles": c.get("section_titles") or []},
            }
        )
    return out


def run(
    chunks: list[dict[str, Any]], embedder: Any | None = None
) -> list[dict[str, Any]]:
    """Re-embed the captioned chunks. `embedder` defaults to the real
    TextEmbedder (injectable for tests)."""
    items = build_inputs(chunks)
    if not items:
        return []
    if embedder is None:
        from axiom_ng_runner.compute_core.embedder import TextEmbedder

        embedder = TextEmbedder()
    embedder.embed_chunks(items)
    out = []
    for it in items:
        dense = (it.get("embeddings") or {}).get("dense")
        if not dense:
            raise RuntimeError(
                f"chunk {it['chunk_id']}: embedder returned no dense vector"
            )
        out.append(
            {
                "chunk_id": it["chunk_id"],
                "model": DENSE_MODEL,
                "dimensions": len(dense),
                "values": [float(v) for v in dense],
            }
        )
    return out


def main() -> int:
    try:
        chunks = json.load(sys.stdin)
        if not isinstance(chunks, list):
            raise TypeError("input must be a JSON array")
        results = run(chunks)
    except Exception as err:  # noqa: BLE001 — engine failures must be loud
        print(f"caption-backfill engine FAILED: {err}", file=sys.stderr)
        return 1
    json.dump(results, sys.stdout)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
