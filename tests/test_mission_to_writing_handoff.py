"""Tests for the mission → writing handoff projector (#73)."""

from __future__ import annotations

import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Any, List

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402

import api as _api_primer  # noqa: F401, E402

from services import mission_to_writing_handoff as handoff  # noqa: E402
from database import models  # noqa: E402


# ---------------------------------------------------------------------------
# _parse_authors_string
# ---------------------------------------------------------------------------


class TestParseAuthors:
    def test_semicolon_separated(self):
        out = handoff._parse_authors_string("Müller, Peter; Schmidt, Anna")
        assert out == [
            {"family": "Müller", "given": "Peter"},
            {"family": "Schmidt", "given": "Anna"},
        ]

    def test_and_separated(self):
        out = handoff._parse_authors_string("Peter Müller and Anna Schmidt")
        assert out == [
            {"family": "Müller", "given": "Peter"},
            {"family": "Schmidt", "given": "Anna"},
        ]

    def test_ampersand_separated(self):
        out = handoff._parse_authors_string("Smith, J. & Jones, A.")
        assert out == [
            {"family": "Smith", "given": "J."},
            {"family": "Jones", "given": "A."},
        ]

    def test_institutional_author(self):
        assert handoff._parse_authors_string("Destatis") == [
            {"family": "Destatis", "given": ""}
        ]

    def test_list_of_dicts_passes_through(self):
        pre = [{"family": "X", "given": "Y"}, {"family": "Z"}]
        out = handoff._parse_authors_string(pre)
        assert out == [
            {"family": "X", "given": "Y"},
            {"family": "Z", "given": ""},
        ]

    def test_empty(self):
        assert handoff._parse_authors_string(None) == []
        assert handoff._parse_authors_string("") == []


# ---------------------------------------------------------------------------
# _coerce_year
# ---------------------------------------------------------------------------


class TestCoerceYear:
    @pytest.mark.parametrize(
        "raw,expected",
        [
            (2024, 2024),
            ("2024", 2024),
            ("2024-01-01", 2024),
            ("n.d.", None),
            ("o. J.", None),
            ("", None),
            (None, None),
            ("garbage", None),
        ],
    )
    def test_cases(self, raw, expected):
        assert handoff._coerce_year(raw) == expected


# ---------------------------------------------------------------------------
# project_mission_into_draft — uses a session stub that mirrors what the
# real path does: db.query(Mission).filter().first(), db.add(Reference),
# db.commit(). Enough to pin the projection semantics without a real DB.
# ---------------------------------------------------------------------------


class _Query:
    def __init__(self, stub, model):
        self.stub = stub
        self.model = model
        self.filters: List[Any] = []

    def filter(self, *args):
        self.filters.extend(args)
        return self

    def first(self):
        if self.model is models.Mission:
            return self.stub.mission
        if self.model is models.Reference or self.model.__name__ == "Reference":
            return None
        return None

    def all(self):
        # Existing-entry-key query in the projector
        return []


class _QueryOnColumn(_Query):
    """Matches `db.query(models.Reference.entry_key).filter(...).all()`
    returning empty — no pre-existing refs in the stubbed draft."""
    def all(self):
        return []


class Stub:
    def __init__(self, mission):
        self.mission = mission
        self.added_refs: List[models.Reference] = []
        self.commit_count = 0
        self.draft = SimpleNamespace(
            id="draft-1",
            portfolio_output=None,
        )

    def query(self, model):
        # The projector calls db.query(models.Reference.entry_key) which
        # returns a Column, not a model. Treat any Reference-ish arg as
        # the existing-entry-key query.
        if model is models.Mission:
            return _Query(self, models.Mission)
        return _QueryOnColumn(self, model)

    def add(self, instance):
        if isinstance(instance, models.Reference):
            self.added_refs.append(instance)

    def commit(self):
        self.commit_count += 1

    def refresh(self, obj):
        pass


MISSION_CONTEXT = {
    "notes": [
        {
            "source_id": "doc-aaa",
            "source_type": "document",
            "source_metadata": {
                "authors": "Müller, Peter",
                "publication_year": 2024,
                "title": "China in der Weltwirtschaft",
                "publisher": "Vahlen",
                "pages": "23-45",
            },
        },
        {
            "source_id": "web-bbb",
            "source_type": "web",
            "source_metadata": {
                "authors": "Destatis",
                "year": 2024,
                "title": "Außenhandel 2024",
                "url": "https://www.destatis.de/DE/Home/",
            },
        },
        {
            "source_id": "doc-ccc",
            "source_type": "document",
            "source_metadata": {
                "authors": "Smith, J.; Jones, A.",
                "publication_year": 2020,
                "title": "Trade policy",
                "journal": "JIE",
                "pages": "1-10",
                "doi": "10.1/abc",
            },
        },
    ],
    "used_doc_ids": ["doc-aaa", "web-bbb", "doc-ccc"],
}


PORTFOLIO = {
    "mission_id": "m-1",
    "language_code": "de",
    "generated_at": "2026-04-24T00:00:00",
    "entries": [],
    "compliance": {
        "source_count": 3,
        "source_count_ok": False,
        "scientific_share": 1.0,
        "scientific_share_ok": True,
        "blacklist_hits": [],
        "recency_warnings": [],
        "traffic_light": "yellow",
        "advice": [],
    },
    "markdown_table": "## Literaturportfolio\n\n| … |",
}


def _mission(**overrides):
    base = SimpleNamespace(
        id="m-1",
        mission_context=dict(MISSION_CONTEXT),
        literature_portfolio_output=dict(PORTFOLIO),
    )
    for k, v in overrides.items():
        setattr(base, k, v)
    return base


class TestProjectMissionIntoDraft:
    def test_projects_three_refs_and_copies_portfolio(self):
        stub = Stub(_mission())
        result = handoff.project_mission_into_draft(
            stub,
            mission_id="m-1",
            draft=stub.draft,
            user_id=4,
        )
        assert result["refs_created"] == 3
        assert result["refs_skipped"] == 0
        assert result["portfolio_copied"] is True
        assert stub.draft.portfolio_output == PORTFOLIO
        # References carry structured fields
        by_title = {r.title: r for r in stub.added_refs}
        mueller = by_title["China in der Weltwirtschaft"]
        assert mueller.authors == [{"family": "Müller", "given": "Peter"}]
        assert mueller.year == 2024
        assert mueller.publisher == "Vahlen"
        assert mueller.document_id == "doc-aaa"
        assert mueller.reference_type == "document"
        assert mueller.entry_key  # slug populated
        destatis = by_title["Außenhandel 2024"]
        assert destatis.reference_type == "web"
        assert destatis.web_url == "https://www.destatis.de/DE/Home/"
        smith = by_title["Trade policy"]
        assert smith.container_title == "JIE"
        assert smith.doi == "10.1/abc"
        assert len(smith.authors) == 2

    def test_skip_notes_without_title(self):
        mission = _mission()
        mission.mission_context = {
            "notes": [
                {"source_id": "x", "source_type": "document",
                 "source_metadata": {"authors": "X"}},  # no title
                {"source_id": "y", "source_type": "document",
                 "source_metadata": {"authors": "Y", "title": "Real one"}},
            ],
            "used_doc_ids": ["x", "y"],
        }
        stub = Stub(mission)
        result = handoff.project_mission_into_draft(
            stub,
            mission_id="m-1",
            draft=stub.draft,
            user_id=4,
        )
        assert result["refs_created"] == 1
        assert result["refs_skipped"] == 1

    def test_respects_used_doc_ids_when_present(self):
        mission = _mission()
        mission.mission_context = dict(MISSION_CONTEXT)
        # Only accept one of three
        mission.mission_context["used_doc_ids"] = ["doc-aaa"]
        stub = Stub(mission)
        result = handoff.project_mission_into_draft(
            stub,
            mission_id="m-1",
            draft=stub.draft,
            user_id=4,
        )
        assert result["refs_created"] == 1
        assert stub.added_refs[0].document_id == "doc-aaa"

    def test_no_used_doc_ids_projects_all(self):
        mission = _mission()
        mission.mission_context = dict(MISSION_CONTEXT)
        mission.mission_context.pop("used_doc_ids", None)
        stub = Stub(mission)
        result = handoff.project_mission_into_draft(
            stub,
            mission_id="m-1",
            draft=stub.draft,
            user_id=4,
        )
        assert result["refs_created"] == 3

    def test_missing_mission_is_noop(self):
        stub = Stub(None)
        result = handoff.project_mission_into_draft(
            stub,
            mission_id="missing",
            draft=stub.draft,
            user_id=4,
        )
        assert result == {"refs_created": 0, "refs_skipped": 0, "portfolio_copied": False}

    def test_mission_without_portfolio_still_projects_refs(self):
        mission = _mission(literature_portfolio_output=None)
        stub = Stub(mission)
        result = handoff.project_mission_into_draft(
            stub,
            mission_id="m-1",
            draft=stub.draft,
            user_id=4,
        )
        assert result["refs_created"] == 3
        assert result["portfolio_copied"] is False
        assert stub.draft.portfolio_output is None

    def test_mission_without_notes_copies_portfolio_only(self):
        mission = _mission()
        mission.mission_context = {}
        stub = Stub(mission)
        result = handoff.project_mission_into_draft(
            stub,
            mission_id="m-1",
            draft=stub.draft,
            user_id=4,
        )
        assert result["refs_created"] == 0
        assert result["portfolio_copied"] is True
