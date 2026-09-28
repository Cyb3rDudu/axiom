# F11: Local + HTTP Component Adapters — Design

**Status:** Landed with #305 (0.2.0 Zug, Epic #342)
**Scope:** Library/Store contract bindings, internal versioned edge, split topology, parity suite
**Implementation:** `axiom_ng/internal/bindings` (the package doc is the normative, code-adjacent copy of this comment)

---

## 1. Binding matrix

| Topology | Library seam | Store seam | Public edge |
|---|---|---|---|
| `serve all` | Local — the `*library.Service` behind `LocalLibraryClient` where mediation is injected; the public import surface binds the service directly (its F06 extended methods are the surface) | Local — `*store.Service` behind `LocalStoreClient` (explicit since F11) | one process |
| split (`serve library` + `serve store` + `serve api`) | `HTTPLibraryClient` → `/internal/v1/library/…` on the library process | `HTTPStoreClient` → `/internal/v1/store/…` on the store process | the api process only |

The SAME `contractsuite` (F03 harness, all fixtures, fault probes) runs against
both bindings — split-process semantics are a test result, not a claim
(`internal/bindings/bindings_test.go`).

## 2. Port resolution (runtime config, never public contract)

The public client surface stays ONE base URL. Which ports components listen on
is deployment configuration (all in `config`/effective-config):

| Env | Meaning |
|---|---|
| `AXIOM_INTERNAL_LIBRARY_ADDR` | library process serves `/internal/v1/library/…` here ("" = off) |
| `AXIOM_INTERNAL_STORE_ADDR` | store process serves `/internal/v1/store/…` here ("" = off) |
| `AXIOM_LIBRARY_URL` | api process binds its Library contract surface to this edge ("" = local) |
| `AXIOM_STORE_URL` | api process binds its Store contract surface to this edge ("" = local) |
| `AXIOM_COMPONENT_TIMEOUT` | per-request budget for non-streaming internal calls (default 30s) |

Internal edge bind failures are loud start failures (like the public listener);
serve-loop errors after a successful bind only log — clients degrade to typed
503s, which is the fault-parity contract.

## 3. Transport error mapping

The wire carries the typed error envelope
(`{"error":{component,class,message[,idempotency_key]}}` — the same dialect as
the public routes). The client never string-matches:

| Wire / transport | contract class | retryable |
|---|---|---|
| envelope class (any typed answer) | that class | per class |
| 409 + `idempotency_key` | Conflict as `*IdempotencyMismatch` | no |
| connection refused / reset / EOF (killed backend) | Unavailable | yes |
| request budget expired | Deadline | no (fresh budget only) |
| non-envelope body | status-table fallback → Internal | no |

(`context.Canceled` propagates unwrapped — the caller gave up.)

Messages of transport-mapped errors are fixed generic strings: no component
host:port, no dial text. The raw transport error goes to the log. The public
answers therefore cannot leak topology (the kill-probe leak sonde proves it),
and a killed Library process surfaces the SAME class as the local crash
injection (Unavailable → 503 typed envelope on the import surface; the frozen
legacy degradation shape on search).

Retry policy: GET-shaped calls retry ONCE on retryable transport failure; the
keyed writes (`StartImport`, `IngestRevision`) fly exactly once — idempotent by
key, but a replay is the caller's explicit decision, never a hidden second
flight.

## 4. WS/event edge decision: forwarding

The event bus is process-local (#249). F11 chooses FORWARDING: the store
process's internal edge streams its bus as SSE (`GET /internal/v1/store/events`,
heartbeat 15s), and the api process bridges it onto its own broker
(`bindings.BridgeEvents`, exponential reconnect backoff, capped 30s). The public
`/api/ws` and `/api/runners/live` therefore work identically in both
topologies.

Derived frames (`RunnerStateChanged`) are NOT relayed — both sides run the
deterministic deriver over the source events; relaying derived frames would
double-publish. Known semantics: the bridge is best-effort observability (no
snapshot/replay across the edge — an event published during a bridge gap is
lost on the api side, exactly like a WS gap; the durable truth is the DB).

## 5. AuthN at the internal edge

`bindings.Authenticator` guards every internal route; 0.2.0 ships the Noop
default (the internal edge is deployment-private by topology — loopback binds).
A denying authenticator answers 403 with the typed envelope; the client maps
that to Internal (terminal). Real auth arrives post-0.2.0 through the same
hook, with no component or binding changes.

## 6. Public adapters and the field-name translation

The api process's public routes keep their exact shapes in split mode:
`PublicLibrary` (F06 replay signal, confirm/retry, multipart bound),
`PublicSearch`/`PublicPassage` implement the server's per-route service
interfaces over the HTTP clients, owning the ADR-0001 reverse translation
(`record_id` → `doc_id`, `rendition_id` → `attachment_id`). The byte-identity
test drives the REAL store forward mapping end to end — local public JSON and
HTTP-bound public JSON are byte-equal for a field-rich fixture.

Known delta (documented): the passage route's inactive-snapshot HINT degrades
to the plain 404 in split mode (the contract carries one NotFound class; the
hint body is 0.1.x legacy sugar). The legacy 0.1.x DB-backed surfaces (zotero
sync/selection, KG, repair, jobs, processor source) stay process-local in every
topology — they are not F03 contract surfaces; F12/F14 own their fate.

## 7. Dev topology

`scripts/dev/split-up.sh` drives the three working-tree processes with real
ports (api :8111 — the public base URL, library :8113 + edge :8211, store :8114
+ edge :8212, dispatcher in the store process) on the dev substrate.
`scripts/dev/split-down.sh` stops them. The header documents the split golden
run (`AXIOM_BASELINE_EXPECT_BUILD` — the documented BASELINE_UPDATE that runs
the SAME frozen fixtures through the new bits) and the kill probe.
