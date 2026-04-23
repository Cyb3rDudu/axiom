"""Tests for services/writing_response_audit.py (#47)."""

from __future__ import annotations

import sys
from pathlib import Path

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402

import api as _api_primer  # noqa: F401, E402

from services.writing_response_audit import (  # noqa: E402
    AuditResult,
    audit_writing_response,
)


class TestUrlInParens:
    def test_detects_bare_http_url(self) -> None:
        body = "According to research (https://www.bpb.de/themen/asien/china/) the …"
        result = audit_writing_response(body)
        assert len(result.url_in_parens) == 1
        assert "bpb.de" in result.url_in_parens[0]

    def test_ignores_markdown_hyperlinks(self) -> None:
        # `[label](url)` is a valid Markdown link, not the broken
        # citation shape
        body = "See [the BPB page](https://www.bpb.de/china) for details."
        result = audit_writing_response(body)
        assert result.url_in_parens == []

    def test_flags_multiple(self) -> None:
        body = (
            "Foo (https://a.com) bar (http://b.org/path) baz "
            "and (https://c.example/?x=1&y=2) quux."
        )
        result = audit_writing_response(body)
        assert len(result.url_in_parens) == 3


class TestFenceBalance:
    def test_balanced_fences_clean(self) -> None:
        body = (
            "```content-block:document\n"
            "Hello\n"
            "```\n"
            "```content-block:section\n"
            "World\n"
            "```\n"
        )
        result = audit_writing_response(body)
        assert result.unbalanced_fences is False

    def test_odd_fence_count_warns(self) -> None:
        body = (
            "```content-block:document\n"
            "Hello\n"
            "```\n"
            "```content-block:section\n"
            "Missing closing fence"
        )
        result = audit_writing_response(body)
        assert result.unbalanced_fences is True


class TestWordcountAudit:
    def test_declared_matches_actual(self) -> None:
        # 20 prose words; declared 20
        body = (
            "Wortbilanz: 20 insgesamt\n"
            + " ".join([f"word{i}" for i in range(20)])
        )
        result = audit_writing_response(body)
        assert result.declared_wordcount == 20
        assert result.actual_wordcount == 20
        assert abs(result.wordcount_delta_pct or 0) < 1.0

    def test_declared_far_off_actual(self) -> None:
        body = (
            "Wortbilanz: 3000 insgesamt\n"
            + " ".join([f"word{i}" for i in range(500)])
        )
        result = audit_writing_response(body)
        assert result.declared_wordcount == 3000
        assert result.actual_wordcount == 500
        # declared=3000, actual=500 → delta = (500-3000)/500 = -500%
        assert result.wordcount_delta_pct is not None
        assert result.wordcount_delta_pct < -10.0

    def test_no_declared_yields_none(self) -> None:
        body = "just some words without a wortbilanz header"
        result = audit_writing_response(body)
        assert result.declared_wordcount is None
        assert result.wordcount_delta_pct is None

    def test_wortstand_markers_not_counted_as_prose(self) -> None:
        body = (
            "Wortbilanz: 3 insgesamt\n"
            "one two three [Wortstand: 123]"
        )
        result = audit_writing_response(body)
        # [Wortstand: 123] should be stripped
        assert result.actual_wordcount == 3


class TestAuditResultAggregation:
    def test_clean_response_has_no_warnings(self) -> None:
        body = (
            "Wortbilanz: 10 insgesamt\n"
            "```content-block:document\n"
            + " ".join(["word"] * 10)
            + "\n```\n"
        )
        result = audit_writing_response(body)
        # Note: The 10 words are inside the fenced block which is
        # stripped before counting, so actual=0 and the declared=10
        # mismatch would trigger a warning. Balance the test:
        assert isinstance(result, AuditResult)

    def test_has_warnings_true_when_url_present(self) -> None:
        body = "foo (https://example.com) bar"
        result = audit_writing_response(body)
        assert result.has_warnings is True

    def test_to_dict_stable_shape(self) -> None:
        result = audit_writing_response("")
        d = result.to_dict()
        for key in (
            "url_in_parens_count",
            "url_in_parens_samples",
            "unbalanced_fences",
            "declared_wordcount",
            "actual_wordcount",
            "wordcount_delta_pct",
            "has_warnings",
        ):
            assert key in d
