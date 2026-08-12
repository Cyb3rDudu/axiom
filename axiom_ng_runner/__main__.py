"""Run the processor service: ``python -m axiom_ng_runner``.

Binds to 127.0.0.1 by default (contract §18). Configure via the
``AXIOM_PROCESSOR_*`` env vars (see ``config.py`` / work order §11).
"""

from __future__ import annotations

import logging

import uvicorn

from .config import load_settings

_LOG_FORMAT = "%(asctime)s [processor] %(levelname)s %(name)s: %(message)s"


def main() -> None:
    s = load_settings()
    logging.basicConfig(
        level=getattr(logging, s.log_level.upper(), logging.INFO), format=_LOG_FORMAT
    )

    uvicorn.run(
        "axiom_ng_runner.app:app",
        host=s.bind_addr,
        port=s.port,
        log_level=s.log_level.lower(),
    )


if __name__ == "__main__":
    main()
