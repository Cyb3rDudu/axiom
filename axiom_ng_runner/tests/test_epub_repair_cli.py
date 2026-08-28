"""#220 Stage 2 — EPUB repair-case executor (fix.sh EPUB arm, end of the
chain: preflight red → repair case → invoker → fix.sh --format epub →
this CLI → healed work.epub or honestly parked exit 3).

Run: .venv/bin/python -m pytest tests/test_epub_repair_cli.py
"""
from __future__ import annotations

import json
import zipfile
from pathlib import Path

from axiom_ng_runner.compute_core.epub_repair_cli import main

_CONTAINER = """<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
 <rootfiles><rootfile full-path="content.opf"
  media-type="application/oebps-package+xml"/></rootfiles>
</container>
"""
_CHAPTER = ('<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml">'
            "<body><p>" + "Kapiteltext mit ausreichend Inhalt zum Extrahieren. " * 30 +
            "</p></body></html>")


def _broken_spine_epub(path: Path) -> None:
    with zipfile.ZipFile(path, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml", _CONTAINER)
        z.writestr("content.opf", (
            '<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" '
            'version="3.0"><metadata/><manifest>'
            '<item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>'
            "</manifest></package>"))
        z.writestr("c1.xhtml", _CHAPTER)


def test_broken_spine_heals_to_work_epub(tmp_path, capsys):
    src = tmp_path / "broken.epub"
    _broken_spine_epub(src)
    rc = main(["--key", "KEY1", "--source", str(src),
               "--work-root", str(tmp_path), "--apply"])
    assert rc == 0
    work = tmp_path / "KEY1" / "work.epub"
    assert work.is_file() and work.read_bytes()[:2] == b"PK"
    report = json.loads((tmp_path / "KEY1" / "epub_repair_report.json").read_text())
    assert "repair_spine" in report["applied"]
    assert report["preflight"]["verdacht"].startswith("🟢")
    out = json.loads(capsys.readouterr().out.splitlines()[-1])
    assert out["ok"] is True


def test_dry_run_reports_without_artifact(tmp_path):
    src = tmp_path / "broken.epub"
    _broken_spine_epub(src)
    rc = main(["--key", "KEY2", "--source", str(src), "--work-root", str(tmp_path)])
    assert rc == 0  # repairable
    assert not (tmp_path / "KEY2" / "work.epub").exists()


def test_truncated_epub_parks_honestly(tmp_path, capsys):
    """The E2E broken_truncated class: unreadable source → exit 3 (parked),
    never a retry circus over a file no mechanical op can fix."""
    src = tmp_path / "truncated.epub"
    src.write_bytes(b"PK\x03\x04truncated garbage")
    rc = main(["--key", "KEY3", "--source", str(src),
               "--work-root", str(tmp_path), "--apply"])
    assert rc == 3
    out = json.loads(capsys.readouterr().out.splitlines()[-1])
    assert out["ok"] is False and "unreadable" in out["error"]


def test_healthy_epub_needs_no_repair(tmp_path):
    """Nothing broken → no op applies → exit 3 (nothing to heal), the case
    is parked with the green report rather than looping."""
    src = tmp_path / "healthy.epub"
    with zipfile.ZipFile(src, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml", _CONTAINER)
        z.writestr("content.opf", (
            '<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" '
            'version="3.0"><metadata/><manifest>'
            '<item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>'
            '</manifest><spine><itemref idref="c1"/></spine></package>'))
        z.writestr("c1.xhtml", _CHAPTER)
    rc = main(["--key", "KEY4", "--source", str(src),
               "--work-root", str(tmp_path), "--apply"])
    assert rc == 3
