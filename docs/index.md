# axiom

A Zotero-powered research knowledge system that turns your literature into a
searchable knowledge base.

```text
Zotero library → processing pipeline → searchable data
```

**axiom** orchestrates processing with a Go application (dispatcher,
persistence, fencing, outbox), while the actual compute-heavy work is done by a
Python compute runner. The result is a structured, searchable knowledge base of
your documents, entities, and relationships.

## Who is this for?

- **Users** — connect Zotero, process documents, search the data.
  → continue to the [User Guide](user-guide/ingest.md)
- **Developers** — understand the architecture, contract, configuration, testing.
  → continue to the [Developer Guide](developer-guide/architecture.md)
- **Operators** — set up a runner, monitor jobs, resolve failures.
  → continue to [Operations](operations/deployment.md)

## Where to go next?

| Goal | Path |
| --- | --- |
| Understand in 10 minutes what happens | [Concept Tour](get-started/concept-tour.md) |
| Setup and first run | [Quickstart](get-started/quickstart.md) |
| System architecture | [Architecture Overview](developer-guide/architecture.md) |
| Operate a runner | [Deployment](operations/deployment.md) |
| Measurements and analyses | [Benchmarks & Analyses](references/benchmarks.md) |
| Origin, license, logo | [About](about/index.md) |
