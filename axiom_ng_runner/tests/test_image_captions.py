"""#230 image_captions stage: hash gate, budgets, honest partial, profile
gate (byte-identical off), artifact attributes, chunk image_captions."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from axiom_ng_runner.compute_core import image_captioner as ic
from axiom_ng_runner.runner import (
    _adapt_chunk,
    _caption_augmentation,
    _caption_images_stage,
)


class FakeCaptioner:
    model = "fake-vision-1"

    def __init__(self, fail: bool = False):
        self.calls = 0
        self.fail = fail

    def caption(self, image_bytes: bytes, media_type: str) -> tuple[str, str]:
        self.calls += 1
        if self.fail:
            raise RuntimeError("model exploded")
        return f"Chart showing revenue for {len(image_bytes)} bytes", "cloud"


def _art(ref: str, sha: str) -> dict:
    return {"ref": ref, "kind": "extracted_image", "media_type": "image/png",
            "sha256": sha, "size_bytes": 10, "retention": "durable_if_referenced"}


def _run(arts, chunks, tmp_path, captioner, budget=900.0, timeout=60.0, cache=None,
         flag=True):
    # image files on disk (the stage reads work_dir/artifacts/<ref>)
    art_dir = tmp_path / "artifacts"
    art_dir.mkdir(exist_ok=True)
    for a in arts:
        (art_dir / a["ref"]).write_bytes(b"x" * 8)
    stage_completion: dict = {}
    import axiom_ng_runner.config as cfg

    class _FakeSettingsModule:
        @staticmethod
        def get():
            st = cfg.load_settings()
            object.__setattr__(st, "image_captions_budget_seconds", budget)
            object.__setattr__(st, "image_caption_timeout_seconds", timeout)
            return st
    import axiom_ng_runner.runner as runner_mod
    real_settings = runner_mod.settings
    runner_mod.settings = _FakeSettingsModule  # type: ignore[assignment]
    try:
        import os
        old_cache = os.environ.get("AXIOM_CAPTION_CACHE_DIR")
        os.environ["AXIOM_CAPTION_CACHE_DIR"] = str(cache or (tmp_path / "cache"))
        old_resolve = ic.resolve_captioner
        ic.resolve_captioner = lambda: captioner  # type: ignore[assignment]
        try:
            ran = _caption_images_stage(
                {"extract_image_captions": flag}, arts, chunks, tmp_path,
                stage_completion, None, lambda cds: None,
            )
        finally:
            ic.resolve_captioner = old_resolve  # type: ignore[assignment]
            if old_cache is None:
                del os.environ["AXIOM_CAPTION_CACHE_DIR"]
            else:
                os.environ["AXIOM_CAPTION_CACHE_DIR"] = old_cache
    finally:
        runner_mod.settings = real_settings  # type: ignore[assignment]
    return ran, stage_completion


def test_profile_off_runs_nothing():
    arts = [_art("image-0000", "aa" * 32)]
    chunks = [{"text": "t", "metadata": {"image_refs": ["image-0000"]}}]
    ran, sc = _run(arts, chunks, Path("/tmp"), FakeCaptioner(), flag=False)
    assert ran is None
    assert sc == {}
    assert "machine_caption" not in arts[0]


def test_captions_land_on_artifact_and_chunk(tmp_path):
    arts = [_art("image-0000", "aa" * 32), _art("image-0001", "bb" * 32)]
    chunks = [
        {"text": "t1", "metadata": {"image_refs": ["image-0000"]}},
        {"text": "t2", "metadata": {"image_refs": []}},
    ]
    ran, sc = _run(arts, chunks, tmp_path, FakeCaptioner())
    assert ran is True
    assert sc["image_captions"] is True and sc["image_captions_reason"] is None
    # C1 (#230 review): the fields MUST nest under "attributes" — the Go
    # processor reads only that key and silently drops unknown top-level
    # keys at the persist boundary (W9). Proven end-to-end: this exact
    # artifact dict JSON-unmarshals into processor.Artifact with the
    # attributes intact (see the Go round-trip test).
    a0 = arts[0]["attributes"]
    assert a0["caption_model"] == "fake-vision-1"
    assert a0["caption_path"] == "cloud"
    assert a0["machine_caption"] == arts[0]["attributes"]["machine_caption"]
    assert "machine_caption" in arts[1]["attributes"]
    assert chunks[0]["metadata"]["image_captions"] == {
        "image-0000": arts[0]["attributes"]["machine_caption"]}
    assert "image_captions" not in chunks[1]["metadata"]


def test_hash_gate_second_run_captions_nothing(tmp_path):
    arts = [_art("image-0000", "aa" * 32)]
    chunks = [{"text": "t", "metadata": {"image_refs": ["image-0000"]}}]
    cap = FakeCaptioner()
    _run(arts, chunks, tmp_path, cap)
    assert cap.calls == 1
    # second run, fresh captioner instance, same cache dir: cache hit
    arts2 = [_art("image-0000", "aa" * 32)]
    chunks2 = [{"text": "t", "metadata": {"image_refs": ["image-0000"]}}]
    cap2 = FakeCaptioner()
    _run(arts2, chunks2, tmp_path, cap2)
    assert cap2.calls == 0
    assert arts2[0]["attributes"]["machine_caption"] == arts[0]["attributes"]["machine_caption"]


def test_budget_abort_leaves_rest_empty(tmp_path):
    # zero budget: deadline passed before image 1 → honest partial
    arts = [_art("image-0000", "aa" * 32)]
    chunks = [{"text": "t", "metadata": {"image_refs": ["image-0000"]}}]
    ran, sc = _run(arts, chunks, tmp_path, FakeCaptioner(), budget=0.000001)
    assert ran is True
    assert sc["image_captions"] is False
    assert sc["image_captions_reason"] == "STAGE_BUDGET_EXCEEDED"
    # no placeholder, no caption — the honest empty
    assert "machine_caption" not in arts[0]
    assert "image_captions" not in chunks[0]["metadata"]


def test_all_calls_failed_reason(tmp_path):
    arts = [_art("image-0000", "aa" * 32)]
    chunks = [{"text": "t", "metadata": {"image_refs": ["image-0000"]}}]
    ran, sc = _run(arts, chunks, tmp_path, FakeCaptioner(fail=True))
    assert ran is True
    assert sc["image_captions_reason"] == "CAPTION_CALLS_FAILED"
    assert "machine_caption" not in arts[0]


def test_no_captioner_provisioned_reason(tmp_path):
    arts = [_art("image-0000", "aa" * 32)]
    chunks = [{"text": "t", "metadata": {"image_refs": ["image-0000"]}}]
    ran, sc = _run(arts, chunks, tmp_path, None)
    assert ran is True
    assert sc["image_captions_reason"] == "CAPTIONER_NOT_PROVISIONED"


def test_augmentation_is_machine_marked_never_text():
    c = {"metadata": {"image_captions": {"image-0000": "A bar chart of sales"}}}
    aug = _caption_augmentation(c)
    assert "machine-generated" in aug and "A bar chart of sales" in aug
    assert _caption_augmentation({"metadata": {}}) == ""
    # chunk text stays pure: augmentation never touches chunk["text"]
    assert "text" not in c or c.get("text") is None


def test_adapt_chunk_carries_and_omits_captions():
    base = {"text": "t", "metadata": {"image_refs": ["image-0000"],
                                      "image_captions": {"image-0000": "cap"}}}
    out = _adapt_chunk(base, 0, {}, None, None)
    assert out["image_captions"] == {"image-0000": "cap"}
    out2 = _adapt_chunk({"text": "t", "metadata": {}}, 0, {}, None, None)
    assert "image_captions" not in out2


def test_cloud_captioner_request_shape(monkeypatch):
    """The cloud client must send ONE OpenAI-compatible chat request with a
    base64 data-URL image and the calibrated prompt; the API key rides the
    Authorization header only."""
    captured = {}

    class FakeResp:
        def raise_for_status(self):
            pass

        def json(self):
            return {"choices": [{"message": {"content": "  A line chart.  "}}]}

    def fake_post(url, json=None, headers=None, timeout=None):
        captured.update(url=url, json=json, headers=headers)
        return FakeResp()

    import httpx
    monkeypatch.setattr(httpx, "post", fake_post)
    cap = ic.CloudCaptioner("http://localhost:9999/v1/", "sk-test", "test-vision")
    caption, path = cap.caption(b"\x89PNG", "image/png")
    assert caption == "A line chart." and path == "cloud"
    assert captured["url"] == "http://localhost:9999/v1/chat/completions"
    assert captured["headers"]["Authorization"] == "Bearer sk-test"
    content = captured["json"]["messages"][0]["content"]
    assert content[0]["image_url"]["url"].startswith("data:image/png;base64,")
    assert content[1]["text"] == ic.CAPTION_PROMPT


def test_cloud_captioner_empty_content_raises(monkeypatch):
    import httpx

    class FakeResp:
        def raise_for_status(self):
            pass

        def json(self):
            return {"choices": [{"message": {"content": ""}}]}

    monkeypatch.setattr(httpx, "post", lambda *a, **k: FakeResp())
    cap = ic.CloudCaptioner("http://x", "k", "m")
    with pytest.raises(ValueError):
        cap.caption(b"x", "image/png")


def test_caption_image_cache_roundtrip(tmp_path):
    (tmp_path / "img.bin").write_bytes(b"imgdata")
    cap = FakeCaptioner()
    rec = ic.caption_image(cap, tmp_path / "img.bin", "image/png",
                           "cc" * 32, tmp_path / "cache", 5.0)
    assert rec == {"caption": rec["caption"], "model": "fake-vision-1", "path": "cloud"}
    assert json.loads((tmp_path / "cache" / ("cc" * 32 + ".json")).read_text()) == rec
    # hash-gated second call: no captioner interaction
    rec2 = ic.caption_image(None, tmp_path / "img.bin", "image/png",
                            "cc" * 32, tmp_path / "cache", 5.0)
    assert rec2 == rec and cap.calls == 1


def test_per_image_timeout_abandons_the_call(monkeypatch, tmp_path):
    """C2 (#230 review): the deadline must ABANDON the model call, not
    join it — a 1.0s captioner with a 0.1s timeout must return in ~0.1s
    (the context-manager shutdown(wait=True) variant blocks for the full
    call; measured in review)."""
    import time as _time

    class SlowCaptioner:
        model = "slow"

        def caption(self, b, m):
            _time.sleep(1.0)
            return "late", "local"

    tmp = tmp_path / "c2img.bin"
    tmp.write_bytes(b"x")
    cap = SlowCaptioner()
    t0 = _time.monotonic()
    rec = ic.caption_image(cap, tmp, "image/png", "dd" * 32,
                           tmp_path / "c2cache", 0.1)
    elapsed = _time.monotonic() - t0
    assert rec is None, "timed-out call must be an honest miss"
    assert elapsed < 0.6, f"deadline must abandon, not join ({elapsed:.2f}s)"


def test_resolve_incomplete_local_dir_is_not_provisioned(tmp_path, monkeypatch):
    """HOCH-2 (#230 review round 2): a model dir with config.json but
    WITHOUT the snapshot's package files must resolve to NOT-PROVISIONED
    (CAPTIONER_NOT_PROVISIONED), not fail later per-image."""
    d = tmp_path / "moondream3"
    d.mkdir()
    (d / "config.json").write_text("{}")
    monkeypatch.delenv("AXIOM_CAPTION_API_BASE", raising=False)
    monkeypatch.delenv("AXIOM_CAPTION_API_KEY", raising=False)
    monkeypatch.setenv("AXIOM_CAPTION_LOCAL_MODEL_DIR", str(d))
    assert ic.resolve_captioner() is None


def test_resolve_complete_local_dir(monkeypatch, tmp_path):
    """With the complete snapshot file set present and torch importable,
    resolve returns the local captioner. torch is faked via sys.modules so
    the test also runs on the LIGHT CI stack (requirements.txt only — no
    heavy deps at collection time, the CI invariant)."""
    import sys
    import types

    fake_torch = types.ModuleType("torch")
    monkeypatch.setitem(sys.modules, "torch", fake_torch)
    d = tmp_path / "moondream3"
    d.mkdir()
    for f in ("config.json", "moondream.py", "text.py", "vision.py"):
        (d / f).write_text("")
    monkeypatch.delenv("AXIOM_CAPTION_API_BASE", raising=False)
    monkeypatch.delenv("AXIOM_CAPTION_API_KEY", raising=False)
    monkeypatch.setenv("AXIOM_CAPTION_LOCAL_MODEL_DIR", str(d))
    cap = ic.resolve_captioner()
    assert isinstance(cap, ic.LocalMoondreamCaptioner)


def test_local_runtime_ready_requires_full_file_set(tmp_path, monkeypatch):
    """W2 (#230 review round 3): a partial snapshot missing vision.py must
    resolve to NOT-PROVISIONED — the caption path needs it and a per-image
    CAPTION_CALLS_FAILED after stage open is the forbidden outcome."""
    import sys
    import types

    monkeypatch.setitem(sys.modules, "torch", types.ModuleType("torch"))
    d = tmp_path / "moondream3"
    d.mkdir()
    (d / "config.json").write_text("{}")
    (d / "moondream.py").write_text("")
    (d / "text.py").write_text("")
    monkeypatch.delenv("AXIOM_CAPTION_API_BASE", raising=False)
    monkeypatch.delenv("AXIOM_CAPTION_API_KEY", raising=False)
    monkeypatch.setenv("AXIOM_CAPTION_LOCAL_MODEL_DIR", str(d))
    assert ic.resolve_captioner() is None


def test_resolve_cloud_wins(monkeypatch):
    monkeypatch.setenv("AXIOM_CAPTION_API_BASE", "http://x/v1")
    monkeypatch.setenv("AXIOM_CAPTION_API_KEY", "sk")
    cap = ic.resolve_captioner()
    assert isinstance(cap, ic.CloudCaptioner)




def _png_bytes() -> bytes:
    """Valid 1x1 PNG via PIL (hand-crafted hex tends to carry broken
    chunk CRCs — the caption path opens the image for real)."""
    import io

    from PIL import Image

    buf = io.BytesIO()
    Image.new("RGB", (1, 1)).save(buf, format="PNG")
    return buf.getvalue()

# ── #230 rider: model-memory discipline ────────────────────────────────────


class _FakeModel:
    def caption(self, img, length="short"):
        return {"caption": "A fake chart"}


def _fake_local_captioner(tmp_path, monkeypatch):
    """A LocalMoondreamCaptioner with the heavy load path stubbed out —
    load() returns a fake model, no torch/HF involved."""
    cap = ic.LocalMoondreamCaptioner(str(tmp_path))
    monkeypatch.setattr(cap, "load", lambda: _FakeModel())
    return cap


def test_local_captioner_unloads_after_stage(tmp_path, monkeypatch):
    """Rule 1: the INGEST-class captioner leaves memory after the stage —
    model registry (captioner._model via unload) is empty again."""
    d = tmp_path / "artifacts"
    d.mkdir(exist_ok=True)
    arts = [_art("image-0000", "aa" * 32)]
    (d / "image-0000").write_bytes(b"x" * 8)
    chunks = [{"text": "t", "metadata": {"image_refs": ["image-0000"]}}]

    cap = ic.LocalMoondreamCaptioner(str(tmp_path))
    unloaded = []
    monkeypatch.setattr(cap, "load", lambda: _FakeModel())
    monkeypatch.setattr(cap, "unload", lambda: unloaded.append(True))

    import axiom_ng_runner.runner as runner_mod
    real_settings = runner_mod.settings
    class _FS:
        @staticmethod
        def get():
            st = real_settings.get()
            return st
    runner_mod.settings = _FS  # type: ignore[assignment]
    try:
        import os
        old = os.environ.get("AXIOM_CAPTION_CACHE_DIR")
        os.environ["AXIOM_CAPTION_CACHE_DIR"] = str(tmp_path / "cache2")
        real_resolve = ic.resolve_captioner
        ic.resolve_captioner = lambda: cap  # type: ignore[assignment]
        try:
            _caption_images_stage(
                {"extract_image_captions": True}, arts, chunks, tmp_path,
                {}, None, lambda cds: None,
            )
        finally:
            ic.resolve_captioner = real_resolve  # type: ignore[assignment]
            if old is None:
                del os.environ["AXIOM_CAPTION_CACHE_DIR"]
            else:
                os.environ["AXIOM_CAPTION_CACHE_DIR"] = old
    finally:
        runner_mod.settings = real_settings  # type: ignore[assignment]
    assert unloaded, "captioner.unload() must run after the stage (Rule 1)"


def test_mrebel_released_after_relationships(monkeypatch):
    """Rule 1: mREBEL is INGEST class — _release_mrebel() empties the
    module registry (the wiring into _compute_real sits right after the
    relationships block; this asserts the registry effect)."""
    from axiom_ng_runner.compute_core import relation_extractor as rx
    from axiom_ng_runner.runner import _release_mrebel

    rx._mrebel_model = object()
    rx._mrebel_tokenizer = object()
    _release_mrebel()
    assert rx._mrebel_model is None and rx._mrebel_tokenizer is None


def test_query_class_stays_resident(monkeypatch):
    """Rule 1 counter-assertion: the QUERY class (embedder, reranker) is
    untouched by the ingest unload discipline."""
    from axiom_ng_runner import query_service
    from axiom_ng_runner.runner import _release_mrebel

    fake_embedder, fake_reranker = object(), object()
    monkeypatch.setattr(query_service, "_embedder", fake_embedder)
    monkeypatch.setattr(query_service, "_reranker", fake_reranker)
    _release_mrebel()
    assert query_service._embedder is fake_embedder
    assert query_service._reranker is fake_reranker


def test_poison_after_timeout_disables_local(tmp_path, monkeypatch):
    """Rule 4: a timed-out LOCAL inference poisons the captioner — every
    further caption returns an honest empty miss without touching the
    inference lock."""
    import time as _time

    cap = ic.LocalMoondreamCaptioner(str(tmp_path))
    state = {"calls": 0}

    class _Slow:
        def caption(self, img, length="short"):
            state["calls"] += 1
            _time.sleep(1.0)
            return {"caption": "late"}

    monkeypatch.setattr(cap, "load", lambda: _Slow())
    img = tmp_path / "p.png"
    img.write_bytes(_png_bytes())
    t0 = _time.monotonic()
    rec = ic.caption_image(cap, img, "image/png", "ee" * 32,
                           tmp_path / "pcache", 0.1)
    assert rec is None and _time.monotonic() - t0 < 0.6
    assert cap.poisoned, "timeout must poison the local captioner"
    # further caption: immediate honest miss, no second inference
    calls_before = state["calls"]
    rec2 = ic.caption_image(cap, img, "image/png", "ff" * 32,
                            tmp_path / "pcache", 5.0)
    assert rec2 is None and state["calls"] == calls_before


def test_inference_lock_serializes(tmp_path, monkeypatch):
    """Rule 3: never two local caption inferences in parallel — the lock
    caps observed concurrency at 1."""
    import threading
    import time as _time

    cap = ic.LocalMoondreamCaptioner(str(tmp_path))
    state = {"active": 0, "max_active": 0}

    class _Tracking:
        def caption(self, img, length="short"):
            state["active"] += 1
            state["max_active"] = max(state["max_active"], state["active"])
            _time.sleep(0.15)
            state["active"] -= 1
            return {"caption": "ok"}

    monkeypatch.setattr(cap, "load", lambda: _Tracking())
    img = tmp_path / "l.png"
    img.write_bytes(_png_bytes())
    results = []

    def worker(i):
        results.append(ic.caption_image(cap, img, "image/png", f"{i:064d}",
                                        tmp_path / "lcache", 10.0))

    ts = [threading.Thread(target=worker, args=(i,)) for i in range(3)]
    [t.start() for t in ts]
    [t.join() for t in ts]
    assert all(r is not None for r in results), results
    assert state["max_active"] == 1, f"parallel inference observed: {state}"


def test_cloud_fallback_helper(monkeypatch):
    from axiom_ng_runner.runner import _cloud_fallback_captioner

    monkeypatch.delenv("AXIOM_CAPTION_API_BASE", raising=False)
    monkeypatch.delenv("AXIOM_CAPTION_API_KEY", raising=False)
    assert _cloud_fallback_captioner() is None
    monkeypatch.setenv("AXIOM_CAPTION_API_BASE", "http://x/v1")
    monkeypatch.setenv("AXIOM_CAPTION_API_KEY", "sk")
    assert isinstance(_cloud_fallback_captioner(), ic.CloudCaptioner)


def test_load_gate_refuses_tight_memory(tmp_path, monkeypatch):
    """Rule 5: the load gate refuses when free memory is below the fp16
    requirement — CaptionerNotProvisioned instead of an OOM adventure."""
    cap = ic.LocalMoondreamCaptioner(str(tmp_path))
    cap.MIN_FREE_GB = 24.0

    class _Mem:
        available = 10 * 1024 ** 3  # 10 GB — far below 24

    import sys
    import types
    fake_psutil = types.ModuleType("psutil")
    fake_psutil.virtual_memory = lambda: _Mem()
    monkeypatch.setitem(sys.modules, "psutil", fake_psutil)
    import pytest as _pytest
    with _pytest.raises(ic.CaptionerNotProvisioned):
        cap.load()


def test_mrebel_release_is_wired_after_relationships():
    """Wiring pin for Rule 1: _release_mrebel() must actually be CALLED in
    the runner module (the registry test alone can't see a dropped call
    site). Probe: removing the call turns this red."""
    import ast
    import inspect
    import pathlib

    import axiom_ng_runner.runner as runner_mod
    src = pathlib.Path(inspect.getsourcefile(runner_mod)).read_text()
    tree = ast.parse(src)
    called = {
        n.func.id
        for n in ast.walk(tree)
        if isinstance(n, ast.Call) and isinstance(n.func, ast.Name)
    }
    assert "_release_mrebel" in called, (
        "_release_mrebel must be wired (Rule 1: mREBEL unloads after the "
        "relationships stage)")


def test_corrupt_cache_record_is_honest_miss(tmp_path):
    """C1 rider review: a corrupt cache record (JSON array) must be a
    miss, never an AttributeError that unwinds the stage and leaks the
    loaded model."""
    cache = tmp_path / "cc"
    cache.mkdir()
    (cache / ("aa" * 32 + ".json")).write_text('["not", "a", "dict"]')
    assert ic.load_cached_caption(cache, "aa" * 32) is None


def test_stage_unloads_on_midloop_exception(tmp_path, monkeypatch):
    """C1 rider review: load/unload symmetry on EVERY exit path — an
    exception mid-loop must still unload (try/finally), or the ~19 GB
    model stays resident: exactly what rule 1 forbids."""
    d = tmp_path / "artifacts"
    d.mkdir()
    arts = [_art("image-0000", "aa" * 32), _art("image-0001", "bb" * 32)]
    for a in arts:
        (d / a["ref"]).write_bytes(b"x" * 8)
    chunks = [{"text": "t", "metadata": {"image_refs": ["image-0000"]}}]

    cap = ic.LocalMoondreamCaptioner(str(tmp_path))
    events: list[str] = []
    monkeypatch.setattr(cap, "load", lambda: events.append("load"))
    monkeypatch.setattr(cap, "unload", lambda: events.append("unload"))

    import pytest as _pytest

    def exploding_caption(b, m):
        raise RuntimeError("mid-loop boom")

    monkeypatch.setattr(cap, "caption", exploding_caption)

    import os

    import axiom_ng_runner.runner as runner_mod
    real_settings = runner_mod.settings

    class _FS:
        @staticmethod
        def get():
            return real_settings.get()

    runner_mod.settings = _FS  # type: ignore[assignment]
    old_env = os.environ.get("AXIOM_CAPTION_CACHE_DIR")
    os.environ["AXIOM_CAPTION_CACHE_DIR"] = str(tmp_path / "cache3")
    real_resolve = ic.resolve_captioner
    ic.resolve_captioner = lambda: cap  # type: ignore[assignment]
    try:
        # caption_image swallows captioner exceptions (honest miss), so the
        # mid-loop exception is delivered via the artifact shape instead:
        del arts[1]["sha256"]  # KeyError mid-loop, after load()
        with _pytest.raises(KeyError):
            _caption_images_stage(
                {"extract_image_captions": True}, arts, chunks, tmp_path,
                {}, None, lambda cds: None,
            )
    finally:
        ic.resolve_captioner = real_resolve  # type: ignore[assignment]
        runner_mod.settings = real_settings  # type: ignore[assignment]
        if old_env is None:
            del os.environ["AXIOM_CAPTION_CACHE_DIR"]
        else:
            os.environ["AXIOM_CAPTION_CACHE_DIR"] = old_env
    assert events == ["load", "unload"], (
        f"mid-loop exception must still unload: {events}")


def test_cloud_switch_still_unloads_local(tmp_path, monkeypatch):
    """C2 rider review: the poison→cloud fallback must NOT leak the local
    model — the stage unloads the ORIGINAL captioner even after the
    captioner variable was rebound to the cloud fallback."""
    d = tmp_path / "artifacts"
    d.mkdir()
    arts = [_art("image-0000", "aa" * 32)]
    (d / "image-0000").write_bytes(_png_bytes())
    chunks = [{"text": "t", "metadata": {"image_refs": ["image-0000"]}}]

    cap = ic.LocalMoondreamCaptioner(str(tmp_path))
    cap._poisoned = True  # already poisoned: first image switches to cloud
    unloaded: list[bool] = []
    monkeypatch.setattr(cap, "load", lambda: None)
    monkeypatch.setattr(cap, "unload", lambda: unloaded.append(True))

    cloud = FakeCaptioner()
    import axiom_ng_runner.runner as runner_mod
    monkeypatch.setattr(runner_mod, "_cloud_fallback_captioner", lambda: cloud)

    import os
    real_settings = runner_mod.settings

    class _FS:
        @staticmethod
        def get():
            return real_settings.get()

    runner_mod.settings = _FS  # type: ignore[assignment]
    old_env = os.environ.get("AXIOM_CAPTION_CACHE_DIR")
    os.environ["AXIOM_CAPTION_CACHE_DIR"] = str(tmp_path / "cache4")
    real_resolve = ic.resolve_captioner
    ic.resolve_captioner = lambda: cap  # type: ignore[assignment]
    try:
        sc: dict = {}
        ran = _caption_images_stage(
            {"extract_image_captions": True}, arts, chunks, tmp_path,
            sc, None, lambda cds: None,
        )
    finally:
        ic.resolve_captioner = real_resolve  # type: ignore[assignment]
        runner_mod.settings = real_settings  # type: ignore[assignment]
        if old_env is None:
            del os.environ["AXIOM_CAPTION_CACHE_DIR"]
        else:
            os.environ["AXIOM_CAPTION_CACHE_DIR"] = old_env
    assert ran is True
    assert cloud.calls >= 1, "cloud fallback must caption after the poison"
    assert arts[0]["attributes"]["caption_path"] == "cloud"
    assert unloaded, "the ORIGINAL (local) captioner must be unloaded, not the cloud one"
