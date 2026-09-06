"""#257 — caption dense backfill engine tests."""

from __future__ import annotations

import json
import os
import subprocess
import sys

import pytest
from axiom_ng_runner.compute_core.caption_backfill_cli import build_inputs, run


class FakeEmbedder:
    """Deterministic stand-in: dense vector = [len(text)] — good enough to
    prove which text was embedded (caption included or not)."""

    def embed_chunks(self, chunks):
        for c in chunks:
            c["embeddings"] = {"dense": [float(len(c["text"]))]}


SWIFT = "Figure 1. Annual SWIFT Messages in Millions*"


def _chunk(**over):
    c = {
        "chunk_id": "uuid-1",
        "text": "prose about the figure",
        "section_titles": ["Chapter"],
        "image_captions": {},
        "figure_captions": {"image-0000": SWIFT},
    }
    c.update(over)
    return c


def test_only_captioned_chunks_are_embedded_with_labels():
    items = build_inputs(
        [_chunk(), _chunk(chunk_id="uuid-2", figure_captions={}, image_captions={})]
    )
    assert [i["chunk_id"] for i in items] == ["uuid-1"]
    assert "[document figure caption: " + SWIFT + "]" in items[0]["text"]
    # section titles ride along (the ingest embedder prefixes them)
    assert items[0]["metadata"]["section_titles"] == ["Chapter"]


def test_run_emits_vectors_for_captioned_chunks():
    out = run([_chunk()], embedder=FakeEmbedder())
    assert out == [
        {
            "chunk_id": "uuid-1",
            "model": "BAAI/bge-m3",
            "dimensions": 1,
            "values": [
                float(
                    len(
                        "prose about the figure\n[document figure caption: "
                        + SWIFT
                        + "]"
                    )
                )
            ],
        }
    ]


def test_engine_failure_is_loud(monkeypatch):
    """#257 B probe: a broken engine must fail BEFORE emitting anything —
    no partial plan, so no mixed old/new vectors can ever land."""

    class Boom:
        def embed_chunks(self, chunks):
            raise RuntimeError("backend down")

    with pytest.raises(RuntimeError):
        run([_chunk()], embedder=Boom())


def test_cli_roundtrip(tmp_path):
    payload = json.dumps([_chunk()])
    env = dict(
        os.environ,
        PYTHONPATH=os.path.dirname(
            os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
        ),
    )
    proc = subprocess.run(
        [sys.executable, "-m", "axiom_ng_runner.compute_core.caption_backfill_cli"],
        input=payload,
        capture_output=True,
        text=True,
        cwd=str(tmp_path),
        env=env,
        check=False,
    )
    if proc.returncode != 0:
        err = proc.stderr.lower()
        heavy_model_unavailable = any(
            k in err
            for k in (
                "huggingface",
                "connection",
                "offline",
                "reach",
                "timed out",
                "maxretries",
                "urlerror",
                "errno",
                "resolve",
                "entry point",
                "httperror",
                "ssl",
            )
        )
        if heavy_model_unavailable and os.getenv("AXIOM_CAPTION_BACKFILL_IT") != "1":
            pytest.skip(
                "heavy BGE-M3 embedder not reachable in this environment "
                "(set AXIOM_CAPTION_BACKFILL_IT=1 to force)"
            )
        # anything else (KeyError / JSONDecodeError / protocol) is a REAL failure
    assert proc.returncode == 0, proc.stderr
    out = json.loads(proc.stdout)
    assert out and out[0]["chunk_id"] == "uuid-1"
