// Package store is the Store component (F09, #303; contract F03 #297).
//
// Store owns all processed-corpus truth: revision intake, ingest jobs,
// processing snapshots, chunks + provenance, dense/sparse embeddings, the
// KG read model, the OpenSearch outbox, and the search/passage read
// paths. The component's single intake is IngestRevision — a validated,
// versioned revision.SourceRevision (the Library→Store bridge DTO).
// Zotero/provider identities never enter this component: they are opaque
// external references, and no Store package may import the Zotero
// adapter, the sync path, the Library implementation, or the credential
// carrier — transitively, which the boundary lint in this package proves
// (boundary_lint_test.go).
//
// Transition (documented dual-read, abated by F12/DM06): the legacy
// sync→enqueue lane still writes ingest_jobs via internal/repo's exported
// sync-job effects, and the claim path resolves revision identities
// against the shared Zotero mirror tables (SQL, not imports) until the
// revision lane replaces the legacy lane and the persistence split drops
// the cross-component FKs.
package store
