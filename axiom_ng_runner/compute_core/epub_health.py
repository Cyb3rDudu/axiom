"""#220/#175 — EPUB preflight verdict (read-only, mirrors pdf_health).

ONE policy with the PDF gate: red → skip + repair-case, green/yellow with
extractable text → ok. epubcheck (W3C, BSD-3) is the conformance authority
for zip/OPF/spine — run as an EXTERNAL subprocess when configured
(settings.epubcheck_cmd or `epubcheck` on PATH), reported as
not_available otherwise. Bauentscheidung (#220 decision ruling 1): no Java
bundled into the conda-pack runner artifact (#208 lesson — never touch the
artifact interpreter/runtime); the jar lives in the host/container env and
the gate degrades honestly to the built-in light checks when absent.
"""
from __future__ import annotations

import json
import logging
import re
import shutil
import subprocess
import tempfile
import zipfile
from pathlib import Path

logger = logging.getLogger(__name__)

_FONT_OBFUSCATION = "http://www.idpf.org/2008/embedding"


def _epubcheck(path: str, cmd: str | None) -> dict:
    if cmd is None:
        cmd = shutil.which("epubcheck")
    if not cmd:
        return {"status": "not_available"}
    with tempfile.NamedTemporaryFile(suffix=".json", delete=False) as tf:
        out = Path(tf.name)
    try:
        subprocess.run(
            [cmd, path, "--json", str(out)],
            capture_output=True, text=True, timeout=120, check=False,
        )
        try:
            payload = json.loads(out.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            payload = {}
    except (OSError, subprocess.TimeoutExpired) as exc:
        return {"status": "error", "error": f"{type(exc).__name__}: {exc}"}
    finally:
        out.unlink(missing_ok=True)
    msgs = [
        {"severity": m.get("severity"), "message": m.get("message", "")[:200]}
        for m in payload.get("messages", [])
        if m.get("severity") in ("FATAL", "ERROR")
    ][:50]
    return {
        "status": "failed" if msgs else "ok",
        "fatal_or_error": len(msgs),
        "messages": msgs,
    }


def analyze_epub(path: str, epubcheck_cmd: str | None = None) -> dict:
    """Read-only EPUB diagnostic. Same return contract as pdf_health's
    analyze_pdf core fields (verdacht/label_befund/text_layer) so the
    /v1/pdf/preflight endpoint computes ok identically."""
    try:
        epub = zipfile.ZipFile(path)
    except (zipfile.BadZipFile, FileNotFoundError) as exc:
        raise ValueError(f"EPUB nicht lesbar: {exc}") from None

    names = epub.namelist()
    grund = []
    verdict = "🟢"

    # 1) zip/container/OPF/spine — structural presence
    opf_ok = False
    try:
        container = epub.read("META-INF/container.xml").decode("utf-8", "replace")
        m = re.search(r'full-path="([^"]+)"', container)
        if m and m.group(1) in names:
            opf = epub.read(m.group(1)).decode("utf-8", "replace")
            opf_ok = bool(re.search(r"<spine", opf))
    except KeyError:
        pass
    spine_docs = [
        n for n in names
        if n.lower().endswith((".xhtml", ".html")) and not n.startswith("META-INF/")
    ]
    if not opf_ok:
        verdict = "🔴"
        grund.append("OPF/Spine fehlt oder unlesbar")

    # 2) DRM — rights.xml is DRM by definition; encryption.xml is only
    # legit for IDPF font obfuscation (embedded fonts), anything else is
    # a lock we must not (and cannot) process around.
    drm = "META-INF/rights.xml" in names
    encrypted = "META-INF/encryption.xml" in names
    if encrypted:
        enc = epub.read("META-INF/encryption.xml").decode("utf-8", "replace")
        if _FONT_OBFUSCATION not in enc:
            drm = True
    if drm:
        verdict = "🔴"
        grund.append("DRM (rights.xml/encryption)")

    # 3) text extractable — at least one spine doc with real characters
    total_chars = 0
    for n in spine_docs[:50]:
        raw = epub.read(n).decode("utf-8", "replace")
        text = re.sub(r"<[^>]+>", " ", raw)
        total_chars += len(re.sub(r"\s", "", text))
        if total_chars > 500:
            break
    text_layer = total_chars > 500
    if not text_layer:
        verdict = "🔴"
        grund.append("kein extrahierbarer Text")
    epub.close()

    # 4) epubcheck conformance (external, optional by deployment)
    ec = _epubcheck(path, epubcheck_cmd or None)
    if ec.get("status") == "failed":
        verdict = "🔴"
        grund.append(f"epubcheck: {ec['fatal_or_error']} FATAL/ERROR")

    verdacht = {
        "🔴": "🔴 defekt/DRM (epub)",
    }.get(verdict, "🟢 gesund (epub)")
    return {
        "format": "epub",
        "verdacht": verdacht,
        "label_befund": "; ".join(grund) if grund else "Struktur ok, Text extrahierbar",
        "text_layer": text_layer,
        "opf_spine": opf_ok,
        "spine_docs": len(spine_docs),
        "drm": drm,
        "epubcheck": ec,
    }
