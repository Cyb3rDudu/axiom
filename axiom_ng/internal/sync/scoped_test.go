package sync

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
)

var _scopedN int

// timeNowNanos returns a unique-ish timestamp for per-test isolation.
func timeNowNanos() int64 { return time.Now().UnixNano() }

// makePdf writes a small real PDF file for attachment-resolution tests.
func makePdf(t *testing.T, text string) string {
	p := t.TempDir() + "/" + text + ".pdf"
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// openTestDB opens and migrates the integration-test database.
func openTestDB(t *testing.T, ctx context.Context) *db.DB {
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

// newScriptedBase returns a unique source base URL so each test is isolated
// from data left behind by earlier runs against the persistent test database.
func newScriptedBase() string {
	_scopedN++
	return fmt.Sprintf("http://test/%d", timeNowNanos()) + strconv.Itoa(_scopedN)
}

// attIDByKey returns the attachment projection id for an attachment key.
func attIDByKey(t *testing.T, d *db.DB, sourceID, attKey string) string {
	t.Helper()
	var id string
	if err := d.Pool().QueryRow(context.Background(),
		`SELECT id::text FROM zotero_attachments WHERE source_id=$1 AND zotero_key=$2`,
		sourceID, attKey).Scan(&id); err != nil {
		t.Fatalf("attachment id: %v", err)
	}
	return id
}
