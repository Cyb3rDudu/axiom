"""Single source of truth for parsing free-form author strings.

Prior state: four near-duplicate parsers across structured_bibliography,
mission_to_writing_handoff, bibliography_migrator, reference_service —
each handling separators in a slightly different order, so a string
that parsed cleanly in one place drifted when round-tripped through
handoff → migration → registry.

Input examples covered:
  "Müller, Peter; Schmidt, Anna"
  "Peter Müller and Anna Schmidt"
  "Smith, J. & Jones, A."
  "Destatis"
  "Müller, P. U."  (multi-initial given)
  "Hotz-Hart, B. & Rohner, A."
  [{"family": "X", "given": "Y"}, {"family": "Z"}]  # already structured

Output: uniform list of {family, given} dicts. Empty list on empty
input, never None. Institutional authors (single token, no comma)
return {family: "Destatis", given: ""}.
"""

from __future__ import annotations

import re
from typing import Any, Dict, List, Mapping


# Separators between author entries, ordered longest-first so "and" doesn't
# eat into "Strand" etc. ";" dominates when both are present.
_SPLIT_SEPARATORS_RE = re.compile(
    r";\s*|(?:\s+and\s+)|(?:\s*&\s*)",
    re.IGNORECASE,
)


def parse_authors(raw: Any) -> List[Dict[str, str]]:
    """Parse a free-form author string (or structured list) into [{family, given}].

    Accepts:
      - None / "" → []
      - str → split on separators, classify each token
      - list of dicts → normalise each, drop entries without a family

    Never raises on unparseable input; returns what it could recover.
    """
    if raw is None or raw == "":
        return []

    if isinstance(raw, list):
        return _parse_structured_list(raw)

    if not isinstance(raw, str):
        return []

    text = raw.strip()
    if not text:
        return []

    # Split on ; / and / & first, then classify each author
    chunks = _SPLIT_SEPARATORS_RE.split(text)
    authors: List[Dict[str, str]] = []
    for chunk in chunks:
        chunk = chunk.strip(" ,;")
        if not chunk:
            continue
        authors.append(_classify_single_author(chunk))
    return authors


def _parse_structured_list(raw: List[Any]) -> List[Dict[str, str]]:
    out: List[Dict[str, str]] = []
    for a in raw:
        if isinstance(a, Mapping):
            family = (a.get("family") or "").strip()
            given = (a.get("given") or "").strip()
            if family:
                out.append({"family": family, "given": given})
        elif isinstance(a, str):
            parsed = _classify_single_author(a)
            if parsed["family"]:
                out.append(parsed)
        elif hasattr(a, "family"):
            family = (getattr(a, "family", "") or "").strip()
            given = (getattr(a, "given", "") or "").strip()
            if family:
                out.append({"family": family, "given": given})
    return out


def _classify_single_author(token: str) -> Dict[str, str]:
    """Classify one author token into {family, given}.

    Rules:
      - "Family, Given" (contains comma) → split at first comma
      - "Given Family" (no comma, multi-word) → last token = family,
        rest = given
      - "Destatis" (no comma, single token) → family-only (institution)
    """
    if "," in token:
        family, _, given = token.partition(",")
        return {"family": family.strip(), "given": given.strip()}

    tokens = token.strip().split()
    if len(tokens) >= 2:
        return {"family": tokens[-1], "given": " ".join(tokens[:-1])}
    return {"family": token.strip(), "given": ""}
