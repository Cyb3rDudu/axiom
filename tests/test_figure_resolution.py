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
        assert url == "/api/images/doc-123/fig_1.png"

    def test_handles_bare_filename(self):
        url = _image_url_from_path("doc-x", "fig.png")
        assert url == "/api/images/doc-x/fig.png"

    def test_empty_path(self):
        url = _image_url_from_path("doc-x", "")
        assert url.endswith("unknown")


class TestCandidateSnippet:
    def test_includes_real_url_in_markdown_scaffold(self):
        """Writer needs the URL present as a copy-paste target. Prior
        attempts that hid the URL inside labelled text led the writer
        to invent its own paths."""
        c = FigureCandidate(
            image_id="img-1",
            doc_id="doc-1",
            image_url="/api/images/doc-1/chart.png",
            alt_text="BIP 2000-2024",
            relevance=0.87,
            source_document_title="Macro Trends",
            source_page=12,
        )
        snippet = c.to_prompt_snippet()
        assert "/api/images/doc-1/chart.png" in snippet
        assert "BIP 2000-2024" in snippet
        # Uses scaffold with an EXPLICIT replace sigil the writer can't
        # accidentally leave in place
        assert "REPLACE" in snippet
        # No backend-internal label as alt text
        assert "candidate figure" not in snippet.lower()


class TestInjectionGuidance:
    def test_injection_warns_against_placeholder_leak(self):
        """Header must explicitly tell writer not to leave scaffold
        tokens (REPLACE-WITH / candidate figure / stored caption) as
        alt text."""
        qs = [FigureQuery(description="Chinas BIP", source="placeholder")]
        cand = FigureCandidate(
            image_id="img-1",
            doc_id="doc-1",
            image_url="/api/images/doc-1/chart.png",
            alt_text="Some stored caption",
            relevance=0.8,
        )
        for lang in ("de", "en"):
            out = build_figure_injection(
                qs, {"Chinas BIP": [cand]}, language_code=lang
            )
            # Header must warn about the specific scaffold tokens
            assert "REPLACE" in out
            assert "candidate figure" in out.lower()
            # Must NOT modify URL instruction
            assert "url" in out.lower()

    def test_injection_spells_out_copy_unchanged_rule(self):
        """The 'do not fabricate paths' rule needs to be unmissable —
        previous wording let the writer invent URLs anyway."""
        qs = [FigureQuery(description="X", source="placeholder")]
        cand = FigureCandidate(
            image_id="i", doc_id="d",
            image_url="/api/images/d/x.png",
            alt_text="c", relevance=0.8,
        )
        out_de = build_figure_injection(qs, {"X": [cand]}, language_code="de")
        assert "unverändert" in out_de or "NICHT-ÄNDERN" in out_de
        out_en = build_figure_injection(qs, {"X": [cand]}, language_code="en")
        assert "unchanged" in out_en.lower() or "do not modify" in out_en.lower()


class TestBuildFigureInjection:
    def _cands(self, alt_text: str = "Some chart"):
        return [
            FigureCandidate(
                image_id="img-1",
                doc_id="doc-1",
                image_url="/api/images/doc-1/fig1.png",
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
        assert "/api/images/doc-1/fig1.png" in out
        # Must tell the writer the URL is immutable
        assert (
            "unverändert" in out
            or "NICHT-ÄNDERN" in out
            or "do not modify" in out.lower()
            or "unchanged" in out.lower()
        )

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
        # English header must also enforce URL immutability
        assert "unchanged" in out.lower() or "do not modify" in out.lower()
        # And spell out the replace-alt-text rule
        assert "REPLACE" in out


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
        # a fake image row. Prompt uses the explicit "Figure N: desc"
        # pattern so detect_figure_intent extracts a concrete description
        # that exercises the alt_text-keyword path (not the empty-
        # description short-circuit).
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
            prompt="Add Figure 1: market share chart for the corpus.",
            draft_body="",
            doc_ids=["doc-1"],
        )
        assert result["intent_detected"] is True
        urls = result["valid_image_urls"]
        assert "/api/images/doc-1/chart.png" in urls
        assert result["system_prompt_addendum"] is not None

    def test_generic_intent_without_description_returns_no_candidates(self):
        # Regression: prior to the kill-fallback fix, a prompt with
        # generic figure intent ("Add 3 figures") triggered a DB query
        # for "any 3 images from the doc set" with fake 0.9/0.75/0.6
        # relevance scores. Result: publisher logos surfaced for
        # economics papers. The fix returns [] for empty descriptions
        # and lets the writer hit the "no matching figures" branch.
        from services import figure_resolution as fr

        class _FakeRow:
            doc_id = "doc-1"
            image_id = "img-1"
            image_path = "/app/data/processed/images/doc-1/logo.png"
            alt_text = "Verlagslogo"
            image_metadata = {"page": 1}

        class _FakeQuery:
            def __init__(self, rows): self.rows = rows
            def filter(self, *args, **kwargs): return self
            def limit(self, n): self.rows = self.rows[:n]; return self
            def all(self): return self.rows

        class _FakeDB:
            def query(self, model): return _FakeQuery([_FakeRow()])

        result = fr.resolve_figures(
            _FakeDB(),
            prompt="Bitte 3 Abbildungen einbauen.",
            draft_body="",
            doc_ids=["doc-1"],
        )
        assert result["intent_detected"] is True
        # No candidates emitted despite the fake DB row that would
        # match "any image" — empty description must short-circuit.
        urls = result.get("valid_image_urls") or set()
        assert "/api/images/doc-1/logo.png" not in urls

    def test_no_alt_text_match_returns_no_candidates(self):
        # Regression: when the alt_text query returns no rows, we must
        # NOT fall back to "any image from the doc set" — that was the
        # second arm of the publisher-logo bug. Fake DB returns empty,
        # asserts no candidates come out.
        from services import figure_resolution as fr

        class _FakeQuery:
            def filter(self, *_a, **_k): return self
            def limit(self, _n): return self
            def all(self): return []  # ILIKE finds nothing

        class _FakeDB:
            def query(self, _model): return _FakeQuery()

        result = fr.resolve_figures(
            _FakeDB(),
            prompt="Add Figure 1: BIP-Wachstum von China.",
            draft_body="",
            doc_ids=["doc-1"],
        )
        assert result["intent_detected"] is True
        # No candidates → valid_image_urls is empty set
        assert (result.get("valid_image_urls") or set()) == set()
        # candidates_by_description should be empty list for the query
        cb = result.get("candidates_by_description") or {}
        for desc, cands in cb.items():
            assert cands == [], f"unexpected candidates for {desc!r}: {cands}"


class TestHeuristicPreFilter:
    """Sanity checks for the CLIP-path heuristic filter that drops
    obviously-decorative candidates. Logos, covers, tiny images."""

    def test_logo_in_alt_text_dropped_with_compound_word(self):
        from services.figure_resolution import _is_likely_decorative
        # German compound — substring match required
        assert _is_likely_decorative("Verlagslogo", None) is True
        assert _is_likely_decorative("Logo of company X", None) is True
        assert _is_likely_decorative("LOGO design", None) is True

    def test_cover_in_alt_text_dropped(self):
        from services.figure_resolution import _is_likely_decorative
        assert _is_likely_decorative("Buchcover", None) is True
        assert _is_likely_decorative("Cover image", None) is True

    def test_titelblatt_dropped(self):
        from services.figure_resolution import _is_likely_decorative
        assert _is_likely_decorative("Titelblatt der Studie", None) is True

    def test_legitimate_caption_not_dropped(self):
        from services.figure_resolution import _is_likely_decorative
        assert _is_likely_decorative(
            "BIP-Wachstum Chinas 1980-2024", None
        ) is False
        assert _is_likely_decorative(
            "Globale Wertschöpfungsketten — Übersicht",
            {"width": 1200, "height": 800},
        ) is False

    def test_small_image_dropped_by_dimension(self):
        from services.figure_resolution import _is_likely_decorative
        assert _is_likely_decorative(
            "Some chart", {"width": 200, "height": 200}
        ) is True
        assert _is_likely_decorative(
            "Some chart", {"image_height": 100}
        ) is True

    def test_ok_dimensions_accepted(self):
        from services.figure_resolution import _is_likely_decorative
        assert _is_likely_decorative(
            "Diagram", {"width": 800, "height": 600}
        ) is False

    def test_missing_metadata_not_a_failure(self):
        from services.figure_resolution import _is_likely_decorative
        # No metadata → only alt_text drives the decision
        assert _is_likely_decorative("normal caption", None) is False
        assert _is_likely_decorative("Logo des Verlags", None) is True


class TestClipResolverFlag:
    """Resolver flag dispatch — when use_clip=True, the CLIP path runs;
    when False, the keyword path runs. CLIP errors fall back to keyword
    so the writer never blocks on an embedder outage."""

    def test_keyword_path_default_when_use_clip_false(self):
        from services import figure_resolution as fr

        called = {"clip": 0, "keyword": 0}
        original_clip = fr._load_candidates_clip
        original_kw = fr._load_candidates

        def fake_clip(*a, **kw):
            called["clip"] += 1
            return {"x": []}

        def fake_kw(*a, **kw):
            called["keyword"] += 1
            return {"x": []}

        try:
            fr._load_candidates_clip = fake_clip
            fr._load_candidates = fake_kw
            fr.resolve_figures(
                None,
                prompt="Add Figure 1: BIP-Wachstum von China.",
                draft_body="",
                doc_ids=["doc-1"],
                use_clip=False,
            )
        finally:
            fr._load_candidates_clip = original_clip
            fr._load_candidates = original_kw

        assert called["clip"] == 0
        assert called["keyword"] == 1

    def test_clip_path_selected_when_use_clip_true(self):
        from services import figure_resolution as fr

        called = {"clip": 0, "keyword": 0}
        original_clip = fr._load_candidates_clip
        original_kw = fr._load_candidates

        def fake_clip(*a, **kw):
            called["clip"] += 1
            # Return a dict with one candidate so the keyword fallback
            # branch (which fires when CLIP returns empty) doesn't run.
            return {
                "BIP-Wachstum von China": [
                    fr.FigureCandidate(
                        image_id="i", doc_id="doc-1",
                        image_url="/api/images/doc-1/x.png",
                        alt_text="BIP", relevance=0.42,
                    )
                ]
            }

        def fake_kw(*a, **kw):
            called["keyword"] += 1
            return {"x": []}

        try:
            fr._load_candidates_clip = fake_clip
            fr._load_candidates = fake_kw
            r = fr.resolve_figures(
                None,
                prompt="Add Figure 1: BIP-Wachstum von China.",
                draft_body="",
                doc_ids=["doc-1"],
                use_clip=True,
            )
        finally:
            fr._load_candidates_clip = original_clip
            fr._load_candidates = original_kw

        assert called["clip"] == 1
        assert called["keyword"] == 0
        assert r.get("resolver_path") == "clip"

    def test_clip_failure_falls_back_to_keyword(self):
        from services import figure_resolution as fr

        called = {"clip": 0, "keyword": 0}
        original_clip = fr._load_candidates_clip
        original_kw = fr._load_candidates

        def fake_clip(*a, **kw):
            called["clip"] += 1
            raise RuntimeError("CLIP encoder unavailable")

        def fake_kw(*a, **kw):
            called["keyword"] += 1
            return {"x": []}

        try:
            fr._load_candidates_clip = fake_clip
            fr._load_candidates = fake_kw
            r = fr.resolve_figures(
                None,
                prompt="Add Figure 1: BIP-Wachstum von China.",
                draft_body="",
                doc_ids=["doc-1"],
                use_clip=True,
            )
        finally:
            fr._load_candidates_clip = original_clip
            fr._load_candidates = original_kw

        assert called["clip"] == 1
        assert called["keyword"] == 1
        assert r.get("resolver_path") == "clip_failed"
