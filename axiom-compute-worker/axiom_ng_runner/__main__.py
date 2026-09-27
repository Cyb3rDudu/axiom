"""``python -m axiom_ng_runner`` — legacy alias entrypoint (F10, ADR 0001 §4).

Importing this package triggers the warn-once deprecation witness (see
``axiom_ng_runner/__init__.py``) and the call delegates to the canonical
``axiom-compute-worker`` main — one process identity, one behavior.
"""

from axiom_compute_worker.__main__ import main  # noqa: F401 — re-export

if __name__ == "__main__":
    main()
