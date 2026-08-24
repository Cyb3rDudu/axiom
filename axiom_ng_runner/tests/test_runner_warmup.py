"""#216 runner warmup (cold-start preload of the query models).

Covers the three pieces of the warmup work order:
  1. flag behavior — AXIOM_PROCESSOR_WARMUP gates the eager preload (and
     warmup=False falls back to the historical lazy-load);
  2. honest readiness — /v1/health and /v1/capabilities report the real
     loaded state (models_warmed), and the runner is never skipped forever
     while merely warming (health goes green once the preload finishes);
  3. the cold-start-with-warmup pin — the FIRST real embed/rerank request is
     served warm (waits out the background preload instead of paying the cold
     load in-band), proven with an INJECTABLE slow load step — no real 90s
     model in the test.

The \"real\" backend is only exercised here with monkeypatched builders, so no
torch/GPU is needed. Run with ``.venv/bin/python -m pytest tests/test_runner_warmup.py``.
"""

from __future__ import annotations

import time

import pytest
from axiom_ng_runner import DENSE_EMBEDDING_DIM, query_service
from axiom_ng_runner.app import app
from axiom_ng_runner.config import Settings, settings
from fastapi.testclient import TestClient


class _SlowFakeEmbedder:
    """Reference-shaped vector stub whose BUILD takes load_ms."""

    model_name = "fake-bge-m3"

    def __init__(self, load_ms: int):
        self._load_ms = load_ms
        if load_ms > 0:
            time.sleep(load_ms / 1000.0)

    def embed_queries_dense(self, texts):
        return [
            [float(ord(t[0]) % DENSE_EMBEDDING_DIM)] * DENSE_EMBEDDING_DIM
            for t in texts
        ]

    def embed_queries_with_sparse(self, texts):
        return self.embed_queries_dense(texts), [{} for _ in texts]


class _SlowFakeReranker:
    model_name = "fake-reranker"

    def __init__(self, load_ms: int):
        self._load_ms = load_ms
        if load_ms > 0:
            time.sleep(load_ms / 1000.0)

    def rerank(self, query, texts, top_n=None):
        out = [{"index": i, "score": 1.0 / (i + 1)} for i in range(len(texts))]
        return out[:top_n] if top_n is not None else out


def _vcard_embedder(load_ms: int):
    def _build():
        return _SlowFakeEmbedder(load_ms)

    return _build


def _vcard_reranker(load_ms: int):
    def _build():
        return _SlowFakeReranker(load_ms)

    return _build


def _start_warmup_and_wait(timeout: float = 5.0) -> None:
    """Kick the background preload and block until it finishes."""
    query_service.start_warmup()
    query_service.await_warmup()
    deadline = time.monotonic() + timeout
    while not query_service.warmup_status()["warmup_finished"]:
        if time.monotonic() > deadline:
            break
        time.sleep(0.005)


@pytest.fixture()
def client():
    """A bare TestClient. Warmup state is managed per-test (each test resets
    settings + singletons and drives warmup explicitly), so this fixture just
    gives the handle — reference backend default."""
    with TestClient(app) as c:
        yield c


# ---------------------------------------------------------------------------
# 1. Flag behavior
# ---------------------------------------------------------------------------


def test_warmup_flag_false_disables_eager_preload(tmp_path):
    """AXIOM_PROCESSOR_WARMUP=False disables the eager preload: a fresh runner
    starts with no model loaded (the historical lazy behavior)."""
    old = settings.get()
    settings.set(Settings(work_root=tmp_path / "work", warmup=False))
    query_service.reset()
    try:
        query_service.start_warmup()
        st = query_service.warmup_status()
        assert st["warmup_enabled"] is False
        # Nothing planned, so await never blocks.
        assert st["warmup_finished"] is True
        assert st["models_warmed"] is False
        assert query_service.stats()["embedder_loads"] == 0
    finally:
        query_service.reset()
        settings.set(old)


def test_warmup_flag_true_default_marks_finished_in_reference(tmp_path):
    """Default (WARMUP=1): reference mode has nothing to warm — warmup is
    marked finished so await_warmup never blocks; models load lazily (the
    instant stubs), exactly as before this change."""
    old = settings.get()
    settings.set(Settings(work_root=tmp_path / "work", warmup=True))
    query_service.reset()
    try:
        query_service.start_warmup()
        assert query_service.warmup_status()["warmup_finished"] is True
        assert query_service.warmup_status()["models_warmed"] is False
        # First use loads the instant stub; still one load total.
        query_service.get_query_embedder()
        assert query_service.stats()["embedder_loads"] == 1
    finally:
        query_service.reset()
        settings.set(old)


# ---------------------------------------------------------------------------
# 2. Honest readiness (health + capabilities)
# ---------------------------------------------------------------------------


def test_health_ok_and_capabilities_surface_models_warmed_reference():
    """Reference backend: health is green; models_warmed reflects the true
    loaded state (False until first use, since nothing is eager-loaded)."""
    old = settings.get()
    settings.set(Settings(work_root=None, warmup=True))
    query_service.reset()
    try:
        with TestClient(app) as c:
            h = c.get("/v1/health")
            assert h.status_code == 200
            assert h.json()["status"] == "ok"
            assert h.json()["models_warmed"] is False
            caps = c.get("/v1/capabilities").json()
            assert caps["warmup_enabled"] is True
            assert caps["models_warmed"] is False
    finally:
        query_service.reset()
        settings.set(old)


def test_real_backend_warming_health_then_green(monkeypatch, tmp_path):
    """The #216 readiness contract: while the real-model preload is in flight
    the runner reports warming (not silently 'capable'); once it finishes it
    goes green — a merely-warming runner is never skipped forever by the #207
    health monitor. Injected slow load; no real 90s model."""
    old = settings.get()
    settings.set(
        Settings(work_root=tmp_path / "work", compute_backend="real", warmup=True)
    )
    query_service.reset()
    monkeypatch.setattr(query_service, "_build_embedder", _vcard_embedder(80))
    monkeypatch.setattr(query_service, "_build_reranker", _vcard_reranker(80))
    try:
        _start_warmup_and_wait()
        st = query_service.warmup_status()
        assert st["warmup_finished"] is True
        assert st["models_warmed"] is True
        assert st["warmup_failed"] is False
        # Single eager load each — no lazy reload.
        assert query_service.stats()["embedder_loads"] == 1
        assert query_service.stats()["reranker_loads"] == 1
    finally:
        query_service.reset()
        settings.set(old)


def test_health_answers_warming_503_promptly_mid_preload(client, monkeypatch, tmp_path):
    """Mid-preload the runner must answer /v1/health FAST with 503 warming:
    the #207 probe times out at 5s, so a health call that blocks on the
    model-build lock reads a merely-warming runner as DOWN — the exact
    failure warmup_status's lock-free read guards against. Then, once the
    preload finishes, health goes green (never skipped forever)."""
    old = settings.get()
    settings.set(
        Settings(work_root=tmp_path / "work", compute_backend="real", warmup=True)
    )
    query_service.reset()
    monkeypatch.setattr(query_service, "_build_embedder", _vcard_embedder(1500))
    monkeypatch.setattr(query_service, "_build_reranker", _vcard_reranker(10))
    try:
        query_service.start_warmup()  # 1.5s embedder load now in flight
        t0 = time.monotonic()
        h = client.get("/v1/health")
        elapsed = time.monotonic() - t0
        assert h.status_code == 503, h.text
        assert h.json()["status"] == "warming"
        assert h.json()["models_warmed"] is False
        # Prompt: answered long before the 1.5s in-flight build could elapse
        # (a lock-blocking read would take >= 1.5s here; the probe budget 5s).
        assert elapsed < 0.5, f"health took {elapsed * 1000:.0f}ms mid-warmup"
        query_service.await_warmup()
        h2 = client.get("/v1/health")
        assert h2.status_code == 200
        assert h2.json() == {"status": "ok", "models_warmed": True}
    finally:
        query_service.reset()
        settings.set(old)


# ---------------------------------------------------------------------------
# 3. Cold-start-with-warmup time-budget pin (injectable slow load)
# ---------------------------------------------------------------------------


def test_cold_start_warmup_first_request_is_warm_on_budget(client, monkeypatch, tmp_path):
    """The heart of #216: after a cold start with warmup, the FIRST real
    embed/rerank request is served warm — it inherits the background preload
    (simulated by an injectable slow load) instead of paying the cold load
    in-band. Proves: one load per model, no second load on the first request."""
    old = settings.get()
    settings.set(
        Settings(work_root=tmp_path / "work", compute_backend="real", warmup=True)
    )
    query_service.reset()
    monkeypatch.setattr(query_service, "_build_embedder", _vcard_embedder(70))
    monkeypatch.setattr(query_service, "_build_reranker", _vcard_reranker(70))
    try:
        _start_warmup_and_wait()
        assert query_service.warmup_status()["models_warmed"] is True
        r = client.post(
            "/v1/embed", json={"contract_version": "1.0", "texts": ["kapitel eins"]}
        )
        assert r.status_code == 200, r.text
        # The first real request did NOT trigger a second load.
        assert query_service.stats()["embedder_loads"] == 1
    finally:
        query_service.reset()
        settings.set(old)


def test_request_during_warmup_waits_instead_of_cold_loading(client, monkeypatch, tmp_path):
    """If a request arrives WHILE the preload is still running, the endpoint
    awaits warmup rather than racing a cold load of its own — the
    first-request-warm guarantee even for an unlucky request timing."""
    old = settings.get()
    settings.set(
        Settings(work_root=tmp_path / "work", compute_backend="real", warmup=True)
    )
    query_service.reset()
    monkeypatch.setattr(query_service, "_build_embedder", _vcard_embedder(120))
    monkeypatch.setattr(query_service, "_build_reranker", _vcard_reranker(120))
    try:
        query_service.start_warmup()  # ~120ms preload now in flight
        t0 = time.monotonic()
        with TestClient(app) as c:
            r = c.post(
                "/v1/embed", json={"contract_version": "1.0", "texts": ["eins"]}
            )
        elapsed = time.monotonic() - t0
        assert r.status_code == 200, r.text
        # The request waited out the preload; it did NOT add a second load.
        assert query_service.stats()["embedder_loads"] == 1
        # Budget: completes at warmup speed, far below a cold second load.
        assert elapsed < 2.0, f"request took {elapsed * 1000:.0f}ms past warmup"
    finally:
        query_service.reset()
        settings.set(old)


def test_capabilities_readiness_field_for_rag(client, monkeypatch, tmp_path):
    """The RAG reads models_warmed from /v1/capabilities to distinguish a warm
    runner from a merely-declared-capable one."""
    old = settings.get()
    settings.set(
        Settings(work_root=tmp_path / "work", compute_backend="real", warmup=True)
    )
    query_service.reset()
    monkeypatch.setattr(query_service, "_build_embedder", _vcard_embedder(10))
    monkeypatch.setattr(query_service, "_build_reranker", _vcard_reranker(10))
    try:
        _start_warmup_and_wait()
        caps = client.get("/v1/capabilities").json()
        assert caps["warmup_enabled"] is True
        assert caps["models_warmed"] is True
    finally:
        query_service.reset()
        settings.set(old)
