# Testing

This chapter documents the two test suites, how the integration databases are
set up, and the mutation-testing culture that keeps the suite meaningfully
green. Testing is how this system's strongest invariants — fencing, atomic
persistence, contract conformance, no durable-side writes — stay enforced.

## The two suites

axiom has two independent suites, one per code base:

| Suite | Command | What it covers |
| --- | --- | --- |
| Go (`axiom`) | `go build ./... && go vet ./... && go test ./...` | Unit + integration tests. Integration tests against a real Postgres (see below). |
| Python (`axiom_compute_worker`) | `pytest tests/ -v` | The contract black-box suite (§19) against the `reference` backend + compute-core import/unit tests. |

The contract black-box suite (Python, `reference` backend) needs only the
runtime + pymupdf + fastapi: health/capabilities, idempotency,
PDF→markdown+chunks, page/section provenance round-trip, hash mismatch,
reference integrity, embeddings vs capabilities, no durable-store access,
cancellation, ack cleanup+idempotency, restart recovery without fake success,
and no durable source copy.

## Integration databases — the `AXIOM_TEST_DATABASE_URL` proviso

The Go integration tests that exercise the dispatcher, leases, persistence, and
outbox against a **real** database are gated behind the `AXIOM_TEST_DATABASE_URL`
environment variable:

- If it is **set**, the dispatcher/lease/persistence suites connect to that
  Postgres and **clone a throwaway test database** (a `CREATE DATABASE <test-db>`
  derived from the DSN), so their runs cannot disturb the DSN database.
- If it is **unset**, those integration tests **skip** (they do not fail, so the
  unit-only path stays green for someone without a database).

```text
# Run Go unit tests without a database
go test ./...

# Run the full Go suite including DB integration (point at a THROWAWAY db —
# the db/sync/search integration tests write to the DSN database directly).
# -p 1 is REQUIRED for the green claim: several packages share the DSN
# database, and parallel package binaries interfere (post-heal-sync and
# contextual-boot ITs are the usual victims — flaky FAILs whose loser
# changes per run; identical interference reproduces on older mains).
AXIOM_TEST_DATABASE_URL=postgresql://axiom_user@127.0.0.1:5444/axiom_ng_scratch_test?sslmode=disable go test -p 1 ./...
```

The isolation split, honestly: the **dispatcher/lease/persistence/failover**
suites clone a throwaway test database from `AXIOM_TEST_DATABASE_URL` and never
touch the DSN database itself; the remaining integration tests (db, Zotero
sync, search) open the DSN database directly and **write** to it. So point the
variable at a scratch database — never a production or shared-development DSN.

## CI — when the gate runs

The `ci.yml` gate fires on `pull_request` events and on pushes to `main`
(#354): work strands open a PR from the start, so a bare branch push never
bills a full matrix run, and a merged SHA runs at most once — superseded
runs on the same ref (rapid merges, rapid review-round pushes) are
cancelled mid-flight, not billed in full. The heavy legs (`go-db-it`,
`split-topology`, `container-topology` — together ~16 of the ~24 billed
minutes per run) are path-gated by the `changes` job: `go-db-it` runs
when anything under `axiom/` changed — Go sources, `go.mod`/`go.sum`,
SQL schemas, and the binary `testdata` fixtures the DB ITs consume
(`axiom/docs` prose over-triggers deliberately, on the fail-open side)
— or the role drill (`deploy/postgres/roles.sql`) or the workflow
definition changed; the topology legs additionally run for
`axiom-compute-worker/**` and `deploy/**` changes. Changes outside
those surfaces (root `docs/`, `scripts/`) run the remaining jobs only. The gate
is fail-open: an unresolvable diff base classifies everything as changed,
so verification is never skipped by an accident of history. Heavy legs
run per cumulative PR diff — the merged result is what must verify,
not the last commit.

## The local pipeline — `make ci-local`

Since #354, CI runs **only on a PR against `main`** (plus pushes to
`main` itself): a bare branch push bills nothing. Strand verification is
LOCAL — the whole pipeline in one command:

```text
make ci-local                          # legs (0)–(5)
make ci-local ARGS="--with-topology --with-act"   # + optional legs
```

`scripts/ci-local.sh` chains ONLY proven parts in CI order — it invents
no checks and touches no workflow file:

0. **drift preflight** — compares the host clock with the local podman
   VM clock and aborts with a repair hint (`chronyc makestep` inside the
   VM) before any leg burns time. A drifted VM clock is a known relapse
   after host sleep: timestamp-based tests then fail spuriously.
1. **fix-convention** — `scripts/test_fix_convention.sh`.
2. **go vet + go unit** — the DB-gated suites skip by design, exactly
   like the CI `go-unit` job.
3. **DB legs, natively** — every run derives its OWN scratch base
   (`ci_local_base_<pid>_test`, pid-suffixed): no database object is
   shared between runs, so parallel runs of different agents coexist
   cleanly on the cluster — neither ever sees or drops the other's
   databases, and foreign bases are never touched. The DSN comes from
   `AXIOM_TEST_DATABASE_URL` if set (that base then belongs to the
   operator and is never dropped), else is derived read-only from the
   environment's database URL with the name rewritten; the source
   variables are never exported. Reused IT databases drift (42P10 on a
   phantom constraint) — runs never inherit database state. Every
   DB-touching run carries `-p 1 -count=1`: parallel package binaries
   contend on the shared DSN database. The baseline leg's admin channel
   is `axiom_ci_test` — a name on the baseline suite's scratch allowlist
   that also satisfies the suites' `_test` guard (the other allowlisted
   names lack the suffix, so renaming would take test-code changes).
   Four suite-internal fixed IT database names
   (`axiom_mirror_it_test`, `axiom_repair_test`, `axiom_repo_test`,
   `axiom_server_test`) plus the baseline suite's
   `axiom_baseline_scratch` cannot be per-run (test-code constants):
   the legs touching them are serialized across concurrent local runs,
   and they are only ever refreshed or removed when no other session is
   connected. Then:
   `go-db-it` (the whole tree — with the compute-worker venv cloaked
   for this leg, so the local leg runs exactly the proven CI set;
   engine-backed suites that auto-detect a local venv skip in CI's
   `go-db-it` job by design), `golden-baseline` (the
   environment-independent half via `AXIOM_BASELINE_DSN`; the live half
   stays `make golden-baseline`), `library-engine-postgres`, and
   `library-engine-sqlite` (DSN-free — PG-free is that leg's point).
   The **role drill stays CI-exclusive**: `AXIOM_REQUIRE_DRILL` is
   never set locally (a cluster with standing roles must skip the
   drill, not fail it). Teardown — on success AND on abort (trap) —
   removes everything the run created, except the shared baseline
   fixture `axiom_ci_test` and the fixed-name IT databases: nothing
   writes into the former, and the next run's refresh owns the
   latter's lifecycle.
4. **runner + fixer pytest** — the same venv-based suites `make test`
   runs. The venvs are *checked preconditions*: a missing venv skips the
   leg with a bootstrap hint (venv building is not the pipeline's
   business).
5. **docs gate** — the naming gate and `mkdocs build --strict`, as in
   the docs workflow.

The optional flags: `--with-topology` boots the split topology
(`split-up.sh` → `split-smoke.sh`, which tears down and verifies the
teardown itself), `--with-act` replays the service-free CI jobs
(`go-lint`, `go-unit`, `library-engine-sqlite`, `runner-pytest`,
`fixer-pytest`) through act against the local podman machine. The
service-backed jobs stay GitHub-only: act publishes service ports onto
the host, where a real PostgreSQL/OpenSearch already listens — which is
exactly why the DB legs run natively instead. Both flags skip softly
(with a hint) when their tooling is missing.

The split of labor: **local = pre-check** — fast, unbilled, one command,
run per review round; **GitHub CI = fidelity** — the same legs on the
canonical runner, once, on the PR against `main`.

Concurrent `ci-local` runs (two agents, two worktrees, one host)
coexist by design — each run owns its scratch bases — but their Go
legs take turns via a host-local lock: full-tree test builds share the
Go build cache (racing its trim produces phantom build failures), and
two legs touch suite-internal fixed database names; the docs gate
takes its own lock the same way (`mkdocs build --clean` wipes a shared
site dir). A run never waits for the drift preflight, the
fix-convention probe, or the Python legs.

## Mutation-testing culture (the "probe")

The suites do not just assert happy paths — the Python suite carries explicit
**mutation barriers**: tests whose *stated purpose* is to fail if a specific
real coupling is broken. Examples seen in the code:

- A test named for the mutation it guards (e.g. "`!` missing → CFIs are
  spec-invalid", "odd index → fails"), so the reason the assertion exists is in
  the test name or docstring.
- Tests that prove a *wiring*, not just an output: e.g. killing the
  pipeline→extractor wiring must make a specific test fail, so the extractor
  cannot silently stop being invoked while the suite stays green.
- A compute-core import test whose purpose is exactly to catch a moved-module
  mistake that would otherwise leave the whole suite green (an import that
  succeeds only by accident).

### Why these probes exist

A suite can turn green for the wrong reason — a moved import, a broken wiring
that still emits something, an assertion that passes vacuously. Mutation tests
are the **deliberate counter-example**: each one encodes "if this real coupling
were severed, this test would catch it", so the green result is *evidence*,
not just a signal. They are cheap to write (small, focused, one invariant each)
and expensive to do without — they are this project's quality DNA.

## Where each invariant is pinned

The strongest system invariants each have a test (or a named mutation barrier):

- **Fencing / lost-lease:** Go integration tests prove a stale token cannot
  update a reclaimed job, and that concurrent claimers never receive the same
  job.
- **Atomic persistence:** Go tests prove an invalid/partial result cannot
  replace the last valid snapshot, and that previous active snapshot survives
  every failure mode.
- **Contract conformance:** Python §19 black-box suite is the single
  acceptance bar for any processor implementation.
- **Section-trail invariant (#186):** a chunk's deepest
  `structure.section_titles` entry is the heading under which its first
  content sits — first NON-overlap content for chunks that open with
  recycled overlap text (trail state at chunk start, not after the closing
  boundary); pinned by `axiom-compute-worker/tests/test_chunker_section_trail.py`,
  which is red under the pre-fix chunker.
- **No durable-side writes:** the Python suite asserts the runner never touches
  Postgres/OpenSearch/graph/Zotero.

Next: [Configuration](configuration.md) · [Architecture Overview](architecture.md)
