-- 0020_quality_state_english.sql
-- #219: rename German keys in stored quality_state / repair-case analysis
-- JSON to the canonical English contract names (verdacht → finding,
-- grund → reason) BEFORE the client contract freeze (#196). In-place,
-- idempotent: rows without the German keys match no WHERE and stay
-- untouched; unknown keys are preserved verbatim. The jsonb_typeof
-- guard keeps non-object values (arrays/scalars that the ? operator's
-- array semantics would match) out of the rewrite entirely.
UPDATE ingest_jobs
SET quality_state = jsonb_set(quality_state, '{finding}', quality_state->'verdacht') - 'verdacht'
WHERE jsonb_typeof(quality_state) = 'object' AND quality_state ? 'verdacht';

UPDATE ingest_jobs
SET quality_state = jsonb_set(quality_state, '{reason}', quality_state->'grund') - 'grund'
WHERE jsonb_typeof(quality_state) = 'object' AND quality_state ? 'grund';

-- repair_cases.analysis receives the SAME dispatcher quality_state JSON
-- (CreateRepairCase), so it carries the same German keys.
UPDATE repair_cases
SET analysis = jsonb_set(analysis, '{finding}', analysis->'verdacht') - 'verdacht'
WHERE jsonb_typeof(analysis) = 'object' AND analysis ? 'verdacht';

UPDATE repair_cases
SET analysis = jsonb_set(analysis, '{reason}', analysis->'grund') - 'grund'
WHERE jsonb_typeof(analysis) = 'object' AND analysis ? 'grund';
