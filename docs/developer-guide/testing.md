# Testing

This chapter documents the two test suites, how the integration databases are
set up, and the mutation-testing culture that keeps the suite meaningfully
green. Testing is how this system's strongest invariants — fencing, atomic
persistence, contract conformance, no durable-side writes — stay enforced.

## The two suites

axiom has two independent suites, one per code base:

| Suite | Command | What it covers |
| --- | --- | --- |
| Go (`axiom_ng`) | `go build ./... && go vet ./... && go test ./...` | Unit + integration tests. Integration tests against a real Postgres (see below). |
| Python (`axiom_ng_runner`) | `pytest tests/ -v` | The contract black-box suite (§19) against the `reference` backend + compute-core import/unit tests. |

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
# the db/sync/search integration tests write to the DSN database directly)
AXIOM_TEST_DATABASE_URL=postgresql://axiom_user@127.0.0.1:5444/axiom_ng_scratch_test?sslmode=disable go test ./...
```

The isolation split, honestly: the **dispatcher/lease/persistence/failover**
suites clone a throwaway test database from `AXIOM_TEST_DATABASE_URL` and never
touch the DSN database itself; the remaining integration tests (db, Zotero
sync, search) open the DSN database directly and **write** to it. So point the
variable at a scratch database — never a production or shared-development DSN.

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
  content sits (trail state at chunk start, not after the closing boundary);
  pinned by `axiom_ng_runner/tests/test_chunker_section_trail.py`, which is
  red under the pre-fix chunker.
- **No durable-side writes:** the Python suite asserts the runner never touches
  Postgres/OpenSearch/graph/Zotero.

Next: [Configuration](configuration.md) · [Architecture Overview](architecture.md)
