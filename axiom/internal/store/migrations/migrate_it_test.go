// migrate_it_test.go — the Store ledger's idempotency witness (review
// R2-5): a second Migrate run against an already-migrated database (the
// ledger row wiped — the bootstrap-repair scenario) must succeed, proving
// the constraint guard agrees with the ADD for both the corrected and the
// original (typo'd) constraint names. Gated: AXIOM_TEST_DATABASE_URL.
package migrations

import (
	"context"
	"maps"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStoreMigrateIdempotentAfterLedgerWipe(t *testing.T) {
	// These ledger tests own the database's ingest_jobs shape; leftovers
	// from other packages' ITs can legally carry duplicate ACTIVE
	// revision identities that break the index re-creation below.
	if pool := migOwnedPool(t); pool != nil {
		_, _ = pool.Exec(context.Background(), `TRUNCATE ingest_jobs CASCADE`)
		pool.Close()
	}
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping migrations IT")
	}
	if !strings.HasSuffix(strings.Split(dsn, "?")[0], "_test") {
		t.Fatalf("refusing to run against non-_test database %q", dsn)
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("core migrate: %v", err)
	}

	// First apply (may already be applied by sibling tests — idempotent).
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	// A DB ledgered at the FIRST strand build carries the OLD identity
	// index shape (rendition+hash, status-blind): the migration must
	// replace it with the active-scoped three-column shape.
	if _, err := d.Pool().Exec(ctx, `DROP INDEX IF EXISTS ingest_jobs_revision_identity_uq`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `CREATE UNIQUE INDEX ingest_jobs_revision_identity_uq
		ON ingest_jobs (revision_rendition_id, content_hash)
		WHERE intake_kind='revision' AND force_rebuild=false`); err != nil {
		t.Fatal(err)
	}

	// The original (typo'd) constraint name must satisfy the guard too:
	// pre-landing dev DBs carry intake_jobs_intake_kind_chk (and not the
	// corrected name) — simulate exactly that shape.
	for _, name := range []string{"ingest_jobs_intake_kind_chk", "intake_jobs_intake_kind_chk"} {
		if _, err := d.Pool().Exec(ctx, `ALTER TABLE ingest_jobs DROP CONSTRAINT IF EXISTS `+name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Pool().Exec(ctx, `ALTER TABLE ingest_jobs ADD CONSTRAINT intake_jobs_intake_kind_chk CHECK (intake_kind IN ('zotero','revision'))`); err != nil {
		t.Fatal(err)
	}
	// Simulate the bootstrap repair: the ledger forgets, the schema stays.
	if _, err := d.Pool().Exec(ctx, `DELETE FROM store_schema_migrations`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Migrate(context.Background(), d.Pool()) })
	// The re-run must not error (the pre-fix guard/ADD mismatch failed
	// here with "duplicate constraint").
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("re-run after ledger wipe must be idempotent: %v", err)
	}
	var n int
	if err := d.Pool().QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid='ingest_jobs'::regclass AND conname IN ('ingest_jobs_intake_kind_chk','intake_jobs_intake_kind_chk')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("exactly one intake-kind CHECK expected (typo or corrected), got %d", n)
	}
	// The identity index must now carry the ACTIVE-status, source-scoped
	// predicate (the ON CONFLICT arbiter needs exactly this shape).
	var idxdef string
	if err := d.Pool().QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname='ingest_jobs_revision_identity_uq'`).Scan(&idxdef); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(idxdef, "revision_source_id, revision_rendition_id, content_hash") ||
		!strings.Contains(idxdef, "status = ANY") {
		t.Fatalf("identity index not migrated to the active-scoped source shape: %s", idxdef)
	}
}

// TestStoreMigrate0002FixesLedgeredOldIndex — the review-R5 witness: a
// DB ledgered at the FIRST strand build (0001 recorded, 0002 not — it
// carries the old status-blind identity index) must be repaired by a
// plain Migrate run, no ledger wipe: 0002 always runs on such DBs.
func TestStoreMigrate0002FixesLedgeredOldIndex(t *testing.T) {
	if pool := migOwnedPool(t); pool != nil {
		_, _ = pool.Exec(context.Background(), `TRUNCATE ingest_jobs CASCADE`)
		pool.Close()
	}
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping migrations IT")
	}
	if !strings.HasSuffix(strings.Split(dsn, "?")[0], "_test") {
		t.Fatalf("refusing to run against non-_test database %q", dsn)
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("core migrate: %v", err)
	}
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	// Simulate the ledgered-at-first-build state WITH the ledger intact:
	// the old index shape, and 0002's row absent (0001's stays).
	if _, err := d.Pool().Exec(ctx, `DROP INDEX IF EXISTS ingest_jobs_revision_identity_uq`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `CREATE UNIQUE INDEX ingest_jobs_revision_identity_uq
		ON ingest_jobs (revision_rendition_id, content_hash)
		WHERE intake_kind='revision' AND force_rebuild=false`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `DELETE FROM store_schema_migrations WHERE version='0002_revision_identity_active_scope.sql'`); err != nil {
		t.Fatal(err)
	}

	// The plain run must repair: 0002 executes on the ledgered DB.
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("migrate on ledgered DB: %v", err)
	}
	var idxdef string
	if err := d.Pool().QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname='ingest_jobs_revision_identity_uq'`).Scan(&idxdef); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(idxdef, "revision_source_id, revision_rendition_id, content_hash") ||
		!strings.Contains(idxdef, "status = ANY") {
		t.Fatalf("0002 must replace the old identity index on a ledgered DB, got: %s", idxdef)
	}
}

// TestStoreMigrate0003DropsExactlyTheFiveCrossFKs — the DM06 #315
// witnesses on an ISOLATED scratch database (sibling ITs share the
// _test DSN and call Migrate at startup; the ledger-wipe and orphan
// injections below must never cross-talk with them):
//  1. exactly-five: the store migration removes exactly the five
//     allowlisted cross-component FKs — full FK inventory diff, nothing
//     else falls, nothing appears;
//  2. postcondition: zero FKs remain from the two Store tables onto the
//     three Library tables;
//  3. orphan-guard teeth for ALL FIVE references, under a pending drop:
//     the five constraints are re-added (clean data), then one dangling
//     row per reference must abort the re-run naming exactly that
//     reference and leave no ledger row (transaction rollback);
//  4. gated no-op: with all five constraints ABSENT, a legal post-drop
//     orphan must NOT block the bootstrap-repair re-run (the audit
//     guards the drop decision only);
//  5. postcondition teeth: a sixth cross-component FK aborts the run;
//  6. the final FK inventory equals the post-drop baseline exactly.
func TestStoreMigrate0003DropsExactlyTheFiveCrossFKs(t *testing.T) {
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping migrations IT")
	}
	if !strings.HasSuffix(strings.Split(dsn, "?")[0], "_test") {
		t.Fatalf("refusing to run against non-_test database %q", dsn)
	}
	const v0003 = "0003_drop_cross_component_fks.sql"
	ctx := context.Background()

	// Fixed-name scratch (the baseline fingerprint pattern): self-healing
	// across crashed runs — a leftover is dropped, not accumulated.
	// ponytail: fixed name — concurrent same-package runs collide loudly
	// on DROP+CREATE; accepted ceiling for a single-operator dev host and
	// CI (-p 1); switch to pid-suffixed names if suites ever race.
	scratch := "axiom_dm06_0003_test"
	admin, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	if _, err := admin.Pool().Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, scratch); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Pool().Exec(ctx, "DROP DATABASE IF EXISTS "+scratch)
	if _, err := admin.Pool().Exec(ctx, "CREATE DATABASE "+scratch); err != nil {
		admin.Close()
		t.Fatalf("create scratch: %v", err)
	}
	admin.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + scratch
	scratchDSN := u.String()
	t.Cleanup(func() {
		c, err := db.Open(context.Background(), dsn)
		if err == nil {
			c.Pool().Exec(context.Background(),
				`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, scratch)
			c.Pool().Exec(context.Background(), "DROP DATABASE IF EXISTS "+scratch)
			c.Close()
		}
	})
	d, err := db.Open(ctx, scratchDSN)
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("core migrate: %v", err)
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool().Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	wipeLedger := func() {
		t.Helper()
		exec(`DELETE FROM store_schema_migrations WHERE version=$1`, v0003)
	}
	ledgered := func() bool {
		t.Helper()
		var b bool
		if err := d.Pool().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM store_schema_migrations WHERE version=$1)`, v0003).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	fkInventory := func() map[string]struct{} {
		rows, err := d.Pool().Query(ctx, `
			SELECT conrelid::regclass::text, conname
			FROM pg_constraint WHERE contype='f'
			ORDER BY 1, 2`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]struct{}{}
		for rows.Next() {
			var tbl, name string
			if err := rows.Scan(&tbl, &name); err != nil {
				t.Fatal(err)
			}
			out[tbl+"."+name] = struct{}{}
		}
		return out
	}
	dropped := []string{
		"ingest_jobs.fk_ingest_jobs_source",
		"ingest_jobs.fk_ingest_jobs_document",
		"ingest_jobs.fk_ingest_jobs_attachment",
		"processing_snapshots.processing_snapshots_document_id_fkey",
		"processing_snapshots.processing_snapshots_attachment_id_fkey",
	}

	// (1) exactly five — full inventory diff.
	before := fkInventory()
	for _, fk := range dropped {
		if _, ok := before[fk]; !ok {
			t.Fatalf("precondition failed: %s absent before the store migrate (core schema drifted?)", fk)
		}
	}
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("store migrate: %v", err)
	}
	after := fkInventory()
	var fell []string
	for fk := range before {
		if _, ok := after[fk]; !ok {
			fell = append(fell, fk)
		}
	}
	sort.Strings(fell)
	sort.Strings(dropped)
	if len(fell) != len(dropped) {
		t.Fatalf("exactly five FKs must fall, got %d: %v", len(fell), fell)
	}
	for i := range fell {
		if fell[i] != dropped[i] {
			t.Fatalf("the five dropped FKs drifted: got %v", fell)
		}
	}
	for fk := range after {
		if _, ok := before[fk]; !ok {
			t.Fatalf("the store migration must not ADD constraints, found new %s", fk)
		}
	}

	// (2) postcondition: no Store→Library FK remains.
	var n int
	if err := d.Pool().QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint
		WHERE contype='f'
		  AND conrelid IN ('ingest_jobs'::regclass, 'processing_snapshots'::regclass)
		  AND confrelid IN ('zotero_sources'::regclass, 'zotero_documents'::regclass, 'zotero_attachments'::regclass)`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("cross-component FKs remain after the drop: %d", n)
	}

	// (3) orphan-guard teeth for ALL five references. The gate means the
	// abort is only reachable while a drop is PENDING, so each probe
	// reconstructs the real drifted shape: the orphan lands first (FKs
	// absent), then the five constraints come back NOT VALID — the
	// online-migration form that skips existing rows — exactly the state
	// the audit exists for. Both snapshot reference columns are NOT NULL:
	// each snapshot injection keeps the counterpart REAL (one mirror
	// chain seeded below), so exactly ONE reference dangles per probe.
	dropFive := func() {
		t.Helper()
		exec(`ALTER TABLE ingest_jobs DROP CONSTRAINT IF EXISTS fk_ingest_jobs_source`)
		exec(`ALTER TABLE ingest_jobs DROP CONSTRAINT IF EXISTS fk_ingest_jobs_document`)
		exec(`ALTER TABLE ingest_jobs DROP CONSTRAINT IF EXISTS fk_ingest_jobs_attachment`)
		exec(`ALTER TABLE processing_snapshots DROP CONSTRAINT IF EXISTS processing_snapshots_document_id_fkey`)
		exec(`ALTER TABLE processing_snapshots DROP CONSTRAINT IF EXISTS processing_snapshots_attachment_id_fkey`)
	}
	readdFiveNotValid := func() {
		t.Helper()
		exec(`ALTER TABLE ingest_jobs ADD CONSTRAINT fk_ingest_jobs_source
			FOREIGN KEY (source_id) REFERENCES zotero_sources(id) ON DELETE CASCADE NOT VALID`)
		exec(`ALTER TABLE ingest_jobs ADD CONSTRAINT fk_ingest_jobs_document
			FOREIGN KEY (document_id) REFERENCES zotero_documents(id) ON DELETE CASCADE NOT VALID`)
		exec(`ALTER TABLE ingest_jobs ADD CONSTRAINT fk_ingest_jobs_attachment
			FOREIGN KEY (attachment_id) REFERENCES zotero_attachments(id) ON DELETE CASCADE NOT VALID`)
		exec(`ALTER TABLE processing_snapshots ADD CONSTRAINT processing_snapshots_document_id_fkey
			FOREIGN KEY (document_id) REFERENCES zotero_documents(id) ON DELETE CASCADE NOT VALID`)
		exec(`ALTER TABLE processing_snapshots ADD CONSTRAINT processing_snapshots_attachment_id_fkey
			FOREIGN KEY (attachment_id) REFERENCES zotero_attachments(id) ON DELETE CASCADE NOT VALID`)
	}
	readdFive := func() {
		t.Helper()
		exec(`ALTER TABLE ingest_jobs ADD CONSTRAINT fk_ingest_jobs_source
			FOREIGN KEY (source_id) REFERENCES zotero_sources(id) ON DELETE CASCADE`)
		exec(`ALTER TABLE ingest_jobs ADD CONSTRAINT fk_ingest_jobs_document
			FOREIGN KEY (document_id) REFERENCES zotero_documents(id) ON DELETE CASCADE`)
		exec(`ALTER TABLE ingest_jobs ADD CONSTRAINT fk_ingest_jobs_attachment
			FOREIGN KEY (attachment_id) REFERENCES zotero_attachments(id) ON DELETE CASCADE`)
		exec(`ALTER TABLE processing_snapshots ADD CONSTRAINT processing_snapshots_document_id_fkey
			FOREIGN KEY (document_id) REFERENCES zotero_documents(id) ON DELETE CASCADE`)
		exec(`ALTER TABLE processing_snapshots ADD CONSTRAINT processing_snapshots_attachment_id_fkey
			FOREIGN KEY (attachment_id) REFERENCES zotero_attachments(id) ON DELETE CASCADE`)
	}
	var srcID string
	if err := d.Pool().QueryRow(ctx,
		`INSERT INTO zotero_sources (base_url, library_id, server_id)
		 VALUES ('https://dm06-teeth.local','users/0','dm06-teeth') RETURNING id::text`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1,'DM06REALDOC',1,'book','DM06 Teeth Real')`, srcID)
	exec(`INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
		parent_zotero_key, link_mode, content_type, filename, preferred, deleted)
		VALUES ($1,(SELECT id FROM zotero_documents WHERE zotero_key='DM06REALDOC'),'DM06REALATT',1,
		'DM06REALDOC','imported_file','application/pdf','dm06.pdf',true,false)`, srcID)

	probes := []struct {
		label  string // the reference the abort must name
		table  string // where the dangling row lands (cleanup by id)
		inject string // mints exactly ONE dangling row for the reference
	}{
		{"ingest_jobs.source_id", "ingest_jobs",
			`INSERT INTO ingest_jobs (source_id) VALUES (gen_random_uuid()) RETURNING id::text`},
		{"ingest_jobs.document_id", "ingest_jobs",
			`INSERT INTO ingest_jobs (document_id) VALUES (gen_random_uuid()) RETURNING id::text`},
		{"ingest_jobs.attachment_id", "ingest_jobs",
			`INSERT INTO ingest_jobs (attachment_id) VALUES (gen_random_uuid()) RETURNING id::text`},
		{"processing_snapshots.document_id", "processing_snapshots",
			`INSERT INTO processing_snapshots (attachment_id, document_id, content_hash,
			   processor_name, processor_version, profile_hash, profile)
			 VALUES ((SELECT id FROM zotero_attachments WHERE zotero_key='DM06REALATT'),
			         gen_random_uuid(), 'dm06-teeth', 'p', 'v1', 'ph', '{}')
			 RETURNING id::text`},
		{"processing_snapshots.attachment_id", "processing_snapshots",
			`INSERT INTO processing_snapshots (attachment_id, document_id, content_hash,
			   processor_name, processor_version, profile_hash, profile)
			 VALUES (gen_random_uuid(),
			         (SELECT id FROM zotero_documents WHERE zotero_key='DM06REALDOC'),
			         'dm06-teeth', 'p', 'v1', 'ph', '{}')
			 RETURNING id::text`},
	}
	for _, p := range probes {
		dropFive() // ensure the FKs are absent so the orphan can land
		var orphanID string
		if err := d.Pool().QueryRow(ctx, p.inject).Scan(&orphanID); err != nil {
			t.Fatalf("inject orphan for %s: %v", p.label, err)
		}
		readdFiveNotValid() // pending drop over drifted data
		wipeLedger()
		err := Migrate(ctx, d.Pool())
		if err == nil || !strings.Contains(err.Error(), "orphan guard") || !strings.Contains(err.Error(), p.label) {
			t.Fatalf("orphan guard must abort naming %s, got: %v", p.label, err)
		}
		if ledgered() {
			t.Fatalf("the aborted 0003 must not be ledgered after the %s probe (transaction rollback)", p.label)
		}
		dropFive()
		exec(`DELETE FROM `+p.table+` WHERE id=$1`, orphanID)
	}

	// Constraints present again (fully validated — data is clean now): the
	// re-run drops the five (the plain second application) and restores
	// the ledger row.
	readdFive()
	wipeLedger()
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("second drop run: %v", err)
	}
	if !ledgered() {
		t.Fatal("0003 must be ledgered after the second drop run")
	}
	for _, fk := range dropped {
		if _, ok := fkInventory()[fk]; ok {
			t.Fatalf("%s must be gone after the second drop run", fk)
		}
	}

	// (4) gated no-op: constraints ABSENT + a legal post-drop orphan —
	// the audit is skipped (nothing left to drop; orphans belong to the
	// contract layer), the postcondition still holds, the repair lands.
	var legalOrphan string
	if err := d.Pool().QueryRow(ctx,
		`INSERT INTO ingest_jobs (source_id) VALUES (gen_random_uuid()) RETURNING id::text`).Scan(&legalOrphan); err != nil {
		t.Fatal(err)
	}
	wipeLedger()
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("bootstrap repair over a legal post-drop orphan must succeed: %v", err)
	}
	if !ledgered() {
		t.Fatal("0003 must be ledgered after the gated no-op run")
	}
	exec(`DELETE FROM ingest_jobs WHERE id=$1`, legalOrphan)

	// (5) postcondition teeth: a SIXTH cross-component FK (inventory
	// drift beyond the allowlisted five) aborts the run.
	exec(`ALTER TABLE ingest_jobs ADD CONSTRAINT dm06_probe_fk
		FOREIGN KEY (attachment_id) REFERENCES zotero_attachments(id)`)
	wipeLedger()
	err = Migrate(ctx, d.Pool())
	if err == nil || !strings.Contains(err.Error(), "cross-component FKs remain") {
		t.Fatalf("the postcondition must abort on a sixth cross-component FK, got: %v", err)
	}
	if ledgered() {
		t.Fatal("the postcondition-aborted 0003 must not be ledgered")
	}
	exec(`ALTER TABLE ingest_jobs DROP CONSTRAINT dm06_probe_fk`)
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("re-run after the probe FK removal: %v", err)
	}
	if !ledgered() {
		t.Fatal("0003 must be ledgered after the clean re-run")
	}

	// (6) the final inventory equals the post-drop baseline in CONTENT
	// (not just cardinality).
	if got := fkInventory(); !maps.Equal(got, after) {
		t.Fatalf("the probes changed the FK inventory: %d baseline entries, %d final", len(after), len(got))
	}
}

// TestStoreMigrate0004BackfillsPopulatedArchive — the bestand backfill
// against a POPULATED archive (#358 review): preferred live renditions
// project with the bibliography translation (creators flattened to
// strings, tags extracted), non-preferred and deleted rows stay out,
// historical repair cases seed the retention flag — and a re-run is a
// no-op.
func TestStoreMigrate0004BackfillsPopulatedArchive(t *testing.T) {
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping migrations IT")
	}
	if !strings.HasSuffix(strings.Split(dsn, "?")[0], "_test") {
		t.Fatalf("refusing to run against non-_test database %s", dsn)
	}
	scratch := "axiom_0004_backfill_test"
	admin, err := db.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	ctx := context.Background()
	admin.Pool().Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, scratch)
	admin.Pool().Exec(ctx, "DROP DATABASE IF EXISTS "+scratch)
	if _, err := admin.Pool().Exec(ctx, "CREATE DATABASE "+scratch); err != nil {
		admin.Close()
		t.Fatalf("create scratch: %v", err)
	}
	admin.Close()
	t.Cleanup(func() {
		c, err := db.Open(context.Background(), dsn)
		if err == nil {
			c.Pool().Exec(context.Background(),
				`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, scratch)
			c.Pool().Exec(context.Background(), "DROP DATABASE IF EXISTS "+scratch)
			c.Close()
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + scratch
	d, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("core migrate: %v", err)
	}
	pool := d.Pool()

	seed := `
	INSERT INTO zotero_sources (base_url, library_id, server_id)
	VALUES ('https://backfill.local','users/0','srv-1');
	INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title,
		creators, publication_year, publisher, language, tags, citation_class, deleted)
	SELECT (SELECT id FROM zotero_sources), k, 3, 'book', t,
	       c::jsonb, 2001, 'Pub', 'en', g::jsonb, 'citable', false
	FROM (VALUES
	  ('BFDOC1', 'Backfill Book', '[{"creatorType":"author","firstName":"Ada","lastName":"Lovelace"}]', '[{"tag":"VWL_HA"},{"tag":"neutral"}]'),
	  ('BFDOC2', 'Deleted Book', '[]', '[]'),
	  ('BFDOC3', 'No Preferred Live', '[]', '[]')
	) AS v(k, t, c, g);
	UPDATE zotero_documents SET deleted=true WHERE zotero_key='BFDOC2';
	INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
	   parent_zotero_key, link_mode, content_type, filename, local_path, content_hash, file_size, mtime_ms, preferred, deleted)
	SELECT d.source_id, d.id, a.k, 4, d.zotero_key,
	       'imported_file', 'application/pdf', a.k||'.pdf', '/tmp/'||a.k||'.pdf', a.h, 1024, 5, a.p, false
	FROM (VALUES
	  ('BFDOC1', 'BFATT1', 'sha256:one', true),
	  ('BFDOC1', 'BFATT2', 'sha256:two', false),
	  ('BFDOC3', 'BFATT3', 'sha256:three', false)
	) AS a(doc, k, h, p)
	JOIN zotero_documents d ON d.zotero_key = a.doc;
	UPDATE zotero_attachments SET preferred=true, deleted=true
	 WHERE document_id=(SELECT id FROM zotero_documents WHERE zotero_key='BFDOC2');
	INSERT INTO repair_cases (attachment_id, document_id, status, suspicion_class)
	VALUES ((SELECT id FROM zotero_attachments WHERE zotero_key='BFATT1'),
	        (SELECT id FROM zotero_documents WHERE zotero_key='BFDOC1'), 'healed', 'x');
	`
	if _, err := pool.Exec(ctx, seed); err != nil {
		t.Fatalf("seed archive: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM store_documents`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("backfill projected %d rows, want exactly the 1 preferred live rendition (BFDOC1/BFATT1)", n)
	}
	var creators, tags []byte
	var repairLinked bool
	var title, hash string
	if err := pool.QueryRow(ctx,
		`SELECT title, creators, tags, content_hash, repair_linked FROM store_documents WHERE rendition_key='BFATT1'`,
	).Scan(&title, &creators, &tags, &hash, &repairLinked); err != nil {
		t.Fatal(err)
	}
	if title != "Backfill Book" || hash != "sha256:one" {
		t.Fatalf("backfill identity: title=%q hash=%q", title, hash)
	}
	if string(creators) != `["Ada Lovelace"]` {
		t.Fatalf("creators translation: %s, want [\"Ada Lovelace\"]", creators)
	}
	if string(tags) != `["VWL_HA", "neutral"]` {
		t.Fatalf("tags translation: %s", tags)
	}
	if !repairLinked {
		t.Fatal("the repair-cased rendition must backfill repair_linked=true (retention anchor)")
	}
	// re-run is a no-op
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM store_documents`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("re-migrate changed the projection: %d rows", n)
	}
}

// migOwnedPool opens the shared test DB when the migration-IT environment
// is present (nil otherwise — the caller then skips the hygiene step).
func migOwnedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		return nil
	}
	d, err := db.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open shared db: %v", err)
	}
	return d.Pool()
}
