"""
Multi-signal activity detector for smart GPU model unloading.

Aggregates signals to determine whether Axiom is actively in use. Used by
the model_cache idle monitor to decide when it's safe to unload GPU models.

Signals checked (unload only if ALL false):
  1. Running missions (mission_lifecycle_manager)
  2. Doc-processor pending/processing documents (DB)
  3. Recent non-trivial API requests (tracked via middleware)

WebSocket connections are intentionally NOT checked directly — open but idle
browser tabs would keep them alive forever. Mission-subscribed connections are
implicitly covered by signal #1.
"""

import logging
import threading
import time
from typing import Tuple

logger = logging.getLogger(__name__)

_last_request_at: float = time.time()
_last_request_lock = threading.Lock()


def mark_request() -> None:
    """Called by middleware on every non-trivial HTTP request."""
    global _last_request_at
    with _last_request_lock:
        _last_request_at = time.time()


def seconds_since_last_request() -> float:
    with _last_request_lock:
        return time.time() - _last_request_at


def is_system_in_use(max_request_idle_sec: int = 300) -> Tuple[bool, str]:
    """Check if Axiom is actively in use.

    Returns (in_use, reason).
    """
    # Signal 1: running missions
    try:
        from ai_researcher.agentic_layer.controller.utils.mission_lifecycle import (
            mission_lifecycle_manager,
        )
        running = mission_lifecycle_manager.get_running_missions()
        if running:
            return True, f"{len(running)} running missions"
    except Exception as e:
        logger.debug(f"Mission check failed: {e}")

    # Signal 2: doc-processor pending/processing (cross-container via DB)
    try:
        from database.database import get_db
        from sqlalchemy import text
        db = next(get_db())
        try:
            row = db.execute(text(
                "SELECT count(*) FROM documents "
                "WHERE processing_status IN ('pending','processing')"
            )).scalar()
            if row and row > 0:
                return True, f"{row} documents in pipeline"
        finally:
            db.close()
    except Exception as e:
        logger.debug(f"Doc-processor check failed: {e}")

    # Signal 3: recent API requests
    idle_sec = seconds_since_last_request()
    if idle_sec < max_request_idle_sec:
        return True, f"API active {idle_sec:.0f}s ago"

    return False, f"idle {idle_sec:.0f}s, no missions/docs/API"
