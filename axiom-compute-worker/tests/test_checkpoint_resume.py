"""#372 checkpointed job resumption — hermetic probes through compute().

The kill probe simulates the production abort at a defined phase
boundary (the conversion raises mid-attempt like a killed worker); the
retry (a FRESH work dir — a new attempt/job id) must resume at the
first incomplete phase: convert reused (~0 timing witness), chunk
computed. The hash probe pins invalidation: a changed file starts
clean. The honesty probe pins manifest.phase_reuse.
"""

from __future__ import annotations

import hashlib
import json
import threading
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

    def test_contiguity_middle_hole(self, tmp_path):
        # A marker is only resumable when every EARLIER phase is complete:
        # entities with a valid marker but a missing chunk marker must NOT
        # resume. Mutation probe: removing the break in _scan (scanning all
        # markers regardless of holes) makes the first assertion fail.
        req = _request(_pdf(tmp_path))
        cp = PhaseCheckpoints(req, tmp_path)
        cp.save("convert", jsons={})
        cp.save("entities", jsons={})  # chunk hole in between
        assert cp.resume("convert") is True
        assert cp.resume("chunk") is False
        assert cp.resume("entities") is False, "resumed across a middle hole"

        cp2 = PhaseCheckpoints(req, tmp_path / "b")
        cp2.save("convert", jsons={})
        cp2.save("chunk", jsons={})
        cp2.save("embed", jsons={})  # prefix must be COMPLETE, not just present
        cp2.save("entities", jsons={})
        assert cp2.resume("entities") is True

    def test_numpy_scalars_serialize(self, tmp_path):
        # f3369908 regression pin: the real embedder writes float32 arrays;
        # the payload dump must normalize them or the save crashes.
        numpy = pytest.importorskip("numpy")
        import json as _json

        from axiom_compute_worker.checkpoints import _json_default

        assert _json.loads(
            _json.dumps({"x": numpy.float32(0.5)}, default=_json_default)
        )["x"] == 0.5
        cp = PhaseCheckpoints(_request(_pdf(tmp_path)), tmp_path)
        cp.save("embed", jsons={"vals": [numpy.float32(0.25)]})
        assert cp.load_json("embed", "vals") == [0.25]

    def test_force_rebuild_never_prunes_base_and_namespaces_split(self, tmp_path):
        """Ruling (2026-10-11, #372): force-rebuild is the operator's
        escape hatch — recompute from zero BY DESIGN, but its checkpoints
        live in a separate namespace and can never destroy the base
        key's resume basis (a dying force attempt must not cost the next
        plain retry its resume)."""
        req = _request(_pdf(tmp_path))
        base_cp = PhaseCheckpoints(req, tmp_path)
        base_cp.save("convert", jsons={})
        base_root = base_cp.root
        assert base_root.parent.name == "base"

        force_req = _request(_pdf(tmp_path))
        force_req["processing"]["force_rebuild"] = True
        force_cp = PhaseCheckpoints(force_req, tmp_path)
        force_cp.save("convert", jsons={})
        assert force_cp.root.parent.name == "force"
        assert base_root.exists(), "force attempt pruned the base key"

        # base prunes only its OWN superseded keys; the force namespace
        # is untouchable from base and vice versa
        other_req = _request(_pdf(tmp_path))
        other_req["attachment"]["attachment_id"] = "att-372"  # same id, new key
        other_req["processing"]["compute_dense_embeddings"] = True
        other_cp = PhaseCheckpoints(other_req, tmp_path)
        other_cp.save("convert", jsons={})
        assert not base_root.exists()  # base pruned its superseded sibling
        assert force_cp.root.exists(), "base prune reached into the force namespace"

    def test_off_profile_full_prefix_resumes_captions(self, tmp_path):
        """Contextual documents clear entities+relationships at claim time
        (#255) while captions stay on — the off phases must leave empty
        markers so the captions prefix stays complete."""
        req = _request(_pdf(tmp_path))
        req["processing"].update({"compute_dense_embeddings": False,
                                  "extract_entities": False,
                                  "extract_relationships": False,
                                  "extract_image_captions": True})
        cp = PhaseCheckpoints(req, tmp_path)
        for ph in ("convert", "chunk", "embed", "entities", "relationships", "captions"):
            cp.save(ph, jsons={})  # the empty-marker shape the off-branches write
        assert cp.resume("captions") is True

    def test_image_payload_follows_mapping_not_whitelist(self, tmp_path):
        """The pdf worker preserves arbitrary original extensions (.tiff,
        …): the convert payload derives from image_mapping's saved names,
        not an extension whitelist — a .tiff image must survive the
        checkpoint round-trip. Pinned at the PhaseCheckpoints level with
        the runner's mapping-derived save shape mirrored."""
        import inspect

        from axiom_compute_worker import runner as _r

        src = inspect.getsource(_r._real_pipeline)
        assert "saved_names = {Path(v).name" in src, (
            "convert payload filter must derive from image_mapping values"
        )
        assert "_IMAGE_EXTS_CP" not in src, "extension whitelist still in use"
        # round-trip: a non-whitelist payload file stores and re-links
        req = _request(_pdf(tmp_path))
        work = tmp_path / "w"
        work.mkdir()
        odd = work / "image_0.tiff"
        odd.write_bytes(b"II*\x00tiff-bytes")
        cp = PhaseCheckpoints(req, tmp_path)
        cp.save("convert", files={"markdown.md": _pdf(tmp_path), "image_0.tiff": odd},
                jsons={"image_mapping": {"fig.png": "image_0.tiff"}})
        dest = cp.load_file("convert", "image_0.tiff", tmp_path / "out" / "image_0.tiff")
        assert dest.read_bytes() == b"II*\x00tiff-bytes"

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

            orig_chunk = _cm.chunk_markdown
            monkeypatch.setattr(runner, "_convert_reference", counting_convert)
            monkeypatch.setattr(
                "axiom_compute_worker.chunking.chunk_markdown", killed_chunk
            )
            with pytest.raises(RuntimeError):
                runner.compute(_request(src), _workdir(tmp_path, "attempt1"))

            # convert completed before the kill — its checkpoint must exist
            assert calls["convert"] == 1

            # attempt 2 (fresh work dir = new job id): resume, don't restart.
            monkeypatch.setattr(
                "axiom_compute_worker.chunking.chunk_markdown", orig_chunk
            )
            result = runner.compute(_request(src), _workdir(tmp_path, "attempt2"))

            assert calls["convert"] == 1, "convert re-ran on resume (must be reused)"
            reuse = result["manifest"]["phase_reuse"]
            assert reuse["convert"] == "reused"
            assert reuse["chunk"] == "computed"
            # timing witness: a RESUMED convert enters and chunk enters
            # within the same instant (adjacent start timestamps); a real
            # convert takes minutes, so adjacency distinguishes reuse from
            # recompute where start-ordering alone could not.
            from datetime import datetime as _dt

            timings = result["manifest"]["stage_timings"]
            delta = (_dt.fromisoformat(timings["chunk"])
                     - _dt.fromisoformat(timings["convert"])).total_seconds()
            assert delta < 2, f"convert→chunk gap {delta}s — convert re-ran"
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


class TestWatchdogEvictionResume:
    """#369 integration: the watchdog's orphan-compute stop cancels the
    runner job mid-compute; the claim scan evicts; the reclaim resubmits
    under the SAME idempotency key (the runner relaunches the cancelled
    entry). The relaunched attempt must RESUME at the phase checkpoint,
    not restart: the conversion that completed before the cancel is not
    re-run."""

    def test_captions_resume_skips_dense_reembed(self, tmp_path, monkeypatch):
        """The captions checkpoint payload is POST-re-embed — a resumed
        captions phase must not re-run the embedder over every captioned
        chunk (the pre-fix catch-up re-paid the model pass on every
        resume while phase_reuse claimed 'reused')."""
        import types

        from axiom_compute_worker import runner as runner_mod

        # L6-style heavy stubs: fake conversion, no models
        class _FakePopen:
            def __init__(self, cmd, **_kw):
                Path(cmd[4]).write_text("# T\n\nAlpha works at Beta Corp",
                                        encoding="utf-8")
                self.out = '{"image_mapping": {}}'
                self.returncode = 0

            def communicate(self):
                return (self.out, "")

        import subprocess as _sub

        monkeypatch.setattr(_sub, "Popen", _FakePopen)
        pt = types.ModuleType("axiom_compute_worker.compute_core.page_trust")
        pt.build_page_trust = lambda p: ({}, {}, {})
        monkeypatch.setitem(__import__("sys").modules,
                            "axiom_compute_worker.compute_core.page_trust", pt)
        ch = types.ModuleType("axiom_compute_worker.compute_core.chunker")

        class _Ch:
            def chunk(self, md, doc_metadata):
                return [{"text": "Alpha works at Beta Corp",
                         "metadata": {"start_paragraph_index": 0}}]

            def chapter_starts(self):
                return []

        ch.Chunker = _Ch
        monkeypatch.setitem(__import__("sys").modules,
                            "axiom_compute_worker.compute_core.chunker", ch)
        reembed_calls = []
        monkeypatch.setattr(runner_mod, "_reembed_captioned",
                            lambda chunks: reembed_calls.append(len(chunks)) or True)

        src = _pdf(tmp_path)
        req = _request(src, "cap-resume")
        req["processing"].update({"compute_dense_embeddings": True,
                                  "extract_entities": False,
                                  "extract_relationships": False,
                                  "extract_image_captions": True})
        work_root = tmp_path / "wr"
        old = settings.get()
        settings.set(Settings(work_root=work_root,
                              allowed_source_roots=(str(tmp_path),),
                              compute_backend="real", warmup=False,
                              max_concurrent_jobs=1, admission_queue_capacity=9))
        try:
            # Seed the checkpoint prefix through captions exactly as a
            # completed first attempt would have (captions payload is
            # post-re-embed by construction).
            from axiom_compute_worker.checkpoints import PhaseCheckpoints

            seed_md = tmp_path / "seed.md"
            seed_md.write_text("# T\n\nAlpha works at Beta Corp", encoding="utf-8")
            cp0 = PhaseCheckpoints(req, work_root)
            cp0.save("convert", files={"markdown.md": seed_md},
                     jsons={"image_mapping": {}, "page_label_map": {},
                            "page_source_map": {}, "page_chapter_map": {},
                            "marker_pagemap_max": None, "cfi_entries": []})
            for ph in ("chunk", "embed", "entities", "relationships", "captions"):
                # captions seed mirrors a real zero-captioned save: chunks
                # only, no artifact-attrs sidecar
                cp0.save(ph, jsons={"chunks": []} if ph in ("chunk", "embed", "captions") else {})

            work = _workdir(tmp_path, "resume")
            result = runner_mod._real_pipeline(req, work)
            reuse = (result.get("manifest") or {}).get("phase_reuse") or {}
            assert reuse.get("captions") == "reused", reuse
            assert not reembed_calls, (
                f"captions resume re-ran the dense re-embed: {reembed_calls}"
            )
        finally:
            settings.set(old)

    def test_cancel_then_resubmit_resumes_at_phase(self, tmp_path, monkeypatch):
        from fastapi.testclient import TestClient

        from axiom_compute_worker import app as appmod

        src = _pdf(tmp_path)
        started = threading.Event()
        release = threading.Event()
        convert_calls = []

        real_convert = runner._convert_reference

        def slow_convert(request, source_path, work_dir):
            convert_calls.append(request["job_id"])
            out = real_convert(request, source_path, work_dir)
            started.set()
            release.wait(10)  # hold inside the convert phase (post-output)
            return out

        monkeypatch.setattr(runner, "_convert_reference", slow_convert)
        old = settings.get()
        settings.set(Settings(work_root=tmp_path / "work",
                              allowed_source_roots=(str(tmp_path),),
                              compute_backend="reference", warmup=False,
                              max_concurrent_jobs=1, admission_queue_capacity=9))
        try:
            with TestClient(appmod.app) as client:
                r1 = client.post("/v1/process", json=_payload_372(src, "k-evict"))
                assert r1.status_code == 202, r1.text
                assert started.wait(5), "compute never started"
                # the watchdog's cancel lands mid-compute (post-convert-output,
                # pre-marker in this backend — the phase is NOT yet complete)
                assert client.post("/v1/jobs/k-evict/cancel").json()["status"] == "cancelled"
                release.set()
                _wait_untracked(appmod, "k-evict")

                # The reference backend's cancel is cooperative: attempt 1's
                # compute thread finished its convert phase (and its marker)
                # after the cancel — so the relaunched attempt RESUMES and
                # never re-converts. A resumed convert does not call
                # _convert_reference at all — the witness is the terminal
                # status plus the convert-call counter, not a stage event.
                r2 = client.post("/v1/process", json=_payload_372(src, "k-evict"))
                assert r2.status_code == 202, r2.text
                assert r2.json().get("deduplicated") is True
                _wait_terminal(client, "k-evict")
                assert len(convert_calls) == 1, (
                    f"relaunched attempt re-converted despite the checkpoint: "
                    f"{convert_calls}"
                )
        finally:
            release.set()
            settings.set(old)


def _payload_372(src: Path, key: str) -> dict:
    req = _request(src, key)
    return req


def _wait_terminal(client, job_id: str, timeout_s: float = 15.0) -> None:
    import time as _t

    deadline = _t.monotonic() + timeout_s
    while True:
        st = client.get(f"/v1/jobs/{job_id}").json().get("status")
        if st in ("completed", "failed", "cancelled"):
            assert st == "completed", f"relaunched job ended {st}"
            return
        assert _t.monotonic() < deadline, f"job {job_id} never reached terminal"
        _t.sleep(0.05)


def _wait_untracked(appmod, job_id: str, timeout_s: float = 10.0) -> None:
    import time as _t

    deadline = _t.monotonic() + timeout_s
    while appmod._scheduler().is_relevant(job_id):
        assert _t.monotonic() < deadline, f"job {job_id} never left the scheduler"
        _t.sleep(0.02)
