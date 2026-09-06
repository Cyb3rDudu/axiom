"""folio_harvest — #258: VENDORED mirror of the #254 preflight harvest.

Source of truth: axiom_ng_runner/compute_core/page_trust.py (harvest_
folio_candidates / _drop_constants / _pick_candidates / extract_folio_
candidates). The fixer ships as a STANDALONE artifact (own venv, no
project imports — package discipline), so the harvest is vendored
byte-discipline-identical instead of imported. What #254 preflight
classifies as repairable-by-folios must be healable by those same
folios (issue #258): same bands (top 12%, bottom from 75%), same forms
(bare/eli/lseries/lead/mid/trail/roman/weak), same constants drop, same
strength+chain picking. Keep in sync with page_trust.py on purpose —
drift here re-opens the heal-loop.
"""

from __future__ import annotations

import re

import pymupdf  # type: ignore[reportMissingImports]

# #254: bottom harvest band reaches up to 75% of the page — the Plantin
# class of mid-page running heads (folio row at ~75-76%).
_BOT_BAND = 0.75

_FOLIO_LINE = re.compile(r"^\s*(\d{1,4})\s*$")
_ELI_BARE = re.compile(r"^(\d{1,4})/(\d{1,4})$")
_ELI_TAIL = re.compile(r"(?:^|\s)(\d{1,4})/(\d{1,4})(?:\s|$)")
_ROMAN_LINE = re.compile(r"^\s*([ivxlcdm]{1,7})\s*$", re.IGNORECASE)
_LSERIES = re.compile(r"\bL\s*\d{1,4}\s*/\s*(\d{1,4})\b")
_NUM_IN_LINE = re.compile(r"(?<![\d./(])(\d{1,4})(?![\d./)])")
_HEAD_LINE_MAX = 90


def _is_year(v: int) -> bool:
    return 1900 <= v <= 2030


def harvest_folio_candidates(doc: pymupdf.Document) -> dict[int, list[tuple[str, str, str]]]:
    """Zone-based multi-form folio harvest (#254 discipline; see page_trust)."""
    out: dict[int, list[tuple[str, str, str]]] = {}
    for i in range(doc.page_count):
        page = doc[i]
        rect = page.rect
        cands: list[tuple[str, str, str]] = []
        for zone, clip in (
            ("top", pymupdf.Rect(rect.x0, rect.y0, rect.x1, rect.y1 * 0.12)),
            ("bot", pymupdf.Rect(rect.x0, rect.y1 * _BOT_BAND, rect.x1, rect.y1)),
        ):
            text = page.get_text("text", clip=clip)
            for line in (l.strip() for l in text.splitlines()):
                if not line:
                    continue
                if _FOLIO_LINE.match(line):
                    v = int(line)
                    if _is_year(v):
                        cands.append(("weak", line, zone))
                    else:
                        cands.append(("bare", line, zone))
                    continue
                if _ROMAN_LINE.match(line):
                    cands.append(("roman", line.lower(), zone))
                    continue
                if len(line) > _HEAD_LINE_MAX:
                    continue  # body prose bleeding into the band: not a head
                m = _LSERIES.search(line)
                if m:
                    cands.append(("lseries", m.group(1), zone))
                    continue
                mb = _ELI_BARE.match(line)
                if mb and not (len(mb.group(1)) > 1 and mb.group(1)[0] == "0"):
                    cands.append(("eli", mb.group(1), zone))
                    continue
                if "eli" in line.lower():
                    mt = _ELI_TAIL.search(line)
                    if mt and not (len(mt.group(1)) > 1 and mt.group(1)[0] == "0"):
                        cands.append(("eli", mt.group(1), zone))
                        continue
                mr = re.search(r"\s([ivxlcdm]{1,7})\s*$", line, re.IGNORECASE)
                if mr and len(line.split()) >= 2:
                    cands.append(("roman", mr.group(1).lower(), zone))
                    continue
                nums = _NUM_IN_LINE.findall(line)
                if not nums:
                    continue
                if len(nums) >= 2 and len(re.sub(r"[\d\s.,/–-]", "", line)) < 4:
                    continue
                if line[-1] in ".!;:":
                    continue
                stripped = line.lstrip()
                for v in nums:
                    vi = int(v)
                    if _is_year(vi):
                        form = "weak"
                    elif stripped.startswith(v):
                        form = "lead" if v == nums[0] else "mid"
                    elif stripped.endswith(v):
                        form = "trail" if v == nums[-1] else "mid"
                    else:
                        form = "mid"
                    cands.append((form, v, zone))
        if cands:
            out[i] = cands
    return out


def _drop_constants(harvest: dict[int, list[tuple[str, str, str]]], page_count: int) -> None:
    """Running-head constants drop from medium/weak slots (#254; see page_trust)."""
    if page_count < 4:
        return
    limit = max(2, (page_count + 2) // 3)
    seen: dict[str, int] = {}
    for cands in harvest.values():
        for form, v, _ in cands:
            if form in ("lead", "mid", "trail", "weak"):
                seen[v] = seen.get(v, 0) + 1
    consts = {v for v, k in seen.items() if k >= limit}
    if not consts:
        return
    for p in list(harvest):
        kept = [c for c in harvest[p] if not (c[0] in ("lead", "mid", "trail", "weak") and c[1] in consts)]
        if kept:
            harvest[p] = kept
        else:
            del harvest[p]


def _pick_candidates(harvest: dict[int, list[tuple[str, str, str]]]) -> dict[int, str]:
    """One candidate per page: strength first, chain continuation second (#254)."""
    _STRENGTH = {"bare": 0, "eli": 1, "lseries": 1, "lead": 2, "trail": 2, "mid": 3, "roman": 4, "weak": 5}
    picked: dict[int, str] = {}
    last_strong: tuple[int, int] | None = None
    for p in sorted(harvest):
        cands = harvest[p]
        prev_v = last_strong[1] if last_strong and last_strong[0] == p - 1 else None
        best = None
        best_key = None
        for form, v, zone in cands:
            vi = _arabic(v)
            cont = False
            if vi is not None and prev_v is not None and vi == prev_v + 1:
                cont = True
            key = (0 if cont else 1, _STRENGTH.get(form, 9), 0 if zone == "top" else 1)
            if best_key is None or key < best_key:
                best, best_key = (form, v, zone), key
        if best is None:
            continue
        picked[p] = best[1]
        if best[0] in ("bare", "eli", "lseries"):
            vi = _arabic(best[1])
            last_strong = (p, vi) if vi is not None else None
    return picked


def extract_folio_candidates(doc: pymupdf.Document) -> dict[int, str]:
    """Folio candidates per page (0-based page -> candidate string)."""
    harvest = harvest_folio_candidates(doc)
    _drop_constants(harvest, doc.page_count)
    return _pick_candidates(harvest)


def _arabic(v: str | None) -> int | None:
    if v is None:
        return None
    m = _FOLIO_LINE.match(v)
    return int(m.group(1)) if m else None
