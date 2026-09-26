// zotero_it_test.go — the REAL Zotero integration test (F07 #301),
// dev-only and env-gated: no Zotero, no network, no test DB in CI.
//
//	Gate (all required): AXIOM_ZOTERO_IT=1
//	                     AXIOM_TEST_DATABASE_URL (scratch-DB base, *_test)
//	Optional overrides:  AXIOM_ZOTERO_BASE (default http://localhost:23119/api)
//	                     AXIOM_ZOTERO_IT_WRITE_KEY (else AXIOM_ZOTERO_WRITE_KEY_FILE)
//
// One book PDF and one webpage PDF run the FULL ladder (real PDF rung 1,
// scripted resolvers for determinism — the resolver port is exactly that
// seam; the real-Crossref ambiguity probe stays an optional manual run)
// against the real Zotero local API: typed record, enrichment with
// provenance, dedup replay, parent-first collection path, readback,
// source revision, and a complete cleanup witness of the dedicated test
// collection afterwards. The single-writer lease is probed against the
// real Library persistence (second writer refused at start, release on
// Close).
package zoteroprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	libcontracts "github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
	"github.com/jackc/pgx/v5/pgxpool"
)

func itEnabled(t *testing.T) bool {
	t.Helper()
	if os.Getenv("AXIOM_ZOTERO_IT") != "1" {
		t.Skip("AXIOM_ZOTERO_IT != 1 — the real Zotero IT is dev-only (needs Zotero + local API + a write key)")
	}
	if os.Getenv("AXIOM_TEST_DATABASE_URL") == "" {
		t.Fatal("AXIOM_ZOTERO_IT=1 requires AXIOM_TEST_DATABASE_URL (scratch-DB base ending in _test)")
	}
	return true
}

func itBaseURL() string {
	if v := os.Getenv("AXIOM_ZOTERO_BASE"); v != "" {
		return v
	}
	return "http://localhost:23119/api"
}

func itWriteKey(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("AXIOM_ZOTERO_IT_WRITE_KEY"); v != "" {
		return v
	}
	if f := os.Getenv("AXIOM_ZOTERO_WRITE_KEY_FILE"); f != "" {
		b, kerr := os.ReadFile(f)
		if kerr != nil {
			t.Fatalf("write key file %s unreadable: %v", f, kerr)
		}
		return strings.TrimSpace(string(b))
	}
	f := os.Getenv("HOME") + "/.axiom-ng/write-api-key"
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("no write key: set AXIOM_ZOTERO_IT_WRITE_KEY or AXIOM_ZOTERO_WRITE_KEY_FILE (read %s: %v)", f, err)
	}
	return strings.TrimSpace(string(b))
}

// itScratchDB builds a session-unique scratch database (library
// migrations only) — the same convention the library IT suite uses.
func itScratchDB(t *testing.T) (*library.Store, func()) {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	base := dbOf(dsn)
	if !strings.HasSuffix(base, "_test") {
		t.Fatalf("refusing to run against non-_test database %q", base)
	}
	dbName := strings.TrimSuffix(base, "_test") + fmt.Sprintf("_zf07%d_test", os.Getpid())
	// DDL and database identifiers cannot take bind parameters — the
	// scratch name is ALLOWLISTED ([A-Za-z0-9_] only) before any
	// interpolation below (same convention as the library IT suite).
	for _, r := range dbName {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			t.Fatalf("scratch db name %q is not a safe identifier", dbName)
		}
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid<>pg_backend_pid()`, dbName)); err != nil {
		t.Fatalf("kill connections: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName)); err != nil {
		t.Fatalf("drop old scratch: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatalf("create scratch: %v", err)
	}
	admin.Close()
	pool, err := pgxpool.New(ctx, withDBOf(dsn, dbName))
	if err != nil {
		t.Fatalf("open scratch pool: %v", err)
	}
	st := library.NewStore(pool)
	if err := st.Migrate(ctx); err != nil {
		pool.Close()
		t.Fatalf("library migrate: %v", err)
	}
	return st, func() {
		ctx2 := context.Background()
		pool.Close()
		a, aerr := pgxpool.New(ctx2, dsn)
		if aerr == nil {
			_, _ = a.Exec(ctx2, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName))
			a.Close()
		}
	}
}

// precleanZotero removes every leftover of a previous IT run directly
// over the write client (no provider yet — no lease held): items whose
// tags mark them as IT artifacts (the axiom-imp anchor prefix, or one of
// the two deterministic test-content shas), and the test collections.
// Double-delete purges from the trash when the instance allows it;
// failures are tolerated (the witness at the end proves cleanliness).
func precleanZotero(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	base := strings.TrimSuffix(strings.TrimSuffix(itBaseURL(), "/api"), "/")
	read := NewLocalAPI(itBaseURL(), "users/0")
	write := NewWriteClient(base, read.ServerID(), itWriteKey(t))
	testShas := map[string]bool{
		sha256hex(itBookPDF()): true,
		sha256hex(itWebPDF()):  true,
	}
	env, err := read.getItems(ctx, nil)
	if err != nil {
		t.Fatalf("pre-clean catalog read: %v", err)
	}
	_ = env // used by deleteITCollections below
	for _, raw := range env {
		it, ok := itemFromEnvelope(raw)
		if !ok {
			continue
		}
		sha, hasSha := hasTag(it.Tags, "axiom-sha256:")
		isITAttachment := hasSha && testShas[sha]
		isITRecord := false
		for _, tag := range it.Tags {
			if strings.HasPrefix(tag.Tag, "axiom-imp:imp-zotero-it-") {
				isITRecord = true
			}
		}
		if !isITAttachment && !isITRecord {
			continue
		}
		_ = write.DeleteAttachmentItem(it.Key) // into the trash (version-guarded)
		_ = write.DeleteAttachmentItem(it.Key) // purge if the instance allows
	}
	cols, err := read.ListCanonicalCollections()
	if err != nil {
		t.Fatalf("pre-clean collections read: %v", err)
	}
	if err := deleteITCollections(ctx, read, write, env, cols); err != nil {
		t.Fatalf("pre-clean collections: %v", err)
	}
}

// deleteITCollections removes the IT-named collections ONLY when EMPTY:
// a user collection that happens to share the name carries items and
// must survive (the API DELETE is permanent). Emptiness = no live item
// references the collection key.
func deleteITCollections(ctx context.Context, read *LocalAPI, write *WriteClient, env []json.RawMessage, cols []CanonicalCollection) error {
	used := map[string]bool{}
	for _, raw := range env {
		it, ok := itemFromEnvelope(raw)
		if !ok || it.Deleted {
			continue
		}
		for _, c := range it.Collections {
			used[c] = true
		}
	}
	for _, c := range cols {
		if c.Name != "Axiom IT" && c.Name != "F07 Zotero IT" {
			continue
		}
		if used[c.Key] {
			continue // non-empty (foreign?) collection: never delete
		}
		_ = write.DeleteCollection(c.Key)
		_ = write.DeleteCollection(c.Key) // purge if the instance allows
	}
	return nil
}

// itBookPDF/itWebPDF — the deterministic IT fixtures (stable bytes →
// stable content hashes across runs; the pre-clean keys on them).
func itBookPDF() []byte {
	return buildPDF(
		"Strukturwandel der Öffentlichkeit",
		"Untersuchungen zu einer Kategorie der bürgerlichen Gesellschaft",
		"ISBN 978-3-518-28815-4",
		"© 2021",
	)
}

func itWebPDF() []byte {
	return buildPDF("Quarterly Engineering Report", "Q3 highlights")
}

func dbOf(dsn string) string {
	u := dsn
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	if i := strings.LastIndexByte(u, '/'); i >= 0 {
		return u[i+1:]
	}
	return u
}

func withDBOf(dsn, db string) string {
	if i := strings.LastIndexByte(dsn, '/'); i >= 0 {
		var q string
		if j := strings.IndexByte(dsn, '?'); j >= 0 {
			q = dsn[j:]
		}
		return dsn[:i+1] + db + q
	}
	return dsn
}

// TestRealZoteroFullLadderIT — the acceptance witness (see file comment).
func TestRealZoteroFullLadderIT(t *testing.T) {
	if !itEnabled(t) {
		return
	}
	ctx := context.Background()

	// Zotero must be LIVE (the operator opted in explicitly).
	probe := NewLocalAPI(itBaseURL(), "users/0")
	if probe.ServerID() == "" {
		t.Fatalf("Zotero local API not reachable at %s — start Zotero with the local API enabled", itBaseURL())
	}

	st, dropDB := itScratchDB(t)
	defer dropDB()

	// PRE-CLEAN of a previous (possibly failed) run — the probe stays
	// idempotent across runs: every IT item carries an axiom-imp anchor tag
	// from the known key set (plus the deterministic content shas); the
	// collections live under a known path.
	precleanZotero(t)

	// Single-writer against the REAL persistence: this instance takes the
	// lease; a second construction against the same scope is refused.
	prov, err := New(ctx, Options{
		BaseURL: itBaseURL(), LibraryID: "users/0",
		APIKey: itWriteKey(t), Store: st, Owner: "zotero-it-writer",
	})
	if err != nil {
		t.Fatalf("provider New (lease acquire): %v", err)
	}
	defer func() {
		if err := prov.Close(); err != nil {
			t.Fatalf("lease release: %v", err)
		}
	}()
	if _, err := New(ctx, Options{
		BaseURL: itBaseURL(), LibraryID: "users/0",
		APIKey: itWriteKey(t), Store: st, Owner: "zotero-it-second",
	}); err == nil {
		t.Fatal("a second write-capable provider against the same scope must be refused at start")
	} else if !strings.Contains(err.Error(), "single-writer") && !strings.Contains(err.Error(), "lease") {
		t.Fatalf("second writer refusal must diagnose the lease, got: %v", err)
	}

	// The service over the REAL ports; resolvers are scripted for
	// determinism (no network in the IT — the port is the seam).
	cross := library.NewFakeResolver("crossref", "scripted-v1", []library.ResolverFixture{
		{MatchTitle: "Strukturwandel", Candidates: []library.Candidate{{
			CandidateID: "it-book-1", Confidence: 0.95,
			Fields: library.ResolvedFields{
				Title: "Strukturwandel der Öffentlichkeit", Authors: []string{"Habermas"},
				Year: &[]int{2021}[0], Publisher: "Suhrkamp", Language: "de", RecordType: "book",
			},
		}}},
		{MatchTitle: "Quarterly Engineering Report", Candidates: []library.Candidate{{
			CandidateID: "it-web-1", Confidence: 0.9,
			Fields: library.ResolvedFields{
				Title: "Quarterly Engineering Report Q3", RecordType: "webpage",
			},
		}}},
	})
	svc := library.NewService(library.Config{
		SourceID: "src-zotero-it", Provider: "zotero", LibraryID: "users/0",
	}, st, library.NewStaging(t.TempDir()), library.Ports{
		Catalog: prov, Records: prov, Renditions: prov, Collections: prov,
		Resolvers: []library.BibliographicResolver{cross},
		Documents: NewPDFInspector(),
	})

	// Everything the IT creates lives under this path — the cleanup witness.
	itPath := []string{"Axiom IT", "F07 Zotero IT"}
	collID, err := prov.ResolvePath(ctx, itPath, true)
	if err != nil {
		t.Fatalf("test collection path: %v", err)
	}

	var createdItems []string
	var bookRecordID string
	track := func(op libcontracts.ImportOperation) {
		if op.Result != nil {
			createdItems = append(createdItems, op.Result.RecordID, op.Result.RenditionID)
		}
	}

	t.Run("book import through the full ladder", func(t *testing.T) {
		pdf := itBookPDF()
		op, err := svc.StartImport(ctx, libcontracts.ImportRequest{
			IdempotencyKey: "zotero-it-book-1",
			RecordType:     "book",
			Target:         libcontracts.ImportTarget{CollectionPath: itPath, CreateMissing: false},
			MetadataHints:  libcontracts.MetadataHints{Title: "Strukturwandel der Öffentlichkeit"},
		}, bytes.NewReader(pdf))
		if err != nil {
			t.Fatal(err)
		}
		if op.Status != libcontracts.ImportCommitted {
			b, _ := json.MarshalIndent(op, "", "  ")
			t.Fatalf("book import status = %s\n%s", op.Status, b)
		}
		track(op)
		bookRecordID = op.Result.RecordID

		// Typisiert: a BOOK item — and the document rung's identifiers
		// survived as provenance (real PDF extraction).
		var sawDocDOI bool
		for _, f := range op.Fields {
			if f.Field == "isbn" && f.Source == "document" && f.Applied {
				sawDocDOI = true
			}
		}
		if !sawDocDOI {
			t.Fatalf("document-rung ISBN provenance missing: %+v", op.Fields)
		}

		// Deterministic schema filename from verified metadata:
		// first author lastName - year - cleaned title.
		want := "Habermas - 2021 - Strukturwandel der Öffentlichkeit.pdf"
		rec, err := catalogFind(prov, ctx, op.Result.RecordID)
		if err != nil || rec == nil {
			t.Fatalf("record not observable in catalog: %v", err)
		}
		if rec.RecordType != "book" {
			t.Fatalf("record type = %s, want book", rec.RecordType)
		}
		if len(rec.Renditions) != 1 {
			t.Fatalf("renditions = %d, want 1", len(rec.Renditions))
		}
		if rec.Renditions[0].Filename != want {
			t.Fatalf("filename = %q, want %q", rec.Renditions[0].Filename, want)
		}
		if !membershipOf(rec, collID) {
			t.Fatal("collection membership not observable")
		}
		// Source revision published only after verified commit; the
		// staging ticket redeems the bytes.
		if op.Result.Revision.ContentTicket == "" || op.Result.Revision.ContentHash == "" {
			t.Fatalf("no published revision: %+v", op.Result.Revision)
		}
		rc, err := svc.OpenRendition(ctx, libcontracts.ContentTicket(op.Result.Revision.ContentTicket))
		if err != nil {
			t.Fatalf("ticket redemption: %v", err)
		}
		rc.Close()
	})

	t.Run("dedup replay adds nothing", func(t *testing.T) {
		pdf := itBookPDF()
		op, err := svc.StartImport(ctx, libcontracts.ImportRequest{
			IdempotencyKey: "zotero-it-book-2", // different key, same content
			RecordType:     "book",
			Target:         libcontracts.ImportTarget{CollectionPath: itPath, CreateMissing: false},
			MetadataHints:  libcontracts.MetadataHints{Title: "Strukturwandel der Öffentlichkeit"},
		}, bytes.NewReader(pdf))
		if err != nil {
			t.Fatal(err)
		}
		if op.Status != libcontracts.ImportCommitted {
			t.Fatalf("replay status = %s: %+v", op.Status, op.Failure)
		}
		if op.Result.RecordID != bookRecordID {
			t.Fatalf("replay must link the SAME record: %s != %s", op.Result.RecordID, bookRecordID)
		}
		rec, err := catalogFind(prov, ctx, op.Result.RecordID)
		if err != nil || rec == nil {
			t.Fatal(err)
		}
		if len(rec.Renditions) != 1 {
			t.Fatalf("replay must not add a rendition, got %d", len(rec.Renditions))
		}
	})

	t.Run("webpage import keeps its original URL", func(t *testing.T) {
		pdf := itWebPDF()
		accessed := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
		op, err := svc.StartImport(ctx, libcontracts.ImportRequest{
			IdempotencyKey: "zotero-it-web-1",
			RecordType:     "webpage",
			Target:         libcontracts.ImportTarget{CollectionPath: itPath, CreateMissing: false},
			Source: &libcontracts.ImportSourceInfo{
				OriginalURL: "https://example.org/reports/q3",
				AccessedAt:  &accessed,
			},
			MetadataHints: libcontracts.MetadataHints{Title: "Quarterly Engineering Report"},
		}, bytes.NewReader(pdf))
		if err != nil {
			t.Fatal(err)
		}
		if op.Status != libcontracts.ImportCommitted {
			t.Fatalf("webpage import status = %s: %+v", op.Status, op.Failure)
		}
		track(op)

		rec, err := catalogFind(prov, ctx, op.Result.RecordID)
		if err != nil || rec == nil {
			t.Fatal(err)
		}
		if rec.RecordType != "webpage" {
			t.Fatalf("webpage record typed as %s — never book/article", rec.RecordType)
		}
		// The original URL persisted on the Zotero item (snapshot
		// provenance) — read back through the write client.
		data, _, err := prov.write.GetItem(op.Result.RecordID)
		if err != nil {
			t.Fatal(err)
		}
		var it zotItem
		_ = json.Unmarshal(data, &it)
		if it.URL != "https://example.org/reports/q3" || it.AccessDate == "" {
			t.Fatalf("webpage provenance lost: url=%q access=%q", it.URL, it.AccessDate)
		}
	})

	t.Run("write audit 1:1 and cleanup witness", func(t *testing.T) {
		// Audit rows exist for every mutation kind the ladder drove.
		n, err := st.CountWriteAudit(ctx, prov.LeaseScopeLabel())
		if err != nil {
			t.Fatal(err)
		}
		// The 1:1 witness, exactly: 2 create_collection (ResolvePath) +
		// book {record, rendition, membership} + replay {membership reused}
		// + webpage {record, rendition, membership} = 9 mutations = 9 rows.
		if n != 9 {
			t.Fatalf("write audit rows = %d, want exactly 9 (one per mutation, replay membership reused)", n)
		}

		// Cleanup: delete every created item (parent first? children ride
		// along into the trash) and the whole test collection path, then
		// prove the catalog is clean.
		for _, key := range createdItems {
			if err := prov.write.DeleteAttachmentItem(key); err != nil && !isStatus(err, http.StatusNotFound) {
				t.Fatalf("cleanup item %s: %v", key, err)
			}
		}
		// The IT-created collections go — through the SAME empty-guard as
		// the pre-clean (a foreign same-named collection with items
		// survives; ours are empty once the items are gone).
		cols, cerr := prov.read.ListCanonicalCollections()
		if cerr != nil {
			t.Fatalf("cleanup collections read: %v", cerr)
		}
		env2, eerr := prov.read.getItems(ctx, nil)
		if eerr != nil {
			t.Fatalf("cleanup items read: %v", eerr)
		}
		if err := deleteITCollections(ctx, prov.read, prov.write, env2, cols); err != nil {
			t.Fatalf("cleanup collections: %v", err)
		}
		prov.invalidate()

		for _, key := range createdItems {
			// Records: the catalog walk proves absence (it lists
			// documents). Renditions: the catalog skips attachments, so
			// the witness is the item GET itself — gone or trashed.
			if rec, cerr := catalogFind(prov, ctx, key); cerr != nil {
				t.Fatalf("catalog walk after cleanup: %v", cerr)
			} else if rec != nil {
				t.Fatalf("cleanup witness: record %s still observable", key)
			}
			data, _, gerr := prov.write.GetItem(key)
			if gerr == nil {
				var it zotItem
				if json.Unmarshal(data, &it) == nil && !it.Deleted {
					t.Fatalf("cleanup witness: item %s still live", key)
				}
			} else if !isStatus(gerr, http.StatusNotFound) {
				t.Fatalf("cleanup witness read %s: %v", key, gerr)
			}
		}
		cols, err = prov.read.ListCanonicalCollections()
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cols {
			if c.Name == itPath[0] || c.Name == itPath[1] {
				t.Fatalf("cleanup witness: collection %q survived", c.Name)
			}
		}
	})
}

// catalogFind walks the FULL catalog (real pagination) for a provider id.
func catalogFind(p *Provider, ctx context.Context, providerID string) (*library.CatalogRecord, error) {
	token := ""
	for {
		page, err := p.ListRecords(ctx, token)
		if err != nil {
			return nil, err
		}
		for i := range page.Records {
			if providerID == "" || page.Records[i].ProviderRecordID == providerID {
				cp := page.Records[i]
				return &cp, nil
			}
		}
		if page.NextPageToken == "" {
			return nil, nil
		}
		token = page.NextPageToken
	}
}

func membershipOf(rec *library.CatalogRecord, collectionID string) bool {
	for _, c := range rec.Collections {
		if c == collectionID {
			return true
		}
	}
	return false
}
