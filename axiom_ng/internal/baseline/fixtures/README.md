# Baseline fixtures (frozen v0.1.18 compatibility baseline, #295)

Three fixture kinds live here:

- **Probe INPUTS (hand-edit):** `*_probe.json`, `search_golden.json` — the
  query/id/rules definitions the live probes execute. Edited by hand when
  a probe should look at a different object; not regenerated.
- **Gating OUTPUTS (regenerate):** `health.json`, `capabilities.json`,
  `search_classes.json`, `passage_shape.json`, `kg_*_shape.json`,
  `ingest_outcome.json`, `custody_protocol.json`, `schema_fingerprint.txt`,
  `api_inventory.txt`, `canonical_identity.json` — the frozen expected
  values. Regenerate deliberately:
  `BASELINE_UPDATE=1 go test ./internal/baseline -run <Test> -count=1`
  (or `-update`).
  `canonical_identity.json` is the one WORKING-TREE golden (the live
  fixtures witness the freeze bits): the additive canonical identity
  fields of /api/health per ADR 0001 (#296).
- **Non-gating reference:** `live_row_counts_freeze_day.txt` — freeze-day
  row counts of axiom_dev, documentation only. Its runtime twin
  `live_row_counts.txt` lands in the actual dir, excluded from the
  determinism self-check.

Rule for deliberate updates: a fixture diff must land in the SAME PR as
the behavior change and be review-visible; CI runs the suite without
update rights and goes red on any drift.
