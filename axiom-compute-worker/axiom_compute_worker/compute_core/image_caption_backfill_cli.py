"""#387 — image-caption backfill engine (operational CLI).

Recovers MISSING machine captions for the extracted_image artifacts of an
ACTIVE snapshot — the cheap repair lane after a caption phase lost
requests (silently dropped API failures, budget tail cutoff). No re-ingest:
image extraction is done, the artifact bytes live in the store; the
operator feeds them here (stdin JSON), the engine captions the misses
under the existing sha256 hash-gate cache (previously captioned images
cost nothing) and emits a per-ref result with reason buckets for whatever
remains missing.

Protocol (#233 pattern): stdin = JSON array of
  {"ref", "sha256", "media_type",            # required
   "image_b64": "...",                       # artifact bytes (base64)
   | "image_path": "/store/.../image.png"}   # ...or a readable path
stdout = JSON array of
  {"ref", "caption", "model", "path"}        # success (path: cloud|local)
  {"ref", "reason": "rate_limited|..."}      # honest miss, itemized
Exit codes: 0 = every input captioned; 2 = partial (apply the successes,
re-run for the itemized remainder); 1 = engine failure (no captioner
configured, unreadable input) — nothing usable is emitted.

After applying the artifact attributes + chunk image_captions (runbook),
the dense arm catches up via the existing caption-backfill CLI (#257).
"""

from __future__ import annotations

import base64
import json
import logging
import os
import sys
import tempfile
from pathlib import Path
from typing import Any

from axiom_compute_worker.compute_core.image_captioner import (
    DEFAULT_CACHE_DIR,
    caption_image_detailed,
    resolve_captioner,
)

log = logging.getLogger(__name__)


def _decode_image(item: dict[str, Any]) -> tuple[bytes, str | None]:
    """Artifact bytes from image_b64 or image_path; (b"", reason) on miss."""
    b64 = item.get("image_b64")
    if b64:
        try:
            return base64.b64decode(b64), None
        except (ValueError, TypeError) as err:
            return b"", f"no_image_data ({err})"
    path = item.get("image_path")
    if path:
        try:
            return Path(path).read_bytes(), None
        except OSError as err:
            return b"", f"no_image_data ({err})"
    return b"", "no_image_data (neither image_b64 nor image_path)"


def backfill(
    items: list[dict[str, Any]],
    captioner: Any | None = None,
    cache_dir: Path | None = None,
    sleep: Any = None,
    retries: int | None = None,
    backoff_base: float | None = None,
) -> tuple[list[dict[str, Any]], dict[str, int]]:
    """Caption every item; returns (per-ref results, reason counts).

    `captioner` defaults to the env-resolved captioner (cloud primary);
    the other kwargs are injectable for tests and default to the settings
    discipline (env-overridable cache dir, retry/backoff from settings).
    """
    if captioner is None:
        captioner = resolve_captioner()
    if captioner is None:
        raise RuntimeError(
            "no captioner configured "
            "(AXIOM_CAPTION_API_BASE/_KEY or AXIOM_CAPTION_LOCAL_MODEL_DIR)"
        )
    if cache_dir is None:
        cache_dir = Path(os.getenv("AXIOM_CAPTION_CACHE_DIR", str(DEFAULT_CACHE_DIR)))
    if retries is None or backoff_base is None:
        from axiom_compute_worker.config import settings as _settings

        s = _settings.get()
        retries = retries if retries is not None else s.image_caption_retries
        backoff_base = backoff_base if backoff_base is not None else s.image_caption_backoff_base_seconds
    if sleep is None:
        import time

        sleep = time.sleep

    load_fn = getattr(captioner, "load", None)
    if callable(load_fn):
        load_fn()
    results: list[dict[str, Any]] = []
    reason_counts: dict[str, int] = {}
    try:
        with tempfile.TemporaryDirectory(prefix="axiom-capbackfill-") as td:
            tmp = Path(td)
            for item in items:
                ref = str(item.get("ref", ""))
                sha = str(item.get("sha256", ""))
                media = str(item.get("media_type", "image/jpeg"))
                if not ref or not sha:
                    results.append({"ref": ref, "reason": "no_image_data (ref/sha256 missing)"})
                    reason_counts["no_image_data"] = reason_counts.get("no_image_data", 0) + 1
                    continue
                blob, why = _decode_image(item)
                if not blob:
                    results.append({"ref": ref, "reason": why})
                    key = why.split(" ")[0]
                    reason_counts[key] = reason_counts.get(key, 0) + 1
                    continue
                img_path = tmp / ref.replace("/", "_")
                img_path.write_bytes(blob)
                timeout = float(os.getenv("AXIOM_CAPTION_API_TIMEOUT", "60"))
                rec, reason = caption_image_detailed(
                    captioner, img_path, media, sha, cache_dir, timeout,
                    sleep=sleep, max_attempts=retries, backoff_base=backoff_base,
                )
                if rec is not None:
                    results.append({
                        "ref": ref,
                        "caption": rec["caption"],
                        "model": rec["model"],
                        "path": rec["path"],
                    })
                else:
                    results.append({"ref": ref, "reason": reason or "api_error"})
                    key = (reason or "api_error").split(" ")[0]
                    reason_counts[key] = reason_counts.get(key, 0) + 1
    finally:
        unload_fn = getattr(captioner, "unload", None)
        if callable(unload_fn):
            try:
                unload_fn()
            except Exception as err:  # noqa: BLE001
                log.warning("captioner unload failed: %s", err)
    return results, reason_counts


def main() -> int:
    try:
        items = json.load(sys.stdin)
        if not isinstance(items, list):
            raise TypeError("input must be a JSON array")
        results, reason_counts = backfill(items)
    except Exception as err:  # noqa: BLE001 — engine failures must be loud
        print(f"caption-backfill engine FAILED: {err}", file=sys.stderr)
        return 1
    json.dump(results, sys.stdout)
    print(file=sys.stderr)
    print(f"caption-backfill: {len(results) - sum(reason_counts.values())} captioned, "
          f"{sum(reason_counts.values())} missing by reason: {reason_counts}",
          file=sys.stderr)
    return 2 if reason_counts else 0


if __name__ == "__main__":
    raise SystemExit(main())
