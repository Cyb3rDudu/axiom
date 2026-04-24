"""3-marker integration test for citation_entries persistence (#63/#76).

The chat-task path at api/writing.py:1564–1625 builds an `occurrences`
list from `sync_report.resolved` and passes it to
`record_citation_occurrences`. This test pins:

1. `validate_citations` produces the expected resolved marker set
   (3 markers → 3 tuples with correct offsets + entry_keys).
2. The projection layer (`marker.char_offset_*` → `reference_id`)
   correctly maps resolved markers to their reference IDs via the
   in-memory entry_key → reference_id map.
3. `record_citation_occurrences` replaces prior rows idempotently
   (re-running with a different body leaves exactly the new rows).

Uses an SQLAlchemy session stub that captures add/delete/commit calls
rather than a real DB — the persistence helper does no JSONB work, so
a stub covers the contract without needing SQLite/PG bootstrap.
"""

from __future__ import annotations

import sys
import uuid
from pathlib import Path
from typing import Any, Iterable, List

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402

import api as _api_primer  # noqa: F401, E402

from services.citation_sync import validate_citations  # noqa: E402
from services.structured_bibliography import record_citation_occurrences  # noqa: E402
from database import models  # noqa: E402


# ---------------------------------------------------------------------------
# Minimal session stub — just enough for record_citation_occurrences
# ---------------------------------------------------------------------------


class _DeleteQuery:
    def __init__(self, session: "StubSession", model):
        self.session = session
        self.model = model
        self.filters: List[Any] = []

    def filter(self, *args):
        self.filters.extend(args)
        return self

    def delete(self, synchronize_session: bool = True) -> int:
        # Remove matching entries from the session's list. The real query
        # matches on draft_id == X; mirror that by peeking at the binary
        # comparison's right-hand value.
        before = len(self.session.citation_entries)
        # Filter is "draft_id == draft_id_value"; we extract the right side.
        target = None
        for f in self.filters:
            if hasattr(f, "right") and hasattr(f.right, "value"):
                target = f.right.value
        self.session.citation_entries = [
            e for e in self.session.citation_entries
            if target is None or e.draft_id != target
        ]
        return before - len(self.session.citation_entries)


class StubSession:
    """Captures add/commit calls for citation_entries, enough for tests."""

    def __init__(self):
        self.citation_entries: List[models.CitationEntry] = []
        self.commit_count = 0

    def query(self, model):
        return _DeleteQuery(self, model)

    def add(self, instance):
        if isinstance(instance, models.CitationEntry):
            self.citation_entries.append(instance)

    def commit(self):
        self.commit_count += 1


# ---------------------------------------------------------------------------
# Fixture: 3 refs + body with 3 markers at known offsets
# ---------------------------------------------------------------------------


DRAFT_BODY = (
    "# Einleitung\n\n"
    "China ist ein wichtiger Handelspartner (Destatis, 2024, o. S.). "
    "Das zeigt die Statistik.\n\n"
    "## Theorie\n\n"
    "Nach Heckscher-Ohlin (Müller, 2020, S. 12) gilt...\n\n"
    "## Empirie\n\n"
    "Weitere Daten (Smith, 2022, p. 45)."
)

REGISTRY = [
    {
        "entry_key": "destatis-2024",
        "authors": [{"family": "Destatis", "given": ""}],
        "year": 2024,
        "title": "Außenhandel 2024",
    },
    {
        "entry_key": "mueller-2020",
        "authors": [{"family": "Müller", "given": "P."}],
        "year": 2020,
        "title": "Makroökonomie",
    },
    {
        "entry_key": "smith-2022",
        "authors": [{"family": "Smith", "given": "J."}],
        "year": 2022,
        "title": "Trade Policy",
    },
]

# Reference id map built as if the chat-task loaded rows from DB
REF_ID_BY_KEY = {
    "destatis-2024": "ref-id-destatis",
    "mueller-2020":  "ref-id-mueller",
    "smith-2022":    "ref-id-smith",
}


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


class TestResolvedMarkersProjection:
    """End-to-end: validator → occurrences dict → record_citation_occurrences.
    The chat-task path lives in api/writing.py; this test exercises the same
    sequence to pin the contract.
    """

    def test_three_markers_resolve_to_three_refs(self):
        report = validate_citations(DRAFT_BODY, REGISTRY)
        assert len(report.resolved) == 3
        # All three markers are APA-style
        modes = {m.mode for m, _ in report.resolved}
        assert modes == {"apa"}
        # Each resolved to the expected entry_key
        resolved_keys = [k for _, k in report.resolved]
        assert sorted(resolved_keys) == ["destatis-2024", "mueller-2020", "smith-2022"]

    def test_offsets_point_at_the_markers_in_body(self):
        report = validate_citations(DRAFT_BODY, REGISTRY)
        for marker, _key in report.resolved:
            # The substring at the offsets round-trips to the marker text
            assert DRAFT_BODY[marker.char_offset_start:marker.char_offset_end] == marker.marker

    def test_paragraph_indices_reflect_body_structure(self):
        report = validate_citations(DRAFT_BODY, REGISTRY)
        by_key = {k: m for m, k in report.resolved}
        # Body has 4 "paragraphs" separated by blank lines:
        # 0: # Einleitung
        # 1: "China ist..." (contains Destatis)
        # 2: ## Theorie
        # 3: "Nach Heckscher-Ohlin..." (contains Müller)
        # 4: ## Empirie
        # 5: "Weitere Daten..." (contains Smith)
        assert by_key["destatis-2024"].paragraph_index < by_key["mueller-2020"].paragraph_index
        assert by_key["mueller-2020"].paragraph_index < by_key["smith-2022"].paragraph_index

    def test_occurrences_projection_shape(self):
        """Mirror the exact projection the chat-task does."""
        report = validate_citations(DRAFT_BODY, REGISTRY)
        occurrences = []
        for marker, entry_key in report.resolved:
            ref_id = REF_ID_BY_KEY.get(entry_key)
            if ref_id is None:
                continue
            occurrences.append({
                "reference_id": ref_id,
                "in_text_marker": marker.marker,
                "paragraph_index": marker.paragraph_index,
                "char_offset_start": marker.char_offset_start,
                "char_offset_end": marker.char_offset_end,
            })

        assert len(occurrences) == 3
        for occ in occurrences:
            assert occ["reference_id"] in REF_ID_BY_KEY.values()
            assert occ["in_text_marker"].startswith("(")
            assert occ["char_offset_start"] >= 0
            assert occ["char_offset_end"] > occ["char_offset_start"]


class TestRecordCitationOccurrences:
    def test_persists_exactly_three_rows(self):
        report = validate_citations(DRAFT_BODY, REGISTRY)
        occurrences = [
            {
                "reference_id": REF_ID_BY_KEY[k],
                "in_text_marker": m.marker,
                "paragraph_index": m.paragraph_index,
                "char_offset_start": m.char_offset_start,
                "char_offset_end": m.char_offset_end,
            }
            for m, k in report.resolved
        ]
        db = StubSession()
        rows = record_citation_occurrences(db, "draft-abc", occurrences)
        assert len(rows) == 3
        assert len(db.citation_entries) == 3
        assert all(r.draft_id == "draft-abc" for r in db.citation_entries)
        # Offsets survived the write
        offsets = sorted((r.char_offset_start, r.char_offset_end) for r in db.citation_entries)
        expected = sorted((m.char_offset_start, m.char_offset_end) for m, _ in report.resolved)
        assert offsets == expected

    def test_idempotent_replace_on_second_call(self):
        """Re-running with a different body leaves exactly the NEW rows."""
        db = StubSession()

        # First write: 3 rows for the original body
        first_report = validate_citations(DRAFT_BODY, REGISTRY)
        record_citation_occurrences(
            db,
            "draft-abc",
            [
                {
                    "reference_id": REF_ID_BY_KEY[k],
                    "in_text_marker": m.marker,
                    "paragraph_index": m.paragraph_index,
                    "char_offset_start": m.char_offset_start,
                    "char_offset_end": m.char_offset_end,
                }
                for m, k in first_report.resolved
            ],
        )
        assert len(db.citation_entries) == 3

        # Second write: a shorter body with only ONE marker
        short_body = "Only (Müller, 2020, S. 5) here."
        second_report = validate_citations(short_body, REGISTRY)
        record_citation_occurrences(
            db,
            "draft-abc",
            [
                {
                    "reference_id": REF_ID_BY_KEY[k],
                    "in_text_marker": m.marker,
                    "paragraph_index": m.paragraph_index,
                    "char_offset_start": m.char_offset_start,
                    "char_offset_end": m.char_offset_end,
                }
                for m, k in second_report.resolved
            ],
        )
        # Stale rows from the first write got purged; only the new one stays.
        assert len(db.citation_entries) == 1
        assert db.citation_entries[0].reference_id == REF_ID_BY_KEY["mueller-2020"]

    def test_empty_occurrences_purges_draft_rows(self):
        db = StubSession()
        first_report = validate_citations(DRAFT_BODY, REGISTRY)
        record_citation_occurrences(
            db,
            "draft-abc",
            [
                {
                    "reference_id": REF_ID_BY_KEY[k],
                    "in_text_marker": m.marker,
                    "paragraph_index": m.paragraph_index,
                    "char_offset_start": m.char_offset_start,
                    "char_offset_end": m.char_offset_end,
                }
                for m, k in first_report.resolved
            ],
        )
        assert len(db.citation_entries) == 3
        # Draft body later removes every citation — call with empty list
        record_citation_occurrences(db, "draft-abc", [])
        assert db.citation_entries == []

    def test_multi_draft_isolation(self):
        """Second draft's rows don't clobber the first's on replace."""
        db = StubSession()
        first_report = validate_citations(DRAFT_BODY, REGISTRY)
        first_occs = [
            {
                "reference_id": REF_ID_BY_KEY[k],
                "in_text_marker": m.marker,
                "paragraph_index": m.paragraph_index,
                "char_offset_start": m.char_offset_start,
                "char_offset_end": m.char_offset_end,
            }
            for m, k in first_report.resolved
        ]
        record_citation_occurrences(db, "draft-A", first_occs)
        record_citation_occurrences(db, "draft-B", first_occs)
        assert len(db.citation_entries) == 6

        # Replace draft-A with zero rows: draft-B survives intact
        record_citation_occurrences(db, "draft-A", [])
        assert len(db.citation_entries) == 3
        assert all(r.draft_id == "draft-B" for r in db.citation_entries)
