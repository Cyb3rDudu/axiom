# KG Maintenance

KG maintenance keeps entity identity, alias families, relation topology, and the
materialized KG read model in sync with active snapshots. Raw extractor rows
remain the source of truth; maintenance rewrites derived identity/topology state
and then refreshes the read tables.

## When to run it

Run the chain after deterministic identity rules change, after a large corpus
rebuild, or when a sync burst has created many duplicate entity forms. Normal
successful Zotero syncs already schedule exact-form entity consolidation with a
10-second debounce. The manual chain is for operator-controlled rebuilds of the
KG identity layer.

Take a database backup before any mutating run. Prefer a quiet window: the
maintenance paths serialize themselves with a transaction-scoped PostgreSQL
advisory lock, but a quiet system makes the before/after counts easier to read.

## Standard chain

Use the same binary and database URL that the server uses.

```bash
export AXIOM_DATABASE_URL='postgresql://...'

# 1. Type role/group nouns as CONCEPT. Dry-run first.
axiom-ng -normalize-entity-types
axiom-ng -normalize-entity-types --apply

# 2. Merge guarded exact-form duplicates. Dry-run first.
axiom-ng -consolidate-entities
axiom-ng -consolidate-entities --apply

# 3. Bind exact and flexion alias families. Dry-run first.
axiom-ng -bind-all-aliases
axiom-ng -bind-all-aliases --apply

# 4. Repoint relation endpoints from variants to family roots.
# This command mutates immediately: there is no --apply gate.
axiom-ng -repoint-alias-edges

# 5. Collapse duplicate relation triples. Dry-run first.
axiom-ng -consolidate-relations
axiom-ng -consolidate-relations --apply
```

The dry-run contract is exact for `-normalize-entity-types`,
`-consolidate-entities`, and `-consolidate-relations`: the candidate count is
computed by the same selection logic as the apply path. `-bind-all-aliases` has
a narrower dry run: it reports exact-form alias candidates and the current alias
counts, while `--apply` runs exact-form binding and then flexion binding.
`-repoint-alias-edges` is the exception in the current product state; it has no
dry-run mode and should be treated as a mutating maintenance command.

## Watchdog pattern

Record the dry-run output and then compare it with the apply output. A guarded
entity-consolidation dry run reports:

```text
dry-run: <groups> guarded groups / <entities> entities would merge
```

The apply output reports:

```text
entity consolidation complete: <entities> entities merged, duplicate forms <before>-><after>
```

A healthy idempotent rerun reports zero or no eligible work after the chain. If
counts diverge materially, stop and inspect concurrent sync/ingest activity
before continuing.

## Safety boundaries

All KG maintenance transactions except dry-run reads run under the same
transaction-scoped advisory lock. A crash or cancellation rolls back the
transaction and releases the lock. Entity consolidation archives loser evidence
in `kg_superseded_entities` before deleting loser rows. Read tables are
rebuildable projections; a refresh can delete and repopulate `kg_entity_roots`,
`kg_relation_triples`, and `kg_relation_evidence_docs` from active raw KG rows.

Continue: [Monitoring](monitoring.md) · [Troubleshooting](troubleshooting.md) ·
[Data Model](../references/data-model.md)
