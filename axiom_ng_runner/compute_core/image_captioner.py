"""Vision captioning for extracted_image artifacts (#230).

Two paths, one output contract ``(caption, model, path)``:

- **Cloud (primary)**: provider-neutral OpenAI-compatible chat client
  (base64 ``image_url``). Configured via env — ``AXIOM_CAPTION_API_BASE``,
  ``AXIOM_CAPTION_API_KEY``, ``AXIOM_CAPTION_MODEL``. Keys follow the
  settings discipline and NEVER enter the repo or logs.
- **Local (Moondream 3)**: the model is machine-global state, not per-job
  conversion logic — measured size (fp16 shard 4.9 GB, 4 shards ≈ 19 GB;
  fp8 10.5 GB; no official ONNX export for moondream3-preview) rules out
  the pandoc-#224 in-artifact pattern (~40 MB there). Cut: a documented
  download into ``AXIOM_CAPTION_LOCAL_MODEL_DIR`` (HF snapshot layout,
  sha256-pinned manifest), CPU today / CUDA-ready. The GGUF/mmproj door
  (#183) is untouched — this module adds a path, changes none.

Both captioners are invoked through :func:`caption_image`, which enforces
the sha256 hash-gate cache and the per-image timeout BEFORE any model
call. A caption is a machine claim, never a citation address (#241): the
caller stores it marked as machine-generated.
"""

from __future__ import annotations

import base64
import json
import logging
import os
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any, Protocol

log = logging.getLogger(__name__)

# The prompt is deliberately factual and short: captions index the figure
# for retrieval ("Säulendiagramm Umsatz 2018–2024"), they do not interpret.
# Calibrated in the #230 sanity check against corpus diagrams/charts.
CAPTION_PROMPT = (
    "Describe this figure from a non-fiction book in 1-2 factual sentences. "
    "Name the figure type (chart, diagram, table, photo, schematic), its "
    "subject, and any readable labels, axis titles, or value ranges. "
    "Do not speculate beyond what is visible."
)

# Default cache: persistent, machine-global (survives job cleanup) — the
# hash gate's whole point is that a re-ingest of an unchanged image costs
# nothing. Overridable for tests.
DEFAULT_CACHE_DIR = Path.home() / ".cache" / "axiom" / "captions"


class Captioner(Protocol):
    """One image bytes → one caption tuple. ``path`` names the execution
    path ("cloud" | "local") per the #230 artifact-attribute contract."""

    model: str

    def caption(self, image_bytes: bytes, media_type: str) -> tuple[str, str]:
        """Returns (caption, path)."""
        raise NotImplementedError


class CloudCaptioner:
    """OpenAI-compatible vision captioner (primary path, #230).

    Talks to any /v1/chat/completions endpoint that accepts base64
    image_url parts. Provider is env-selected; the key is read from the
    environment at call time and never logged.
    """

    def __init__(self, base: str, api_key: str, model: str, timeout: float = 60.0):
        self._base = base.rstrip("/")
        self._api_key = api_key
        self._timeout = timeout
        self.model = model

    def caption(self, image_bytes: bytes, media_type: str) -> tuple[str, str]:
        import httpx

        b64 = base64.b64encode(image_bytes).decode("ascii")
        body = {
            "model": self.model,
            "max_tokens": 120,
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "image_url",
                            "image_url": {
                                "url": f"data:{media_type};base64,{b64}",
                            },
                        },
                        {"type": "text", "text": CAPTION_PROMPT},
                    ],
                }
            ],
        }
        resp = httpx.post(
            f"{self._base}/chat/completions",
            json=body,
            headers={"Authorization": f"Bearer {self._api_key}"},
            timeout=self._timeout,
        )
        resp.raise_for_status()
        data = resp.json()
        text = (data.get("choices") or [{}])[0].get("message", {}).get("content", "")
        caption = " ".join(str(text).split()).strip()
        if not caption:
            raise ValueError("caption API returned empty content")
        return caption, "cloud"


class LocalMoondreamCaptioner:
    """Local Moondream 3 captioner (CPU trickle path, #230).

    Loads the model from ``AXIOM_CAPTION_LOCAL_MODEL_DIR`` (HF snapshot
    layout of moondream/moondream3-preview). First use of an empty dir
    fails with the documented download instructions (see
    :data:`DOWNLOAD_INSTRUCTIONS`) — no silent network fetch at runtime;
    provisioning is an operator step with pinned checksums.
    """

    DOWNLOAD_INSTRUCTIONS = (
        "Local captioning needs the Moondream 3 snapshot in "
        "AXIOM_CAPTION_LOCAL_MODEL_DIR (HF snapshot layout, e.g. "
        "`hf download moondream/moondream3-preview --local-dir <dir>`); "
        "pin and verify the shard sha256s against the repo manifest."
    )

    def __init__(self, model_dir: str | None = None):
        import torch  # noqa: F401 — fail fast with the honest ImportError

        self._model_dir = Path(
            model_dir or os.getenv("AXIOM_CAPTION_LOCAL_MODEL_DIR", "")
        )
        self.model = "moondream3-preview"
        self._moondream: Any | None = None

    def _load(self) -> Any:
        if self._moondream is not None:
            return self._moondream
        if not self._model_dir or not (self._model_dir / "config.json").exists():
            raise FileNotFoundError(self.DOWNLOAD_INSTRUCTIONS)
        # Import mirrors the GLiNER pattern (runner.py): in-process load,
        # device placement via the hardware detector when available.
        import sys

        sys.path.insert(0, str(self._model_dir))
        try:
            from moondream import Moondream  # type: ignore[import-not-found]
        except ImportError as err:
            raise FileNotFoundError(
                f"{self.DOWNLOAD_INSTRUCTIONS} (snapshot incomplete: {err})"
            ) from err
        ckpt = str(self._model_dir)
        self._moondream = Moondream.from_pretrained(ckpt)
        return self._moondream

    def caption(self, image_bytes: bytes, media_type: str) -> tuple[str, str]:
        import io

        from PIL import Image

        model = self._load()
        img = Image.open(io.BytesIO(image_bytes)).convert("RGB")
        answer = model.caption(img, length="short")["caption"]
        caption = " ".join(str(answer).split()).strip()
        if not caption:
            raise ValueError("local captioner returned empty content")
        return caption, "local"


def resolve_captioner() -> Captioner | None:
    """Env-driven path selection. Cloud wins when configured (primary,
    #230 owner decision); local is the fallback when its model dir exists.
    Returns None when neither is provisioned — the stage then completes
    honestly with uncaptioned images and a named reason (no placeholders).
    """
    base = os.getenv("AXIOM_CAPTION_API_BASE", "")
    key = os.getenv("AXIOM_CAPTION_API_KEY", "")
    if base and key:
        return CloudCaptioner(
            base=base,
            api_key=key,
            model=os.getenv("AXIOM_CAPTION_MODEL", "gpt-4o-mini"),
            timeout=float(os.getenv("AXIOM_CAPTION_API_TIMEOUT", "60")),
        )
    local_dir = Path(os.getenv("AXIOM_CAPTION_LOCAL_MODEL_DIR", ""))
    if local_dir and (local_dir / "config.json").exists():
        # Provisioning is checked HERE, not at first image: an incomplete
        # local installation must resolve to NOT-PROVISIONED (honest reason
        # in stage_completion), never surface later as CAPTION_CALLS_FAILED
        # after the stage already opened. Two hard requirements beyond the
        # snapshot files: torch (inference) and the moondream package code
        # inside the snapshot (moondream.py et al. — downloaded with the
        # model; the runner requirements do NOT pin it, see
        # requirements-heavy.txt).
        if not _local_runtime_ready(local_dir):
            log.warning(
                "local captioner dir set but incomplete (need torch + the "
                "snapshot's moondream package files) — no captioner")
            return None
        try:
            return LocalMoondreamCaptioner(str(local_dir))
        except ImportError:
            log.warning("local captioner requested but torch import failed — no captioner")
            return None
    return None


def _local_runtime_ready(model_dir: Path) -> bool:
    """torch importable AND the snapshot carries its own package code."""
    try:
        import torch  # noqa: F401
    except ImportError:
        return False
    return (model_dir / "moondream.py").exists() and (
        model_dir / "text.py").exists()


def _cache_path(cache_dir: Path, sha256: str) -> Path:
    return cache_dir / f"{sha256}.json"


def load_cached_caption(
    cache_dir: Path, sha256: str
) -> dict[str, str] | None:
    """Hash gate (#230): a cached caption for an unchanged image short-
    circuits every model call. Returns the cached record or None."""
    p = _cache_path(cache_dir, sha256)
    try:
        rec = json.loads(p.read_text())
        if rec.get("caption"):
            return rec
    except (OSError, ValueError):
        pass
    return None


def caption_image(
    captioner: Captioner,
    image_path: Path,
    media_type: str,
    sha256: str,
    cache_dir: Path,
    timeout: float,
    clock: Callable[[], float] = time.monotonic,
) -> dict[str, str] | None:
    """Caption ONE image under the hash gate + per-image budget.

    Returns ``{"caption", "model", "path"}`` (from cache or a fresh call)
    or None when the captioner failed/timed out — an honest miss, never a
    placeholder (#241). Cache writes are best-effort: a read-only cache
    dir must not fail the stage.
    """
    cached = load_cached_caption(cache_dir, sha256)
    if cached is not None:
        return cached

    deadline = clock() + timeout if timeout > 0 else None

    def _left() -> float:
        return (deadline - clock()) if deadline is not None else 1.0

    if deadline is not None and _left() <= 0:
        return None

    try:
        if deadline is None:
            caption, path = captioner.caption(image_path.read_bytes(), media_type)
        else:
            # Per-image timeout: run the model call in a worker thread and
            # ABANDON it on deadline. C2 (#230 review): NO context manager
            # here — its __exit__ shutdown(wait=True) would join the worker
            # and the "timeout" would still block for the full model call
            # (measured: 1.0s call, 0.1s timeout → 1.0s elapsed). One leaked
            # worker thread per abandoned call is the documented cost; the
            # deadline is the hard guarantee (#225 pattern).
            import concurrent.futures

            ex = concurrent.futures.ThreadPoolExecutor(max_workers=1)
            try:
                fut = ex.submit(captioner.caption, image_path.read_bytes(), media_type)
                caption, path = fut.result(timeout=max(_left(), 0.01))
            finally:
                ex.shutdown(wait=False)
    except Exception as err:  # noqa: BLE001 — any captioner miss is an honest miss
        log.warning("caption failed for %s: %s", sha256[:12], err)
        return None

    rec = {"caption": caption, "model": captioner.model, "path": path}
    try:
        cache_dir.mkdir(parents=True, exist_ok=True)
        _cache_path(cache_dir, sha256).write_text(json.dumps(rec))
    except OSError as err:
        log.warning("caption cache write failed (continuing): %s", err)
    return rec
