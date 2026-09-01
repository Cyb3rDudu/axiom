"""#233 — locator backfill CLI (operational).

Runs the alignment engine over an active snapshot's chunks and an enriched
EPUB sibling, producing the enriched/refused chunk plan WITHOUT writing
anywhere itself. The DB transaction + OpenSearch re-index live on the Go side
(cmd/locator-backfill); this CLI is the pure computation + dry-run surface the
operator (or the Go cmd) invokes.

Inputs (all via args):
  --epub   <path>   the enriched EPUB sibling (derived_from_sibling page map)
  --source-kind pdf|epub   kind of the ACTIVE snapshot
  --pdf    <path>   the active PDF snapshot (required when source-kind=pdf)
  --chunks <file>   JSON array of active-snapshot chunks:
                    [{"id","text","locator":{...}}]  (locator carries
                    physical_page_start/end + page_source)
  --dry-run         print the plan without implying any write (default true;
                    the Go side does the actual write)
  --out    <file>   write the JSON result to <file> (stdout if omitted)

Output JSON:
  {"aligned": bool, "refused_reason": str, "anchor_count": int,
   "enrichment_targets": int, "pages_enriched": int, "pages_refused": int,
   "results": [{"chunk_id","enrich","page_start","page_end","source",
                 "confidence","refused","reason"}]}

Never writes to any durable store: this module enforces the #226/#233
"refuse, never guess" discipline and reports, it does not mutate.
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

# pi-lens-ignore: reportMissingImports
from axiom_ng_runner.compute_core import locator_backfill as lb


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--epub", required=True, help="enriched EPUB sibling (derived_from_sibling)")
    ap.add_argument("--source-kind", choices=("pdf", "epub"), required=True,
                    help="kind of the ACTIVE snapshot being enriched")
    ap.add_argument("--pdf", help="active PDF snapshot path (required for source-kind=pdf)")
    ap.add_argument("--chunks", required=True, help="JSON file of active-snapshot chunks")
    ap.add_argument("--out", help="write JSON plan to this file (default: stdout)")
    ap.add_argument("--dry-run", action="store_true", default=True,
                    help="report the plan without writing (this CLI always reports; "
                         "writes happen on the Go side)")
    args = ap.parse_args()

    chunks_arg = Path(args.chunks)
    try:
        chunks = json.loads(chunks_arg.read_text("utf-8"))
    except (OSError, ValueError) as exc:
        print(f"locator-backfill: cannot read chunks file {chunks_arg}: {exc}",
              file=sys.stderr)
        return 2
    if not isinstance(chunks, list):
        print("locator-backfill: chunks file must be a JSON array", file=sys.stderr)
        return 2

    result = lb.backfill_chunks(
        args.epub, args.source_kind, args.pdf, chunks if isinstance(chunks, list) else []
    )
    payload = {
        "aligned": result.aligned,
        "refused_reason": result.refused_reason,
        "anchor_count": result.anchor_count,
        "enrichment_targets": result.enrichment_targets,
        "pages_enriched": result.pages_enriched,
        "pages_refused": result.pages_refused,
        "results": [c.to_dict() for c in result.chunk_results],
    }
    text = json.dumps(payload, indent=1)
    if args.out:
        Path(args.out).write_text(text, "utf-8")
        print(f"locator-backfill: plan written to {args.out} "
              f"({payload['pages_enriched']} enriched, {payload['pages_refused']} refused)")
    else:
        print(text)
    return 0 if result.aligned else 1


if __name__ == "__main__":
    sys.exit(main())
