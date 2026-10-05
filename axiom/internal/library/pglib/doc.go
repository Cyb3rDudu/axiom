// Package pglib is the Library component's PostgreSQL repository
// engine (F12, #306) — one of the library.Repository implementations
// (the SQLite twin lives in internal/library/sqlite). The SQL dialect
// lives here and nowhere else: per-import advisory-lock serialization,
// event sequence minting, revision minting, anchor+audit atomicity.
//
// The zotero_* mirror (mirror_reads.go, revisions.go, schema/0004) is
// Library-database-resident since #358 (ADR 0002): this engine OWNS the
// mirror schema and its reads; a never-synced or pre-0004 database folds
// mirror reads to absence, never an error.
//
// Boot order for the PostgreSQL profile: Migrate (own ledger), then
// optionally VerifyAdoption — the read-only Bestands-DB check (fresh /
// ledgered / refused verdicts; the DM cutover decides, never adopts
// silently).
package pglib
