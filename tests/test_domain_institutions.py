"""Tests for the domain → institution mapping used by the writing agent.

Why this exists: Run 1 of the China/DACH Hausarbeit produced citations
like ``(https://www.bpb.de/themen/…, o. S.)`` because the writing agent
dropped the raw URL into the citation parenthesis when a web source had
no ``authors`` metadata. We now translate the host to a canonical
institution name (``BPB``) where possible, and to the bare domain
(``bpb.de``) as a last resort — these tests pin that contract.
"""

from __future__ import annotations

import sys
from pathlib import Path

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402

from services.domain_institutions import (  # noqa: E402
    organization_for_url,
    short_domain,
    looks_like_raw_url_author,
    web_citation_author,
)


# ─────────────────────────────────────────────────────────────────────────────
# short_domain
# ─────────────────────────────────────────────────────────────────────────────


@pytest.mark.parametrize(
    "url, expected",
    [
        ("https://www.bpb.de/themen/asien/china/", "bpb.de"),
        ("http://bpb.de", "bpb.de"),
        ("www.Faz.NET/artikel/china", "faz.net"),
        ("https://kof.ethz.ch/prognosen.html", "kof.ethz.ch"),
        ("bundesbank.de", "bundesbank.de"),
        ("", None),
        ("not a url at all", None),
    ],
)
def test_short_domain_strips_scheme_www_and_lowercases(url, expected):
    assert short_domain(url) == expected


# ─────────────────────────────────────────────────────────────────────────────
# organization_for_url
# ─────────────────────────────────────────────────────────────────────────────


@pytest.mark.parametrize(
    "url, expected",
    [
        # German political / research institutions
        ("https://www.bpb.de/themen/asien/china/", "BPB"),
        ("https://www.bundesbank.de/de/statistiken", "Deutsche Bundesbank"),
        ("https://www.ifo.de/publikationen", "ifo Institut"),
        ("https://www.diw.de/de/diw_01.c.100919.de/", "DIW Berlin"),
        ("https://www.iwkoeln.de/studien", "IW Köln"),
        ("https://www.iw-koeln.de/studien", "IW Köln"),
        ("https://www.ifw-kiel.de/de/publikationen/", "IfW Kiel"),
        ("https://merics.org/en/report", "MERICS"),
        # International bodies
        ("https://www.imf.org/en/Publications/WP/Issues/2024", "IMF"),
        ("https://www.oecd.org/economic-outlook/", "OECD"),
        ("https://www.worldbank.org/en/topic/trade", "World Bank"),
        ("https://ec.europa.eu/commission/", "Europäische Kommission"),
        # Subdomain beats parent (longest-suffix match)
        ("https://kof.ethz.ch/prognosen.html", "KOF ETH Zürich"),
        ("https://seco.admin.ch/seco/de/home.html", "SECO"),
        ("https://ecb.europa.eu/pub/", "EZB"),
        # Academic publishers
        ("https://link.springer.com/book/10.1007/978-3-642-36522-5", "Springer"),
        ("https://www.sciencedirect.com/science/article/pii/X", "Elsevier"),
        ("https://papers.ssrn.com/sol3/papers.cfm?abstract_id=1234", "SSRN"),
        # Media
        ("https://www.faz.net/aktuell/wirtschaft/china/", "FAZ"),
        ("https://www.handelsblatt.com/politik/international/", "Handelsblatt"),
        # Unknown domain should return None (caller falls back to short_domain)
        ("https://some-random-blog.example/post", None),
        ("https://mycompanyblog.xyz/2024/05/china", None),
    ],
)
def test_organization_for_url_known_institutions(url, expected):
    assert organization_for_url(url) == expected


def test_organization_for_url_handles_bare_domain_input():
    assert organization_for_url("bpb.de") == "BPB"
    assert organization_for_url("www.diw.de") == "DIW Berlin"


def test_organization_for_url_returns_none_for_empty_input():
    assert organization_for_url("") is None
    assert organization_for_url(None) is None  # type: ignore[arg-type]


# ─────────────────────────────────────────────────────────────────────────────
# looks_like_raw_url_author
# ─────────────────────────────────────────────────────────────────────────────


@pytest.mark.parametrize(
    "candidate, expected",
    [
        ("https://www.bpb.de/themen/asien/china/", True),
        ("http://example.com", True),
        ("www.faz.net/artikel", True),
        ("BPB", False),
        ("Müller, P.", False),
        ("Bundesbank", False),
        ("", False),
    ],
)
def test_looks_like_raw_url_author(candidate, expected):
    assert looks_like_raw_url_author(candidate) is expected


# ─────────────────────────────────────────────────────────────────────────────
# web_citation_author — the contract that fixes the Run 1 citation bug
# ─────────────────────────────────────────────────────────────────────────────


def test_web_citation_author_prefers_real_authors():
    assert (
        web_citation_author(
            "Müller, P.", "https://www.bpb.de/themen/asien/china/"
        )
        == "Müller, P."
    )


def test_web_citation_author_joins_list_authors():
    assert (
        web_citation_author(
            ["Müller, P.", "Schmidt, A."],
            "https://www.diw.de/de/diw_01.c.100919.de/",
        )
        == "Müller, P., Schmidt, A."
    )


def test_web_citation_author_discards_url_in_authors_slot():
    """Regression: Run 1 put the URL into 'authors' — we must not echo it."""
    result = web_citation_author(
        "https://www.bpb.de/themen/asien/china/",
        "https://www.bpb.de/themen/asien/china/",
    )
    assert result == "BPB"


def test_web_citation_author_uses_institution_when_authors_missing():
    assert (
        web_citation_author(None, "https://www.bpb.de/themen/asien/china/")
        == "BPB"
    )
    assert (
        web_citation_author(
            None, "https://www.bundesbank.de/de/statistiken"
        )
        == "Deutsche Bundesbank"
    )


def test_web_citation_author_falls_back_to_short_domain():
    # Unknown domain: should downgrade to bare host, NEVER raw URL.
    result = web_citation_author(
        None, "https://some-random-blog.example/post/2024/05"
    )
    assert result == "some-random-blog.example"


def test_web_citation_author_last_resort_when_no_url():
    """Without any signal at all we emit a deliberate placeholder — the
    writer is prompted never to fall back to a raw URL, so there is no
    URL to surface here either."""
    assert web_citation_author(None, None) == "Unbekannte Quelle"
    assert web_citation_author("", "") == "Unbekannte Quelle"
