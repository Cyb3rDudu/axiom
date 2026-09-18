# Repair-Included Waves — the #282 wave semantics

A *wave* is any batch of documents drained through the ingest queue (a
rebuild wave, a rest wave, or a plain sync while dispatchers run). Since
issue #282 a wave is **repair-included**: documents that fail the preflight
dry-run do not strand — the repair loop-back is part of the wave, and no
operator action sits between "healed" and "processing".

## The wave contract

One document at a time, per the owner specification:

1. **Dry-run** the document (preflight quality gate).
2. **Repair if needed and repairable**: the fixer invoker claims the
   repair case and runs the heal (quarantine-first custody protocol).
3. **Auto-sync after a successful heal**: the invoker immediately runs a
   *targeted* sync (include = the healed document), so the healed
   attachment is enqueued and processes like any other job.
4. **Only then the next document is claimed.**
5. **Skip only when genuinely unrepairable**: a terminal park (fixer HALT,
   loop guard, exhausted retries) is documented in the repair case —
   never silent.

## Precondition: the fixer invoker runs

The repair-included wave assumes the fixer invoker is enabled wherever
repairs are expected (`AXIOM_FIXER_INVOKER_ENABLED=1` on the RAG process).
The queued/in_repair arm of the gate has no time bound by design — the
wave waits for its repairs — and the stale-claim reaper that eventually
unsticks a dead claim lives inside the invoker. With the invoker down,
every claim defers (`wave gate: claim deferred (N repair case(s)
queued/in_repair)` in the dispatcher log) until the invoker returns or an
operator blocks/requeues the case via the repair API. The healed arm is
bounded (1 h) either way.

**Blast radius (intended, know it):** the gate is instance-wide, not
per-document — one repair in flight serializes the whole ingest queue for
its duration. A 658-page OCR rebuild holds every claim for up to its
90-minute class budget. That is the owner-specified wave semantics (dry-run
→ repair → sync → NEXT document); if you need ingest throughput during a
long repair, stop the dispatcher for that lane deliberately instead of
wondering why claims defer — the log names the holding reason on every
poll.

## What holds the claim gate (`WaveRepairGate`)

Every dispatcher worker checks the gate before claiming; it defers
(nothing is marked, jobs are not touched) while a repair loop-back drains:

- a repair case is **queued or in_repair** (heal pending/running), or
- a case is **healed within the last hour with no job enqueued for its
  document since** — the post-heal sync has not landed (in flight,
  failed, or the invoker died between heal and sync).

The stranded-heal window is bounded (1 h): a heal whose sync never landed
stops gating after the window and stays visible in the invoker log
(`post-heal sync FAILED`) instead of silently freezing the wave.

**Never gating:** terminal parks (`failed`, `blocked_for_dudu`) and
manual-track `rejected` cases — an unrepairable document is skipped and
documented, it does not block the wave.

## Loop bindings (no sync storms)

- **One sync per heal**, by construction: the invoker has exactly one
  call site, called once per healed case. The sync itself is idempotent
  (content-hash dedup).
- **Per-attachment guard** (pre-existing): `repair_attempts` caps fixer
  attempts per attachment at 2.
- **Per-document guard** (#282): every heal *replaces* the attachment, so
  the attachment counter starts fresh each generation. The document-level
  healed count bounds the heal → sync → re-reject → heal cycle: once a
  document has been healed twice, a fresh preflight reject does **not**
  auto-queue — the case stays `rejected` for the operator (the manual
  queue path remains).

## What operators see

```text
fixer-invoker ... case …: healed (new attachment uploaded, post-heal sync follows)
fixer-invoker ... case …: post-heal sync ok — document … enqueued 1 job(s) (#282)
rag-dispatch log: slot 0: wave gate: claim deferred (1 repair case(s) queued/in_repair) — repair loop-back drains first (#282)
```

If the gate holds for longer than a heal should take, check for a stuck
`in_repair` case (the invoker's stale-claim reaper requeues after the
runtime window) or a failed post-heal sync (log line above names the
document; re-running the heal's document through a manual sync closes the
gap).

## History

Production evidence (twice): a heal completed and the document sat in
"awaiting preflight GREEN" until a manual sync hours later ("Wertorientierte
Unternehmensführung" 2026-09-15, "Nachhaltiges Personalmanagement"
2026-09-17). #282 closed the gap; the ITs pin both halves and the seam —
the invoker's targeted sync call (include = document, one per heal) and,
separately, the real sync service honoring a one-run include with an
enqueue. **Selection boundary:** a healed document outside the effective
selection (collection base or document exclusion) does not re-enqueue —
that is the documented sync semantics; the invoker logs
`enqueued 0 jobs` naming the document and the gate bounds the strand to
its 1 h window. A stranded heal shows up in the dispatcher log (the gate
names the case) and the invoker log (the failed/empty sync names the
document).
