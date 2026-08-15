"""Hermetic query-endpoint tests (epic #130 R1 #131 / R2 #132).

Run against the ``reference`` compute backend: no torch, no FlagEmbedding,
no OpenSearch — deterministic stubs mirror the ingest reference semantics.
The integration proofs (OS-index roundtrip, warm p95 <150 ms, MPS rerank,
real-model quality smoke) live in test_query_endpoints_it.py behind
``AXIOM_QUERY_IT=1``.
"""

from __future__ import annotations

import sys

import pytest
from axiom_ng_runner import (
    CONTRACT_VERSION,
    DENSE_EMBEDDING_DIM,
    DENSE_EMBEDDING_MODEL,
    query_service,
)
from axiom_ng_runner.app import app
from axiom_ng_runner.config import Settings, settings
from fastapi.testclient import TestClient

REPO_ROOT = None  # conftest already puts the repo root on sys.path


@pytest.fixture()
def client(tmp_path):
    """Isolated work root + fresh query-service singletons per test."""
    old = settings.get()
    settings.set(Settings(work_root=tmp_path / "work"))
    query_service.reset()
    try:
        with TestClient(app) as c:
            yield c
    finally:
        query_service.reset()
        settings.set(old)


def _embed_payload(texts, **over):
    p = {"contract_version": CONTRACT_VERSION, "texts": texts}
    p.update(over)
    return p


# ---------------------------------------------------------------------------
# 1. Shape, determinism, reference-space consistency
# ---------------------------------------------------------------------------


def test_embed_shape_and_determinism(client):
    r = client.post("/v1/embed", json=_embed_payload(["kapitel eins", "nettes buch"]))
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["contract_version"] == CONTRACT_VERSION
    assert body["model"] == DENSE_EMBEDDING_MODEL
    assert body["dimensions"] == DENSE_EMBEDDING_DIM
    assert len(body["embeddings"]) == 2
    assert all(len(v) == DENSE_EMBEDDING_DIM for v in body["embeddings"])
    # Deterministic: same text -> same vector.
    r2 = client.post("/v1/embed", json=_embed_payload(["kapitel eins"]))
    assert r2.json()["embeddings"][0] == body["embeddings"][0]
    # Distinct texts -> distinct vectors.
    assert body["embeddings"][0] != body["embeddings"][1]


def test_embed_reference_shares_ingest_vector_space(client):
    """The reference stub must produce the SAME vectors the ingest reference
    backend produces for identical text — the hermetic stand-in for the real
    'query and chunks live in one vector space' property (BGE-M3 symmetry,
    proven against the OS index by the roundtrip IT)."""
    from axiom_ng_runner.runner import _dense_embedding

    text = "The quick brown fox jumps over the lazy dog."
    r = client.post("/v1/embed", json=_embed_payload([text]))
    ingest_vec = _dense_embedding({"text": text})["values"]
    assert r.json()["embeddings"][0] == ingest_vec


def test_embed_single_text_and_type_safety(client):
    r = client.post("/v1/embed", json=_embed_payload(["eins"]))
    assert r.status_code == 200
    assert all(isinstance(x, float) for x in r.json()["embeddings"][0])


# ---------------------------------------------------------------------------
# 2. Guards (mutation-probe targets: each guard has a dedicated probe)
# ---------------------------------------------------------------------------


def test_embed_guard_empty_texts(client):
    r = client.post("/v1/embed", json=_embed_payload([]))
    assert r.status_code == 422
    assert r.json()["detail"]["code"] == "QUERY_TEXTS_EMPTY"


def test_embed_guard_blank_text(client):
    r = client.post("/v1/embed", json=_embed_payload(["ok", "   "]))
    assert r.status_code == 422
    assert r.json()["detail"]["code"] == "QUERY_TEXT_BLANK"


def test_embed_guard_too_many_texts(client):
    r = client.post("/v1/embed", json=_embed_payload([f"t{i}" for i in range(17)]))
    assert r.status_code == 422
    assert r.json()["detail"]["code"] == "QUERY_TEXTS_TOO_MANY"


def test_embed_guard_max_texts_request_cap(client):
    # max_texts may lower the cap; exceeding the per-request cap must 422.
    r = client.post(
        "/v1/embed", json=_embed_payload(["a", "b", "c"], max_texts=2)
    )
    assert r.status_code == 422
    assert r.json()["detail"]["code"] == "QUERY_TEXTS_TOO_MANY"
    # ...and a max_texts above the server cap must not raise the cap.
    r2 = client.post(
        "/v1/embed", json=_embed_payload([f"t{i}" for i in range(17)], max_texts=99)
    )
    assert r2.status_code == 422
    assert r2.json()["detail"]["code"] == "MAX_TEXTS_INVALID"


def test_embed_guard_contract_version(client):
    r = client.post("/v1/embed", json=_embed_payload(["x"], contract_version="2.0"))
    assert r.status_code == 422
    assert r.json()["detail"]["code"] == "CONTRACT_VERSION_UNSUPPORTED"


def test_embed_guard_unknown_field_and_types(client):
    r = client.post("/v1/embed", json=_embed_payload(["x"], bogus=1))
    assert r.status_code == 422  # extra=forbid
    r2 = client.post("/v1/embed", json={"contract_version": CONTRACT_VERSION, "texts": "nope"})
    assert r2.status_code == 422  # texts must be a list


# ---------------------------------------------------------------------------
# 3. Warm-keep + lazy-load (core requirement #131)
# ---------------------------------------------------------------------------


def test_embed_warm_keep_single_load(client):
    """Two requests, exactly one model load: the warm-keep contract."""
    assert query_service.stats() == {"embedder_loads": 0, "embedder_warm": False}
    client.post("/v1/embed", json=_embed_payload(["erstes"]))
    mid = query_service.stats()
    client.post("/v1/embed", json=_embed_payload(["zweites", "drittes"]))
    end = query_service.stats()
    assert mid["embedder_loads"] == 1
    assert end["embedder_loads"] == 1  # no reload between requests
    assert end["embedder_warm"] is True


def test_embed_lazy_no_heavy_imports(client):
    """Lazy-load proof in reference mode: serving /v1/embed must not pull
    torch/FlagEmbedding/compute_core into the process."""
    import axiom_ng_runner.app  # noqa: F401 — baseline

    before = set(sys.modules)
    client.post("/v1/embed", json=_embed_payload(["probe"]))
    heavy = sorted(
        m
        for m in (set(sys.modules) - before)
        if m.split(".")[0] in ("torch", "FlagEmbedding", "transformers", "numpy")
        or m.startswith("axiom_ng_runner.compute_core")
    )
    assert not heavy, f"heavy imports leaked into reference embed: {heavy}"


def test_embed_shape_mismatch_surfaces_as_500(client, monkeypatch):
    """Mutation probe: if the model layer ever returns vectors that disagree
    with the declared capability dimensions, the endpoint must fail loudly
    (500) instead of handing back a poisoned vector."""
    import axiom_ng_runner.query_service as qs

    class _Broken:
        def embed_queries_dense(self, texts):
            return [[0.0] * (DENSE_EMBEDDING_DIM + 1) for _ in texts]

    monkeypatch.setattr(qs, "get_query_embedder", lambda: _Broken())
    r = client.post("/v1/embed", json=_embed_payload(["x"]))
    assert r.status_code == 500
    assert r.json()["detail"]["code"] == "EMBEDDING_SHAPE_MISMATCH"


# ---------------------------------------------------------------------------
# 4. Capabilities extension (R4 dependency)
# ---------------------------------------------------------------------------


def test_capabilities_report_query_embedding(client):
    caps = client.get("/v1/capabilities").json()
    assert caps["features"]["query_embedding"] is True
    assert caps["models"]["query_embedding"] == {
        "name": DENSE_EMBEDDING_MODEL,
        "dimensions": DENSE_EMBEDDING_DIM,
    }
    assert caps["limits"]["max_query_texts"] >= 1
