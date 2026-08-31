"""#236 — entities stage: per-chunk progress reporting.

Pins the acceptance behavior without the heavy models: GLiNER extraction
reports monotonically increasing done/total (unit: chunks) through the same
callback chain relationships uses, and the reporting is pure — the extracted
entities are byte-identical with and without a progress callback.

Run: .venv/bin/python -m pytest tests/test_entities_stage.py
"""
from __future__ import annotations

import pytest

# CI light stack: runner.py imports compute_core.relation_extractor (mREBEL)
# at module level, which needs torch — skip collection there instead of
# failing (the test runs on a fake model, gliner itself is not used).
pytest.importorskip("torch")

from axiom_ng_runner import runner as runnermod


class _FakeGliner:
    """Minimal stand-in for GLiNER: one org span per non-empty chunk."""

    def predict_entities(self, text, labels, threshold=0.5, multi_label=True):
        return [{"text": "Acme Corp", "label": "organization",
                 "start": 0, "score": 0.9}]


def _chunk_items(n: int) -> list[tuple[str, str]]:
    return [(f"chunk-{i:04d}", f"Acme Corp did thing {i}." * 3)
            for i in range(n)]


def test_entities_progress_reported_per_chunk(monkeypatch):
    monkeypatch.setattr(runnermod, "_get_gliner", lambda: _FakeGliner())
    seen: list[tuple[int, int]] = []
    entities = runnermod._extract_real_entities(
        _chunk_items(45), on_progress=lambda d, t: seen.append((d, t)))
    # Monotone 1..45/45 — every chunk reports, never 0/0.
    assert seen == [(i, 45) for i in range(1, 46)]
    assert len(entities) == 1  # same text+type groups into one entity
    assert len(entities[0]["mentions"]) == 45


def test_entities_reporting_is_pure(monkeypatch):
    monkeypatch.setattr(runnermod, "_get_gliner", lambda: _FakeGliner())
    plain = runnermod._extract_real_entities(_chunk_items(12))
    with_cb = runnermod._extract_real_entities(
        _chunk_items(12), on_progress=lambda d, t: None)
    assert plain == with_cb  # no behavior change from reporting


def test_entities_progress_empty_input(monkeypatch):
    monkeypatch.setattr(runnermod, "_get_gliner", lambda: _FakeGliner())
    seen: list[tuple[int, int]] = []
    entities = runnermod._extract_real_entities(
        [], on_progress=lambda d, t: seen.append((d, t)))
    assert entities == []
    assert seen == []  # 0/0 is never emitted (#236: no blindness writes)
