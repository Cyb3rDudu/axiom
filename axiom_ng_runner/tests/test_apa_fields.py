"""#245 APA-7 citation fields frozen at ingest (chapter_number /
section_title / paragraph_in_chapter) — always together or not at all.

Mutation probes documented in the strand:
- capping paragraph_in_chapter (e.g. `max(1, ...)` -> `1`) must turn
  test_apa_fields_chaptered red;
- capping the SourceView content_type fill turns the Go contract test red
  (internal/repo/source_view_content_type_test.go).
"""

from axiom_ng_runner.compute_core.chunker import Chunker
from axiom_ng_runner.runner import _adapt_chunk, _freeze_apa_fields


def _md(*blocks: str) -> str:
    return "\n\n".join(blocks)


PARA = ("The lakehouse combines the flexibility of a data lake with the "
        "management features of a warehouse. " * 3).strip()


def _chunk(md: str):
    chunker = Chunker(max_chunk_tokens=1200)
    chunks = chunker.chunk(md, doc_metadata={"doc_id": "d"})
    starts = chunker.chapter_starts()
    for c in chunks:
        _freeze_apa_fields(c.setdefault("metadata", {}), starts)
    return chunks


def test_apa_fields_chaptered():
    md = _md(
        "# Chapter One", PARA, PARA,
        "# Chapter Two", "## Section A", PARA,
        "## Section B", PARA,
    )
    chunks = _chunk(md)
    assert chunks, "fixture must chunk"
    for c in chunks:
        m = c["metadata"]
        n = m["chapter_number"]
        assert n in (1, 2), m
        if n == 2:
            # deepest section title, not just the chapter heading
            assert m["section_title"] in ("Chapter Two", "Section A", "Section B"), m
            # 1-based paragraph count from the chapter heading
            assert m["paragraph_in_chapter"] >= 1
    # paragraph_in_chapter advances within chapter two
    ch2 = [c["metadata"]["paragraph_in_chapter"]
           for c in chunks if c["metadata"]["chapter_number"] == 2]
    assert ch2 == sorted(ch2), ch2
    assert ch2[0] == 1, "first paragraph after the chapter heading is 1"
    assert ch2[-1] > ch2[0], "later chunks advance within the chapter"


def test_apa_fields_absent_without_chapters():
    md = _md("## Only subsections", PARA, PARA, PARA)
    chunks = _chunk(md)
    for c in chunks:
        m = c["metadata"]
        assert "chapter_number" not in m, m
        assert "paragraph_in_chapter" not in m, m
        assert "section_title" not in m, m


def test_apa_fields_ride_on_locator():
    # synthetic epub_cfi chunk: _adapt_chunk must carry the frozen fields
    c = {"text": PARA, "metadata": {
        "locator_type": "epub_cfi", "cfi_start": "/6/4!/4/2", "cfi_end": "",
        "chapter_number": 3, "section_title": "Section B",
        "paragraph_in_chapter": 17, "token_count": 8,
    }}
    a = _adapt_chunk(c, 0, page_label_map={}, page_source_map={}, page_chapter_map={})
    loc = a["locator"]
    assert loc["type"] == "epub_cfi", loc
    assert loc.get("chapter_number") == 3, loc
    assert loc.get("section_title") == "Section B", loc
    assert loc.get("paragraph_in_chapter") == 17, loc
