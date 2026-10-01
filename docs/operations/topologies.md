# Deployment topologies

Date: F14 (#308) — structural overview; the operator walkthrough (install,
upgrade, secrets, backup) follows with F15 (#309)
Scope: which role runs in which operating form, which edges exist between
processes, and which witness proves each form in CI

The whole point of the 0.2.x architecture: **the same software, every
supported operating form** — one binary, roles per argument, no
orchestration logic in the domain layer (pinned by a lint sonde; the
Kubernetes example ships as manifests, never as imports).

## The roles

`axiom serve <role>` selects the process slice:

| Role | Composition | Serves |
| --- | --- | --- |
| `all` | every role (the default boot) | the full stack in one process |
| `api` | api + store + events + sync + repair + search + ingest (no loops) | the public edge; Library/Store surfaces proxy to the library/store processes when split |
| `library` | api + store + events + sync + **repair** | the Library contract surface on its internal edge; the repair track (supervised `axiom-repair-worker` children, Zotero write gateway) lives here in split |
| `store` | api + store + events + search + ingest + dispatcher | the processing half: intake (`POST /api/v1/store/ingest`), the claim loop, the signed processor-source serving, the OpenSearch outbox |
| compute worker | separate Python service (`axiom-compute-worker`) | the processor contract v1 over HTTP — reference mode for proof/CI, real compute for production |

## The matrix (role × form × witness)

| Form | What runs | Edges | CI witness |
| --- | --- | --- | --- |
| **All-in-one** | `axiom serve all` (or bare boot) — the default | none (in-process) | the F01 golden suite (freeze bits) against release builds; the full unit/IT matrix |
| **Split process** | `serve library` + `serve store` + `serve api` as three OS processes; compute worker as a fourth process | api→library edge, api→store edge, store→worker HTTP, worker→store (signed source URLs) | `split-topology` CI job: three-process E2E over an ephemeral database + index — health, intake, remote-class ride to searchability, typed search/passage, kill probe, teardown |
| **Container** | one image, roles per argument (`deploy/container`) — the same three RAG containers + the worker container + ephemeral Postgres/OpenSearch | as split, over container DNS | `container-topology` CI job: image build, compose up (sequential boot), the same smoke classes against the containerized public edge |
| **Kubernetes** | the same image; `deploy/k8s/topology.yaml` (Deployments api/library/store/compute-worker + Services; roles per args) | as split, over Services | structural: kubeconform in CI + the K8s-import lint sonde keeps the domain layer free of `k8s.io` — a real cluster is deliberately NOT a CI dependency |
| **Remote compute** | the worker reachable only over HTTP (another host, a carrier, a container namespace) — the store signs source URLs, the worker fetches content itself | store→worker (process), worker→store (content + ack) | the split/container rides above prove it per push: the job completes on the only configured worker, ack included |
| **Repair as child** | the library process (or the all-in-one process) supervises `axiom-repair-worker` children — event runner, no service manager entry | parent→child process | kill sonde in both shapes: worker SIGKILL mid-case → retryable, no zombie lease, recovery heals (F08 executor test = all-in-one leg; the library-role IT = split leg) |
| **launchd (macOS)** | the shipped templates under `deploy/launchd` — CPU-bound services at `ProcessType=Standard` | as all-in-one/split | the plist QoS gate in CI + `qos-probe.sh` for operator self-measurement ([launchd scheduling](launchd-qos.md)) |

## What is deliberately NOT in CI

- A real Kubernetes cluster — the manifests are validated structurally
  (kubeconform) and the domain sonde pins the import boundary; cluster
  behavior is covered by the container-equivalent ride (same image, same
  roles, same edges).
- A real remote host — remote compute is proven by equivalence: the worker
  runs in its own process/network namespace and is reachable exclusively
  over HTTP, exactly the property "remote" adds.
- Real Zotero — the split rides use the fake provider contract (the
  Library's own test surface) and a Server-ID probe stand-in; the real
  provider's Zotero ITs stay gated on a live Zotero.
- The operator's host state — every CI ride is ephemeral: scratch
  databases, fresh indexes, temp state roots, full teardown assertions.

## Pointer map

- Container image + compose + smoke: `deploy/container/`
- K8s example manifests: `deploy/k8s/topology.yaml`
- Split-process dev scripts (working-tree ride on the dev host):
  `scripts/dev/split-up.sh`, `scripts/dev/split-smoke.sh`,
  `scripts/dev/split-down.sh`
- launchd templates + scheduling policy: `deploy/launchd/`
- Persistence profiles per component (PostgreSQL/SQLite): the
  [component persistence](../developer-guide/component-persistence.md) page
