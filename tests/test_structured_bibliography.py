"""Tests for the structured-bibliography helpers (#51/#52).

Covers the pure (non-DB) functions: slugging, fingerprinting, and
author-list normalisation. DB-bound upsert/replace behaviour is
integration-tested separately against a live Postgres.
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

# Prime the import graph (same pattern as test_simplified_writing_router)
import api as _api_primer  # noqa: F401, E402

from services.structured_bibliography import (  # noqa: E402
    _normalise_authors,
    compute_source_fingerprint,
    slugify_entry_key,
)


class TestSlugifyEntryKey:
    @pytest.mark.parametrize(
        "parts, expected",
        [
            (("Müller", "2024", "Handelspolitik"), "mueller-2024-handelspolitik"),
            (("Destatis", "2024", "Außenhandel"), "destatis-2024-aussenhandel"),
            (("BPB", "", "China in der Weltwirtschaft"), "bpb-china-in-der-weltwirtschaft"),
            (("",), "ref"),  # never empty
            (("___",), "ref"),
            (("O'Reilly", "2020"), "o-reilly-2020"),
        ],
    )
    def test_produces_ascii_slugs(self, parts, expected):
        assert slugify_entry_key(*parts) == expected

    def test_strips_leading_trailing_dashes(self):
        assert slugify_entry_key("  Müller  ", "2024") == "mueller-2024"

    def test_multiple_nonalpha_runs_collapse(self):
        assert slugify_entry_key("A---B", "C/D") == "a-b-c-d"


class TestFingerprint:
    def test_document_only(self):
        fp = compute_source_fingerprint(document_id="doc-123")
        assert fp and len(fp) == 16

    def test_document_with_pages_differs_from_without(self):
        a = compute_source_fingerprint(document_id="doc-123")
        b = compute_source_fingerprint(document_id="doc-123", pages="12-15")
        assert a != b

    def test_url_normalisation(self):
        # scheme + www + trailing slash stripped, lowercased
        a = compute_source_fingerprint(url="https://www.Destatis.de/DE/Home/")
        b = compute_source_fingerprint(url="http://destatis.de/DE/Home")
        assert a == b

    def test_none_when_neither_signal(self):
        assert compute_source_fingerprint() is None


class TestNormaliseAuthors:
    def test_none_and_empty(self):
        assert _normalise_authors(None) is None
        assert _normalise_authors([]) is None

    def test_dict_pass_through(self):
        out = _normalise_authors(
            [{"family": "Müller", "given": "Peter"}, {"family": "Schmidt", "given": "A."}]
        )
        assert out == [
            {"family": "Müller", "given": "Peter"},
            {"family": "Schmidt", "given": "A."},
        ]

    def test_string_family_given_split(self):
        out = _normalise_authors(["Müller, Peter", "Schmidt, A."])
        assert out == [
            {"family": "Müller", "given": "Peter"},
            {"family": "Schmidt", "given": "A."},
        ]

    def test_string_given_family_order(self):
        out = _normalise_authors(["Peter Müller"])
        assert out == [{"family": "Müller", "given": "Peter"}]

    def test_single_token_falls_back_to_family_only(self):
        out = _normalise_authors(["Destatis"])
        assert out == [{"family": "Destatis", "given": ""}]

    def test_missing_given_defaults_to_empty(self):
        out = _normalise_authors([{"family": "Destatis"}])
        assert out == [{"family": "Destatis", "given": ""}]
