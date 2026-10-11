"""Phase checkpoints for cross-attempt job resumption (#372).

A job attempt that dies mid-pipeline loses everything today: retries and
force-rebuilds of the same file restart from zero (the PEN-200 pattern —
hours of table/embedding/mREBEL work lost per abort). This module gives
the compute pipelines phase-granular completion markers whose payloads
survive the per-job work dir:

* Store: ``<work_root>/checkpoints/<attachment_id>/<key>/`` where the key
  derives from (content hash, processing-profile hash, processor version).
  A retry hits the same key and resumes; a changed file, a changed
  profile or a new build invalidates cleanly (different key). A
  force-rebuild recomputes from zero by design — the key hashes the
  whole processing block including force_rebuild (contract §19's fresh
  recompute semantics).
* Atomicity: payload files land first, the phase marker LAST via
  ``os.replace`` — a kill mid-write leaves an incomplete phase (marker
  absent) that simply recomputes. Contiguity is enforced: only a
  contiguous prefix of PHASE_ORDER counts as completed.
* Honesty: the pipeline reports per phase ``computed`` vs ``reused``
  into ``manifest.phase_reuse`` — an operator can tell a fresh
  processing from a resumed one at a glance.
* Retention: saving into a new key prunes sibling keys of the same
  attachment (content moved on — the old checkpoints are dead weight).
  One key (one book's intermediates) per attachment is the steady-state
  disk ceiling; no TTL machinery. Prune ceiling, documented: the prune
  is immediate — a re-keyed attempt that dies early has already pruned
  the base key, and the next plain retry recomputes from zero.
  Accepted because checkpoints are an optimization, never a
  correctness dependency.

Boundary (documented non-goal): phase granularity only — a phase that
died halfway recomputes wholly; page-level resume inside a phase stays
out of scope.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import shutil
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from . import __version__

log = logging.getLogger(__name__)

# The canonical phase order — the SAME ontology the #369 progress
# reporting and the stage timings use (one taxonomy, no second one),
# in EXECUTION order of the real pipeline (captions run last, after
# relationships). Contiguity scanning relies on this order: markers
# complete strictly in sequence.
PHASE_ORDER = ["convert", "chunk", "embed", "entities", "relationships", "captions"]


def _json_default(obj: Any) -> Any:
    """numpy scalars (embeddings arrive as float32 arrays) → JSON natives."""
    if type(obj).__module__.startswith("numpy"):
        return obj.item()
    raise TypeError(f"Object of type {type(obj).__name__} is not JSON serializable")


def _link_or_copy(src: Path, dest: Path) -> None:
    """Hardlink when possible (same filesystem, cheap, keeps the inode
    alive when the job dir dies), plain copy otherwise. Idempotent: a
    dest that already holds the same content (the resubmit-same-job-id
    shape re-links into a work dir the prior attempt already populated —
    possibly the very same inode the checkpoint was hardlinked FROM) is
    a no-op; a DIFFERENT existing dest is replaced."""
    dest.parent.mkdir(parents=True, exist_ok=True)
    if dest.exists():
        try:
            if src.samefile(dest):
                return  # already the staged content
        except OSError:
            pass
        dest.unlink()
    try:
        os.link(src, dest)
    except OSError:
        shutil.copy2(src, dest)


class PhaseCheckpoints:
    """Cross-attempt phase checkpoint store for ONE (attachment, content,
    profile, build) key. Construct per compute attempt; query with
    ``resume(phase)``/``save(phase)``; the reuse report lands in the
    result manifest via ``report()``."""

    def __init__(self, request: dict[str, Any], work_root: Path) -> None:
        self._phases_run: dict[str, str] = {}
        attach = request.get("attachment") or {}
        content = str(attach.get("content_hash") or "")
        if not content or not attach.get("attachment_id"):
            # No verifiable identity (the direct-pipeline shape without a
            # hash) — checkpoints stay disabled; every phase computes.
            self.root: Path | None = None
            self._completed: set[str] = set()
            return
        profile_src = json.dumps(request.get("processing") or {}, sort_keys=True)
        profile = hashlib.sha256(profile_src.encode()).hexdigest()
        digest = content.split(":")[-1]
        # force-rebuild is the operator's escape hatch: recompute from
        # zero BY DESIGN (decision 2026-10-11, #372). Its checkpoints live
        # in a separate namespace so a force attempt can never prune the
        # normal key's resume basis (a dying force attempt must not cost
        # the next plain retry its resume).
        self._force = bool((request.get("processing") or {}).get("force_rebuild"))
        ns = "force" if self._force else "base"
        key = f"{digest[:16]}-{profile[:8]}-{__version__}"
        self.root = (Path(work_root) / "checkpoints" / str(attach["attachment_id"])
                     / ns / key)
        self._completed = self._scan()

    # -- query / accounting ------------------------------------------------

    def resume(self, phase: str) -> bool:
        """True when the phase AND every earlier phase is completed —
        the contiguity (prefix) property is enforced HERE, dynamically,
        not only at the init scan: a save() that lands out of order (or
        a hand-repaired store) must never make a downstream phase
        resumable across an upstream hole."""
        if phase not in self._completed:
            return False
        idx = PHASE_ORDER.index(phase)
        return all(p in self._completed for p in PHASE_ORDER[:idx])

    def report(self) -> dict[str, str]:
        """Phase → computed|reused for the result manifest (only phases
        the pipeline actually ran through)."""
        return dict(self._phases_run)

    def mark(self, phase: str, state: str | bool) -> None:
        """Record the phase's outcome for the report. Accepts the
        historical booleans (False → computed, True → reused) or the
        explicit strings computed / reused / skipped (off in profile)."""
        if isinstance(state, bool):
            state = "reused" if state else "computed"
        self._phases_run[phase] = state

    # -- payload IO ---------------------------------------------------------

    def save(self, phase: str, *, files: dict[str, Path] | None = None,
             jsons: dict[str, Any] | None = None) -> None:
        """Persist a completed phase: payload files (hardlinked into the
        store) and JSON blobs first, the marker last (atomic). Prunes
        superseded sibling keys of the same attachment."""
        if self.root is None:
            return
        payload_dir = self.root / phase
        payload_dir.mkdir(parents=True, exist_ok=True)
        entries: dict[str, int] = {}
        for name, obj in (jsons or {}).items():
            p = payload_dir / f"{name}.json"
            p.write_text(json.dumps(obj, ensure_ascii=False, default=_json_default), encoding="utf-8")
            entries[p.name] = p.stat().st_size
        for name, src in (files or {}).items():
            dest = payload_dir / name
            _link_or_copy(Path(src), dest)
            entries[dest.name] = dest.stat().st_size
        marker = {
            "phase": phase,
            "completed_at": datetime.now(UTC).isoformat(),
            "files": entries,
        }
        tmp = self.root / f".{phase}.marker.tmp"
        tmp.write_text(json.dumps(marker), encoding="utf-8")
        os.replace(tmp, self.root / f"{phase}.marker.json")
        self._completed.add(phase)
        self._prune_siblings()

    def load_file(self, phase: str, name: str, dest: Path) -> Path:
        """Re-link a staged payload file into the job work dir; returns
        the destination path. Raises when absent (caller treats the phase
        as incomplete — the resume() gate normally prevents this)."""
        assert self.root is not None
        _link_or_copy(self.root / phase / name, dest)
        return dest

    def load_json(self, phase: str, name: str) -> Any:
        assert self.root is not None
        return json.loads((self.root / phase / f"{name}.json").read_text(encoding="utf-8"))

    # -- internals -----------------------------------------------------------

    def _marker_ok(self, phase: str) -> bool:
        assert self.root is not None
        m = self.root / f"{phase}.marker.json"
        if not m.is_file():
            return False
        try:
            marker = json.loads(m.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return False
        for fname, size in (marker.get("files") or {}).items():
            p = self.root / phase / fname
            try:
                if not p.is_file() or p.stat().st_size != size:
                    return False
            except OSError:
                return False
        return True

    def _scan(self) -> set[str]:
        """Contiguous completed prefix of PHASE_ORDER (an upstream hole
        invalidates everything downstream — resume starts at the hole)."""
        assert self.root is not None
        done: set[str] = set()
        for phase in PHASE_ORDER:
            if self._marker_ok(phase):
                done.add(phase)
            else:
                break
        return done

    def _prune_siblings(self) -> None:
        """One live key per attachment AND namespace: content/profile/build
        moved on → the old intermediates are dead weight. The force
        namespace never prunes the base namespace (the escape hatch must
        not destroy the normal retry's resume basis) and vice versa."""
        assert self.root is not None
        parent = self.root.parent
        try:
            siblings = [d for d in parent.iterdir() if d.is_dir() and d != self.root]
        except OSError:
            return
        for sib in siblings:
            shutil.rmtree(sib, ignore_errors=True)
            log.info("checkpoints: pruned superseded key %s", sib.name)
