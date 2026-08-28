"""#220/#223 — epub_cfi locator enrichment through _adapt_chunk.

Pins the additive contract fields: page_start/page_end + page_source =
print_verified (TOC-proven) or print_unverified (markers, no proof) ONLY
with a monotone, plausible, non-divergent anchor map; chapter parity
(1-based spine ordinal); without a map the locator stays chapter+CFI with
page_source=none. Go client compatibility = additive fields only.

Run: .venv/bin/python -m pytest tests/test_epub_locator_enrichment.py
"""
from __future__ import annotations

from axiom_ng_runner.runner import _adapt_chunk


def _epub_chunk(meta: dict) -> dict:
    return {
        "text": "Text",
        "metadata": {
            "locator_type": "epub_cfi",
            "section_titles": [],
            "token_count": 2,
            **meta,
        },
    }


def test_epub_locator_verified_map():
    c = _adapt_chunk(_epub_chunk({
        "cfi_start": "epubcfi(/6/2!/4/2)",
        "cfi_end": "epubcfi(/6/2!/4/6)",
        "page_start": 10,
        "page_end": 12,
        "page_verified": True,
        "chapter": 1,
    }), 0, {}, {}, {})
    loc = c["locator"]
    assert loc["type"] == "epub_cfi"
    assert loc["page_start"] == 10 and loc["page_end"] == 12
    assert loc["chapter"] == 1  # PDF chapter parity
    assert loc["page_source"] == "print_verified"
    # page_verified is a runner-internal handoff — the wire locator must
    # not carry the unknown field (trust travels via page_source only).
    assert "page_verified" not in loc


def test_epub_locator_unverified_map():
    """Markers without TOC proof → print_unverified, never verified."""
    c = _adapt_chunk(_epub_chunk({
        "cfi_start": "epubcfi(/6/2!/4/2)",
        "cfi_end": "epubcfi(/6/2!/4/6)",
        "page_start": 10,
        "page_end": 12,
    }), 0, {}, {}, {})
    loc = c["locator"]
    assert loc["page_source"] == "print_unverified"
    assert "page_verified" not in loc  # honest: no proof, no flag


def test_epub_locator_without_map_stays_honest():
    c = _adapt_chunk(_epub_chunk({
        "cfi_start": "epubcfi(/6/4!/4/2)",
        "cfi_end": "epubcfi(/6/4!/4/2)",
    }), 0, {}, {}, {})
    loc = c["locator"]
    assert "page_start" not in loc and "page_end" not in loc
    assert loc["page_source"] == "none"  # never silently upgraded
    assert "chapter" not in loc  # no spine info without enrichment


def test_contract_shaped_locator_stamp():
    """Pass-through branch: an already-shaped epub_cfi locator gets the
    print_verified/print_unverified stamp iff it carries pages."""
    verified = {"locator": {"type": "epub_cfi", "cfi_start": "x",
                            "page_start": 3, "page_end": 3, "page_verified": True}}
    unverified = {"locator": {"type": "epub_cfi", "cfi_start": "x",
                              "page_start": 3, "page_end": 3}}
    without = {"locator": {"type": "epub_cfi", "cfi_start": "x"}}
    _adapt_chunk({**verified, "ref": "chunk-0000", "index": 0}, 0, {}, {}, {})
    _adapt_chunk({**unverified, "ref": "chunk-0001", "index": 1}, 1, {}, {}, {})
    _adapt_chunk({**without, "ref": "chunk-0002", "index": 2}, 2, {}, {}, {})
    assert verified["locator"]["page_source"] == "print_verified"
    assert unverified["locator"]["page_source"] == "print_unverified"
    assert without["locator"]["page_source"] == "none"
