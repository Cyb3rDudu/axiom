"""EPUB -> markdown conversion via pandoc, as a short-lived subprocess.

Usage:
    python -m axiom_ng_runner.compute_core.epub_worker <epub_path> <out_markdown_path> <out_images_dir>

Writes:
    - markdown to ``out_markdown_path``
    - each extracted image to ``out_images_dir/image_<N>.<ext>``
    - a final JSON line to stdout:
        {"ok": true, "image_mapping": {saved_filename: saved_filename, ...}}
      The worker itself rewrites every image reference in the markdown to
      the saved ``image_<N>.<ext>`` name (#274) — the mapping keys are the
      saved names, so the caller's basename lookup resolves directly.

Exits non-zero on any failure (with JSON error on stderr).

This is the EPUB counterpart of ``axiom_ng_runner.compute_core.pdf_worker`` — same CLI
contract, same stdout/stderr JSON protocol, same image-naming scheme.
The only difference is the engine: pandoc (CPU) instead of Marker (GPU).
The ``pandoc`` binary ships in the artifact env (bundled-binaries
standard #224/#286: env-relative resolution, PATH only as dev fallback).

Deliberately minimal imports at module-load time so startup is fast.
"""

from __future__ import annotations

import json
import logging
import os
import re
import shutil
import subprocess
import sys
import tempfile
import traceback
from pathlib import Path
from typing import Any, Dict


def _stderr_err(payload: Dict[str, Any]) -> None:
    """Emit a single-line JSON error on stderr."""
    print(json.dumps(payload), file=sys.stderr, flush=True)


def _result(payload: Dict[str, Any]) -> None:
    """Emit the single-line JSON result on stdout (last line wins)."""
    print(json.dumps(payload), flush=True)


# Image extensions pandoc may extract from an EPUB. Matched
# case-insensitively when walking the --extract-media dir.
_IMAGE_EXTS = {".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp"}

# Purely-stylistic inline/block HTML tags pandoc leaves in GFM output when
# converting EPUB XHTML. EPUBs wrap text in <span class="bold">,
# <div class="img_container">, etc.; these carry no semantic value (no
# href/src/data-* attrs) and only clutter the markdown, so we strip them
# and keep the inner text. Markdown image refs ![](...) and links []()
# are untouched because they're not HTML tags.
_STYLING_TAG_RE = re.compile(r"</?(span|div)\b[^>]*>", re.IGNORECASE)


def _strip_styling_html(markdown: str) -> str:
    """Remove stylistic ``<span>``/``<div>`` wrappers pandoc leaks into GFM.

    Without this, headings come out as
    ``# <span class="bold">Title</span>`` instead of ``# Title``. Stripping
    yields clean markdown comparable to Marker's PDF output. Verified safe on
    real textbooks: the matched tags never carry href/src/data attributes.
    """
    cleaned = _STYLING_TAG_RE.sub("", markdown)
    # Block <div> removal can open up runs of blank lines; collapse 3+ down to
    # a single blank paragraph break so the chunker's splitting stays tight.
    cleaned = re.sub(r"\n{3,}", "\n\n", cleaned)
    return cleaned


# EPUB 3 pagebreak landmarks surface from pandoc as
# ``\[P\]<span id="..._page_P"></span>`` where P is the printed page number.
# Must run BEFORE _strip_styling_html (which would delete the landmark spans).
_PAGEBREAK_RE = re.compile(
    r'(?:\\\[(\d+)\\\])?\s*<span id="[^"]*?_page_(\d+)"></span>'
)


# #226 F1: canonicalize ALL four print-page dialects into the landmark form
# pandoc passes through (and _PAGEBREAK_RE above understands). The old worker
# only caught native Apress ``…_page_P`` ids by luck; the #222-injected shape
# (epub:type pagebreak id="PBn" role="doc-pagebreak"), Jossé class="page" and
# Bieger id="page_N" never became {N} markers. One source of truth: the
# dialect GRAMMAR mirrors compute_core.epub_pagelist (same number precedence
# title > aria-label > id > text).
def _canonicalize_page_anchors(raw: str) -> str:
    """Rewrite every dialect anchor into ``<span id="axiom_page_N"></span>``."""
    import re as _re

    def _num_from(attrs: str, text: str = "") -> str | None:
        for key in ("title", "aria-label", "id"):
            m = _re.search(rf'\b{key}="([^"]*)"', attrs)
            if m:
                d = _re.search(r"\d{1,4}", m.group(1))
                if d:
                    return d.group(0)
        d = _re.search(r"\d{1,4}", text)
        return d.group(0) if d else None

    def _span(n: str) -> str:
        return f'<span id="axiom_page_{n}"></span>'

    # 1) epub:type="pagebreak" on any element (native Apress + #222-injected)
    def _re_pagebreak(m: _re.Match[str]) -> str:
        n = _num_from(m.group(2))
        return _span(n) if n else m.group(0)

    raw = _re.sub(
        r"<(\w+)((?:[^>\"']|\"[^\"]*\"|'[^']*')*"
        r"\bepub:type=([\"'])[^\"']*\bpagebreak\b[^\"']*\3"
        r"(?:[^>\"']|\"[^\"]*\"|'[^']*')*)\s*/?>",
        _re_pagebreak, raw,
    )

    # 2) Jossé/dtv: <a class="page" id="page_N">N</a> (inline, number in text).
    # W1: "page" must be a WHITESPACE TOKEN of the class list — mirroring
    # epub_pagelist._is_candidate's class split — so class="page-num" or
    # class="page-break" does NOT match.
    def _re_class_page(m: _re.Match[str]) -> str:
        n = _num_from(m.group(1), m.group(3))
        return _span(n) if n else m.group(0)

    raw = _re.sub(
        r"<a\b([^>]*\bclass=([\"'])(?:[^\"']*\s)?page(?:\s[^\"']*)?\2[^>]*)>"
        r"\s*(\[?\d{1,4}\]?)\s*</a>",
        _re_class_page, raw,
    )

    # 3) Bieger/Springer: <a id="page_N"/> — self-closing or paired-empty —
    # followed by an optional [N] echo
    def _re_id_page(m: _re.Match[str]) -> str:
        return _span(m.group(2))

    raw = _re.sub(
        r"<a\b[^>]*\bid=([\"'])page[_-]?(\d{1,4})\1[^>]*(?:/>|>\s*</a>)"
        r"\s*(?:\[\d{1,4}\])?",
        _re_id_page, raw,
    )
    return raw


def _marker_canonicalized_copy(epub_path: Path, out_dir: Path) -> Path:
    """Copy with all page anchors canonicalized (fast-path: none found)."""
    import zipfile

    out = out_dir / ("anchors_" + epub_path.name)
    changed = False
    with zipfile.ZipFile(epub_path) as z, \
            zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zout:
        for item in z.infolist():
            data = z.read(item.filename)
            if item.filename.lower().endswith((".xhtml", ".html")):
                raw = data.decode("utf-8", "replace")
                fixed = _canonicalize_page_anchors(raw)
                if fixed != raw:
                    changed = True
                    data = fixed.encode("utf-8")
            zout.writestr(item, data)
    return out if changed else epub_path


def _inject_page_markers(markdown: str) -> str:
    """Turn EPUB pagebreak landmarks into Marker-style ``{N}----`` markers.

    Each landmark carries the printed page P (in the ``_page_P`` anchor id,
    optionally echoed as a visible ``\\[P\\]``). We replace it with a
    ``{P-1}----`` paragraph so the chunker — which maps marker index N to
    ``str(N + 1)`` — assigns the surrounding text to printed page P. The
    visible ``\\[P\\]`` is dropped so the page number doesn't echo in the
    body, mirroring how Marker strips page numbers from PDF output. This
    gives EPUBs accurate printed page numbers (when the EPUB ships a
    page-list); EPUBs without landmarks are unaffected (chunks stay "1").
    """
    def repl(match: re.Match) -> str:
        page = int(match.group(2))
        return f"\n\n{{{page - 1}}}" + "-" * 48 + "\n\n"

    result = _PAGEBREAK_RE.sub(repl, markdown)
    # Strip standalone page-number echoes (\\[N\\]) that pandoc also emits at
    # page breaks but which weren't adjacent to a landmark span. The {N}
    # markers already carry the page info, so these echoes are redundant body
    # noise (mirrors Marker stripping page numbers from PDF body text).
    # Only matches escaped brackets with digits — leaves markdown links like
    # [97](#..._page_97) (index entries) and footnotes [^1] untouched.
    result = re.sub(r"\\\[\d+\\\]\s*", "", result)
    return result


# --- #274: HTML-image inlining ------------------------------------------
#
# Root cause of the systematic EPUB image loss (#274 corpus evidence: 0
# image-marker chunks while artifacts exist): publisher EPUBs wrap figures
# in <figure>/<figcaption>, and pandoc's GFM writer keeps those blocks as
# RAW HTML instead of markdown images. The chunker only recognizes
# ``![alt](path)`` — every figure-wrapped image vanished from the text
# flow, so no chunk ever carried an image ref. Parity with the PDF path
# means: an ``![](...)`` marker at the image's text position, the figcaption
# kept as a plain text line (PDF captions ride as text too — the #268
# matcher pairs them positionally).
# Attribute character class: anything except a bare > or quote marks —
# quoted attribute values may CONTAIN '>' (Databricks Fig15:
# alt="… path: All catalogs > unitygo > …"), which the old [^>]* form
# truncated at, silently dropping the image (review #274 round 2).
_ATTRS = r'(?:[^>"\']|"[^"]*"|\'[^\']*\')*'
_FIGURE_BLOCK_RE = re.compile(
    # Content guarded against a nested <figure> open: lazy content without
    # the guard rescans to EOF per unmatched start on malformed input
    # (measured 35 s at 20k opens — review #274 perf finding); the guard
    # bounds every scan to the next open/close pair. Nested figures do
    # not match (corpus: none; acceptable ceiling).
    rf"<figure\b{_ATTRS}>((?:(?!<figure\b)[\s\S])*?)</figure>",
    re.IGNORECASE,
)
_FIGCAPTION_RE = re.compile(
    rf"<figcaption\b{_ATTRS}>(.*?)</figcaption>", re.IGNORECASE | re.DOTALL
)
_IMG_TAG_RE = re.compile(rf"<img\b{_ATTRS}>", re.IGNORECASE)
_TAG_RE = re.compile(r"<[^>]+>")

# Code protection: pandoc's GFM writer emits fenced blocks for ```
# sources and 4-SPACE-INDENTED blocks for <pre><code> samples, but it
# also indents LIST content at 4 spaces (a nested item's content
# column) — indent alone cannot tell code from list content. The
# fence guard below splits fenced regions out; _indented_code_spans()
# classifies indented lines via a small list-context stack. The
# transition rules live in ITS docstring — the source of truth for
# what opens/closes a span, updated with every review finding.
# ponytail: pandoc-shape heuristic, not full CommonMark list parsing;
# corpus census: no ```/~~~ and no code-in-list anywhere — revisit only
# if a corpus book ships one.
_FENCE_SPLIT_RE = re.compile(
    # tempered content: the scan cannot cross another fence-open LINE
    # - that bounds unterminated opens to the next candidate instead
    # of rescanning to EOF per open (measured 15.9 s at 20k
    # unterminated fence lines before the guard; review #274 r4).
    r"(?m)^(?P<fence>`{3,}|~{3,})[^\n]*\n"
    r"(?:(?!^(?:`{3,}|~{3,}))[\s\S])*?"
    r"^(?P=fence)[ \t]*$"
)
_LIST_MARKER_RE = re.compile(r"(?:[-*+]|\d{1,9}[.)])(?:[ \t]+|$)")
# Thematic breaks (* * *, ---, ___) look like marker soup — never lists.
_THEMATIC_BREAK_RE = re.compile(r"(?:[-*_])(?:[ \t]*(?:[-*_])){2,}[ \t]*$")


def _indent_cols(line: str) -> int:
    """Visual column of the first non-whitespace char (tab = 4)."""
    n = 0
    for ch in line:
        if ch == " ":
            n += 1
        elif ch == "\t":
            n += 4 - (n % 4)
        else:
            break
    return n


def _indented_code_spans(markdown: str) -> list[tuple[int, int]]:
    """Character ranges of 4-space-indented lines that are NOT list
    content (list marker content columns are tracked; blank lines keep
    the context open — pandoc's loose lists).

    Transitions (review #274 round 5 — the round-4 machine was missing
    the dedent close and the empty-stack marker):
    - an open code span CLOSES on the first non-blank line indented < 4
      (round-4 let it run to EOF, eating refs after the sample);
    - a list marker line opens list context REGARDLESS of indent when
      the stack is open or the marker has no content (pandoc emits
      loose-list marker-only lines like '    4.  ' with an empty
      stack); a marker-WITH-content line at >=4 with an empty stack is
      an indented code sample line (phantom-marker guard), not a list;
      thematic breaks never count as markers;
    - inside an open code span, marker-looking lines stay code.
    ponytail: blockquote-nested code is untracked (corpus census: 0) —
    quote lines simply close code spans and pop the list stack.
    """
    spans: list[tuple[int, int]] = []
    stack: list[int] = []          # open list-item content columns
    code_start: int | None = None
    pos = 0
    for line in markdown.splitlines(keepends=True):
        start = pos
        pos += len(line)
        body = line.rstrip("\n")
        if not body.strip():
            continue               # blank: code AND list context persist
        indent = _indent_cols(body)
        rest = body.lstrip(" \t")
        if code_start is not None:
            if indent >= 4:
                continue           # still code (blank lines kept it open)
            spans.append((code_start, start))
            code_start = None      # dedent closes — never run to EOF
        is_quote = rest.startswith(">")
        marker = (
            _LIST_MARKER_RE.match(rest)
            if not is_quote and not _THEMATIC_BREAK_RE.fullmatch(rest)
            else None
        )
        if marker and (stack or indent < 4 or not rest[marker.end():].strip()):
            while stack and indent < stack[-1]:
                stack.pop()
            stack.append(indent + marker.end())
            continue
        while stack and indent < stack[-1]:
            stack.pop()
        if not stack and indent >= 4:
            code_start = start
    if code_start is not None:
        spans.append((code_start, len(markdown)))
    return spans


def _protected_spans(markdown: str) -> list[tuple[int, int]]:
    """Character ranges that are code (fenced or indented)."""
    spans = [m.span() for m in _FENCE_SPLIT_RE.finditer(markdown)]
    spans += _indented_code_spans(markdown)
    spans.sort()
    merged: list[tuple[int, int]] = []
    for s, e in spans:
        if merged and s <= merged[-1][1]:
            merged[-1] = (merged[-1][0], max(merged[-1][1], e))
        else:
            merged.append((s, e))
    return merged


def _outside_code_blocks(markdown: str, fn) -> str:
    """Apply ``fn`` to every segment that is not a code block."""
    spans = _protected_spans(markdown)
    if not spans:
        return fn(markdown)
    out: list[str] = []
    pos = 0
    for s, e in spans:
        out.append(fn(markdown[pos:s]))
        out.append(markdown[s:e])  # code: verbatim
        pos = e
    out.append(fn(markdown[pos:]))
    return "".join(out)


def _img_attr(tag: str, name: str) -> str:
    """Value of attribute ``name`` in an HTML tag ('' when absent)."""
    m = re.search(
        rf'(?<![\w-]){name}\s*=\s*("([^"]*)"|\'([^\']*)\')', tag,
        re.IGNORECASE,
    )
    if not m:
        return ""
    return m.group(2) if m.group(2) is not None else (m.group(3) or "")


def _img_tag_to_markdown(tag: str) -> str | None:
    """One ``<img>`` tag → ``![alt](src)``; None when it must NOT inline.

    Remote/data-URI sources return None (no artifact exists for them — a
    marker would die at the runner's CHUNK_IMAGE_REF_UNRESOLVED gate).
    """
    src = _img_attr(tag, "src")
    if not src or src.startswith(("http://", "https://", "data:")):
        return None
    import html as _html

    src = _html.unescape(src)
    alt = _html.unescape(_img_attr(tag, "alt")).replace("]", "")
    return f"![{alt}]({src})"


def _strip_to_text(html_fragment: str) -> str:
    """HTML fragment → plain text line(s): tags out, entities decoded.

    Tag-stripping runs twice — BEFORE and AFTER unescaping: an escaped
    ``&lt;img src="…"&gt;`` inside a figure description would otherwise
    materialize as a real tag and the later bare-<img> pass would turn it
    into a phantom marker (review #274 round 2 finding).
    """
    import html as _html

    text = _TAG_RE.sub("", html_fragment)
    text = _html.unescape(text)
    text = _TAG_RE.sub("", text)
    return "\n".join(
        line.strip() for line in text.splitlines() if line.strip()
    )


def _inline_html_images(markdown: str) -> str:
    """Rewrite every raw-HTML image into a markdown ``![](src)`` marker.

    ``<figure><img …/><figcaption>C</figcaption></figure>`` becomes the
    image marker followed by the caption as its own paragraph; a bare
    ``<img>`` becomes the marker in place. Any OTHER text inside the
    figure (Springer-style ``<div class="TextObject"><p>…`` image
    descriptions) is kept as text too — discarding it was a silent
    content regression (review #274 blocker, Sonko exemplar). Only HTML
    tags are touched — markdown images pandoc already emitted pass
    through unchanged. Fenced code blocks are skipped verbatim.
    """
    def _figure_repl(m: re.Match) -> str:
        inner = m.group(1)
        markers = [md for md in (_img_tag_to_markdown(t)
                                 for t in _IMG_TAG_RE.findall(inner)) if md]
        cap_m = _FIGCAPTION_RE.search(inner)
        cap = _strip_to_text(cap_m.group(1)) if cap_m else ""
        # residual content: everything except the imgs and the figcaption
        residual = _FIGCAPTION_RE.sub("", inner)
        residual = _IMG_TAG_RE.sub("", residual)
        residual = _strip_to_text(residual)
        if not markers:
            # Remote-only images inline to nothing below — returning the
            # block verbatim would leave raw <figure> HTML in the text
            # after the bare-<img> pass deletes the tags. Keep whatever
            # text exists; an entirely empty figure collapses to nothing.
            text = "\n\n".join(p for p in (cap, residual) if p)
            return "\n\n" + text + "\n\n" if text else ""
        parts = markers + [p for p in (cap, residual) if p]
        return "\n\n" + "\n\n".join(parts) + "\n\n"

    def _inline_pass(md: str) -> str:
        # Cheap guard against the quadratic worst case: without any
        # closing tag the DOTALL pattern rescans to EOF per unmatched
        # <figure> start (review #274 perf finding).
        if "</figure>" in md.lower():
            md = _FIGURE_BLOCK_RE.sub(_figure_repl, md)
        return _IMG_TAG_RE.sub(
            lambda m: _img_tag_to_markdown(m.group(0)) or "", md
        )

    return _outside_code_blocks(markdown, _inline_pass)


def _save_extracted_images(
    media_dir: Path, out_dir: Path
) -> tuple[Dict[str, str], Dict[str, str]]:
    """Copy every image pandoc extracted into ``out_dir`` under a stable
    name; return ``(mapping, ref_index)``.

    ``mapping`` is ``{saved_name: saved_name}`` — since #274 the worker
    itself rewrites every markdown image reference to the saved name, so
    the runner's basename lookup resolves directly (and duplicate
    basenames across EPUB subdirs can no longer mis-pair, because the
    rewrite matches the full extracted path first).

    ``ref_index`` maps every reference form pandoc may have written —
    the path relative to ``media_dir``, the absolute path in both its
    unresolved form (what pandoc writes with an absolute
    ``--extract-media`` dir — ``resolve()`` would follow macOS
    ``/tmp``/``/var`` symlinks and never match) and its resolved form,
    and (last resort, first wins) the bare basename — to the saved name.
    """
    out_dir.mkdir(parents=True, exist_ok=True)
    mapping: Dict[str, str] = {}
    ref_index: Dict[str, str] = {}

    extracted = sorted(
        (p for p in media_dir.rglob("*")
         if p.is_file() and p.suffix.lower() in _IMAGE_EXTS),
        key=lambda p: p.relative_to(media_dir).as_posix(),
    )
    for idx, src in enumerate(extracted):
        ext = src.suffix.lower() or ".png"
        new_name = f"image_{idx}{ext}"
        shutil.copy2(src, out_dir / new_name)
        mapping[new_name] = new_name
        for key in (
            src.relative_to(media_dir).as_posix(),
            str(src),          # unresolved absolute — pandoc's literal form
            str(src.resolve()),
            src.name,
        ):
            ref_index.setdefault(key, new_name)  # first wins on collisions
    return mapping, ref_index


# Markdown image ref, non-greedy to the FIRST ')': pandoc's link-wrapped
# license badges are ``[![alt](path)](url)`` — a greedy/\S+ path swallowed
# the ``)](url`` suffix, leaving the temp path in place and terminally
# failing the persist gate on 10 corpus books (review #274 round 2
# blocker). The optional quoted TITLE is consumed and DROPPED: the PDF
# path emits no titles, a title left in the ref would miss the runner's
# basename lookup (round 2 minor). Space-carrying paths resolve again
# (non-greedy stops at ')', not at whitespace).
_MD_IMG_RE = re.compile(r"!\[[^\]]*\]\(")   # image-ref open; path via scanner
_MD_TITLE_RE = re.compile(r'\s+"[^"]*"$')
_SRC_ATTR_RE = re.compile(
    rf'(<img\b{_ATTRS}?\bsrc=")([^"]+)(")', re.IGNORECASE
)


def _md_rewrite(md: str, resolve) -> str:
    """Rewrite markdown image refs by candidate scanning.

    For each ``![alt](`` opener the path is tried up to EVERY following
    ')' — the first candidate that resolves in the ref index wins and
    the remainder (a swallowed ')' plus any quoted title) is dropped.
    Handles the three real shapes one static regex cannot: link-wrapped
    badges (``[![a](p)](url)`` — candidate 'p' resolves first), titles
    (``![a](p "t")`` — the title tail never reaches the lookup), and
    bracket-carrying paths (``fig(1).png`` — the short candidate fails,
    the full one resolves). Unresolvable refs stay verbatim (first
    candidate; the runner's gates handle them downstream).
    """
    out: list[str] = []
    pos = 0
    for m in _MD_IMG_RE.finditer(md):
        start = m.end()
        scan_end = min(len(md), start + 4096)  # ponytail: sane path cap
        j = start
        hit = None
        while j < scan_end:
            k = md.find(")", j)
            if k < 0 or k >= scan_end:
                break
            cand = md[start:k]
            path = _MD_TITLE_RE.sub("", cand)
            saved = resolve(path) if path else None
            if saved:
                hit = (k, saved)
                break
            j = k + 1
        if hit is not None:
            k, saved = hit
            out.append(md[pos:m.end()])
            out.append(saved)
            pos = k  # leave the ')' in place
    out.append(md[pos:])
    return "".join(out)


def _rewrite_image_refs(markdown: str, ref_index: Dict[str, str]) -> str:
    """Point every image reference at its saved ``image_<N>.<ext>`` name.

    Covers the markdown form (link-wrapped, titled, bracket-carrying and
    spaced paths; titles are stripped) and (for anything
    `_inline_html_images` left as HTML) the ``src`` attribute. Code
    blocks are skipped verbatim. Unresolvable refs are left untouched —
    the runner's http/ref gates handle them downstream.
    """
    def _lookup(ref: str) -> str | None:
        return ref_index.get(ref) or ref_index.get(Path(ref).name) or None

    def _md_pass(md: str) -> str:
        return _md_rewrite(md, _lookup)

    def _src_pass(md: str) -> str:
        # Defense-in-depth only: in the normal flow no local-src <img>
        # survives `_inline_html_images`, so this pass rewrites at most
        # leftover remote refs (which the runner's link-ref gate drops
        # downstream anyway).
        return _SRC_ATTR_RE.sub(
            lambda m: m.group(1) + (_lookup(m.group(2)) or m.group(2)) + m.group(3),
            md,
        )

    return _outside_code_blocks(markdown, lambda md: _src_pass(_md_pass(md)))


# #220 Stage 2 promotion: the Z3 normalization lives in the shared repair
# toolbelt now (compute_core.epub_repair) so the fixer side can import it
# without the heavy worker. The local name stays for the W9 callers/tests.
from axiom_ng_runner.compute_core.epub_repair import (
    normalize_entry_paths as _normalized_epub_copy,
)

def _convert_via_pandoc(epub_path: Path, out_md: Path, media_dir: Path) -> None:
    """Shell out to the ``pandoc`` binary: EPUB -> GFM, images into media_dir.

    #286: pandoc resolves ENV-RELATIVELY first (bundled_bin: the artifact
    env ships pandoc since #224 — launchd/container PATHs never include
    env/bin, so a PATH-only lookup made the bundled copy unreachable);
    PATH remains the dev-machine fallback. Raises FileNotFoundError when
    pandoc is nowhere, or RuntimeError on a non-zero pandoc exit.
    ``--wrap=none`` keeps paragraphs on one line so the chunker sees
    coherent text; ``--extract-media`` pulls images out so we can rename
    + serve them like PDF figures.
    """
    from axiom_ng_runner.compute_core import bundled_env

    pandoc = bundled_env.bundled_bin("pandoc")
    if not pandoc:
        raise FileNotFoundError(
            "pandoc binary not found (bundled env or PATH) — install pandoc to convert EPUB"
        )

    media_dir.mkdir(parents=True, exist_ok=True)
    cmd = [
        pandoc,
        "--from", "epub",
        "--to", "gfm",
        "--wrap", "none",
        f"--extract-media={media_dir}",
        "--output", str(out_md),
        str(epub_path),
    ]
    proc = subprocess.run(
        # #286: child env carries env/bin first on PATH (bundled-tool
        # standard — same resolution discipline for the child as here)
        cmd, capture_output=True, text=True, env=bundled_env.child_env(with_tessdata=False)
    )
    if proc.returncode != 0:
        detail = (proc.stderr or "").strip().splitlines()
        detail_str = detail[-1] if detail else f"exit code {proc.returncode}"
        raise RuntimeError(f"pandoc failed: {detail_str}")


def main() -> int:
    logging.basicConfig(
        level=os.getenv("LOG_LEVEL", "INFO"),
        format="%(asctime)s [epub-worker] %(levelname)s %(name)s: %(message)s",
    )
    logger = logging.getLogger(__name__)

    if len(sys.argv) < 4:
        _stderr_err(
            {
                "ok": False,
                "error": (
                    "usage: python -m axiom_ng_runner.compute_core.epub_worker "
                    "<epub_path> <out_markdown> <out_images_dir>"
                ),
            }
        )
        return 2

    epub_path = Path(sys.argv[1])
    out_md = Path(sys.argv[2])
    out_images_dir = Path(sys.argv[3])

    if not epub_path.exists():
        _stderr_err({"ok": False, "error": f"EPUB not found: {epub_path}"})
        return 2

    # Ensure output dirs exist before we do any work.
    out_md.parent.mkdir(parents=True, exist_ok=True)

    # pandoc dumps extracted images here; we copy the ones we want into
    # out_images_dir, then discard the rest. System temp is fine — we copy
    # (not rename), so a cross-filesystem tmp doesn't matter.
    media_tmp = Path(tempfile.mkdtemp(prefix="epub_media_"))
    try:
        # Z3: normalisierte Paketansicht — pandoc stirbt an '..'-OPF-Referenzen
        norm = _normalized_epub_copy(epub_path, media_tmp)
        if norm != epub_path:
            logger.info(f"OPF hrefs normalized for pandoc: {epub_path.name}")
        # #226 F1: all four anchor dialects → landmark spans → {N} markers
        norm = _marker_canonicalized_copy(norm, media_tmp)
        if norm != epub_path:
            logger.info(f"page anchors canonicalized: {epub_path.name}")
        logger.info(f"Converting {epub_path.name} via pandoc...")
        _convert_via_pandoc(norm, out_md, media_tmp)

        markdown = out_md.read_text(encoding="utf-8")
        # Convert EPUB pagebreak landmarks -> {N} markers FIRST (the landmarks
        # live in <span> tags that _strip_styling_html would otherwise delete).
        markdown = _inject_page_markers(markdown)
        # #274: figures/bare imgs pandoc keeps as raw HTML -> markdown
        # markers at their text position (figcaption becomes a text line).
        markdown = _inline_html_images(markdown)
        # pandoc leaves stylistic <span>/<div> wrappers in GFM output; strip
        # them and persist the cleaned markdown so the caller reads a clean
        # file from out_md (it consumes markdown_path, not the in-memory text).
        markdown = _strip_styling_html(markdown)
        if not markdown.strip():
            _stderr_err({"ok": False, "error": "pandoc returned empty markdown"})
            return 1

        mapping, ref_index = _save_extracted_images(media_tmp, out_images_dir)
        # #274: rewrite refs to the saved names BEFORE persisting — the
        # markdown the runner reads carries stable names, no temp paths.
        markdown = _rewrite_image_refs(markdown, ref_index)
        out_md.write_text(markdown, encoding="utf-8")
        logger.info(f"Wrote markdown ({len(markdown)} chars) to {out_md}")
        logger.info(f"Wrote {len(mapping)} images to {out_images_dir}")

        _result(
            {
                "ok": True,
                "markdown_path": str(out_md),
                "images_dir": str(out_images_dir),
                "image_mapping": mapping,
            }
        )
        return 0

    except FileNotFoundError as exc:
        # Missing pandoc binary — operator actionable, no traceback noise.
        _stderr_err({"ok": False, "error": str(exc)})
        return 3

    except Exception as exc:
        # Includes DRM-protected / corrupt EPUBs: pandoc exits non-zero and
        # _convert_via_pandoc wraps it as RuntimeError. Surface the message
        # so the doc-processor can store it on the failed row.
        _stderr_err(
            {
                "ok": False,
                "error": str(exc),
                "traceback": traceback.format_exc(),
            }
        )
        return 1

    finally:
        shutil.rmtree(media_tmp, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
