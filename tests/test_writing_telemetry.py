"""Tests for the writing-subsystem telemetry helpers (#74).

No metrics backend is wired in axiom today; telemetry rides on the
stdlib logger via structured `extra` dicts. These tests pin the event
shape so downstream log shippers can rely on a stable schema.
"""

from __future__ import annotations

import logging
import sys
from pathlib import Path

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402

import api as _api_primer  # noqa: F401, E402

from services import writing_telemetry  # noqa: E402


@pytest.fixture
def log_records(caplog):
    caplog.set_level(logging.INFO, logger="axiom.writing_telemetry")
    yield caplog


def _extras(caplog) -> list[dict]:
    """Flatten the records into their structured `extra` fields."""
    rows = []
    for r in caplog.records:
        rows.append(
            {
                "message": r.getMessage(),
                **{k: v for k, v in r.__dict__.items() if k.startswith(("subsystem", "metric", "trigger", "outcome", "traffic_light", "draft_id", "user_id", "source_count", "result", "entries_count", "errors_count", "resolved_count", "orphan_count", "dead_count", "ambiguous_count", "orphans_bucket", "dead_bucket", "bibliography_source", "portfolio_source", "markdown_size", "flag_env", "flag_user", "flag_resolved", "flag_name", "session_id"))},
            }
        )
    return rows


class TestFlagState:
    def test_logs_triple_and_subsystem(self, log_records, monkeypatch):
        monkeypatch.delenv("WRITING_STRUCTURED_BIBLIOGRAPHY_ENABLED", raising=False)
        writing_telemetry.log_flag_state(
            subsystem="writing_chat",
            user_settings={"writing_settings": {"structured_bibliography_enabled": True}},
            draft_id="d-1",
            user_id=4,
            session_id="s-1",
        )
        rows = _extras(log_records)
        assert len(rows) == 1
        r = rows[0]
        assert r["subsystem"] == "writing_chat"
        assert r["metric"] == "writing_flag_state"
        assert r["flag_resolved"] is True
        assert r["flag_user"] is True
        assert r["draft_id"] == "d-1"
        assert r["user_id"] == 4

    def test_flag_off_by_default(self, log_records, monkeypatch):
        monkeypatch.delenv("WRITING_STRUCTURED_BIBLIOGRAPHY_ENABLED", raising=False)
        writing_telemetry.log_flag_state(
            subsystem="writing_chat",
            user_settings=None,
            draft_id="d-1",
            user_id=4,
        )
        rows = _extras(log_records)
        assert rows[0]["flag_resolved"] is False


class TestPortfolioGeneration:
    def test_generated_event(self, log_records):
        writing_telemetry.record_portfolio_generation(
            trigger="manual",
            outcome="generated",
            traffic_light="green",
            draft_id="d-1",
            user_id=4,
            source_count=12,
        )
        r = _extras(log_records)[0]
        assert r["metric"] == "writing_portfolio_generations_total"
        assert r["trigger"] == "manual"
        assert r["outcome"] == "generated"
        assert r["traffic_light"] == "green"
        assert r["source_count"] == 12

    @pytest.mark.parametrize(
        "outcome",
        ["skipped_flag", "skipped_optout", "skipped_empty", "error"],
    )
    def test_skip_outcomes_traffic_light_is_none(self, log_records, outcome):
        writing_telemetry.record_portfolio_generation(
            trigger="session_close",
            outcome=outcome,  # type: ignore[arg-type]
            draft_id="d-1",
        )
        r = _extras(log_records)[0]
        assert r["outcome"] == outcome
        assert r["traffic_light"] is None


class TestBibliographyParse:
    @pytest.mark.parametrize(
        "result,entries,errors",
        [
            ("no_block", 0, 0),
            ("parsed", 5, 0),
            ("malformed", 3, 2),
            ("empty_valid", 0, 0),
        ],
    )
    def test_records_result_and_counts(self, log_records, result, entries, errors):
        writing_telemetry.record_bibliography_parse(
            result=result,  # type: ignore[arg-type]
            entries_count=entries,
            errors_count=errors,
            draft_id="d-1",
        )
        r = _extras(log_records)[0]
        assert r["metric"] == "structured_bibliography_parse_total"
        assert r["result"] == result
        assert r["entries_count"] == entries
        assert r["errors_count"] == errors
        assert r["subsystem"] == "writing_bibliography"


class TestSyncReport:
    @pytest.mark.parametrize(
        "orphans,dead,orphans_bucket,dead_bucket",
        [
            (0, 0, "0", "0"),
            (1, 2, "1-2", "1-2"),
            (3, 4, "3+", "3+"),
            (10, 0, "3+", "0"),
        ],
    )
    def test_bucketing(self, log_records, orphans, dead, orphans_bucket, dead_bucket):
        writing_telemetry.record_sync_report(
            resolved_count=5,
            orphan_count=orphans,
            dead_count=dead,
            draft_id="d-1",
        )
        r = _extras(log_records)[0]
        assert r["metric"] == "citation_sync_report_total"
        assert r["orphans_bucket"] == orphans_bucket
        assert r["dead_bucket"] == dead_bucket

    def test_ambiguous_count_included(self, log_records):
        writing_telemetry.record_sync_report(
            resolved_count=3,
            orphan_count=0,
            dead_count=0,
            ambiguous_count=2,
            draft_id="d-1",
        )
        r = _extras(log_records)[0]
        assert r["ambiguous_count"] == 2


class TestDocxExport:
    def test_structured_both(self, log_records):
        writing_telemetry.record_docx_export(
            bibliography_source="structured",
            portfolio_source="structured",
            draft_id="d-1",
            user_id=4,
            markdown_size=12345,
        )
        r = _extras(log_records)[0]
        assert r["metric"] == "docx_export_total"
        assert r["bibliography_source"] == "structured"
        assert r["portfolio_source"] == "structured"
        assert r["markdown_size"] == 12345
        assert r["subsystem"] == "writing_export"

    def test_none_sources(self, log_records):
        writing_telemetry.record_docx_export(
            bibliography_source="none",
            portfolio_source="none",
        )
        r = _extras(log_records)[0]
        assert r["bibliography_source"] == "none"
        assert r["portfolio_source"] == "none"


class TestBucketHelper:
    def test_zero(self):
        assert writing_telemetry._bucket_count(0) == "0"

    def test_one_and_two(self):
        assert writing_telemetry._bucket_count(1) == "1-2"
        assert writing_telemetry._bucket_count(2) == "1-2"

    def test_three_and_above(self):
        assert writing_telemetry._bucket_count(3) == "3+"
        assert writing_telemetry._bucket_count(100) == "3+"
