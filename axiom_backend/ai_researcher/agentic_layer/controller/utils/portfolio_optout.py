"""
Literaturportfolio opt-out keyword detection.

Defaults to *enabled*. A user who doesn't want the Literaturportfolio for a
given mission can say so in natural language — we honour it by setting
`deliverables.literature_portfolio = False` on the mission settings.
"""

from __future__ import annotations

import re
from typing import Iterable, Optional


# Word-boundary patterns, case-insensitive. Multilingual (DE + EN).
# Additions should always anchor to a word boundary to avoid false positives
# like "ohne Literaturportfolio-Pflicht" → still an opt-out, fine;
# but "Portfolio" alone is NOT enough and must NOT trigger.
_OPTOUT_PATTERNS: list[re.Pattern[str]] = [
    re.compile(r"\bohne\s+literatur\s*portfolio\b", re.IGNORECASE),
    re.compile(r"\bohne\s+portfolio\b", re.IGNORECASE),
    re.compile(r"\bkein(?:e|en|es|er)?\s+literatur\s*portfolio\b", re.IGNORECASE),
    re.compile(r"\bkein(?:e|en|es|er)?\s+portfolio\b", re.IGNORECASE),
    re.compile(r"\bno\s+literature\s+portfolio\b", re.IGNORECASE),
    re.compile(r"\bno\s+portfolio\b", re.IGNORECASE),
    re.compile(r"\bskip\s+(?:the\s+)?(?:literature\s+)?portfolio\b", re.IGNORECASE),
    re.compile(r"\bportfolio\s+off\b", re.IGNORECASE),
    re.compile(r"\bdisable\s+(?:literature\s+)?portfolio\b", re.IGNORECASE),
]


def detect_portfolio_optout(user_request: Optional[str], *extra_sources: Optional[str]) -> bool:
    """Return True when the user *explicitly* wants the Literaturportfolio off.

    The default (everywhere the user does not opt out) is always ON, so this
    function returns False for empty / None inputs.
    """
    candidates: Iterable[Optional[str]] = (user_request, *extra_sources)
    for chunk in candidates:
        if not chunk:
            continue
        for pat in _OPTOUT_PATTERNS:
            if pat.search(chunk):
                return True
    return False


def deliverables_for_mission(
    user_request: Optional[str],
    explicit_flag: Optional[bool] = None,
    *extra_sources: Optional[str],
) -> dict:
    """Build the `deliverables` sub-dict that goes into mission_settings.

    Precedence:
      1. Explicit API flag wins.
      2. Otherwise, opt-out keyword disables; default is enabled.
    """
    if explicit_flag is not None:
        enabled = bool(explicit_flag)
    else:
        enabled = not detect_portfolio_optout(user_request, *extra_sources)
    return {"literature_portfolio": enabled}
