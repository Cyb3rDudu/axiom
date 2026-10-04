"""Run the compute worker service: ``python -m axiom_compute_worker``.

Canonical entrypoint of axiom-compute-worker (ADR 0001 §4, F10 #304);
the legacy ``python -m axiom_ng_runner`` alias delegates here through
the warn-once compat package. Binds to 127.0.0.1 by default (contract
§18). Configure via the ``AXIOM_PROCESSOR_*`` env vars (see ``config.py``
/ work order §11 — the runner-side env contract is unchanged in 0.2.x).
"""

from __future__ import annotations

import logging

import uvicorn

from .config import assert_credential_free_env, load_settings

_LOG_FORMAT = "%(asctime)s [compute-worker] %(levelname)s %(name)s: %(message)s"


def main() -> None:
    assert_credential_free_env()
    s = load_settings()
    logging.basicConfig(
        level=getattr(logging, s.log_level.upper(), logging.INFO), format=_LOG_FORMAT
    )

    uvicorn.run(
        "axiom_compute_worker.app:app",
        host=s.bind_addr,
        port=s.port,
        log_level=s.log_level.lower(),
    )


if __name__ == "__main__":
    main()
