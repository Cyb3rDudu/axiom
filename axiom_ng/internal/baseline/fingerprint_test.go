// fingerprint_test.go — schema fingerprint of the freeze state (#295 Ziel 4,
// DM01/#310 dependency: this artifact must be deterministic and
// reproducible — on the dev host AND in CI.
//
// Canonical source: a throwaway scratch database freshly built by the
// CURRENT migration files (0001..0023 at freeze). The structure therefore
// is exactly "what the migrations produce" — reproducible everywhere, and
// the standing gate: an edited or new migration goes red until the frozen
// fixture is deliberately updated in the same PR.
//
// axiom_db / axiom_dev reality differs from the migration-derived
// structure by DOCUMENTED legacy leftovers (pre-0011 manual index, see
// devStructureAllowlist) — pinned by TestSchemaFingerprintDevLive so any
// NEW structural drift on dev is caught, while the known legacy delta
// stays green. The fingerprint itself never depends on live DB state.
//
// Determinism contract: two runs must produce byte-identical files; the
// header records only the PG major version (point releases must not
// matter).
package baseline

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
)

const scratchDBName = "axiom_baseline_scratch"

func fingerprintDSN() string {
	if d := os.Getenv("AXIOM_BASELINE_DSN"); d != "" {
		return d
	}
	return os.Getenv("AXIOM_DATABASE_URL")
}

// withScratchDB rewrites the DSN to <db>_baseline_scratch, recreates that
// database empty (drops a leftover first), and runs fn against it.
// ponytail: the FIXED scratch name fails loudly on concurrent suite runs
// (DROP+CREATE under a running migrate errors out instead of silently
// cross-talking) — accepted ceiling: single-operator dev host and CI.
func withScratchDB(ctx context.Context, t *testing.T, dsn string, fn func(dsn string)) {
	t.Helper()
	// guard FIRST, before any connection: cluster-level DROP/CREATE only
	// ever happens on dev/CI clusters (see scratchableDSNs)
	requireScratchableDSN(t, dsn)
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Path == "" {
		t.Fatalf("cannot rewrite non-URL DSN (need postgres://…/dbname form): %v", err)
	}

	admin, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	// DDL identifiers cannot be bound as parameters; every statement below
	// interpolates ONLY the package-const scratchDBName — no request data.
	terminate := fmt.Sprintf(
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid<>pg_backend_pid()", scratchDBName)
	if _, err := admin.Pool().Exec(ctx, terminate); err != nil {
		admin.Close()
		t.Fatalf("kill scratch connections: %v", err)
	}
	if _, err := admin.Pool().Exec(ctx, "DROP DATABASE IF EXISTS "+scratchDBName); err != nil {
		admin.Close()
		t.Fatalf("drop old scratch: %v", err)
	}
	if _, err := admin.Pool().Exec(ctx, "CREATE DATABASE "+scratchDBName); err != nil {
		admin.Close()
		t.Fatalf("create scratch (needs CREATEDB privilege): %v", err)
	}
	admin.Close()

	u.Path = "/" + scratchDBName
	defer func() {
		// cleanup on its own context: a timeout-expired request ctx would
		// leave the scratch DB behind (poisoning the next run) — dropping
		// must work even then. (Same non-bindable DDL pattern as above:
		// interpolates ONLY the package-const scratchDBName — no request data.)
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanup, err := db.Open(cctx, dsn)
		if err == nil {
			_, _ = cleanup.Pool().Exec(cctx, fmt.Sprintf(
				"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid<>pg_backend_pid()", scratchDBName))
			_, _ = cleanup.Pool().Exec(cctx, "DROP DATABASE IF EXISTS "+scratchDBName)
			cleanup.Close()
		}
	}()
	fn(u.String())
}

// schemaFingerprint renders the canonical structure block. Every query
// ORDERs by stable names; definitions are single-lined. No OIDs, no
// owners, no timestamps, no row data.
func schemaFingerprint(ctx context.Context, d *db.DB) (string, string, error) {
	q := func(sql string) ([]string, error) {
		rows, err := d.Pool().Query(ctx, sql)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				return nil, err
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				if v == nil {
					parts[i] = "∅"
					continue
				}
				parts[i] = strings.Map(func(r rune) rune {
					if r == '\n' || r == '\t' {
						return ' '
					}
					return r
				}, fmt.Sprintf("%v", v))
			}
			out = append(out, strings.Join(parts, " | "))
		}
		sort.Strings(out) // belt over the SQL-level ORDER BY: catalog order is not a contract
		return out, rows.Err()
	}

	var major string
	if err := d.Pool().QueryRow(ctx,
		`SELECT split_part(current_setting('server_version'), '.', 1)`).Scan(&major); err != nil {
		return "", "", err
	}

	var b strings.Builder
	section := func(title, sql string) error {
		lines, err := q(sql)
		if err != nil {
			return fmt.Errorf("%s: %w", title, err)
		}
		fmt.Fprintf(&b, "## %s (%d)\n", title, len(lines))
		for _, l := range lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
		return nil
	}
	for _, s := range []struct{ title, sql string }{
		{"extensions", `SELECT extname, extversion FROM pg_extension ORDER BY extname`},
		{"migrations ledger", `SELECT version FROM schema_migrations ORDER BY version`},
		{"enum types", `
			SELECT t.typname, string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder)
			FROM pg_type t
			JOIN pg_enum e ON e.enumtypid = t.oid
			JOIN pg_namespace n ON n.oid = t.typnamespace
			WHERE n.nspname = 'public'
			GROUP BY t.typname ORDER BY t.typname`},
		{"relations", `
			SELECT c.relname, c.relkind
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'S', 'v', 'm')
			ORDER BY c.relname`},
		{"columns", `
			SELECT table_name, column_name, udt_name, is_nullable, coalesce(column_default, '')
			FROM information_schema.columns
			WHERE table_schema = 'public'
			ORDER BY table_name, ordinal_position`},
		{"constraints", `
			SELECT conrelid::regclass::text, conname, contype::text, pg_get_constraintdef(c.oid)
			FROM pg_constraint c
			WHERE connamespace = 'public'::regnamespace
			ORDER BY conrelid::regclass::text, conname`},
		{"indexes", `
			SELECT tablename, indexname, indexdef
			FROM pg_indexes WHERE schemaname = 'public'
			ORDER BY tablename, indexname`},
		{"triggers", `
			SELECT c.relname, t.tgname, pg_get_triggerdef(t.oid)
			FROM pg_trigger t
			JOIN pg_class c ON c.oid = t.tgrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND NOT t.tgisinternal
			ORDER BY c.relname, t.tgname`},
	} {
		if err := section(s.title, s.sql); err != nil {
			return "", "", err
		}
	}
	return b.String(), major, nil
}

// fingerprintArtifact renders the full committed artifact (header + block).
func fingerprintArtifact(block, major string) string {
	return fmt.Sprintf(
		"# axiom schema fingerprint — frozen compatibility baseline (#295)\n"+
			"# tag %s (commit %s), deployed RAG generation %s\n"+
			"# canonical source: fresh db.Migrate of the freeze migrations (reproducible);\n"+
			"# live axiom_db/axiom_dev delta: documented in devStructureAllowlist (baseline tests)\n"+
			"# postgres major %s; structure only (row counts live in the separate live inventory)\n"+
			"# sha256 structure: %s\n%s",
		FreezeTag, FreezeCommit, FreezeGen, major, sha256Hex([]byte(block)), block)
}

func TestSchemaFingerprintFrozen(t *testing.T) {
	dsn := fingerprintDSN()
	if dsn == "" {
		t.Skip("no AXIOM_BASELINE_DSN / AXIOM_DATABASE_URL — schema fingerprint needs a database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	var artifact string
	withScratchDB(ctx, t, dsn, func(scratchDSN string) {
		d, err := db.Open(ctx, scratchDSN)
		if err != nil {
			t.Fatalf("open scratch: %v", err)
		}
		defer d.Close()
		if err := d.Migrate(ctx); err != nil {
			t.Fatalf("migrate scratch: %v", err)
		}
		block, major, err := schemaFingerprint(ctx, d)
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		artifact = fingerprintArtifact(block, major)
	})

	if writeActualOrFixture(t, "schema_fingerprint.txt", []byte(artifact), fixtureUpdate()) {
		return
	}
	if want := readFixture(t, "schema_fingerprint.txt"); string(want) != artifact {
		t.Fatalf("schema fingerprint drift:\n--- frozen ---\n%s\n+++ actual +++\n%s", want, artifact)
	}
}

// devStructureAllowlist pins the DOCUMENTED structural delta between live
// databases (axiom_db at freeze, mirrored by axiom_dev) and the
// migration-derived canonical structure. Anything NOT listed here makes
// TestSchemaFingerprintDevLive red.
//
// Delta 1: `snapshots_one_active_per_attachment` — the pre-0011 manual
// partial unique index from the #125 production proof (2026-08). Migration
// 0011 shipped the properly-named twin
// (processing_snapshots_one_active_per_attachment_uq) but never dropped
// the manual original; both live DBs still carry it. Documented as
// baseline debt in #295; harmless duplicate of the 0011 invariant.
//
// Delta 2: the `library_` namespace — the Library component's OWN
// migration set (F06 #300): own ledger (library_schema_migrations),
// additive tables on the same physical dev DB. The frozen fingerprint
// stays derived from the CORE migration set alone (TestSchemaFingerprint
// green without a fixture update); axiom_dev legitimately carries the
// library tables because the 0.2.x dev env applies them. One prefix
// covers the whole component-owned namespace — anything outside it
// still gates.
var devStructureAllowlist = []string{
	"processing_snapshots | snapshots_one_active_per_attachment |",
	"library_",
}

// TestSchemaFingerprintDevLive — axiom_dev must be exactly the canonical
// freeze structure plus the documented allowlist delta. Live-gated: needs
// the dev host's database (any structural drift on dev gets caught here,
// not silently adopted into the baseline).
func TestSchemaFingerprintDevLive(t *testing.T) {
	liveEnabled(t)
	dsn := fingerprintDSN()
	if dsn == "" {
		t.Fatal("live mode without AXIOM_DATABASE_URL (source scripts/dev/env.sh)")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// canonical block comes from the frozen fixture (header stripped)
	frozen := string(readFixture(t, "schema_fingerprint.txt"))
	canonical := frozen[strings.Index(frozen, "## extensions"):]

	admin, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open dev: %v", err)
	}
	defer admin.Close()
	live, _, err := schemaFingerprint(ctx, admin)
	if err != nil {
		t.Fatalf("fingerprint dev: %v", err)
	}

	// live row-count inventory of axiom_dev (NON-GATING — dev may live; the
	// freeze-day copy is fixtures/live_row_counts_freeze_day.txt, deltas get
	// documented in #295)
	if actualDir != "" {
		counts, err := liveRowCounts(ctx, admin)
		if err != nil {
			t.Logf("live row counts: %v", err)
		} else {
			var out strings.Builder
			out.WriteString("# live row counts of axiom_dev (NON-GATING live inventory; freeze-day copy: fixtures/live_row_counts_freeze_day.txt)\n")
			for _, c := range counts {
				out.WriteString(c)
				out.WriteByte('\n')
			}
			if err := os.MkdirAll(actualDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(actualDir+"/live_row_counts.txt", []byte(out.String()), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	canon := func(s string) []string {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		out := make([]string, 0, len(lines))
		for _, l := range lines {
			// section count headers are derived (an allowlisted extra index
			// shifts them) — normalize the count away for the live diff
			if strings.HasPrefix(l, "## ") {
				if i := strings.Index(l, " ("); i > 0 {
					l = l[:i]
				}
			}
			out = append(out, l)
		}
		return out
	}
	canonicalLines := canon(canonical)
	liveLines := canon(live)
	cs, ls := map[string]bool{}, map[string]bool{}
	for _, l := range canonicalLines {
		cs[l] = true
	}
	for _, l := range liveLines {
		ls[l] = true
	}
	var extra, missing []string
	for _, l := range liveLines {
		if !cs[l] {
			extra = append(extra, l)
		}
	}
	for _, l := range canonicalLines {
		if !ls[l] {
			missing = append(missing, l)
		}
	}
	allow := func(line string) bool {
		for _, p := range devStructureAllowlist {
			if strings.HasPrefix(line, p) {
				return true
			}
		}
		return false
	}
	var unexpected []string
	for _, e := range extra {
		if !allow(e) {
			unexpected = append(unexpected, "+ "+e)
		}
	}
	for _, m := range missing {
		unexpected = append(unexpected, "- "+m)
	}
	if len(unexpected) > 0 {
		t.Fatalf("axiom_dev structure drift beyond the documented allowlist:\n%s\n(extend devStructureAllowlist ONLY with a documented #295 debt entry)",
			strings.Join(unexpected, "\n"))
	}
}

// liveRowCounts lists public table row counts, sorted by table name.
func liveRowCounts(ctx context.Context, d *db.DB) ([]string, error) {
	rows, err := d.Pool().Query(ctx, `
		SELECT relname, n_live_tup
		FROM pg_stat_user_tables
		ORDER BY relname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%-42s %d", name, n))
	}
	return out, rows.Err()
}
