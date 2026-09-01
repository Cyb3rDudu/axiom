"""Integrity triad (#240, #241): honest failure over silent degradation.

#240 — compute=real with a broken environment must FAIL the job retryably
(ComputeEnvironmentError), never silently process on the reference stub.
The query endpoints already forbid this fallback by test
(test_query_endpoints.py:173) — these tests bring the batch path to the
same standard, plus the explicit ALLOW_REFERENCE_FALLBACK=1 opt-out (loud
log + processor metadata marker).

#241 — an embedding batch raising mid-book must fail the stage with ZERO
placeholder vectors persisted (the old code appended zero-vector
placeholders and padded length mismatches — index poisoning: chunks
invisible to dense retrieval while appearing processed, violating the
#225 "what is committed must be real" discipline).

Mutation probes (documented in the issue comments):
- #240: revert compute() to the silent fallback -> the no-fallback test
  fails (reference executed despite real mode).
- #241: re-insert the placeholder/padding branch -> both embedder tests
  fail (placeholders observed where a raise is required).
"""

from __future__ import annotations

import threading
from unittest import mock

import pytest
from axiom_ng_runner import runner
from axiom_ng_runner.compute_core.embedder import TextEmbedder

# ---------------------------------------------------------------------------
# #240: real-mode ImportError -> retryable failure, no reference run
# ---------------------------------------------------------------------------


def _real_request(tmp_path) -> dict:
    return {
        "job_id": "j1",
        "attachment": {
            "attachment_id": "a1",
            "local_path": str(tmp_path / "book.epub"),
            "content_type": "application/epub+zip",
        },
        "processing": {},
    }


def _force_real_backend(monkeypatch):
    """Point runner.settings at a real-mode singleton."""
    fake = mock.Mock()
    fake.get.return_value.compute_backend = "real"
    monkeypatch.setattr(runner, "settings", fake)


def test_real_mode_import_error_fails_no_reference(monkeypatch, tmp_path):
    """compute=real + unimportable real pipeline -> ComputeEnvironmentError
    naming the import error; the reference path must NOT run."""
    monkeypatch.setattr(
        runner, "_real_pipeline",
        mock.Mock(side_effect=ImportError(
            "No module named 'axiom_ng_runner.compute_core.marker'")),
    )

    def _reference_must_not_run(*a, **k):
        raise AssertionError("reference fallback executed in real mode (#240)")

    monkeypatch.setattr(runner, "_compute_reference", _reference_must_not_run)
    _force_real_backend(monkeypatch)
    monkeypatch.delenv("ALLOW_REFERENCE_FALLBACK", raising=False)

    with pytest.raises(runner.ComputeEnvironmentError) as ctx:
        runner.compute(_real_request(tmp_path), tmp_path)
    assert "compute_core.marker" in str(ctx.value)
    assert "compute=real" in str(ctx.value)


def test_real_mode_fallback_opt_out_is_loud_and_marked(
        monkeypatch, tmp_path, caplog):
    """ALLOW_REFERENCE_FALLBACK=1: reference runs, the fallback is logged
    loudly (WARNING) and marked in the processor metadata."""
    monkeypatch.setattr(
        runner, "_real_pipeline",
        mock.Mock(side_effect=ImportError("No module named 'heavy.dep'")),
    )
    monkeypatch.setattr(
        runner, "_compute_reference",
        mock.Mock(return_value={"processor": {"name": "axiom-python-marker"},
                                "chunks": [], "artifacts": []}))
    _force_real_backend(monkeypatch)
    monkeypatch.setenv("ALLOW_REFERENCE_FALLBACK", "1")

    with caplog.at_level("WARNING"):
        result = runner.compute(_real_request(tmp_path), tmp_path)

    assert any(
        "ALLOW_REFERENCE_FALLBACK" in r.message and r.levelname == "WARNING"
        for r in caplog.records
    ), "the reference fallback must be loud (WARNING), not silent"
    assert result["processor"]["reference_fallback"] is True


def test_compute_environment_error_maps_to_retryable_job(monkeypatch, tmp_path):
    """App-level proof: _run_compute translates ComputeEnvironmentError into
    set_error(INTERNAL_ERROR, retryable=True) — a failed, retryable job."""
    from axiom_ng_runner import app

    job = mock.Mock()
    job.path = tmp_path
    job.job_id = "job-1"
    job.request = _real_request(tmp_path)

    store = mock.Mock()
    monkeypatch.setattr(app, "_store_impl", mock.Mock(return_value=store))
    monkeypatch.setattr(
        runner, "compute",
        mock.Mock(side_effect=runner.ComputeEnvironmentError(
            "compute=real but the real compute pipeline is unavailable: boom")))
    app._run_compute(job)

    store.set_error.assert_called_once()
    args, kwargs = store.set_error.call_args
    assert args[1] == "INTERNAL_ERROR"
    assert kwargs.get("retryable") is True


# ---------------------------------------------------------------------------
# #241: embedder — no placeholder vectors, ever
# ---------------------------------------------------------------------------


def _embedder_with(fake_model, batch_size=2):
    """TextEmbedder without the heavy __init__ (no BGEM3 load): the fields
    embed_chunks actually touches, plus a fake model whose encode we
    control per batch."""
    emb = object.__new__(TextEmbedder)
    emb.model = fake_model
    emb.batch_size = batch_size
    emb.max_length = 8192
    emb.enable_memory_management = False
    emb._model_lock = threading.Lock()
    return emb


def _chunks(n):
    return [{"text": f"chunk text number {i} with words", "metadata": {}}
            for i in range(n)]


class _FakeEncode:
    """encode() succeeds for the first batches, raises on a chosen batch
    index; can also return fewer vector rows than input texts."""

    def __init__(self, fail_on_batch=None, rows=None):
        self.calls = 0
        self.fail_on_batch = fail_on_batch
        self.rows = rows  # None -> len(texts)

    def encode(self, texts, **kwargs):
        if self.fail_on_batch is not None and self.calls == self.fail_on_batch:
            self.calls += 1
            raise RuntimeError("CUDA OOM (simulated mid-book)")
        self.calls += 1
        rows = len(texts) if self.rows is None else self.rows
        return {
            "dense_vecs": [[0.11] * 8] * rows,
            "lexical_weights": [{"5": 1.0}] * rows,
        }


def test_embed_batch_failure_raises_no_placeholders():
    """Batch 2 of 3 raises -> embed_chunks raises (retryable upstream), and
    NO chunk carries placeholder/zero vectors (nothing persisted)."""
    fake = _FakeEncode(fail_on_batch=1)  # 6 chunks / batch 2 -> fails mid-book
    emb = _embedder_with(fake, batch_size=2)
    chunks = _chunks(6)
    with pytest.raises(RuntimeError, match="chunk offset 2"):
        emb.embed_chunks(chunks)
    assert all("embeddings" not in c for c in chunks), \
        "placeholder embeddings leaked into chunks (#241)"


def test_embed_length_mismatch_is_hard_failure_not_padding():
    """Model returns fewer vectors than chunks -> hard failure, never
    zero-padding to length."""
    fake = _FakeEncode(rows=3)  # 4 chunks, only 3 vectors
    emb = _embedder_with(fake, batch_size=8)
    chunks = _chunks(4)
    with pytest.raises(RuntimeError, match="length mismatch"):
        emb.embed_chunks(chunks)
    assert all("embeddings" not in c for c in chunks)


def test_embed_success_still_works():
    """Passenger proof: the honest-failure change must not break the happy
    path — chunks come back with real (fake-model) dense+sparse vectors."""
    emb = _embedder_with(_FakeEncode(), batch_size=2)
    out = emb.embed_chunks(_chunks(4))
    assert len(out) == 4
    assert all(c["embeddings"]["dense"] == pytest.approx([0.11] * 8)
               for c in out)
    assert all(c["embeddings"]["sparse"] == {"5": 1.0} for c in out)
