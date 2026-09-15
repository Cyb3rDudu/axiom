#!/usr/bin/env python3
"""#274 corpus counter-probe: image-parity claim over a whole EPUB corpus.

Runs the REAL epub_worker (subprocess, same invocation the runner uses) on
every .epub under a directory tree (recursively — e.g. a Zotero storage
root), then checks per book through the runner seam (chunker →
_collect_image_artifacts, exactly like tests/test_epub_image_parity.py::
_linkage):

  (a) no raw `<figure` / `<img` remains in the worker's markdown, and
  (b) every markdown image ref on every chunk resolves to an artifact.

Fixture-only validation missed two corpus regressions during the #274
reviews; this probe is the regression bar the reviews actually used.
Recorded baseline (175 Zotero EPUBs, pandoc 3.7, 2026-09-15):
5,570 markers / 0 raw remains / 0 unresolved refs / 5,367 artifacts.
Re-run and compare against those numbers before any pandoc bump.

Usage: python axiom_ng_runner/scripts/epub_image_census.py <epub-root>
Exit non-zero on any raw remain or unresolved ref (worker failures on
deliberately corrupt fixtures are reported but do not fail the gate).
"""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT))

from axiom_ng_runner.compute_core.chunker import Chunker
from axiom_ng_runner.runner import (
    _collect_image_artifacts,
    _drop_link_refs,
)


def census(root: Path) -> int:
    epubs = sorted(root.rglob("*.epub"))
    failed = books_markers = markers = raw = unresolved = artifacts = 0
    failures: list[tuple[str, str]] = []
    for i, epub in enumerate(epubs, 1):
        with tempfile.TemporaryDirectory(prefix="census_") as td:
            td = Path(td)
            out_md, out_img = td / "md.md", td / "img"
            proc = subprocess.run(
                [sys.executable, "-m", "axiom_ng_runner.compute_core.epub_worker",
                 str(epub), str(out_md), str(out_img)],
                capture_output=True, text=True, check=False, cwd=str(REPO_ROOT),
            )
            if proc.returncode != 0:
                failed += 1
                failures.append((epub.name, proc.stderr.strip().splitlines()[-1][:80]))
                continue
            res = json.loads(proc.stdout.strip().splitlines()[-1])
            md = out_md.read_text(encoding="utf-8")
            chunks = Chunker(max_chunk_tokens=1200).chunk(md, doc_metadata={"doc_id": "census"})
            arts, orig_to_ref = _collect_image_artifacts(
                out_img, res.get("image_mapping") or {}, td
            )
            artifacts += len(arts)
            n_markers = md.count("![")
            markers += n_markers
            books_markers += bool(n_markers)
            raw += md.count("<figure") + md.count("<img")
            art_refs = {a["ref"] for a in arts}
            for c in chunks:
                for r in _drop_link_refs(c["metadata"].get("image_refs", [])):
                    orig = str(r.get("path", "")) if isinstance(r, dict) else str(r)
                    unresolved += orig_to_ref.get(Path(orig).name, orig) not in art_refs
        if i % 25 == 0:
            print(f"  {i}/{len(epubs)} markers={markers} raw={raw} unresolved={unresolved}",
                  flush=True)
    print(f"books={len(epubs)} ok={len(epubs) - failed} failed={failed} "
          f"with_markers={books_markers} markers={markers} raw_remains={raw} "
          f"unresolved_refs={unresolved} artifacts={artifacts}")
    for name, err in failures[:5]:
        print(f"  worker-fail: {name}: {err}")
    baseline = (5570, 0, 0)
    print(f"baseline (175 books, pandoc 3.7, 2026-09-15): "
          f"markers={baseline[0]} raw={baseline[1]} unresolved={baseline[2]}")
    return 1 if raw or unresolved else 0


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(f"usage: {sys.argv[0]} <epub-root>")
    sys.exit(census(Path(sys.argv[1])))
