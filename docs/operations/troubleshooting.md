# Troubleshooting

This chapter turns hard-won operating lessons into transferable patterns:
symptom → likely causes → diagnose → fix. There are **no dates, machines, or
one-off stories here** — only patterns that apply to any deployment. When
something goes wrong, start at the symptom table, follow the diagnostic step,
and fix the cause (not the symptom).

> **Reading the table:** each row names a symptom class. Multiple causes are
> possible for the same symptom; the diagnostic step tells you which one to
> rule in or out first. Apply fixes at the root, not by re-running the same job
> blindly (reprocessing only re-does changed/invalidated work, so a blind
> restart rarely helps a real cause).

## The symptom table

| Symptom | Likely causes | Diagnose | Fix (root cause) |
| --- | --- | --- | --- |
| **GPU idle, no errors, jobs stuck between "compute done" and "persisted"** | Transport ceiling (bulk collapses on a tunnel or a port-forward); silent exit in a poll retry path | Check the serving path, not the compute: loopback inside the container vs. the mapped port vs. a tunnel. Is a multi-MB result crawling? | Ensure direct LAN reachability for both directions (see [Deployment → transport rule](deployment.md)); do not route bulk flows through a tunnel or userspace port-forward; use host networking. |
| **Every runner stacked on GPU 0 (VRAM pileup, one OOM)** | GPU pinning was trampled (container-detection mis-read the environment and overwrote `CUDA_VISIBLE_DEVICES`) | Confirm each container sees exactly the intended single device (`torch.cuda.device_count()==1` and the expected device name) | Make the image emit the container-detection marker (`RUN touch /.dockerenv`) and add a start gate that asserts per-card device count/name before a parallel run. |
| **Entity extraction is ~10× slower than expected (CPU-bound)** | GLiNER fell back to its CPU default instead of the chosen accelerator | Check the device env for the entity extractor; watch cores saturate on one book for an hour that should take minutes on GPU | Set `DEVICE_GLINER=<cuda|mps>` explicitly (the default is CPU; the accelerator is not auto-selected for this model). |
| **Jobs vanish to `failed` without an obvious file problem** | `SOURCE_NOT_FOUND`/hash mismatch (allowed roots too narrow, or file moved); a deterministic conversion failure | Look at the terminal error code on the job; check whether the attachment path is inside the runner's allowed roots | Widen `AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS` to the real storage root; confirm the attachment still exists and its hash matches. |
| **A scan-heavy PDF dies with `CHILD_OOM_SIGKILL`** | The conversion child was killed by memory pressure while processing raster pages | Inspect the scan profile and decoded-byte volume in worker logs | Use the batched scan path on an updated runner; treat this as runner/resource pressure before declaring the source broken. |
| **Persist rejects `CHUNK_IMAGE_REF_UNRESOLVED` for an external URL** | Markdown link syntax was misread as an image reference | Check whether the unresolved ref starts with `http://` or `https://` | Rerun on a runner that drops external URL refs before artifact validation; local image refs must still resolve strictly. |
| **A re-run of a document produces different bytes** (nondeterminism) | A temp-path leak wobbles the output (e.g. EPUB extraction tempdir suffix lands in chunk text), or a Marker layout edge case | Compare two independent runs; diff to classify (path leak vs. heading/table-classification flip) | Normalize temp/media paths before chunking; for pure Marker classification variance, decide whether deterministic output is a requirement (it is not for retrieval). |
| **The search index shows stale or duplicated content after a rebuild** | The index served a superseded generation (no tombstone/obsolete handling), or a force-rebuild double-activated a snapshot | Compare OpenSearch doc count to the active snapshot count; look for orphaned/duplicated chunks | Ensure outbox delete/obsolete operations run in the same persist transaction; rely on latest-persist-wins per attachment. |
| **Parallel workers clash on a fresh database (one crashes at startup)** | Concurrent schema migration racing (a `pg_type`-style conflict among same-kind objects) | See which instance failed and whether a restart succeeds | On a clean slate, bring up **one** instance to migrate first, then the others (fail-fast + restart is safe; no corruption). |
| **A processing job resumes but the runner rejects it after a restart** | The runner was acknowledged already (its artifacts are gone); a re-submit hits a wall | Check the job's error for a terminal "artifacts expired" code | Recompute with a fresh idempotency key (`force_rebuild`); do not retry the same key against an acknowledged job. |

## The top diagnostic moves

1. **Check the serving path before blaming compute.** "GPU idle + jobs stuck"
   is almost always transport, not models. Loopback-fast does not mean
   mapped-port-fast.
2. **Read the terminal error code, not the log noise.** Terminal failures carry
   a stable machine-readable code that classifies retryable vs. not.
3. **Confirm the device, not just the model.** The most common "slow" culprit
   is an extractor running on the CPU default when the accelerator was expected.
4. **Verify the index against the active snapshot count.** Index divergence is
   a symptom of a missed tombstone/obsolete step, visible only by comparing
   counts.
5. **Separate source quality from runner resource classes.** A scan-only PDF
   that OOMs is not automatically corrupt; an EPUB with normalized OPF paths is
   not automatically invalid because pandoc rejected literal `..` references.

## Recovery rules

- **Never "blast away" a job before a reset.** Jobs, leases, and the index are
  tied together; tear down in a defined order (stop dispatch, drain, then
  reset) and rebuild from a clean baseline when required.
- **A restart is safe.** In-flight work is recovered to a valid state; a job
  never shows `completed` without being durably committed.
- **Reprocessing respects the hash gate.** Only changed or invalidated
  documents are redone — a fix, then a re-sync, is the normal recovery path.

## Sizing / performance reference points

These are *measured orders of magnitude* to set expectations and pick hardware.
They reference the dated [measurement reports](../references/benchmarks.md);
treat them as guidance, not guarantees.

| Workload | Order of magnitude |
| --- | --- |
| Small document, warm cache, GPU class (RTX-3090-class) | ~30 s |
| Book (~300 pages, full profile) on a 3090-class GPU | ~6 GPU-minutes / ~0.7–1.2 s per page |
| Book on a laptop-class GPU (RTX-A3000-class) | ~18 GPU-minutes / book (a straggler in a mixed pool) |
| Device-embedded alternative (Apple MPS) | complete but **~10× slower** than an external GPU — fine for occasional runs, not for mass processing |
| Library average throughput (concurrency=1) | ~20 documents/hour |
| Runner VRAM footprint (all three model families) | ~2.8 GB (fits a 12 GB card; room on 24 GB for a second pinned runner) |

**Hardware guideline:** for production mass processing prefer an NVIDIA GPU with
≥12 GB VRAM; a laptop-class card works but bottlenecks a mixed pool. MPS is a
fully supported single-host alternative when external GPUs are not an option,
with the ~10× performance penalty noted above.

Next: [Deployment](deployment.md) · [Monitoring](monitoring.md)
