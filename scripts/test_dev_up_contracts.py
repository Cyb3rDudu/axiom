#!/usr/bin/env python3
"""Assert the dev-up hardening contracts from the v0.2.4-RC test round.

dev-up.sh bootstraps a full dev stack by sourcing PROD agenix env files and
then overriding every isolation lever. The RC round surfaced four contracts
that a plain shellcheck cannot see — this probe pins them against the actual
worker/config sources instead of trusting comments:

  F1  the runner start scrubs the whole #316 credential family (the worker
      refuses to boot otherwise — dev-up must honor the contract). The list
      is cross-checked against CREDENTIAL_ENV_NAMES in
      axiom_compute_worker/config.py, so extending the worker guard without
      dev-up (or vice versa) fails here.
  F2  the runner warmup wait gates on models_warmed via /v1/capabilities
      (honest readiness #216): a health 200 alone goes green even when the
      real-model preload FAILED.
  F3  both env name families stay on their own side of the wire: the runner
      start strips the RAG-side AXIOM_COMPUTE_WORKER_* family, and the RAG
      start exports the source BASE_URL at the DEV RAG (an inherited prod
      base URL made source fetches 404 in the RC round).
  F4  the dev-DB legacy stub probe exists: <=8-dim reference-era dense
      embeddings are recognized loudly and BOTH documented reset paths are
      offered (surgical delete + index rebootstrap; full re-clone from
      axiom_db per AGENT.md).
"""

import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
DEV_UP = os.path.join(HERE, "dev", "dev-up.sh")
WORKER_CONFIG = os.path.join(
    HERE, "..", "axiom-compute-worker", "axiom_compute_worker", "config.py"
)

failures: list[str] = []


def check(cond: bool, label: str) -> None:
    if cond:
        print(f"ok   {label}")
    else:
        failures.append(label)
        print(f"FAIL {label}")


dev_up = open(DEV_UP, encoding="utf-8").read()
config_py = open(WORKER_CONFIG, encoding="utf-8").read()

# --- F1: credential-family scrub mirrors the worker's #316 boot guard -------

m = re.search(
    r"CREDENTIAL_ENV_NAMES = \((.*?)\)", config_py, re.DOTALL
)
assert m, "CREDENTIAL_ENV_NAMES tuple not found in worker config.py"
worker_names = set(re.findall(r'"(AXIOM_[A-Z_]+)"', m.group(1)))

m = re.search(
    r"unset AXIOM_DATABASE_URL AXIOM_STORE_DATABASE_URL.*?(?=AXIOM_PROCESSOR_PORT)",
    dev_up,
    re.DOTALL,
)
assert m, "credential scrub unset block not found in dev-up.sh"
scrub_names = set(re.findall(r"\b(AXIOM_[A-Z_]+)\b", m.group(0)))

check(
    worker_names <= scrub_names,
    f"F1 dev-up scrubs every worker #316 credential name "
    f"(missing: {sorted(worker_names - scrub_names) or 'none'})",
)

# --- F2: warmup wait requires models_warmed ----------------------------------

check(
    "/v1/capabilities" in dev_up
    and re.search(r"\.warmup_enabled == true then \.models_warmed == true", dev_up)
    is not None,
    "F2 runner warmup gates on models_warmed (capabilities, #216 honest readiness)",
)

# --- F3: name families wired on their own sides ------------------------------

runner_block = re.search(
    r"--- start the runner.*?(?=--- start the RAG)", dev_up, re.DOTALL
)
rag_block = dev_up[re.search(r"--- start the RAG", dev_up).start() :]
assert runner_block, "runner section not found"
runner_txt = runner_block.group(0)

rag_family = [
    "AXIOM_COMPUTE_WORKER_URL",
    "AXIOM_COMPUTE_WORKER_URLS",
    "AXIOM_COMPUTE_WORKER_SOURCE_BASE_URL",
    "AXIOM_COMPUTE_WORKER_NAME",
]
flat = runner_txt.replace("\\\n", " ")
unset_lines = " ".join(l for l in flat.splitlines() if l.lstrip().startswith("unset"))
check(
    all(re.search(rf"\b{n}\b", unset_lines) for n in rag_family),
    "F3 runner start strips the RAG-side AXIOM_COMPUTE_WORKER_* family",
)
check(
    re.search(
        r'export "\$SRCBASE_KEY=http://127\.0\.0\.1:\$RAG_PORT"', rag_block
    )
    is not None,
    "F3 RAG exports source BASE_URL at the dev RAG (RC-round 404 fix)",
)

# --- F4: dev-DB stub probe + reset paths --------------------------------------

check(
    "dimensions <= 8" in dev_up and "processing_chunk_dense_embeddings" in dev_up,
    "F4 dev-up probes the dev DB for <=8-dim stub embeddings",
)
check(
    "pg_dump -Fc | pg_restore" in dev_up
    and "DELETE FROM processing_chunk_dense_embeddings" in dev_up,
    "F4 both documented reset paths offered (surgical + prod-mirror re-clone)",
)

if failures:
    print(f"\n{len(failures)} contract probe(s) FAILED")
    sys.exit(1)
print("\nall dev-up contract probes green")
