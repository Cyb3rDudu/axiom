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
// #358 (ADR 0002): the transition is complete. The revision lane is the
// only claim lane (legacy-lane rows drain as LEGACY_LANE_RETIRED); the
// claim resolves rendition identities against the Store's OWN
// store_documents projection, written at intake/sync time — the repo
// package contains no mirror-table SQL (the boundary lint's grep sonde
// proves it, with teeth).
package store
