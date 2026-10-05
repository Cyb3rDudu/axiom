# ADR 0002: Mirror plane end state — Library-database mirror, Store-side projection

- Status: Accepted (#358 + #356, release v0.2.3; Decision 2026-10-06 — "Option 1")
- Date: 2026-10-06
- Components: Library (Zotero mirror, sync, repair), Store (processing, search)

## Context

The DM cutover moved the Library service plane (imports, sagas,
provenance, source revisions, provider anchors, writer leases) to the
Library database — but the Zotero mirror plane (`zotero_documents`,
`zotero_items`, `zotero_collections`, `zotero_attachments`,
`zotero_selections`, and the sync that writes them) kept running on the
Store database (`mirror.New(r.rep)` over the store pool). The Library
database carried a frozen cutover copy of the mirror that silently
diverged; roughly ten store-side code paths still read mirror tables
live (search hydration, processor-source serving, lease metadata,
completion guards, force rebuild, retention anchors, intake
suppression, the listing, the repair track). Held mirror rows for
Zotero-absent items were never reconciled away — deleted items lingered
as phantoms in the documents listing (the "126 failed documents"
investigation started there; 9 rows were cleaned in a one-off surgery).

Two options were on the table:

1. Complete the split: the mirror runs on the Library database; the
   Store denormalizes everything it needs at intake time into its own
   projection; cross-component reads are merged in code.
2. Formally declare the mirror store-resident and drop the Library copy.

## Decision

**Option 1.** "Components never share tables" is completed for the
mirror plane:

### 1. The Zotero mirror is Library-database-resident

The mirror repository (canonical apply, projections, selections,
contextual rules) runs on the Library component's own PostgreSQL pool in
every topology. The Library migrations carry the mirror schema
(idempotently — the production Library database already holds the
cutover copy, which becomes the LIVING mirror; the documented catch-up
is one full-reconcile sync, `full: true`). The Store database's
`zotero_*` tables remain as a frozen archive for the v0.2.3 soak; their
drop is documented follow-up.

The SQLite library profile is not combinable with the sync role: the
mirror is PostgreSQL-only, and a boot that would half-work refuses
loudly instead.

### 2. The Store denormalizes at intake: `store_documents`

Everything the Store needs to run processing is captured per rendition
at revision-intake time in the Store's own projection table: the durable
mirror identities (document/attachment/source UUIDs — opaque here),
record/rendition keys, content hash, the revision contract's
bibliography (title, authors, year, publisher, language, tags, citation
class), file facts (path, size, mtime, content type) and liveness
(preferred, deleted). Search hydration, processor-source serving, the
revision claim, the completion guard, force rebuild, retention anchors
and the intake suppression read the projection and nothing else — the
`repo` package contains zero references to Library-owned tables (a
boundary-lint sonde pins it, with teeth).

### 3. Reference model: identities instead of joins

The components do not join across the seam. Store-side questions that
used to be SQL joins resolve by identity:

- **Rendition + content hash is the boundary identity.** A revision
  (source UUID, record key, rendition key, content hash) names exactly
  one store_documents row; the legacy idempotency index is scoped to
  the retired zotero lane so the identity arbiters never collide.
- Documents and renditions are referenced by their UUIDs — plain
  columns, no cross-component foreign keys (DM06 #315 removed those;
  nothing reintroduces them).
- Cross-component reads (the documents listing, the repair wave gate)
  are TWO engine-local queries merged in code.

### 4. The sync is two transactions on two databases

The mirror phase (cursor read, canonical apply, projections, citation
class, cursor commit) commits on the Library database under the
per-source session lock — which ENDS at that commit: in the
single-database topology both locks would share one namespace and
self-deadlock across phases. The store-effect phase then runs as ONE
transaction on the Store database under the transaction-scoped twin of
the same lock key (serializing against claims): projection upserts,
revision intakes for processable renditions, failed-job records for
missing files, deletion marks for reconciled renditions, and the
attachment-snapshot reconciliation. A store-phase failure is loud and
structurally retried — the next sync's full derivation re-offers every
rendition.

The sync request accepts `full: true` (since=0): the full reconcile
that marks Zotero-absent items missing. Held rows become TOMBSTONES —
deactivated in the mirror, retired on the Store, visible ONCE in the
documents listing (`sync_state=tombstoned`, `outcome=removed`) for a
bounded window, then gone. The post-switch catch-up sync and
operator-initiated reconciles ride it.

### 5. The legacy claim lane is retired

Jobs of the retired mirror-read lane (`intake_kind='zotero'`) are
obsoleted with `LEGACY_LANE_RETIRED` at claim; new work flows
exclusively through revision intake (the sync drives it per changed
rendition with a content-fingerprint idempotency key — key mismatch is
impossible by construction). In-flight legacy jobs at the switch drain
as obsoletes and re-enter through the next sync's re-offer.

### 6. Repair and the wave gate span both planes honestly

The repair queue (`repair_cases`, loop guard, write audit) is
Library-side. Its Store-side effects go through exported repo seams: the
set-once `repair_linked` retention flag, and the wave gate's post-heal
job check (`HasJobForDocumentSince`). A process with no Library plane
(`serve store`) runs without the repair wave gate — the gate belongs
where repair is visible.

## Consequences

- The Library database's mirror is the single live mirror; the frozen
  divergence ends at the catch-up sync.
- The Store survives Library unavailability for everything already
  projected (search, serving, claims on known renditions); new work
  waits for the sync.
- The listing and every store-side metadata path show projection truth:
  fresh at intake, updated on every sync re-offer of a rendition.
- Migration: see the v0.2.3 section of the migration guide (bestand
  backfill in store migration 0004, the one-time full sync, the archive
  tables' soak).
