# KG relations consolidation + typing + aliases — runbook (#198 targets 2+3)

## Order (blast-radius first, apply only after operator approval)

```bash
# 1. typing (smallest, no schema change)
axiom-ng -normalize-entity-types            # dry-run
axiom-ng -normalize-entity-types --apply

# 2. relations (the aggregation)
axiom-ng -consolidate-relations             # dry-run
axiom-ng -consolidate-relations --apply

# 3. aliases (needs migration 0015 applied first — server restart
#    or migrate; the column is additive + nullable, reads unchanged)
axiom-ng -bind-flexion-aliases              # dry-run
axiom-ng -bind-flexion-aliases --apply

# 3b. re-point variant edges to survivors + delete self-loops (MUST run
#     after alias binding, BEFORE the next consolidation)
axiom-ng -repoint-alias-edges
axiom-ng -consolidate-relations --apply     # resolves resulting pair duplicates
```

## Blast radius (production dry-runs, 2026-08-20)

- relations: **14,511 multi-edge pairs** would collapse
- typing: **25 rows** (17 bare_form: 9 PERSON, 4 ORGANIZATION, 4 already
  CONCEPT-normalized matches; 8 plural_head: 5 PERSON, 3 ORGANIZATION)
- aliases: **3,483 families / 3,876 variants** to link (biggest family: 5)

## Post-apply verification

```sql
-- one edge per pair:
SELECT count(*) FROM (
  SELECT least(source_entity_id,target_entity_id), greatest(source_entity_id,target_entity_id)
  FROM processing_entity_relationships r
  JOIN processing_snapshots s ON s.id=r.snapshot_id AND s.active
  GROUP BY 1,2 HAVING count(*)>1) t;  -- expect 0
```

```bash
axiom-ng -normalize-entity-types  # expect MatchedRows:0 (non-CONCEPT generic forms exhausted)
axiom-ng -bind-flexion-aliases    # expect Families:0 VariantsLinked:0
```

## Idempotency + re-sync

All steps are idempotent re-runs. New syncs re-create per-document
entities/edges — re-run after each sync (same discipline as
-consolidate-entities, which the post-sync hook already triggers).
Full post-sync sequence: -consolidate-entities → -normalize-entity-types
→ -bind-flexion-aliases → -repoint-alias-edges → -consolidate-relations
--apply. The repoint step is REQUIRED: new sync edges land on freshly
bound variants and would resurface as name-level duplicates without it.
