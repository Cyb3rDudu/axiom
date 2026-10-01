# Container topology example (F14 #308)

One image, roles per argument — the same software as the OS-process split,
as containers:

```bash
docker build -f deploy/container/Dockerfile -t axiom-topology .
docker compose -f deploy/container/compose.topology.yml up -d --wait
deploy/container/topology-smoke.sh
docker compose -f deploy/container/compose.topology.yml down -v
```

- **Image**: multi-stage — the Go binary plus the reference-mode compute
  worker (light stack: fastapi/uvicorn/pydantic/pymupdf + pandoc for the
  EPUB lane). Heavy/GPU deps are NOT in the image; the processor contract
  is backend-transparent, and the topology proof runs reference compute.
- **Roles per argument**: `serve all|api|library|store` (the binary) and
  `worker` (the compute service) — see `entrypoint.sh`. Any other
  argument falls through, so the image doubles as an inspect shell.
- **The compose stack**: ephemeral Postgres (pgvector) + OpenSearch + a
  Zotero-probe stand-in + the three RAG role containers (sequential boot:
  `db.Migrate()` owns the schema; concurrent first migrates collide on
  `CREATE TYPE`) + the compute worker in its own network namespace (the
  remote-class equivalence leg — reachable only over HTTP).
- **The smoke** (`topology-smoke.sh`) drives the same contract classes as
  the OS-process split smoke: four-check health, intake through the
  public edge, the remote-class ride to searchability, typed search and
  passage shapes (no component-internal field leaks), the kill probe
  (library container TERM → typed `library/unavailable` envelope), and
  teardown completeness. CI runs it in the `container-topology` job.
- **Kubernetes**: `deploy/k8s/topology.yaml` carries the same roles as
  Deployment/Service manifests (structurally validated in CI with
  kubeconform; no cluster dependency).

Everything is project-scoped and ephemeral: `down -v` drops all state.
