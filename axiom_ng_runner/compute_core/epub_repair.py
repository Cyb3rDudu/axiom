"""#220 Stage 2 — mechanical EPUB repair toolbelt (stage-1 capable).

Pure-zipfile operations, no models, no network — the same class as the
PDF surgery toolbelt. Promoted from the W9/Z3 experiment (entry-path
normalization lived in epub_worker.__main__; it moves here so the fixer
side can import it without the heavy worker) plus the two structural
repairs from the epic:

  normalize_entry_paths  W9/Z3: pandoc-safe package view (OPF at the zip
                        root, href/src rewritten to literal archive names,
                        '..' references eliminated — never inventing paths)
  repair_spine           OPF without a usable <spine>: synthesize one from
                        the manifest's XHTML items (manifest order)
  remove_entry_corpses   zip entries nothing references (not in the
                        manifest, not infrastructure): dead weight out

apply_repairs chains them and re-runs the #175/#220 preflight analyzer on
the result — the same red→green proof discipline as pdf_health (a repair
only counts when the gate turns green).
"""
from __future__ import annotations

import posixpath
import re
import zipfile
from pathlib import Path
from typing import Any

_INFRA = ("mimetype",)


def _read_opf(z: zipfile.ZipFile) -> tuple[str | None, str]:
    for name in z.namelist():
        if name.lower().endswith(".opf"):
            return name, z.read(name).decode("utf-8", "replace")
    return None, ""


def _opf_path_from_container(z: zipfile.ZipFile, names: set[str]) -> str | None:
    try:
        container = z.read("META-INF/container.xml").decode("utf-8", "replace")
    except KeyError:
        return None
    m = re.search(r'full-path="([^"]+)"', container)
    if m and m.group(1) in names:
        return m.group(1)
    return None


def normalize_entry_paths(epub_path: Path, out_dir: Path) -> Path:
    """W9/Z3 promotion: pandoc-safe package view (OPF at the archive root).

    Proven class (jobs CVM26KLA/FFMTJA3S, pandoc error verbatim: 'No entry
    on path: OEBPS/../…/Cover.xhtml'): pandoc resolves OPF hrefs OPF-
    relatively but does NOT normalize the path segments — the literal
    '..'-joined path never exists in the zip although every target does
    after POSIX normalization. The copy moves the OPF to the archive root
    ('axiom_content.opf', container.xml points there) and rewrites
    href/src to literal archive names — root + name needs no '..'.
    Fast-path: without '..' in the OPF the ORIGINAL path is returned."""
    with zipfile.ZipFile(epub_path) as z:
        names = set(z.namelist())
        opf, src = _read_opf(z)
        if opf is None:
            return epub_path
        if "../" not in src:
            return epub_path
        opf_dir = posixpath.dirname(opf)

        def _norm_attr(m: re.Match[str]) -> str:
            raw = m.group(2)
            fixed = posixpath.normpath(posixpath.join(opf_dir, raw))
            if fixed == raw or fixed not in names:
                return m.group(0)  # never invent targets — real ones only
            return f'{m.group(1)}="{fixed}"'

        fixed_src = re.sub(r'(href|src)="([^"]+)"', _norm_attr, src)
        out = out_dir / ("normalized_" + epub_path.name)
        with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zout:
            for item in z.infolist():
                if item.filename == opf:
                    continue  # OPF moves to the root under a new name
                data = z.read(item.filename)
                if item.filename == "META-INF/container.xml":
                    data = (b'<container version="1.0" '
                            b'xmlns="urn:oasis:names:tc:opendocument:xmlns:container">'
                            b'<rootfiles><rootfile full-path="axiom_content.opf" '
                            b'media-type="application/oebps-package+xml"/>'
                            b"</rootfiles></container>")
                zout.writestr(item, data)
            zout.writestr("axiom_content.opf", fixed_src)
        return out


def repair_spine(epub_path: Path, out_dir: Path) -> Path:
    """Synthesize a <spine> from the manifest when the OPF has none (the
    preflight 'OPF/Spine fehlt' class). Manifest order = reading order
    heuristic; fast-path when a spine exists."""
    with zipfile.ZipFile(epub_path) as z:
        opf, src = _read_opf(z)
        if opf is None or re.search(r"<spine\b", src):
            return epub_path
        items = re.findall(
            r'<item\b[^>]*media-type="application/(?:xhtml\+xml|html)"[^>]*>', src
        )
        refs = []
        for tag in items:
            im = re.search(r'\bid="([^"]+)"', tag)
            hm = re.search(r'\bhref="([^"]+)"', tag)
            if im and hm:
                href = posixpath.normpath(posixpath.join(posixpath.dirname(opf), hm.group(1)))
                if href in set(z.namelist()) or hm.group(1) in set(z.namelist()):
                    refs.append(f'<itemref idref="{im.group(1)}"/>')
        if not refs:
            return epub_path
        fixed = src.replace("</package>",
                            f"<spine>{''.join(refs)}</spine></package>")
        out = out_dir / ("spinerepaired_" + epub_path.name)
        with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zout:
            for item in z.infolist():
                data = fixed.encode("utf-8") if item.filename == opf else z.read(item.filename)
                zout.writestr(item, data)
        return out


def remove_entry_corpses(epub_path: Path, out_dir: Path) -> Path:
    """Drop zip entries nothing references: keep infrastructure (mimetype,
    META-INF, the OPF), every manifest target and the nav doc; everything
    else is a corpse (the 'dokumen.pub' junk class). Fast-path: nothing to
    remove returns the original."""
    with zipfile.ZipFile(epub_path) as z:
        names = set(z.namelist())
        opf, src = _read_opf(z)
        if opf is None:
            return epub_path
        opf_dir = posixpath.dirname(opf)
        keep = {n for n in names if n == "mimetype" or n.startswith("META-INF/")}
        keep.add(opf)
        for href in re.findall(r'<item\b[^>]*\bhref="([^"]+)"[^>]*>', src) + \
                re.findall(r'<item\b[^>]*\bhref="([^"]+)"', src):
            fixed = posixpath.normpath(posixpath.join(opf_dir, href))
            for cand in (href, fixed):
                if cand in names:
                    keep.add(cand)
                keep.update(n for n in names if n.endswith("/" + cand))
        for n in names:
            if n.lower().endswith(("nav.xhtml", "nav.html")):
                keep.add(n)
        corpses = names - keep
        if not corpses:
            return epub_path
        out = out_dir / ("descorpsed_" + epub_path.name)
        with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zout:
            for item in z.infolist():
                if item.filename in keep:
                    zout.writestr(item, z.read(item.filename))
        return out


def apply_repairs(epub_path: Path, work_dir: Path) -> dict[str, Any]:
    """Chain all mechanical repairs and prove them via the preflight
    analyzer (red→green discipline). Returns the report; ``out`` is the
    repaired artifact (== epub_path when no op applied)."""
    work_dir.mkdir(parents=True, exist_ok=True)
    current = epub_path
    applied: list[str] = []
    for name, op in (
        ("normalize_entry_paths", normalize_entry_paths),
        ("repair_spine", repair_spine),
        ("remove_entry_corpses", remove_entry_corpses),
    ):
        result = op(current, work_dir)
        if result != current:
            applied.append(name)
            current = result
    from axiom_ng_runner.compute_core.epub_health import analyze_epub

    report = analyze_epub(str(current))
    return {"out": current, "applied": applied, "preflight": report}
