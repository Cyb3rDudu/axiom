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
import gc
import json
import logging
import os
import threading
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


# Rule 3 (#230 rider): ONE local caption inference at a time — never two
# inferences in parallel on the same model.
_LOCAL_INFERENCE_LOCK = threading.Lock()


class CaptionerNotProvisioned(Exception):
    """Raised by a local captioner's load gate (rule 5): not enough free
    VRAM/RAM for the model. The caller treats it as NOT_PROVISIONED (cloud
    fallback or honest empty captions) instead of an OOM crash."""


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

    # Rule 5 (#230 rider): refuse to load into a tight machine instead of
    # OOM-crashing the runner. fp16 weights ≈ 20 GB + activations. The env
    # override exists for hosts with a different gauge (e.g. a CUDA box
    # with ample VRAM but tight host RAM, or a future fp8 snapshot).
    # ponytail: host-RAM gauge only; a VRAM-aware gate if CUDA hosts trip it.
    MIN_FREE_GB = float(os.getenv("AXIOM_CAPTION_LOCAL_MIN_FREE_GB", "24"))

    def __init__(self, model_dir: str | None = None):
        import torch  # noqa: F401 — fail fast with the honest ImportError

        self._model_dir = Path(
            model_dir or os.getenv("AXIOM_CAPTION_LOCAL_MODEL_DIR", "")
        )
        self.model = "moondream3-preview"
        # Rule 1: INGEST class — no persistent model caching; load() on
        # stage order, unload() after the stage. _model is only non-None
        # between an explicit load()/unload() pair.
        self._model: Any | None = None
        # Rule 4: poison flag — set when an inference blew its timeout.
        self._poisoned = False

    def _load_gate_ok(self) -> bool:
        """Rule 5: enough free memory for fp16 weights + activations."""
        try:
            import psutil
        except ImportError:
            return True  # cannot measure → do not block on a missing gauge
        free_gb = psutil.virtual_memory().available / (1024 ** 3)
        if free_gb < self.MIN_FREE_GB:
            log.warning("local captioner load gate: %.1f GB free < %.1f GB "
                        "required — NOT_PROVISIONED", free_gb, self.MIN_FREE_GB)
            return False
        return True

    def load(self) -> Any:
        """Explicit load with EXPLICIT device + dtype (rule 2) — no
        implicit from_pretrained placement."""
        if self._model is not None:
            return self._model
        if not self._model_dir or not (self._model_dir / "config.json").exists():
            raise CaptionerNotProvisioned(self.DOWNLOAD_INSTRUCTIONS)
        if not self._load_gate_ok():
            raise CaptionerNotProvisioned(
                f"less than {self.MIN_FREE_GB:.0f} GB free memory for the "
                "fp16 Moondream weights")
        import sys

        import torch

        sys.path.insert(0, str(self._model_dir))
        try:
            from hf_moondream import HfMoondream  # type: ignore[import-not-found]
        except ImportError as err:
            raise CaptionerNotProvisioned(
                f"{self.DOWNLOAD_INSTRUCTIONS} (snapshot incomplete: {err})"
            ) from err
        device = "cpu"
        try:
            from axiom_ng_runner.compute_core.devices import hardware_detector

            resolved = hardware_detector.get_torch_device()
            device = str(resolved)
        except Exception as err:  # noqa: BLE001 — detector optional
            log.debug("device detector unavailable, local captioner on CPU: %s", err)
        model = HfMoondream.from_pretrained(
            str(self._model_dir), trust_remote_code=True, torch_dtype=torch.float16
        ).to(device).eval()
        self._model = model
        log.info("local captioner loaded: moondream3-preview fp16 on %s", device)
        return model

    def unload(self) -> None:
        """Rule 1: INGEST class leaves memory after the stage."""
        model, self._model = self._model, None
        del model
        gc.collect()
        try:
            import torch

            if torch.cuda.is_available():
                torch.cuda.empty_cache()
        except Exception as err:  # noqa: BLE001 — cache clearing is best-effort
            log.debug("cache clear after unload failed: %s", err)
        log.info("local captioner unloaded")

    def poison(self) -> None:
        """Rule 4: mark after a timed-out inference. All FURTHER images skip
        local inference; the caller falls back to cloud (when configured) or
        completes honestly with empty captions — never placeholders.

        Rest truth, documented here per the rider: the hung worker thread
        itself is NOT handled (Python cannot abort threads) and may still
        hold _LOCAL_INFERENCE_LOCK forever. Collision is impossible because
        every subsequent local caption checks _poisoned BEFORE touching the
        lock and never reaches the inference again.
        """
        self._poisoned = True
        log.warning("local captioner POISONED after a timed-out inference — "
                    "further local inference disabled for this stage")

    @property
    def poisoned(self) -> bool:
        return self._poisoned

    def caption(self, image_bytes: bytes, media_type: str) -> tuple[str, str]:
        import io

        from PIL import Image

        if self._poisoned:
            # Rule 4: honest miss — never queue behind a possibly-hung
            # lock-holding inference.
            return "", "local"
        model = self.load()
        img = Image.open(io.BytesIO(image_bytes)).convert("RGB")
        # Rule 3: serialized inference. The lock is checked only AFTER the
        # poison gate (see poison() for the rest truth about hung holders).
        with _LOCAL_INFERENCE_LOCK:
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
    files = ("moondream.py", "text.py", "vision.py")
    return all((model_dir / f).exists() for f in files)


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
        # isinstance guard: a corrupt record (array, string, null) must be
        # an honest miss, never an AttributeError that unwinds the stage
        # (review C1 — the guard previously only caught OSError/ValueError).
        if isinstance(rec, dict) and rec.get("caption"):
            return rec
    except (OSError, ValueError, TypeError):
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
            except concurrent.futures.TimeoutError:
                # Rule 4 (#230 rider): a LOCAL captioner that blew its
                # deadline is poisoned — further images never queue behind
                # the possibly-hung inference (rest truth at poison()).
                poison = getattr(captioner, "poison", None)
                if callable(poison):
                    poison()
                raise
            finally:
                ex.shutdown(wait=False)
    except Exception as err:  # noqa: BLE001 — any captioner miss is an honest miss
        log.warning("caption failed for %s: %s", sha256[:12], err)
        return None

    if not caption:
        # poisoned local captioner: honest miss, no record written
        return None
    rec = {"caption": caption, "model": captioner.model, "path": path}
    try:
        cache_dir.mkdir(parents=True, exist_ok=True)
        _cache_path(cache_dir, sha256).write_text(json.dumps(rec))
    except OSError as err:
        log.warning("caption cache write failed (continuing): %s", err)
    return rec
