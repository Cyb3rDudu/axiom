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
from posixpath import dirname as _posix_dirname
from posixpath import normpath as _normpath
from urllib.parse import unquote
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
    analyze_pdf core fields (finding/label_befund/text_layer) so the
    /v1/pdf/preflight endpoint computes ok identically. Diagnostic
    sub-keys like label_befund deliberately remain German — internal
    analyzer payload, not part of the #219/#196 quality_state contract
    (the dispatcher copies only the English detail keys)."""
    try:
        epub = zipfile.ZipFile(path)
    except (zipfile.BadZipFile, FileNotFoundError) as exc:
        raise ValueError(f"EPUB nicht lesbar: {exc}") from None

    names = epub.namelist()
    reasons = []
    verdict = "🟢"

    # 1) zip/container/OPF/spine — structural presence
    opf_ok = False
    manifest: dict[str, str] = {}
    opf_dir = ""
    spine_idrefs: list[str] = []
    try:
        container = epub.read("META-INF/container.xml").decode("utf-8", "replace")
        m = re.search(r'full-path="([^"]+)"', container)
        if m and m.group(1) in names:
            opf_path = m.group(1)
            opf_dir = _posix_dirname(opf_path)
            opf = epub.read(opf_path).decode("utf-8", "replace")
            opf_ok = bool(re.search(r"<spine", opf))
            # #378 — derive spine docs from the OPF itself instead of
            # guessing by extension: Wiley & co. legally use .htm filenames
            # that the old .xhtml/.html filter missed entirely.
            for item in re.findall(r"<item\b[^>]*/?>", opf):
                i_id = re.search(r'id="([^"]+)"', item)
                i_href = re.search(r'href="([^"]+)"', item)
                if i_id and i_href:
                    manifest[i_id.group(1)] = unquote(i_href.group(1))
            for ref in re.findall(r"<itemref\b[^>]*/?>", opf):
                r_id = re.search(r'idref="([^"]+)"', ref)
                if r_id:
                    spine_idrefs.append(r_id.group(1))
    except KeyError:
        pass
    if spine_idrefs and manifest:
        base = f"{opf_dir}/" if opf_dir else ""
        # Normalization + membership check, proven class (jobs CVM26KLA/
        # FFMTJA3S, cf. epub_repair.normalize_entry_paths): manifest hrefs
        # may carry ../ segments, ./ prefixes, or fragments — resolve to a
        # real zip entry and never invent or crash on unresolvable targets.
        spine_docs = []
        for rid in spine_idrefs:
            href = manifest.get(rid)
            if not href:
                continue
            target = _normpath(base + href.split("#")[0])
            if target in names:
                spine_docs.append(target)
    else:
        # Fallback for OPFs we could not fully parse (missing manifest or
        # spine idrefs): extension heuristic, .htm included (#378).
        spine_docs = [
            n for n in names
            if n.lower().endswith((".xhtml", ".html", ".htm"))
            and not n.startswith("META-INF/")
        ]
    if not opf_ok:
        verdict = "🔴"
        reasons.append("OPF/Spine fehlt oder unlesbar")

    # 2) DRM — rights.xml is DRM by definition; encryption.xml is only
    # legit when EVERY EncryptedData uses the IDPF font-obfuscation
    # algorithm (embedded fonts). A document that merely mentions the font
    # URI but also encrypts content with anything else (aes128-cbc …) is a
    # lock we must not (and cannot) process around.
    drm = "META-INF/rights.xml" in names
    if "META-INF/encryption.xml" in names:
        enc = epub.read("META-INF/encryption.xml").decode("utf-8", "replace")
        algs = set(re.findall(r'Algorithm\s*=\s*"([^"]+)"', enc))
        if not algs or algs != {_FONT_OBFUSCATION}:
            drm = True
    if drm:
        verdict = "🔴"
        reasons.append("DRM (rights.xml/encryption)")

    # 3) text extractable — at least one spine doc with real characters
    total_chars = 0
    for n in spine_docs[:50]:
        try:
            raw = epub.read(n).decode("utf-8", "replace")
        except KeyError:  # unresolvable entry: skip, never crash the probe
            continue
        text = re.sub(r"<[^>]+>", " ", raw)
        total_chars += len(re.sub(r"\s", "", text))
        if total_chars > 500:
            break
    text_layer = total_chars > 500
    if not text_layer:
        verdict = "🔴"
        reasons.append("kein extrahierbarer Text")
    epub.close()

    # 4) epubcheck conformance (external, optional by deployment)
    ec = _epubcheck(path, epubcheck_cmd or None)
    if ec.get("status") == "failed":
        verdict = "🔴"
        reasons.append(f"epubcheck: {ec['fatal_or_error']} FATAL/ERROR")

    finding = {
        "🔴": "🔴 defekt/DRM (epub)",
    }.get(verdict, "🟢 gesund (epub)")
    return {
        "format": "epub",
        "finding": finding,
        "label_befund": "; ".join(reasons) if reasons else "Struktur ok, Text extrahierbar",
        "text_layer": text_layer,
        "opf_spine": opf_ok,
        "spine_docs": len(spine_docs),
        "drm": drm,
        "epubcheck": ec,
    }
