// #379 IT: the container path map (AXIOM_ZOTERO_PATH_MAP, installed at
// composition startup) reaches the PERSISTED mirror path — with a map
// set, zotero_attachments.local_path holds the translated container
// path; without one, the raw URI-derived host path byte-identical. The
// second run also proves the durability mechanism: a re-sync after the
// deploy rewrites the stored path through the mirror upsert (no
// migration SQL involved).
package sync

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library/mirror"
	"github.com/Cyb3rDudu/axiom/axiom/internal/repo"
	storemigrations "github.com/Cyb3rDudu/axiom/axiom/internal/store/migrations"
	"github.com/Cyb3rDudu/axiom/axiom/internal/zoteroprovider"
)

func TestPathMapMirrorPersistsTranslatedLocalPathIT(t *testing.T) {
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
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := storemigrations.Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("store migrate: %v", err)
	}

	// Host-side path as the Zotero local API would report it. The file
	// does not need to exist — the IT asserts path persistence, not jobs.
	hostPath := "/home/dudu/Zotero/storage/PM1/book.pdf"
	src := &canonicalFake{serverID: "pathmap-it", baseURL: newScriptedBase(), version: 3}
	src.items = []zoteroprovider.CanonicalItem{
		mkItemJSON("PMB1", "book", "", "Path Map Book", map[string]any{
			"creators": []map[string]string{{"firstName": "Ada", "lastName": "Lovelace", "creatorType": "author"}},
		}),
		mkItemJSON("PMA1", "attachment", "PMB1", "book.pdf", map[string]any{
			"contentType": "application/pdf", "filename": "book.pdf",
		}),
	}
	env, _ := json.Marshal(map[string]any{
		"key": "PMA1", "version": 1,
		"links": map[string]any{"enclosure": map[string]any{"href": "file://" + hostPath}},
		"data":  map[string]any{"key": "PMA1", "version": 1, "itemType": "attachment", "parentItem": "PMB1", "contentType": "application/pdf", "filename": "book.pdf"},
	})
	src.items[1].Envelope = env

	repoObj := repo.New(d.Pool())
	// Run 1 uses the no-map service; run 2 constructs a second service
	// WITH the pairs (the post-deploy shape: config carried by consumers).

	storedPath := func(sourceID string) string {
		var p string
		if err := d.Pool().QueryRow(ctx,
			`SELECT local_path FROM zotero_attachments WHERE source_id=$1 AND zotero_key='PMA1'`,
			sourceID).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Run 1 — no map installed: the mirror persists the raw host path.
	svc := New(src, mirror.New(repoObj.Pool(), nil), repoObj, src.baseURL, "users/0", nil, log.Default())
	res, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if got := storedPath(res.SourceID); got != hostPath {
		t.Fatalf("without a map local_path must be the raw host path %q, got %q", hostPath, got)
	}

	// Run 2 — map carried (the post-deploy shape): the mirror upsert
	// rewrites the stored path with the translated container path.
	pairs := []zoteroprovider.PathPair{
		{From: "/home/dudu/Zotero/storage", To: "/var/lib/axiom/zotero-storage"},
	}
	svcMap := New(src, mirror.New(repoObj.Pool(), pairs), repoObj, src.baseURL, "users/0", pairs, log.Default())
	res, err = svcMap.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	want := "/var/lib/axiom/zotero-storage/PM1/book.pdf"
	if got := storedPath(res.SourceID); got != want {
		t.Fatalf("with a map local_path must be the translated path %q, got %q", want, got)
	}
}
