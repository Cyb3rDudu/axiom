package sync

// #262 contextual boot-order trap — integration proofs through the REAL
// chain (Service.Run against a dedicated *_test database). The 2026-09-11
// production crash-loop pinned the owner ruling: boot ALWAYS succeeds, no
// chicken-and-egg in the product. Four states proven here:
//
//	1. never-synced DB + configured rules → degraded boot (no error, banner
//	   logged, health degraded_no_sync), unruled documents stay citable
//	2. first sync → rules activate WITHOUT restart, transition logged,
//	   projection converges on the activating sync, health active
//	3. synced DB + unknown path/tag → LOUD error naming the value (the
//	   misconfiguration sharpness of #255 is not softened)
//	4. active rules + collection deleted later → stays active (stable
//	   zotero_keys), projection recomputes citable (#255 reversible)
//
// Runs only against a dedicated *_test database (AXIOM_TEST_DATABASE_URL);
// the fixture truncates the canonical tables to simulate a fresh DB.
import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

// ctxBootTruncate wipes the canonical tables so the DB is "never synced"
// (no collections, no source cursor) — the #262 boot-order world.
func ctxBootTruncate(t *testing.T, d *db.DB) {
	t.Helper()
	var dbName string
	if err := d.Pool().QueryRow(context.Background(), `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(dbName, "_test") {
		t.Fatalf("REFUSING to truncate: current_database %q does not end in _test", dbName)
	}
	if _, err := d.Pool().Exec(context.Background(), `
		TRUNCATE zotero_collections, zotero_items, zotero_documents,
		         zotero_attachments, ingest_jobs, zotero_sources CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// ctxBootSvc builds a Service over the fake source and returns a log buffer
// so the degraded/transition banners are assertable (degraded must never be
// silent — the incident was invisible until the crash loop).
func ctxBootSvc(t *testing.T, src zotero.Source) (*Service, *bytes.Buffer) {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping integration test")
	}
	buf := &bytes.Buffer{}
	return New(src, nil, "", "users/0", log.New(buf, "", 0)), buf
}

// ctxDocClass reads citation_class for a document key.
func ctxDocClass(t *testing.T, d *db.DB, sourceID, docKey string) string {
	t.Helper()
	var cls string
	if err := d.Pool().QueryRow(context.Background(),
		`SELECT citation_class FROM zotero_documents WHERE source_id=$1 AND zotero_key=$2`,
		sourceID, docKey).Scan(&cls); err != nil {
		t.Fatalf("read citation_class of %s: %v", docKey, err)
	}
	return cls
}

// ctxBootWorld is the fake Zotero world: a ruled doc (in VWL/Lectures,
// tagged Vorlesung) and a passenger doc outside every rule.
func ctxBootWorld(serverID string, ruled bool) *canonicalFake {
	colls := []zotero.CanonicalCollection{
		{Key: "CVWL", Name: "VWL", Envelope: []byte(`{"key":"CVWL"}`)},
		{Key: "CLECT", Name: "Lectures", ParentKey: "CVWL", Envelope: []byte(`{"key":"CLECT"}`)},
	}
	items := []zotero.CanonicalItem{
		mkItemJSON("PAX1", "book", "", "Passenger Book", nil),
		mkItemJSON("PAX1ATT", "attachment", "PAX1", "pax.pdf", map[string]any{
			"contentType": "application/pdf", "filename": "pax.pdf",
		}),
	}
	if ruled {
		items = append(items,
			mkItemJSON("CTX1", "book", "", "Lecture Slides", map[string]any{
				"collections": []string{"CLECT"},
				"tags":        []map[string]string{{"tag": "Vorlesung"}},
			}),
			mkItemJSON("CTX1ATT", "attachment", "CTX1", "ctx.pdf", map[string]any{
				"contentType": "application/pdf", "filename": "ctx.pdf",
			}),
		)
	}
	return &canonicalFake{serverID: serverID, baseURL: newScriptedBase(), version: 2, collections: colls, items: items}
}

// IT 1 (#262 DoD): never-synced DB + configured rules → boot SUCCEEDS,
// degraded banner logged, health degraded_no_sync, everything citable.
// The fake never carries the ruled world, so even after a sync nothing
// becomes contextual by accident (fail-safe direction).
//
// Mutation: making the degraded branch return the resolve error instead of
// nil (the pre-#262 boot) fails on the first assert — the test pins that
// boot never fatals on an unsynced DB. Making the degraded projection use
// the unresolved rules anyway fails the citable asserts.
func TestContextualBootDegradedNoSyncIT(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	ctxBootTruncate(t, d)

	// Configured rules point at a world this source will NEVER sync — the
	// bootstrap-order case (deploy before the Zotero side exists).
	src := ctxBootWorld("ctxboot1", false)
	svc, buf := ctxBootSvc(t, src)
	svc.repo = repo.New(d.Pool())

	if err := svc.InitContextual(ctx, []string{"VWL/Lectures"}, []string{"Vorlesung"}); err != nil {
		t.Fatalf("#262: boot must succeed degraded on a never-synced DB, got fatal: %v", err)
	}
	if st := svc.ContextualState(); st != "degraded_no_sync" {
		t.Fatalf("health state must be degraded_no_sync, got %q", st)
	}
	if !strings.Contains(buf.String(), "DEGRADED") {
		t.Fatalf("degraded boot must log a loud banner, log: %q", buf.String())
	}

	// A sync lands an UNRULED world: rules stay unresolved (loudly), every
	// document stays citable — nothing becomes contextual by accident.
	res, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("sync under degraded rules: %v", err)
	}
	if st := svc.ContextualState(); st != "degraded_no_sync" {
		t.Fatalf("unresolvable rules must stay degraded after sync, got %q", st)
	}
	for _, k := range []string{"PAX1"} {
		if cls := ctxDocClass(t, d, res.SourceID, k); cls != "citable" {
			t.Fatalf("doc %s must stay citable while degraded, got %q", k, cls)
		}
	}
	if !strings.Contains(buf.String(), "STILL unresolved") {
		t.Fatalf("post-sync non-activation must be loudly logged, log: %q", buf.String())
	}
}

// IT 2 (#262 DoD): after the first sync the degraded rule set ACTIVATES
// without restart — transition logged, health active, and the projection
// converges on the activating sync itself (ruled doc contextual, passenger
// citable).
//
// Mutation: removing the post-sync re-resolve hook fails the state and
// class asserts (stays degraded forever — the pre-#262 operator-lore
// escape). Removing the activation recompute fails the class asserts while
// the state assert still passes, pinning that convergence is not just a
// flag flip.
func TestContextualBootConvergesAfterFirstSyncIT(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	ctxBootTruncate(t, d)

	src := ctxBootWorld("ctxboot2", true)
	svc, buf := ctxBootSvc(t, src)
	svc.repo = repo.New(d.Pool())

	if err := svc.InitContextual(ctx, []string{"VWL/Lectures"}, []string{"Vorlesung"}); err != nil {
		t.Fatalf("degraded boot: %v", err)
	}
	if st := svc.ContextualState(); st != "degraded_no_sync" {
		t.Fatalf("pre-sync state must be degraded_no_sync, got %q", st)
	}

	res, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if st := svc.ContextualState(); st != "active" {
		t.Fatalf("rules must activate after the first sync without restart, got %q", st)
	}
	if !strings.Contains(buf.String(), "DEGRADED->ACTIVE") {
		t.Fatalf("activation transition must be logged, log: %q", buf.String())
	}
	if cls := ctxDocClass(t, d, res.SourceID, "CTX1"); cls != "contextual" {
		t.Fatalf("ruled doc must be contextual after the activating sync, got %q", cls)
	}
	if cls := ctxDocClass(t, d, res.SourceID, "PAX1"); cls != "citable" {
		t.Fatalf("passenger doc must stay citable, got %q", cls)
	}
}

// IT 3 (#262 DoD): synced DB + unknown path/tag → LOUD error naming the
// offending value. This is the genuine misconfiguration (typo, deleted
// collection) and keeps the pre-#262 sharpness — the caller fatals.
//
// Mutation: resolving unknown inputs to degraded regardless of sync state
// (i.e. softening the gate for real typos too) fails every assert here.
// Removing the value from the error message fails the Contains asserts.
func TestContextualBootSyncedUnknownStillFatalIT(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	ctxBootTruncate(t, d)

	// Sync FIRST (DB now has sync state), then configure rules against a
	// world the sync never carried.
	src := ctxBootWorld("ctxboot3", true)
	svc, buf := ctxBootSvc(t, src)
	svc.repo = repo.New(d.Pool())
	if _, err := svc.Run(ctx, nil); err != nil {
		t.Fatalf("seed sync: %v", err)
	}

	for _, tc := range []struct {
		paths, tags []string
		want        string
	}{
		{[]string{"VWL/Nope"}, nil, "VWL/Nope"},
		{nil, []string{"NoSuchTag"}, "NoSuchTag"},
	} {
		buf.Reset()
		err := svc.InitContextual(ctx, tc.paths, tc.tags)
		if err == nil {
			t.Fatalf("paths=%v tags=%v must be a loud boot error on a synced DB", tc.paths, tc.tags)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("error must name %q, got: %v", tc.want, err)
		}
	}
}

// IT 4 (#262 DoD): active rules + the ruled collection deleted later →
// stays ACTIVE until the next sync, then resolves per the #255
// reversible-by-design rule: the projection recomputes the doc citable
// (deleted collection has no members), the rule set itself keeps riding
// the stable zotero_key — no re-resolve, no fatal.
//
// Mutation: re-resolving the ACTIVE rule set on every sync goes red via
// the log assert — with that mutation the second sync's resolve hits the
// deleted-only path match and logs "STILL unresolved" (state itself stays
// "active" because ctxDegraded is already false, which is exactly why the
// log assert exists). Dropping the NOT c.deleted membership guard fails
// the citable assert.
func TestContextualBootActiveSurvivesCollectionDeletionIT(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	ctxBootTruncate(t, d)

	src := ctxBootWorld("ctxboot4", true)
	svc, buf := ctxBootSvc(t, src)
	svc.repo = repo.New(d.Pool())
	// Collection-path rule ONLY: the doc carries no ruled tag, so its class
	// is observable through the collection axis alone.
	if err := svc.InitContextual(ctx, []string{"VWL/Lectures"}, nil); err != nil {
		t.Fatalf("boot: %v", err)
	}
	res, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	if st := svc.ContextualState(); st != "active" {
		t.Fatalf("must be active after sync 1, got %q", st)
	}

	// Sync 2: the collection is GONE from Zotero (reconcile-by-absence
	// marks it deleted). The rules stay active; the doc recomputes citable.
	src.collections = []zotero.CanonicalCollection{{Key: "CVWL", Name: "VWL", Envelope: []byte(`{"key":"CVWL"}`)}}
	src.version = 3
	if _, err := svc.Run(ctx, nil); err != nil {
		t.Fatalf("sync 2 (collection deleted): %v", err)
	}
	if st := svc.ContextualState(); st != "active" {
		t.Fatalf("rules must stay active through a collection deletion, got %q", st)
	}
	if cls := ctxDocClass(t, d, res.SourceID, "CTX1"); cls != "citable" {
		t.Fatalf("doc must recompute citable after the ruled collection is deleted (#255 reversible), got %q", cls)
	}
	if strings.Contains(buf.String(), "STILL unresolved") {
		t.Fatalf("active rules must not re-resolve on sync, log: %q", buf.String())
	}
}
