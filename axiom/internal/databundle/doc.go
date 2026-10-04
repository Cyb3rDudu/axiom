// Package databundle is the backend-neutral data bundle format (DM03
// #312) and the engine-portable export/import/verify machinery (DM04
// #313). A bundle is a directory:
//
//	<bundle>/
//	  manifest.json          metadata + per-table catalog + per-batch digests
//	  bundle.sha256          SHA256SUMS-style pin over manifest.json bytes
//	  tables/<t>/NNNN.jsonl  row batches, one canonical JSON object per line
//
// # Canonical mappings (the portability contract)
//
//	UUID        <-> lowercase 36-char string
//	TIMESTAMPTZ <-> "2006-01-02T15:04:05.000000Z" (UTC, microsecond-aligned)
//	JSONB       <-> canonical JSON embedded as a VALUE: object keys sorted
//	                recursively, compact, number tokens preserved verbatim —
//	                hashing is stable under key permutation
//	enum        <-> validated string (source vocabulary carried in the
//	                manifest; import validates against the manifest AND the
//	                target's own vocabulary — unknown values abort loudly)
//	boolean     <-> true/false (SQLite stores 0/1)
//	int64       <-> JSON integer
//	float64     <-> shortest-round-trip decimal
//	numeric     <-> verbatim decimal token (cast ::text on read)
//
// NULL is JSON null, empty string is "", and a MISSING column is a
// structural fault: bundle rows carry exactly the manifest-declared
// columns, and the import validates the target's columns match that set
// exactly (migration-stand drift aborts loudly, naming the diff).
//
// No DB-generated defaults apply on import: every column of every row is
// written explicitly (bigserial ids included; PostgreSQL sequences are
// re-synced after import via setval, which needs only the sequence USAGE
// grant the runtime role already holds — DM07's axiom_library can run
// the whole import).
//
// # Integrity (hash-verified, SHA256SUMS-signed)
//
// The manifest pins every batch file's SHA-256 and per-table row counts;
// bundle.sha256 pins the manifest bytes themselves. Verification order:
// manifest-bytes digest first (a tampered manifest aborts before any
// digest inside it is trusted), then per-batch digests while streaming.
// "Signed" here means hash-pinned in the SHA256SUMS pattern: the bundle
// is tamper-EVIDENT without a key infrastructure this deployment has;
// when the config secret store grows a signing-key home, the sidecar is
// the upgrade point (it is the only trust root).
//
// # Failure contracts
//
// Digest mismatch: the batch is isolated (nothing of it is applied — the
// digest is verified BEFORE the batch transaction begins) and the whole
// import aborts, naming table, file, and both digests. Retry is safe:
// rows are idempotent per stable primary key + canonical payload
// comparison — a re-run never duplicates.
//
// Unknown enum/status: loud abort naming table+column and the offending
// row's position — never a silent default.
//
// No document text or secret ever enters a log or error: failures name
// tables, columns, counts, digests and primary-key identifiers only.
package databundle
