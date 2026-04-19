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
	if got.MaxConns != 80 {
		t.Errorf("MaxConns: got %d, want 80", got.MaxConns)
	}
	if got.MaxConnLifetime != time.Hour {
		t.Errorf("MaxConnLifetime: got %s, want 1h", got.MaxConnLifetime)
	}
	if got.StatementTimeout != 30*time.Second {
		t.Errorf("StatementTimeout: got %s, want 30s", got.StatementTimeout)
	}
	if got.ConnectTimeout != 10*time.Second {
		t.Errorf("ConnectTimeout: got %s, want 10s", got.ConnectTimeout)
	}
}

func TestNewPoolRequiresURL(t *testing.T) {
	t.Parallel()
	_, err := db.NewPool(context.Background(), db.DefaultConfig())
	if err == nil {
		t.Fatal("expected error for empty URL, got nil")
	}
	if !strings.Contains(err.Error(), "URL is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewPoolRejectsMalformedURL(t *testing.T) {
	t.Parallel()
	cfg := db.DefaultConfig()
	cfg.URL = "::not-a-url::"
	_, err := db.NewPool(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse URL") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewPoolAgainstLivePostgres(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	defer pg.Close()

	ctx := context.Background()
	if err := db.Ping(ctx, pg.Pool); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// statement_timeout should match our 30s setting from DefaultConfig.
	var stmtTimeout string
	if err := pg.Pool.QueryRow(ctx, "SHOW statement_timeout").Scan(&stmtTimeout); err != nil {
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

	if err := db.RequireSchema(context.Background(), pg.Pool); err != nil {
		t.Fatalf("RequireSchema: %v", err)
	}
}

func TestRequireSchemaFailsOnMissingTable(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	defer pg.Close()

	if _, err := pg.Pool.Exec(context.Background(), "DROP TABLE users CASCADE"); err != nil {
		t.Fatalf("drop users: %v", err)
	}
	err := db.RequireSchema(context.Background(), pg.Pool)
	if err == nil {
		t.Fatal("expected error for missing users table")
	}
	if !strings.Contains(err.Error(), "users") {
		t.Errorf("error should name missing table: %v", err)
	}
}

// TestRequireSchemaPropagatesQueryError exercises the pool-query-failed
// branch by closing the pool before RequireSchema runs.
func TestRequireSchemaPropagatesQueryError(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	pg.Close() // close BEFORE calling RequireSchema; all QueryRow calls fail.

	err := db.RequireSchema(context.Background(), pg.Pool)
	if err == nil {
		t.Fatal("expected query error on closed pool")
	}
	if !strings.Contains(err.Error(), "probe") {
		t.Errorf("error should describe probe failure: %v", err)
	}
}

// TestNewPoolRejectsUnreachableHost exercises the pgxpool.NewWithConfig
// error branch by pointing the pool at an unresolvable host. pgxpool.New
// returns immediately on DNS failure.
func TestNewPoolRejectsUnreachableHost(t *testing.T) {
	t.Parallel()
	cfg := db.DefaultConfig()
	cfg.URL = "postgres://u:p@invalid..host..name:5432/db?sslmode=disable"
	cfg.ConnectTimeout = 100 * time.Millisecond
	// pgxpool lazy-connects, so NewPool itself succeeds; we force the
	// failure on Ping.
	pool, err := db.NewPool(context.Background(), cfg)
	if err != nil {
		// Accept either failure mode: lazy-connect may or may not defer
		// hostname resolution depending on resolver behaviour.
		return
	}
	defer pool.Close()
	if err := db.Ping(context.Background(), pool); err == nil {
		t.Fatal("expected Ping to fail on bogus host")
	}
}

func TestVectorRoundTrip(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	defer pg.Close()

	ctx := context.Background()

	// Seed a user + document so document_chunks' FK constraints are satisfied.
	var userID int
	if err := pg.Pool.QueryRow(ctx, `
		INSERT INTO users (username, email, hashed_password, created_at, updated_at)
		VALUES ('vec_user', 'vec@test', 'x', NOW(), NOW()) RETURNING id
	`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var docID string
	if err := pg.Pool.QueryRow(ctx, `
		INSERT INTO documents (id, user_id, filename, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, 'vec.pdf', NOW(), NOW()) RETURNING id
	`, userID).Scan(&docID); err != nil {
		t.Fatalf("seed doc: %v", err)
	}

	got := make([]float32, 1024)
	for i := range got {
		got[i] = float32(i) / 1024.0
	}
	vec := pgvectorVec(got)

	_, err := pg.Pool.Exec(ctx, `
		INSERT INTO document_chunks (id, doc_id, chunk_id, chunk_index, chunk_text, dense_embedding, chunk_metadata)
		VALUES (gen_random_uuid(), $1, 'test-chunk', 0, 'hello', $2, '{}'::jsonb)
	`, docID, vec)
	if err != nil {
		t.Fatalf("insert with pgvector: %v", err)
	}

	var back db.VectorType
	err = pg.Pool.QueryRow(ctx, `SELECT dense_embedding FROM document_chunks WHERE chunk_id='test-chunk'`).Scan(&back)
	if err != nil {
		t.Fatalf("read pgvector: %v", err)
	}
	if len(back.Slice()) != 1024 {
		t.Errorf("vector length: got %d, want 1024", len(back.Slice()))
	}
}

// pgvectorVec is a tiny helper to keep the test free of the pgvector import.
func pgvectorVec(f []float32) db.VectorType {
	return db.NewVector(f)
}
