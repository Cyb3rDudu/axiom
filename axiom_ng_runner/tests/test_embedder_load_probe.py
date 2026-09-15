"""#266: embedder load probe + reload — a model whose weights materialized
on the meta device must fail AT LOAD TIME (reload once, then loud), not at
the first inference batch where every retry replays the broken state.

Production incident 2026-09-12: 7 force-rebuild jobs failed identically 3×
with "Cannot copy out of meta tensor" — the model "loaded" fine and only
exploded at the first batch. A runner restart healed it.

Mutation probes: remove the _verify_model_load() call in
TextEmbedder.__init__ and both tests below go red (constructor would
succeed on broken weights / no reload would happen).
"""

import pytest

pytest.importorskip("torch")
pytest.importorskip("numpy")
pytest.importorskip("FlagEmbedding")

from axiom_ng_runner.compute_core import embedder as emb


class _MetaBrokenModel:
    """First `fail` encode calls raise the production meta-tensor error."""

    def __init__(self, fail: int):
        self.fail = fail
        self.encodes = 0

    def encode(self, texts, **_kw):
        self.encodes += 1
        if self.encodes <= self.fail:
            raise RuntimeError(
                "Cannot copy out of meta tensor; no data! Please use "
                "torch.nn.Module.to_empty()"
            )
        return {"dense_vecs": [[0.5] * 16 for _ in texts]}


def _patch_models(monkeypatch, fails: list[int]):
    """Patch BGEM3FlagModel so instance #i fails its first fails[i] encodes."""
    made = []

    def _factory(_name, use_fp16=False):
        m = _MetaBrokenModel(fails[len(made)] if len(made) < len(fails) else 0)
        made.append(m)
        return m

    monkeypatch.setattr(emb, "BGEM3FlagModel", _factory)
    return made


def test_probe_reloads_once_and_recovers(monkeypatch):
    made = _patch_models(monkeypatch, [1])  # 1st instance broken, reload ok
    emb.TextEmbedder(device="cpu", enable_memory_management=False)
    assert len(made) == 2  # broken instance dropped, exactly one reload
    assert made[0].encodes == 1 and made[1].encodes == 1  # both were probed


def test_probe_fails_loud_after_reload(monkeypatch):
    made = _patch_models(monkeypatch, [99, 99])  # every encode explodes
    with pytest.raises(RuntimeError, match="load probe"):
        emb.TextEmbedder(device="cpu", enable_memory_management=False)
    assert len(made) == 2  # exactly one reload before the loud error


def test_probe_reload_construction_failure_is_loud(monkeypatch):
    """#266 NIT pin: the reload itself may fail (still under memory pressure) —
    that must land in the SAME loud load-probe error path (with the previous
    probe error kept in the message), not escape as a raw constructor error.
    Exactly one reload attempt."""
    calls = []

    def _factory(_name, use_fp16=False):
        calls.append(1)
        if len(calls) == 1:
            return _MetaBrokenModel(1)  # probe fails once, triggers reload
        raise RuntimeError("CUDA error: out of memory during reload")

    monkeypatch.setattr(emb, "BGEM3FlagModel", _factory)
    with pytest.raises(RuntimeError, match="reload failed during load probe"):
        emb.TextEmbedder(device="cpu", enable_memory_management=False)
    assert len(calls) == 2  # initial load + exactly one reload attempt
