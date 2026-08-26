"""#220 Stage 1 — epub_cfi locator enrichment through _adapt_chunk.

Pins the additive contract fields: page_start/page_end + page_source =
epub_pagelist ONLY with a monotone anchor map; chapter parity (1-based
spine ordinal); without a map the locator stays chapter+CFI with
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


def test_epub_locator_with_anchor_map():
    c = _adapt_chunk(_epub_chunk({
        "cfi_start": "epubcfi(/6/2!/4/2)",
        "cfi_end": "epubcfi(/6/2!/4/6)",
        "page_start": 10,
        "page_end": 12,
        "chapter": 1,
    }), 0, {}, {}, {})
    loc = c["locator"]
    assert loc["type"] == "epub_cfi"
    assert loc["page_start"] == 10 and loc["page_end"] == 12
    assert loc["chapter"] == 1  # PDF chapter parity
    assert loc["page_source"] == "epub_pagelist"


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
    epub_pagelist stamp iff it carries pages."""
    with_pages = {"locator": {"type": "epub_cfi", "cfi_start": "x",
                              "page_start": 3, "page_end": 3}}
    without = {"locator": {"type": "epub_cfi", "cfi_start": "x"}}
    _adapt_chunk({**with_pages, "ref": "chunk-0000", "index": 0}, 0, {}, {}, {})
    _adapt_chunk({**without, "ref": "chunk-0001", "index": 1}, 1, {}, {}, {})
    assert with_pages["locator"]["page_source"] == "epub_pagelist"
    assert without["locator"]["page_source"] == "none"
