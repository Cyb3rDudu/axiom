"""#387 image-caption backfill CLI: partial caption recovery for active
snapshots without a full re-ingest. Hash-gate reuse, reason buckets,
exit codes."""

from __future__ import annotations

import base64
import json

import pytest

pytest.importorskip("torch")

from axiom_compute_worker.compute_core import image_caption_backfill_cli as cli
from axiom_compute_worker.compute_core import image_captioner as ic


class FakeCap:
    model = "fake-vision-1"

    def __init__(self, fail_refs: set[str] | None = None):
        self.fail_refs = fail_refs or set()
        self.calls: list[str] = []

    def caption(self, image_bytes: bytes, media_type: str) -> tuple[str, str]:
        # identify by size so different images are distinguishable
        self.calls.append(f"{len(image_bytes)}")
        if len(image_bytes) == 3:
            raise _httpx_429()
        return f"caption of {len(image_bytes)} bytes", "cloud"


def _httpx_429():
    import httpx

    req = httpx.Request("POST", "http://x/v1/chat/completions")
    resp = httpx.Response(429, request=req)
    return httpx.HTTPStatusError("429", request=req, response=resp)


def _item(ref: str, sha: str, blob: bytes) -> dict:
    return {"ref": ref, "sha256": sha, "media_type": "image/png",
            "image_b64": base64.b64encode(blob).decode()}


def test_backfill_captions_misses_and_itemizes_failures(tmp_path, monkeypatch):
    cap_holder: dict[str, FakeCap] = {}

    def _make():
        cap_holder["cap"] = FakeCap()
        return cap_holder["cap"]

    monkeypatch.setattr(cli, "resolve_captioner", _make)
    items = [
        _item("image-0000", "aa" * 32, b"image-one"),
        _item("image-0001", "bb" * 32, b"abc"),  # 3 bytes → 429 path
    ]
    results, counts = cli.backfill(
        items, cache_dir=tmp_path / "cache", sleep=lambda _s: None,
        retries=1, backoff_base=0.0,
    )
    assert results[0]["caption"].startswith("caption of")
    assert results[0]["path"] == "cloud"
    assert results[1]["reason"].startswith("rate_limited")
    assert counts == {"rate_limited": 1}


def test_backfill_hash_gate_skips_cached(tmp_path, monkeypatch):
    cache = tmp_path / "cache"
    cache.mkdir()
    (cache / ("aa" * 32 + ".json")).write_text(json.dumps(
        {"caption": "cached", "model": "m", "path": "cloud"}))
    cap = FakeCap()
    monkeypatch.setattr(cli, "resolve_captioner", lambda: cap)
    results, counts = cli.backfill(
        [_item("image-0000", "aa" * 32, b"zzz")],
        cache_dir=cache, sleep=lambda _s: None,
    )
    assert results[0]["caption"] == "cached"
    assert counts == {}
    assert cap.calls == [], "hash-gated: the captioner must not be called"


def test_backfill_no_captioner_is_engine_failure(monkeypatch):
    monkeypatch.delenv("AXIOM_CAPTION_API_BASE", raising=False)
    monkeypatch.delenv("AXIOM_CAPTION_API_KEY", raising=False)
    monkeypatch.delenv("AXIOM_CAPTION_LOCAL_MODEL_DIR", raising=False)
    with pytest.raises(RuntimeError):
        cli.backfill([_item("image-0000", "aa" * 32, b"x")])


def test_backfill_missing_image_data_bucket(tmp_path, monkeypatch):
    monkeypatch.setattr(cli, "resolve_captioner", lambda: FakeCap())
    results, counts = cli.backfill(
        [{"ref": "image-0000", "sha256": "aa" * 32, "media_type": "image/png"}],
        cache_dir=tmp_path / "cache", sleep=lambda _s: None,
    )
    assert results[0]["reason"].startswith("no_image_data")
    assert counts["no_image_data"] == 1


def test_main_exit_codes(tmp_path, monkeypatch, capsys):
    monkeypatch.setattr(cli, "resolve_captioner", lambda: FakeCap())
    monkeypatch.setattr(cli, "backfill", lambda items: (
        [{"ref": "image-0000", "reason": "rate_limited"}], {"rate_limited": 1}
    ))
    import io

    monkeypatch.setattr("sys.stdin", io.StringIO("[{\"ref\": \"image-0000\", ...}]"))
    # patch json.load via stdin content validity: simpler to feed valid JSON
    monkeypatch.setattr("sys.stdin", io.StringIO(
        "[{\"ref\": \"image-0000\", \"sha256\": \"" + "aa" * 32 + "\"}]"))
    assert cli.main() == 2
    out = json.loads(capsys.readouterr().out)
    assert out == [{"ref": "image-0000", "reason": "rate_limited"}]
    # all-success → 0
    monkeypatch.setattr(cli, "backfill", lambda items: (
        [{"ref": "r", "caption": "c", "model": "m", "path": "cloud"}], {}
    ))
    monkeypatch.setattr("sys.stdin", io.StringIO(
        "[{\"ref\": \"r\", \"sha256\": \"" + "bb" * 32 + "\"}]"))
    assert cli.main() == 0


def test_local_captioner_unloaded_after_backfill(tmp_path, monkeypatch):
    unloaded: list[bool] = []

    class FakeLocal:
        model = "local-fake"

        def load(self):
            return None

        def unload(self):
            unloaded.append(True)

        def caption(self, b, m):
            return "local caption", "local"

    monkeypatch.setattr(cli, "resolve_captioner", lambda: FakeLocal())
    results, counts = cli.backfill(
        [_item("image-0000", "cc" * 32, b"img")],
        cache_dir=tmp_path / "cache", sleep=lambda _s: None,
    )
    assert counts == {}
    assert results[0]["path"] == "local"
    assert unloaded, "Rule 1: the INGEST-class captioner unloads after backfill"
