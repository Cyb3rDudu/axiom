# Benchmarks & Analyses

Historical, dated measurement reports about the processing pipeline. They
document system behavior at a point in time — **not** ongoing documentation of
the current state. The figures are real measurements from the respective runs,
and they remain valid as measurements.

> **Framing:** Every measurement report states its date, setup, and data basis.
> Concrete machine/network details are reduced to roles (e.g. "GPU host") and
> placeholders; the technical lessons (transport, fencing, GPU pinning) are
> generalized as operating rules in [Operations → Deployment](../operations/deployment.md)
> and the [Developer Guide](../developer-guide/architecture.md).

## Reports

| Report | Date | Key takeaway |
| --- | --- | --- |
| [L8 Throughput Analysis](benchmarks/l8-durchstich.md) | 2026-08-15 | Horizontal throughput (16/16 books on 3 GPUs, 1.71× throughput), quality-gate GO, twelve-trap offender chain |
| [TC2: 3-Runner Parallel Test & Determinism](benchmarks/tc2-parallel.md) | 2026-08-15 | Work-conserving distribution, single-snapshot exclusivity, determinism around Marker |
| [Mass Chunking](benchmarks/mass-chunking.md) | 2026-08-14 | 16/16 complete, 0 failures, throughput/cold-warm, profile finding |
| [Chunk Quality (Quality Gate)](benchmarks/chunk-quality.md) | 2026-08-15 | Chunk/locator/entity/relation quality, kNN search test, GO for TC2 |
| [Retrieval Quality](benchmarks/retrieval-quality.md) | 2026-08-16 | Retrieval arm selection (dense/hybrid/rerank/sparse/graph); gold set provisional |

The **canonical originals** live in `axiom_ng/docs/benchmarks/`; these pages are
the site-facing view.

## Related

- [Data Model](data-model.md) · [FAQ](faq.md) · [Welcome](../index.md)
