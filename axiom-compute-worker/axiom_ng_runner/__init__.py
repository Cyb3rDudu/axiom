"""Compat alias package for ``axiom_compute_worker`` (ADR 0001 §4, F10 #304).

``python -m axiom_ng_runner``, the ``axiom-runner`` console script and
``import axiom_ng_runner`` keep working through 0.2.x: importing the
alias warns exactly once via the deprecation witness (counted on every
import; exported in ``/v1/capabilities`` as ``deprecations``) and
re-exports the canonical package's public names. Delegation, not a fork
— the canonical package owns every behavior; removal earliest in an
announced major.
"""

from axiom_compute_worker.deprecations import use

use("axiom_ng_runner")

from axiom_compute_worker import *  # noqa: E402,F401,F403 — alias re-export
