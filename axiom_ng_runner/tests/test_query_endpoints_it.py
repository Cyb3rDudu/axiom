"""Integration proofs for the query endpoints (epic #130, R1 #131 / R2 #132).

NOT part of the hermetic suite: these run the REAL BGE-M3 stack (torch,
FlagEmbedding, MPS/CUDA) against the REAL local OpenSearch index
(axiom-ng-chunks-v1, 4813 chunks) — the roundtrip, latency and quality
evidence Hivemind verifies. No stubs anywhere.

Run with:
    AXIOM_QUERY_IT=1 AXIOM_PROCESSOR_COMPUTE=real \
    .venv/bin/python -m pytest tests/test_query_endpoints_it.py -v -s

Skipped silently otherwise. OS connection via plain HTTP (httpx) — the
runner itself never talks to a durable store (contract ownership boundary).
"""

from __future__ import annotations

import math
import os
import statistics
import time

import httpx
import pytest
from axiom_ng_runner import DENSE_EMBEDDING_DIM, RERANKER_MODEL

if os.getenv("AXIOM_QUERY_IT") != "1":
    pytest.skip("AXIOM_QUERY_IT=1 required (real models + real OS index)", allow_module_level=True)

if DENSE_EMBEDDING_DIM != 1024:
    pytest.skip(
        "start pytest with AXIOM_PROCESSOR_COMPUTE=real "
        "(module constants are resolved at import time)",
        allow_module_level=True,
    )

OS_URL = os.getenv("AXIOM_OPENSEARCH_URL", "http://127.0.0.1:9200").rstrip("/")
OS_INDEX = "axiom-ng-chunks-v1"

# Hermetic-suite compatibility: the app module must exist, but singletons
# reset per test keeps the warm-keep accounting honest.
from axiom_ng_runner import query_service
from axiom_ng_runner.app import app
from axiom_ng_runner.config import Settings, settings
from fastapi.testclient import TestClient


@pytest.fixture(scope="module")
def client(tmp_path_factory):
    old = settings.get()
    settings.set(
        Settings(work_root=tmp_path_factory.mktemp("query_it_work"), compute_backend="real")
    )
    query_service.reset()
    try:
        with TestClient(app) as c:  # first request lazy-loads the models
            yield c
    finally:
        query_service.reset()
        settings.set(old)


def _os_search(payload: dict) -> dict:
    r = httpx.post(f"{OS_URL}/{OS_INDEX}/_search", json=payload, timeout=30)
    r.raise_for_status()
    return r.json()


def _pick_chunk(min_len: int = 300) -> dict:
    """Prose chunk to build a self-retrieval query from.

    Prefers real prose: markdown headings / TOC entries are near-duplicate
    saturated in this corpus (same book title in TOC, preface, and cover
    chunks), which buries the exact source chunk behind content-identical
    hits and makes the rank oracle noisy without saying anything about
    query-mode correctness.
    """
    d = _os_search({"size": 100, "query": {"match_all": {}}})
    fallback = None
    for h in d["hits"]["hits"]:
        src = h["_source"]
        t = src.get("text", "")
        if len(t) < min_len:
            continue
        stripped = t.lstrip()
        prose = (
            not stripped.startswith(("#", "<", "*", "-"))
            and "<span" not in t[:120]
            and "<br" not in t[:120]
            and "| " not in t[:120]  # author-directory table rows
        )
        if prose:
            return {"id": h["_id"], **src}
        fallback = fallback or {"id": h["_id"], **src}
    if fallback is not None:
        return fallback
    raise AssertionError("no chunk with enough text found in the OS index")


# ---------------------------------------------------------------------------
# R1 (#131): OS roundtrip — query embedding must land in the chunk space
# ---------------------------------------------------------------------------


def test_os_roundtrip_known_chunk(client):
    chunk = _pick_chunk()
    # Query = leading sentences of a known chunk (self-retrieval oracle).
    # Build up to ~200 chars / min 80: a single-word first sentence carries
    # no retrieval signal and buries the source chunk behind content hits
    # that genuinely match the word better.
    sentences = [s for s in chunk["text"].split(".") if s.strip()]
    query = ""
    for s in sentences:
        query = (query + " " + s.strip()).strip()[:200]
        if len(query) >= 80:
            break

    r = client.post(
        "/v1/embed", json={"contract_version": "1.0", "texts": [query]}
    )
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["model"] == "BAAI/bge-m3" and body["dimensions"] == 1024
    vec = body["embeddings"][0]

    # k=50: the corpus carries near-duplicates (TOC vs. preface of the same
    # book), so several chunks with essentially the query's content can
    # outrank the exact source chunk. Exact hit within top-50 plus a top-1
    # with high token overlap is the honest quality bar; the strict vector-
    # space proof is the direct cosine below.
    d = _os_search(
        {"size": 50, "query": {"knn": {"embedding": {"vector": vec, "k": 50}}}}
    )
    hits = d["hits"]["hits"]
    ids = [h["_id"] for h in hits]
    rank = ids.index(chunk["id"]) + 1 if chunk["id"] in ids else None
    assert rank is not None and rank <= 50, (
        f"known chunk {chunk['id']} not in top-50 (rank={rank})"
    )

    # Top-1 must carry the query's content (near-duplicate semantics), not
    # just be geometrically close.
    q_tokens = {t for t in query.lower().split() if len(t) > 3}
    top1_tokens = {
        t for t in hits[0]["_source"]["text"].lower().split() if len(t) > 3
    }
    overlap = len(q_tokens & top1_tokens) / max(1, len(q_tokens))
    assert overlap >= 0.6, (
        f"top-1 overlap {overlap:.2f} too low — geometric hit, wrong content"
    )

    # Raw cosine against the stored chunk embedding: THE proof that query
    # and chunk vectors live in one space (symmetric query mode correct).
    stored = chunk["embedding"]
    dot = sum(a * b for a, b in zip(vec, stored, strict=True))
    cos = dot / (
        math.sqrt(sum(a * a for a in vec)) * math.sqrt(sum(b * b for b in stored))
    )
    print(
        f"\n[IT] roundtrip: query[:60]={query[:60]!r} cos={cos:.4f} "
        f"rank={rank} top1-overlap={overlap:.2f}"
    )
    assert cos > 0.5, f"cosine {cos:.4f} implausibly low for self-retrieval"


def test_warm_p95_latency_budget(client):
    """DoD: warm p95 < 150 ms for <=3 texts (MPS locally)."""
    texts_pool = [
        ["Was ist Corporate Social Responsibility?"],
        ["Nachhaltigkeitsberichterstattung in deutschen Unternehmen", "CSRD Regulierung"],
        ["Wie beeinflussen Stakeholder die Unternehmensethik?", "Lieferkettengesetz", "ESG-Rating"],
    ]
    # Warm-up (model resident, kernels compiled): 3 calls.
    for texts in texts_pool:
        assert (
            client.post("/v1/embed", json={"contract_version": "1.0", "texts": texts}).status_code
            == 200
        )
    durations = []
    for i in range(30):
        texts = texts_pool[i % len(texts_pool)]
        t0 = time.perf_counter()
        r = client.post("/v1/embed", json={"contract_version": "1.0", "texts": texts})
        durations.append(time.perf_counter() - t0)
        assert r.status_code == 200
    durations.sort()
    # Nearest-rank p95: for n=30 that is sorted[28] (ceil(0.95*30)=29th
    # value), not the lenient sorted[27] (~p90).
    p95 = durations[math.ceil(0.95 * len(durations)) - 1]
    import torch

    dev = "mps" if torch.backends.mps.is_available() else ("cuda" if torch.cuda.is_available() else "cpu")
    print(
        f"\n[IT] /v1/embed warm latency on {dev}: p50={statistics.median(durations)*1000:.1f}ms "
        f"p95={p95*1000:.1f}ms max={durations[-1]*1000:.1f}ms (n=30)"
    )
    assert p95 < 0.150, f"p95 {p95*1000:.1f}ms exceeds the 150ms budget"


def test_embed_warm_keep_it(client):
    for _ in range(3):
        client.post("/v1/embed", json={"contract_version": "1.0", "texts": ["warm?"]})
    st = query_service.stats()
    assert st["embedder_loads"] == 1 and st["embedder_warm"] is True


# ---------------------------------------------------------------------------
# R2 (#132): quality smoke + MPS proof
# ---------------------------------------------------------------------------


def _quality_candidates() -> tuple[list[str], list[str]]:
    """5 relevant CSR chunks + 15 distractors from other topics."""
    rel = _os_search(
        {
            "size": 5,
            "query": {"match": {"text": "Corporate Social Responsibility Nachhaltigkeit"}},
            "_source": ["text"],
        }
    )["hits"]["hits"]
    dis = _os_search(
        {
            "size": 15,
            "query": {
                "bool": {
                    "must_not": [
                        {"match_phrase": {"text": "Corporate Social Responsibility"}},
                        {"match": {"text": "Nachhaltigkeit"}},
                    ]
                }
            },
            "_source": ["text"],
        }
    )["hits"]["hits"]
    relevant = [h["_source"]["text"][:1200] for h in rel]
    distractors = [h["_source"]["text"][:1200] for h in dis]
    return relevant, distractors


def test_rerank_quality_smoke(client):
    """DoD: >=4/5 relevant candidates in the top-5 after rerank."""
    relevant, distractors = _quality_candidates()
    assert len(relevant) == 5 and len(distractors) == 15

    # Shuffle deterministically so relevant indices are spread out.
    texts, want = [], set()
    for i in range(20):
        if i % 4 == 0 and relevant:
            texts.append(relevant.pop())
            want.add(i)
        else:
            texts.append(distractors.pop())
    query = "Corporate Social Responsibility und Nachhaltigkeit von Unternehmen"

    t0 = time.perf_counter()
    r = client.post(
        "/v1/rerank",
        json={"contract_version": "1.0", "query": query, "texts": texts, "top_n": 20},
    )
    dt = time.perf_counter() - t0
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["model"] == RERANKER_MODEL
    top5 = {e["index"] for e in body["scores"][:5]}
    hits_in_top5 = len(top5 & want)
    print(
        f"\n[IT] rerank 20 pairs on MPS: {dt:.2f}s total, "
        f"{hits_in_top5}/5 relevant in top-5, top score {body['scores'][0]['score']:.4f}"
    )
    assert hits_in_top5 >= 4, (
        f"only {hits_in_top5}/5 relevant in top-5 — ranking broken?\n"
        f"ranking={[e['index'] for e in body['scores']]}"
    )


def test_rerank_warm_keep_it(client):
    """Order-independent: this request must never trigger a SECOND load;
    a first cold load (when run alone against the module fixture) is fine."""
    st_before = query_service.stats()
    client.post(
        "/v1/rerank",
        json={"contract_version": "1.0", "query": "nochmal", "texts": ["a", "b"], "top_n": 2},
    )
    st = query_service.stats()
    assert st["reranker_loads"] in (
        st_before["reranker_loads"],
        st_before["reranker_loads"] + 1,
    )
    assert st["reranker_warm"] is True
