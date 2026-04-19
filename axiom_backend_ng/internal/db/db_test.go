package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/testutil"
)

func TestDefaultConfig(t *testing.T) {
	t.Parallel()
	got := db.DefaultConfig()
	if got.MaxOpenConns != 80 {
		t.Errorf("MaxOpenConns: got %d, want 80", got.MaxOpenConns)
	}
	if got.ConnMaxLifetime != time.Hour {
		t.Errorf("ConnMaxLifetime: got %s, want 1h", got.ConnMaxLifetime)
	}
	if got.StatementTimeout != 30*time.Second {
		t.Errorf("StatementTimeout: got %s, want 30s", got.StatementTimeout)
	}
	if got.ConnectTimeout != 10*time.Second {
		t.Errorf("ConnectTimeout: got %s, want 10s", got.ConnectTimeout)
	}
}

func TestOpenRequiresURL(t *testing.T) {
	t.Parallel()
	_, err := db.Open(context.Background(), db.DefaultConfig())
	if err == nil {
		t.Fatal("expected error for empty URL, got nil")
	}
	if !strings.Contains(err.Error(), "URL is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOpenAgainstLivePostgres(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	defer pg.Close()

	ctx := context.Background()
	if err := db.Ping(ctx, pg.DB); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// statement_timeout should match our 30s setting from DefaultConfig.
	var stmtTimeout string
	if err := pg.DB.WithContext(ctx).Raw("SHOW statement_timeout").Scan(&stmtTimeout).Error; err != nil {
		t.Fatalf("SHOW statement_timeout: %v", err)
	}
	if stmtTimeout != "30s" {
		t.Errorf("statement_timeout: got %q, want %q", stmtTimeout, "30s")
	}
}

func TestRequireSchemaPasses(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	defer pg.Close()

	if err := db.RequireSchema(context.Background(), pg.DB); err != nil {
		t.Fatalf("RequireSchema: %v", err)
	}
}

func TestRequireSchemaFailsOnMissingTable(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	defer pg.Close()

	if err := pg.DB.Exec("DROP TABLE users CASCADE").Error; err != nil {
		t.Fatalf("drop users: %v", err)
	}
	err := db.RequireSchema(context.Background(), pg.DB)
	if err == nil {
		t.Fatal("expected error for missing users table")
	}
	if !strings.Contains(err.Error(), "users") {
		t.Errorf("error should name missing table: %v", err)
	}
}

func TestVectorRoundTrip(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	defer pg.Close()

	ctx := context.Background()

	// Seed a user + document so document_chunks' FK constraints are satisfied.
	var userID int
	if err := pg.DB.WithContext(ctx).Raw(`
		INSERT INTO users (username, email, hashed_password, created_at, updated_at)
		VALUES ('vec_user', 'vec@test', 'x', NOW(), NOW()) RETURNING id
	`).Scan(&userID).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var docID string
	if err := pg.DB.WithContext(ctx).Raw(`
		INSERT INTO documents (id, user_id, filename, created_at, updated_at)
		VALUES (gen_random_uuid(), ?, 'vec.pdf', NOW(), NOW()) RETURNING id
	`, userID).Scan(&docID).Error; err != nil {
		t.Fatalf("seed doc: %v", err)
	}

	got := make([]float32, 1024)
	for i := range got {
		got[i] = float32(i) / 1024.0
	}
	vec := db.NewVector(got)

	if err := pg.DB.WithContext(ctx).Exec(`
		INSERT INTO document_chunks (id, doc_id, chunk_id, chunk_index, chunk_text, dense_embedding, chunk_metadata)
		VALUES (gen_random_uuid(), ?, 'test-chunk', 0, 'hello', ?, '{}'::jsonb)
	`, docID, vec).Error; err != nil {
		t.Fatalf("insert with pgvector: %v", err)
	}

	var back db.VectorType
	err := pg.DB.WithContext(ctx).
		Raw(`SELECT dense_embedding FROM document_chunks WHERE chunk_id='test-chunk'`).
		Scan(&back).Error
	if err != nil {
		t.Fatalf("read pgvector: %v", err)
	}
	if len(back.Slice()) != 1024 {
		t.Errorf("vector length: got %d, want 1024", len(back.Slice()))
	}
}
