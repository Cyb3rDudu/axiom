# ADR 0001: Canonical naming for the 0.2.0 component split

- Status: Accepted (F02, #296; part of the 0.2.0 epic #342)
- Date: 2026-09-22
- Baseline reference: F01 suite e7425f0 (#295); frozen tag v0.1.18,
  deployed binary generation bf77410 (suite ≠ deployed generation)

## Context

The 0.2.0 epic decomposes the current monolith (the RAG binary, the
Python runner and the fixer) into components and workers with new
public names. F01 froze the v0.1.18 compatibility baseline as an
unbiased witness. Without one
binding naming decision, every follow-up step (F05 CLI/runtime, F08
repair rename, F10 compute rename) would drift into its own vocabulary —
names that appear nowhere in the runtime are an intention, not a
decision. This ADR is the repository's first ADR (numbering starts at
0001 for that reason).

## Decision

### 1. Domain components

The domain components of axiom are:

| Component | Responsibility | 0.2.0 status |
|---|---|---|
| **Library** | Zotero selection/sync, document intake | compiled into the RAG process |
| **Store** | durable state: Postgres + OpenSearch | compiled into the RAG process |
| **Research** | research agents on the corpus | reserved — NOT implemented in 0.2.0 |

The RAG process serves all three component roles today (`api`,
`library`, `store` are compiled in). Real role resolution per process
arrives with F04/F05 (`axiom serve all|api|library|store`); until then
the identity fields document the target identity of the process, not a
runtime-resolved fact.

### 2. Workers

| Canonical worker | Role | Today (0.1.x) |
|---|---|---|
| **`axiom-compute-worker`** | document processing, query embedding, reranking | `axiom_ng_runner` (Python, "axiom-runner", processor name `axiom-python-marker`) |
| **`axiom-repair-worker`** | PDF repair (event runner) | `axiom-fixer` (`pdf_repair_agent`) |
| **`axiom-research-worker`** | research execution | reserved — NOT implemented in 0.2.0 |

### 3. Runtime

The public binary is **`axiom`** with role startup:

```
axiom serve all|api|library|store
```

The previous binary name becomes a functional **compatibility alias**
through 0.2.x (wired with F05; it delegates to `axiom` and reports
through the deprecation witness below — mapping table row 1).

### 4. Mapping table (old → canonical → alias → removal horizon)

| Old name (0.1.x) | Canonical (0.2.0) | Alias (functional through 0.2.x) | Removal horizon |
|---|---|---|---|
| `axiom-ng` binary, `axiom_ng` module | `axiom` (`axiom serve …`) | `axiom-ng` delegates | earliest an announced major; never silently (see §6) |
| `axiom_ng_runner`, service "axiom-runner" | `axiom-compute-worker` | old module entrypoint | same rule; alias wired with F10 |
| `axiom-fixer` (`pdf_repair_agent`) | `axiom-repair-worker` | `axiom-fixer` shim | same rule; alias wired with F08 |
| — | `axiom-research-worker` | — | reserved |
| — | components Library / Store / Research | — | extraction is F04/F06/F09 scope |

### 5. Identity fields (additive, ADR-consistent)

Names become a decision by being visible in the runtime. Both services
carry additive identity fields alongside every existing field — nothing
renamed, nothing removed:

| Field | RAG `/api/health` | Runner `/v1/capabilities` |
|---|---|---|
| `canonical_name` | `"axiom"` | `"axiom-compute-worker"` |
| `service_class` | `"api+library+store"` (compact role identity; narrows with F04/F05) | `"compute-worker"` |
| `component_roles` | `["api", "library", "store"]` | — |
| `roles` | — | `["document-processing", "query-embedding", "reranking"]` |
| `implementation` | — | `"python-marker"` |
| `instance` | — | serving host's hostname |

The F02 baseline extension freezes exactly these fields (fixture
`fixtures/canonical_identity.json`, regenerated per `BASELINE_UPDATE=1`
in the same PR — no general "allow additional fields" relaxation of any
comparator; only the concrete new fields are taken up).

### 6. Deprecation witness

One central hook per runtime (Go: `internal/deprecate`; Python:
`axiom_ng_runner.deprecations`) records use of a legacy name:

- **warns exactly once per process** per legacy entrypoint/env var
  (once-semantics; not per call),
- **counts every use** — the counter, exported as
  `deprecations: {<legacy name>: <count>}` in health/capabilities, is
  the data basis for the 0.3.x+ removal decision instead of gut feeling,
- is **silenceable** for tests.

Alias policy: aliases stay functional through all of 0.2.x. Removal
happens earliest in an announced major release, never silently, and only
after the exported counters justify it.

### 7. Language rule

Logs, documentation and capabilities use canonical names exclusively.
Legacy names appear only inside alias deprecation warnings (and,
within this ADR, in the §2 and §4 mapping tables). Code-path names
(`axiom_ng/`, `axiom_ng_runner/`,
`AXIOM_*` env vars) are technical facts of the 0.1.x tree and move with
their own steps (F05/F08/F10).

## Consequences

- F05 (runtime/CLI), F08 (repair rename) and F10 (compute rename)
  implement against this table and wire the legacy aliases through the
  deprecation witness; they must also carry the hand-pinned runner block
  in `fixtures/api_inventory.txt` along (F01 remainder — see #296).
- The F01 golden suite goes red on every identity change; updates land
  deliberately via `BASELINE_UPDATE=1` in the same PR.
- No behavior change: existing fields, logs and the freeze-bit golden
  fixtures are untouched by this ADR.
