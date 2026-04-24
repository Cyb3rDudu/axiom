"""Tests for figure-intent detection + RAG candidate loader."""

from __future__ import annotations

import sys
from pathlib import Path
from types import SimpleNamespace

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402
import api as _api_primer  # noqa: F401, E402

from services.figure_resolution import (  # noqa: E402
    build_figure_injection,
    detect_figure_intent,
    FigureCandidate,
    FigureQuery,
    resolve_figures,
    _image_url_from_path,
)


class TestDetectFigureIntent:
    def test_german_abbildung_keyword(self):
        qs = detect_figure_intent("Bitte füge drei Abbildungen ein.")
        assert len(qs) == 1
        assert qs[0].source == "instruction"

    def test_english_figure_keyword(self):
        qs = detect_figure_intent("Add a figure for each section.")
        assert len(qs) == 1

    def test_market_research_chart(self):
        qs = detect_figure_intent("Include a chart of market share 2020-2024.")
        assert len(qs) == 1

    def test_no_intent(self):
        qs = detect_figure_intent("Please kürze den Entwurf auf 3000 Wörter.")
        assert qs == []

    def test_placeholder_in_draft(self):
        draft = (
            "Some prose.\n"
            "![Abbildung 1: Chinas BIP 2000-2024](placeholder-fig1.png)\n"
            "More prose."
        )
        qs = detect_figure_intent("", draft)
        assert any("BIP" in q.description for q in qs)
        assert qs[0].source == "placeholder"

    def test_explicit_numbered_description_in_prompt(self):
        prompt = (
            "Please add:\n"
            "Abbildung 1: Exports by sector 2020-2024\n"
            "Abbildung 2: Market share shifts\n"
        )
        qs = detect_figure_intent(prompt)
        titles = [q.description for q in qs]
        assert any("Exports by sector" in t for t in titles)
        assert any("Market share shifts" in t for t in titles)

    def test_dedupes_identical_descriptions(self):
        # Two placeholders with byte-identical alt text (same figure
        # requested twice) should collapse to one query.
        draft = (
            "![Exactly same caption](placeholder-fig1.png)\n"
            "![Exactly same caption](placeholder-fig2.png)\n"
        )
        qs = detect_figure_intent("", draft)
        assert len(qs) == 1


class TestImageUrl:
    def test_strips_disk_path(self):
        url = _image_url_from_path(
            "doc-123", "/app/data/processed/images/doc-123/fig_1.png"
        )
        assert url == "/api/documents/images/doc-123/fig_1.png"

    def test_handles_bare_filename(self):
        url = _image_url_from_path("doc-x", "fig.png")
        assert url == "/api/documents/images/doc-x/fig.png"

    def test_empty_path(self):
        url = _image_url_from_path("doc-x", "")
        assert url.endswith("unknown")


class TestBuildFigureInjection:
    def _cands(self, alt_text: str = "Some chart"):
        return [
            FigureCandidate(
                image_id="img-1",
                doc_id="doc-1",
                image_url="/api/documents/images/doc-1/fig1.png",
                alt_text=alt_text,
                relevance=0.85,
                source_document_title="Source Book",
                source_page=45,
            )
        ]

    def test_candidates_present_builds_copy_hint(self):
        qs = [FigureQuery(description="Chinas BIP 2000-2024", source="placeholder")]
        out = build_figure_injection(qs, {"Chinas BIP 2000-2024": self._cands()})
        assert "Chinas BIP 2000-2024" in out
        assert "/api/documents/images/doc-1/fig1.png" in out
        assert "KOPIERE" in out or "COPY" in out

    def test_no_candidates_builds_no_hit_hint(self):
        qs = [FigureQuery(description="Something", source="instruction")]
        out = build_figure_injection(qs, {"Something": []})
        assert out is not None
        assert "keine Treffer" in out or "No matching" in out

    def test_english_variant(self):
        qs = [FigureQuery(description="Market share", source="placeholder")]
        out = build_figure_injection(
            qs, {"Market share": self._cands()}, language_code="en"
        )
        assert "COPY" in out
        assert "verbatim" in out


class TestResolveFiguresEndToEnd:
    def test_no_intent_returns_minimal(self):
        class DB: pass
        result = resolve_figures(
            DB(), prompt="Kürze auf 3000 Wörter.", draft_body="",
            doc_ids=["doc-1"],
        )
        assert result["intent_detected"] is False
        assert result["system_prompt_addendum"] is None

    def test_intent_without_doc_ids_still_detects(self):
        class DB: pass
        result = resolve_figures(
            DB(),
            prompt="Add 3 figures please.",
            draft_body="",
            doc_ids=[],
        )
        assert result["intent_detected"] is True
        # Empty doc_ids → hint that library is empty
        assert result["system_prompt_addendum"] is not None

    def test_returns_valid_urls_for_downstream_validation(self):
        # Stub the DB query path by shimming the model_loader to return
        # a fake image row.
        from services import figure_resolution as fr

        class _FakeRow:
            doc_id = "doc-1"
            image_id = "img-1"
            image_path = "/app/data/processed/images/doc-1/chart.png"
            alt_text = "Some chart"
            image_metadata = {"page": 7}

        class _FakeQuery:
            def __init__(self, rows): self.rows = rows
            def filter(self, *args, **kwargs): return self
            def limit(self, n): self.rows = self.rows[:n]; return self
            def all(self): return self.rows

        class _FakeDB:
            def query(self, model): return _FakeQuery([_FakeRow()])

        result = fr.resolve_figures(
            _FakeDB(),
            prompt="Include a figure about market share.",
            draft_body="",
            doc_ids=["doc-1"],
        )
        assert result["intent_detected"] is True
        urls = result["valid_image_urls"]
        assert "/api/documents/images/doc-1/chart.png" in urls
        assert result["system_prompt_addendum"] is not None
