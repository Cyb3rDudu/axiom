# Component Persistence (F12, #306)

**Status:** Landed with #306 (0.2.0 Zug, Epic #342) · Code: `axiom/internal/library` (interfaces), `…/library/pglib`, `…/library/sqlite`, `…/library/reposuite`, `…/store`, `…/composition` · Issue carries the acceptance evidence.

This document is the design comment the issue demanded: the repository
map, the SQLite operating rules and their boundary, the Store capability
model, the handover points for the Data Migration track, and the
deliberate deferrals recorded as current state.

---

## 1. Repository map — interface → engines

| Interface | Engines | One-line responsibility |
|---|---|---|
| `library.Repository` (`internal/library/repository.go`) | `pglib.Store` (PostgreSQL) · `sqlite.Repo` (library.sqlite) | The Library component's whole durable surface: imports/saga, events, steps, identifiers, provenance, revisions, anchors/audit, writer lease, mirror reads, resume scan, staging retention set. SQL dialects and engine locking live ONLY in the engine packages. |
| `library.RevisionPublisher` | `pglib.Store` | The legacy Zotero-sync Mits-Schrieb lane (reads the shared mirror; wired for shared-database shapes only — §4). |
| `store.RevisionIntake` (`internal/store/service.go`) | `repo.Repo` (PostgreSQL) | The Store's durable intake seam behind `IngestRevision`. Neutral DTOs; the future SQLite-Store engine implements the same seam without PG assumptions. |
| `store.SearchBackend` + `store.CapabilityReporter` | `search.Service` | Retrieval and the honest equipment report the capability model composes (§3). |

Neutral contracts (cross-cutting):

- **Sentinels** — `library.ErrRowAbsent` (absent row / lost CAS),
  `library.ErrDuplicateKey` (concurrent identical insert: resolve by
  re-reading), `repo.ErrRowAbsent` (Store side, two methods today).
  Engines translate driver errors into these; the application never
  touches `pgx`/driver types.
- **42P01 folding is scoped** — `pglib.absent()` is no-rows-only: a
  missing `library_*` relation is a schema fault and stays a raw error.
  `pglib.mirrorAbsent()` (no-rows ∪ 42P01) exists exclusively for the
  Zotero-mirror strangler reads, where a library-only database honestly
  reads as absence.
- **Engine confinement is linted** —
  `internal/store/engine_lint_test.go` bans pgx/pgvector imports outside
  the engine allowlist (`pglib`, `mirror`, `repair`, `repo`, `search`,
  `db`, `store/migrations`) over PREFIX trees (`internal/library/`,
  `internal/store/`, `internal/contracts/`) — the next subpackage fails
  loudly. OpenSearch-client construction (`search.New`, alias-aware
  scan) is confined to `internal/search`, the composition root, and
  `cmd/` tool mains. Documented residual limits: dot-imports escape the
  ctor scan (banned style, unenforced); the import ban covers the
  PostgreSQL/pgvector modules only — a `modernc.org/sqlite` import in an
  application layer is not flagged yet (revisit with the SQLite-Store
  epic).

## 2. SQLite profile — operating rules and boundary

One file per component: `library.sqlite`, created atomically
(`O_EXCL`, 0600; parent dirs 0700) — never a half-visible or clobbered
database; a lost creation race adopts the winner's file.

- **Pragmas startup-asserted on both handles, test-pinned**:
  `journal_mode=WAL`, `foreign_keys=ON`, `busy_timeout=5s`.
- **Single writer**: every mutation rides the write handle
  (`BEGIN IMMEDIATE` via the driver's `_txlock`, `MaxOpenConns=1`).
  The per-import advisory locks of the PostgreSQL engine map onto the
  IMMEDIATE transaction; the regression tooth is
  `TestCrossEngineWriteSerialization` (two engine instances over one
  file — removing `_txlock=immediate` turns it red with `SQLITE_BUSY`).
- **Contention latency ceiling**: a competing writer parks up to 5 s
  inside the busy handler; a cancelled context does NOT interrupt the
  park. Callers that must bound latency cancel above the repository or
  accept the ceiling.
- **Cross-process guarantees** (re-exec probes): the writer lease
  refuses a second process with the typed conflict diagnosing the live
  owner; a child write parks-and-succeeds under a held lock
  (busy-timeout as behavior, not just a value).
- **Boundary — single host**: the WAL lives next to the file; ownership
  is one host's process set. Multi-replica or network-filesystem
  deployments need the PostgreSQL profile (SQLite on NFS is corruption;
  two hosts cannot share one file-lock domain).
- **No ATTACH, no cross-component queries**: the file carries only the
  `library_*` namespace (test-pinned); `config.sqlite` (F13) is a
  separate runtime-configuration file — Fachdaten never land in it.
- **Type mapping** (both engines interpret the DM03 rules identically):
  UUID → canonical lowercase TEXT (app-minted), time → fixed-width µs-UTC
  TEXT (lexicographic = chronological), JSON → TEXT compared
  semantically (JSONB normalizes key order; SQLite stores exact bytes —
  the same document either way), enums share one vocabulary.

The SAME repository contract suite (`internal/library/reposuite`) runs
against both engines — CI legs `library-engine-postgres` and
`library-engine-sqlite` (the latter PostgreSQL-free). Engine
availability is decided BEFORE the suite body; a skip inside a running
engine is a violation the skip guard fails (teeth pinned by probe).
The suite's fixtures are shared; JSON roundtrips assert semantic
equality for exactly the JSONB/TEXT reason above.

## 3. Store capability model

Five capabilities, reported honestly from actual equipment — never
guessed present:

| Capability | Vouched by |
|---|---|
| `dense_vector` | dense arm ∧ query-embedding runner with a PROBED `query_embedding` role |
| `bm25` | BM25 arm over the OpenSearch index |
| `hybrid` | both recall arms (`dense_vector ∧ bm25`) |
| `rerank` | rerank enabled ∧ runner with the probed `reranking` role |
| `graph` | graph arm ∧ wired graph source |

Mechanics: the search stack reports its own equipment
(`search.Service.Capabilities()`, atomics — the startup role probe
writes from its goroutine); the Store keeps the reporter and delegates
LIVE per call — the composition's probe lands AFTER construction and
must be reflected (snapshot semantics is the exact regression the
witness `TestCapabilitiesDelegateLiveAfterConstruction` pins).
Unprobed = not vouched = absent. Withdrawal shrinks the report; the
per-capability sonde proves the model tracks the backend, and the
mapping oracle fails on vocabulary drift between the backend report and
the Store's five fields. The backend may carry report-only extras
(`sparse`) — documented, folded under `bm25` in the Store vocabulary.

## 4. DM handover points (Data Migration track)

- **`pglib.VerifyAdoption(ctx, pool)`** — the read-only Bestands-DB
  check (SELECTs only): fresh database → adoptable; complete ledger →
  idempotent; tables without a complete ledger → REFUSED (no silent
  adoption of drifted state). No production caller by design — it is
  the DM09 hook. The committed witness runs against a restored copy of
  the Bestands database (dev-host artifact like the freeze dump;
  strictly gated by `AXIOM_F12_BESTANDS_DSN`, never
  `AXIOM_TEST_DATABASE_URL` — CI's go-db-it would otherwise run it
  against a fresh, library-less database).
- **Mits-Schreib lane** — wired ONLY for shared-database shapes
  (`AXIOM_LIBRARY_DATABASE_URL` unset or identical to the core DSN);
  a separate library DSN or the SQLite profile unwires it with a loud
  log line (the engine additionally folds mirror reads to absence —
  `TestMitschriebLaneOnLibraryOnlyDatabaseFoldsToAbsence` pins both
  halves, including that library-owned schema faults stay raw errors).
- **Legacy adoption is a check, never a cutover** — the production
  cutover remains DM09. The composition's separate-pool seam is the
  cutover surface: the DM track turns a DSN, not code.
- **`zotero` import providers require the PostgreSQL profile** — the
  provider's source identity lives in the mirror on the shared
  database; a component-local SQLite file must not reach into the store
  repository for it (refused loudly at start, not degraded silently).

## 5. Configuration surface

`AXIOM_STORAGE_LIBRARY_DRIVER` (`postgres`|`sqlite`, default
`postgres`), `AXIOM_LIBRARY_DATABASE_URL` (own DSN, default = core
DSN), `AXIOM_LIBRARY_SQLITE_PATH` (default
`~/.axiom/library.sqlite`) — all visible in
`axiom config get --effective --json` (DSN sanitized like the core
DSN), vocabulary-checked by `axiom config validate`, documented in
`configuration.md`. Shutdown closes in reverse start order: writer
lease → library engine (file/pool) → store pool. A start failure after
a pool opened closes everything it opened (witnessed).

## 6. Deliberate deferrals (current state)

| Item | State | Route |
|---|---|---|
| `seedMirror` fixed-seed duplicate-key flake (store intake IT) | Known flake, 1-in-runs, non-reproducing | Tracked as an issue comment; fix via `ON CONFLICT` or per-test scratch DB when touched next |
| Bestands DSN guard matches the whole pre-`?` DSN, not the db name | Accident guard; read-only downstream | Sharpen to `dbOf`-style name match if it ever misfires |
| `.blankprobe-*` transient dirs not gitignored | Pre-existing (#298 test), interrupted runs can leave residue | `.gitignore` entry when the repo next touches that area |
| Test seams on `library.Service` (`Repo`, `ArmHalt`, `SetPort`, `LoadResolveDetail`, …) | Cost of the engine batteries living outside the package | Narrow (typed setters, exported DTO) with the next service change |
| `repo.ErrRowAbsent` translated at only two methods | Package is engine-allowlisted; internal uses legal | Widen with the SQLite-Store epic |
| `RevisionIntake` DTOs live in `repo` | Neutral by construction (no engine types); lint-guarded | Move to a contracts-adjacent home with the SQLite-Store epic |
| Env naming `AXIOM_STORAGE_LIBRARY_DRIVER` | Briefing-specified key shape (`storage.library.driver`) | Keep |
