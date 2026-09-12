"""#268 — figcap backfill engine (operational CLI).

Pure computation surface of the figcap-backfill one-shot tool
(cmd/figcap-backfill): re-runs the CAPTION EXTRACTION over already-stored
chunks of active snapshots and recomputes the dense vector of every chunk
whose figure_captions actually change (added true captions, purged prose
false positives). No re-ingest, no chunk mutation beyond the figure_captions
column, no re-captioning — the marker files and machine captions are
untouched.

Protocol (#233 pattern): stdin = JSON array of
  {"chunk_id", "text", "section_titles": [...], "image_refs": [...],
   "image_captions": {...}, "figure_captions": {...}}
stdout = JSON array of
  {"chunk_id", "figure_captions": {...}, "changed": bool,
   "model", "dimensions", "values"}        # vector present iff changed
Any failure exits non-zero BEFORE a single vector is emitted — the caller
must never apply a partial plan.
"""

from __future__ import annotations

import json
import sys
from typing import Any

from axiom_ng_runner.runner import _caption_augmentation, reextract_figure_captions

# The real engine only (the CLI loads the actual TextEmbedder) — its dense
# vectors are BAAI/bge-m3, whatever the runner's reference-stub constant says.
DENSE_MODEL = "BAAI/bge-m3"


def plan(chunks: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Re-extract figure_captions for every chunk and mark the affected ones
    (old ≠ new). Pure — the caller decides what to write. Each row carries
    the input chunk under "chunk" for the re-embed pass; main() strips it
    before emitting (it is internal plumbing, not protocol)."""
    out: list[dict[str, Any]] = []
    for c in chunks:
        old = c.get("figure_captions") or {}
        new = reextract_figure_captions(
            {
                "text": c.get("text", ""),
                "metadata": {"image_refs": c.get("image_refs") or []},
            }
        )
        if new is None:
            # positions not reliably reconstructible (multi-image count
            # mismatch) — leave the stored captions alone, never purge
            # captions that may still be correct.
            new = old
        out.append(
            {
                "chunk_id": c["chunk_id"],
                "figure_captions": new,
                "changed": new != old,
                "chunk": c,
            }
        )
    return out


def run(
    chunks: list[dict[str, Any]], embedder: Any | None = None
) -> list[dict[str, Any]]:
    """Plan the re-extraction, then re-embed every affected chunk with the
    NEW caption augmentation. `embedder` defaults to the real TextEmbedder
    (injectable for tests)."""
    planned = plan(chunks)
    affected = [p for p in planned if p["changed"]]
    if not affected:
        return [
            {
                "chunk_id": p["chunk_id"],
                "figure_captions": p["figure_captions"],
                "changed": False,
            }
            for p in planned
        ]
    if embedder is None:
        from axiom_ng_runner.compute_core.embedder import TextEmbedder

        embedder = TextEmbedder()
    items = []
    for p in affected:
        c = p["chunk"]
        aug = _caption_augmentation(
            {
                "metadata": {
                    "image_captions": c.get("image_captions") or {},
                    "figure_captions": p["figure_captions"],
                }
            }
        )
        items.append(
            {
                "chunk_id": p["chunk_id"],
                "text": c.get("text", "") + aug,
                "metadata": {"section_titles": c.get("section_titles") or []},
            }
        )
    embedder.embed_chunks(items)
    by_id = {it["chunk_id"]: it for it in items}
    result: list[dict[str, Any]] = []
    for p in planned:
        row: dict[str, Any] = {
            "chunk_id": p["chunk_id"],
            "figure_captions": p["figure_captions"],
            "changed": p["changed"],
        }
        if p["changed"]:
            dense = (by_id[p["chunk_id"]].get("embeddings") or {}).get("dense")
            if not dense:
                raise RuntimeError(
                    f"chunk {p['chunk_id']}: embedder returned no dense vector"
                )
            row.update(
                model=DENSE_MODEL,
                dimensions=len(dense),
                values=[float(v) for v in dense],
            )
        result.append(row)
    return result


def main() -> int:
    plan_only = "--plan-only" in sys.argv
    try:
        chunks = json.load(sys.stdin)
        if not isinstance(chunks, list):
            raise TypeError("input must be a JSON array")
        if plan_only:
            results = [
                {
                    "chunk_id": p["chunk_id"],
                    "figure_captions": p["figure_captions"],
                    "changed": p["changed"],
                }
                for p in plan(chunks)
            ]
        else:
            results = run(chunks)
    except Exception as err:  # noqa: BLE001 — engine failures must be loud
        print(f"figcap-backfill engine FAILED: {err}", file=sys.stderr)
        return 1
    json.dump(results, sys.stdout)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
