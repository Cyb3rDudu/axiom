"""Shared OPF spine resolution — one parser instead of N diverging ones.

Proven classes this consolidates (#378 and its review rounds):
- Wiley-class .htm filenames (extension guessing misses them),
- ../-segments in manifest hrefs (pandoc/jobs CVM26KLA/FFMTJA3S,
  cf. epub_repair.normalize_entry_paths),
- fragments must be split off BEFORE percent-decoding (RFC 3986: a
  literal '#' in a filename is legally encoded as %23),
- namespaced/prefixed OPF markup that regex parsers miss.

Contract: resolve every spine itemref to a REAL zip entry name — never
invent targets, never return unresolvable paths.
"""
from __future__ import annotations

import re
import xml.etree.ElementTree as ET
import zipfile
from posixpath import join as _pjoin
from posixpath import normpath
from urllib.parse import unquote


def _local(tag: str) -> str:
    return tag.rpartition("}")[2]


def _find_all(root: ET.Element, name: str) -> list[ET.Element]:
    return [el for el in root.iter() if _local(el.tag) == name]


def parse_spine_hrefs(epub: zipfile.ZipFile) -> tuple[bool, list[str]]:
    """Return (opf_spine_present, resolved spine entry names in order).

    opf_spine_present is True when container.xml + OPF + <spine> are
    readable — independent of whether every href resolves.
    """
    try:
        container = epub.read("META-INF/container.xml")
    except KeyError:
        return False, []
    try:
        croot = ET.fromstring(container)
    except ET.ParseError:
        return False, []
    full_path = None
    for rf in _find_all(croot, "rootfile"):
        full_path = full_path or rf.get("full-path")
    if not full_path:
        return False, []
    try:
        opf_bytes = epub.read(full_path)
    except KeyError:
        return False, []
    try:
        oroot = ET.fromstring(opf_bytes)
    except ET.ParseError:
        # Tolerate semi-broken OPFs: degrade to presence checks only.
        opf_ok = bool(re.search(rb"<spine\b", opf_bytes))
        return opf_ok, []
    spines = _find_all(oroot, "spine")
    if not spines:
        return False, []

    manifest: dict[str, str] = {}
    for item in _find_all(oroot, "item"):
        i_id, href = item.get("id"), item.get("href")
        if i_id and href:  # raw href — fragment split happens pre-unquote
            manifest[i_id] = href

    names = set(epub.namelist())
    lower_map = {n.lower(): n for n in names}
    opf_dir = full_path.rpartition("/")[0]
    resolved: list[str] = []
    for ref in _find_all(spines[0], "itemref"):
        href = manifest.get(ref.get("idref") or "")
        if not href:
            continue
        target = normpath(_pjoin(opf_dir, unquote(href.split("#", 1)[0])))
        if target in names:
            resolved.append(target)
        elif target.lower() in lower_map:  # zip entry case drift
            resolved.append(lower_map[target.lower()])
    return True, resolved
