// Package pglib is the Library component's PostgreSQL repository
// engine (F12, #306) — one of the library.Repository implementations
// (the SQLite twin lives in internal/library/sqlite). The SQL dialect
// lives here and nowhere else: per-import advisory-lock serialization,
// event sequence minting, revision minting, anchor+audit atomicity.
//
// The zotero_* mirror reads (mirror_reads.go) are the shared-database
// strangler lane: Library-owned tables on the legacy Store database,
// until the DM track retires the sync lane (they die with the mirror).
//
// Boot order for the PostgreSQL profile: Migrate (own ledger), then
// optionally VerifyAdoption — the read-only Bestands-DB check (fresh /
// ledgered / refused verdicts; the DM cutover decides, never adopts
// silently).
package pglib
