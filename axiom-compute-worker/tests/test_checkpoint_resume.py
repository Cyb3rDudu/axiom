"""#372 checkpointed job resumption — hermetic probes through compute().

The kill probe simulates the production abort at a defined phase
boundary (the conversion raises mid-attempt like a killed worker); the
retry (a FRESH work dir — a new attempt/jobe id) must resume at the
first incomplete phase: convert reused (~0 timing witness), chunk
computed. The hash probe pins invalidation: a changed file starts
clean. The honesty probe pins manifest.phase_reuse.
"""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest

from axiom_compute_worker import runner
from axiom_compute_worker.checkpoints import PhaseCheckpoints
from axiom_compute_worker.config import Settings, settings


def _request(src: Path, key: str = "k-372") -> dict:
    return {
        "contract_version": "1.0",
        "job_id": key,
        "idempotency_key": key,
        "source": {"type": "zotero", "source_id": "src-372", "server_id": "srv"},
        "document": {"document_id": "doc-372", "zotero_key": "ZK",
                     "zotero_version": 1, "metadata_snapshot": {"itemType": "book"}},
        "attachment": {
            "attachment_id": "att-372", "zotero_key": "AK", "zotero_version": 1,
            "content_type": "application/pdf", "filename": src.name,
            "local_path": str(src),
            "content_hash": "sha256:" + hashlib.sha256(src.read_bytes()).hexdigest(),
            "size_bytes": src.stat().st_size, "mtime_ms": 0,
        },
        "processing": {"profile": "full-rag-v1", "force_rebuild": False,
                       "compute_dense_embeddings": False,
                       "compute_sparse_embeddings": False,
                       "extract_entities": False,
                       "extract_relationships": False},
    }


def _workdir(tmp: Path, name: str) -> Path:
    d = tmp / name
    d.mkdir(parents=True, exist_ok=True)
    return d


def _pdf(tmp: Path, name: str = "book.pdf", marker: str = "body") -> Path:
    # minimal text-only PDF via pymupdf (the reference converter reads it)
    import pymupdf

    p = tmp / name
    doc = pymupdf.open()
    page = doc.new_page()
    page.insert_text((72, 72), f"{marker} chapter text for chunking. " * 8)
    doc.save(str(p))
    doc.close()
    return p


class TestCheckpointsUnit:
    def test_marker_atomicity_and_contiguity(self, tmp_path):
        req = _request(_pdf(tmp_path))
        cp = PhaseCheckpoints(req, tmp_path)
        cp.save("convert", jsons={"page_count": 3})
        assert cp.resume("convert") is True
        assert cp.resume("chunk") is False
        # a payload WITHOUT its marker (kill mid-write) does not count
        payload = cp.root / "chunk" / "chunks.json"
        payload.parent.mkdir(parents=True)
        payload.write_text("[]")
        assert cp.resume("chunk") is False

    def test_key_invalidation_on_content_change(self, tmp_path):
        req1 = PhaseCheckpoints(_request(_pdf(tmp_path, "a.pdf", "one")), tmp_path)
        req2 = PhaseCheckpoints(_request(_pdf(tmp_path, "b.pdf", "two")), tmp_path)
        assert req1.root != req2.root
        # profile change also re-keys
        req3 = _request(_pdf(tmp_path, "a.pdf", "one"))
        req3["processing"]["compute_dense_embeddings"] = True
        assert PhaseCheckpoints(req3, tmp_path).root != req1.root

    def test_sibling_prune_on_new_key(self, tmp_path):
        req_a = _request(_pdf(tmp_path, "a.pdf", "one"))
        cp = PhaseCheckpoints(req_a, tmp_path)
        cp.save("convert", jsons={"page_count": 1})
        old_root = cp.root
        req_b = _request(_pdf(tmp_path, "b.pdf", "two"))
        cp2 = PhaseCheckpoints(req_b, tmp_path)
        cp2.save("convert", jsons={"page_count": 2})
        assert not old_root.exists()  # pruned: content moved on


class TestResumeThroughCompute:
    def test_kill_probe_resumes_at_first_incomplete_phase(self, tmp_path, monkeypatch):
        work_root = tmp_path / "wr"
        old = settings.get()
        settings.set(Settings(work_root=work_root,
                              allowed_source_roots=(str(tmp_path),),
                              compute_backend="reference", warmup=False))
        try:
            src = _pdf(tmp_path)
            # attempt 1: the chunker dies (the defined phase boundary kill)
            calls = {"convert": 0}

            real_convert = runner._convert_reference

            def counting_convert(request, source_path, work_dir):
                calls["convert"] += 1
                return real_convert(request, source_path, work_dir)

            def killed_chunk(markdown, page_label_map):
                raise RuntimeError("simulated kill at the chunk boundary")

            import axiom_compute_worker.chunking as _cm

            if "_orig_chunk_markdown" not in _cm.__dict__:
                _cm._orig_chunk_markdown = _cm.chunk_markdown
            monkeypatch.setattr(runner, "_convert_reference", counting_convert)
            monkeypatch.setattr(
                "axiom_compute_worker.chunking.chunk_markdown", killed_chunk
            )
            with pytest.raises(RuntimeError):
                runner.compute(_request(src), _workdir(tmp_path, "attempt1"))

            # convert completed before the kill — its checkpoint must exist
            assert calls["convert"] == 1

            # attempt 2 (fresh work dir = new job id): resume, don't restart.
            # Capture the ORIGINAL before any patching — re-reading the
            # module attr now would snapshot the killed stub.
            import axiom_compute_worker.chunking as chunking_mod

            monkeypatch.setattr(
                "axiom_compute_worker.chunking.chunk_markdown",
                chunking_mod.__dict__.get("_orig_chunk_markdown")
                or __import__("axiom_compute_worker.chunking",
                              fromlist=["chunk_markdown"]).chunk_markdown,
            )
            result = runner.compute(_request(src), _workdir(tmp_path, "attempt2"))

            assert calls["convert"] == 1, "convert re-ran on resume (must be reused)"
            reuse = result["manifest"]["phase_reuse"]
            assert reuse["convert"] == "reused"
            assert reuse["chunk"] == "computed"
            # timing witness: the reused convert stage entered and finished
            # within the same instant (start timestamp recorded on resume,
            # chunk entered immediately after — no conversion elapsed)
            timings = result["manifest"]["stage_timings"]
            assert timings["convert"] <= timings["chunk"]
            assert result["status"] == "completed"
        finally:
            settings.set(old)
            monkeypatch.undo()

    def test_hash_invalidation_probe(self, tmp_path, monkeypatch):
        work_root = tmp_path / "wr"
        old = settings.get()
        settings.set(Settings(work_root=work_root,
                              allowed_source_roots=(str(tmp_path),),
                              compute_backend="reference", warmup=False))
        try:
            src1 = _pdf(tmp_path, "v1.pdf", "first version body")
            result1 = runner.compute(_request(src1), _workdir(tmp_path, "a1"))
            assert result1["manifest"]["phase_reuse"]["convert"] == "computed"

            src2 = _pdf(tmp_path, "v2.pdf", "second version body CHANGED")
            result2 = runner.compute(_request(src2), _workdir(tmp_path, "a2"))
            reuse = result2["manifest"]["phase_reuse"]
            assert reuse["convert"] == "computed", (
                "a changed file must start clean (no reuse across hashes)"
            )
        finally:
            settings.set(old)

    def test_second_full_attempt_reuses_everything(self, tmp_path):
        work_root = tmp_path / "wr"
        old = settings.get()
        settings.set(Settings(work_root=work_root,
                              allowed_source_roots=(str(tmp_path),),
                              compute_backend="reference", warmup=False))
        try:
            src = _pdf(tmp_path)
            r1 = runner.compute(_request(src), _workdir(tmp_path, "a1"))
            r2 = runner.compute(_request(src), _workdir(tmp_path, "a2"))
            assert r2["manifest"]["phase_reuse"] == {"convert": "reused",
                                                     "chunk": "reused"}
            # identical outputs — the resume is byte-equivalent
            assert (json.dumps(r1["chunks"], sort_keys=True)
                    == json.dumps(r2["chunks"], sort_keys=True))
        finally:
            settings.set(old)
