"""#220 Stage 2 — EPUB repair-case executor (fix.sh EPUB arm).

Invoked by scripts/fix.sh when the invoker routes an EPUB repair case
(``--format epub --source PATH``). Runs the mechanical toolbelt
(epub_repair.apply_repairs) against the source and lands the healed file
in the fixer WorkRoot convention (``<work-root>/<key>/work.epub`` +
``epub_repair_report.json``) — the exact analog of the PDF agent's
work.pdf. Exit codes mirror the wrapper contract:

  0    dry-run: repairable (ops applicable and the analyzer turns green)
       --apply: healed — repair applied, preflight green, work.epub written
  3    not repairable — nothing applied, or still red after repair, or
       the source is unreadable (truncated zip): parked honestly instead
       of a retry circus
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        prog="epub_repair_cli",
        description="mechanical EPUB repair for repair cases (#220)",
    )
    p.add_argument("--key", required=True, help="Zotero attachment key")
    p.add_argument("--source", required=True, help="source EPUB path")
    p.add_argument("--work-root", required=True, help="fixer WORK_ROOT")
    p.add_argument("--apply", action="store_true",
                   help="write the healed artifact; without it: dry-run report")
    args = p.parse_args(argv)

    from axiom_ng_runner.compute_core.epub_repair import apply_repairs

    out_dir = Path(args.work_root) / args.key
    source = Path(args.source)

    def _report_line(payload: dict) -> None:
        print(json.dumps(payload, ensure_ascii=False, default=str), flush=True)

    try:
        import tempfile
        import zipfile

        scratch = out_dir if args.apply else Path(tempfile.mkdtemp(prefix="epub_dryrun_"))
        result = apply_repairs(source, scratch)
    except (ValueError, OSError, zipfile.BadZipFile) as exc:
        # unreadable/truncated source: unrepairable by the mechanical belt
        _report_line({"ok": False, "error": f"source unreadable: {exc}"})
        return 3

    preflight = result["preflight"]
    _report_line({
        "ok": bool(preflight.get("verdacht", "").startswith(("🟢", "🟡"))),
        "applied": result["applied"],
        "preflight": preflight,
        "out": str(result["out"]) if args.apply else None,
    })

    ok = preflight.get("verdacht", "").startswith(("🟢", "🟡"))
    if not result["applied"] or not ok:
        return 3
    if not args.apply:
        return 0  # dry-run: repairable, nothing written

    work = out_dir / "work.epub"
    if result["out"] != work:
        work.write_bytes(result["out"].read_bytes())
    (out_dir / "epub_repair_report.json").write_text(
        json.dumps({"applied": result["applied"], "preflight": preflight},
                   ensure_ascii=False, default=str), encoding="utf-8")
    return 0


if __name__ == "__main__":
    sys.exit(main())
