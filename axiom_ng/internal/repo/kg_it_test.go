package repo

// R6 (#136) gated live proof: GraphCandidates must never return a seed chunk
// itself. The seed exclusion is per-SET (NOT m.chunk_id = ANY($1)), because
// the earlier per-pair form (m.chunk_id <> sm.chunk_id) let a seed re-enter
// as a "neighbor" whenever two seeds shared an entity — a rich-get-richer
// RRF boost that defeats the arm's purpose.
//
// Run with:
//   AXIOM_KG_IT=1 \
//   AXIOM_TEST_DATABASE_URL=postgresql://axiom_user:...@.../axiom_db?sslmode=disable \
//   go test ./internal/repo/ -run TestIT_GraphCandidates -v

import (
	"context"
	"os"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
)

// seedPair finds two chunks sharing an entity with >= floor distinct chunks
// (same active-snapshot world as GraphCandidates).
func seedPair(t *testing.T, r *Repo, floor int) (a, b string, ok bool) {
	t.Helper()
	err := r.pool.QueryRow(context.Background(), `
		WITH em AS (
			SELECT e.id AS entity_id, count(DISTINCT m.chunk_id) AS chunks
			FROM processing_entities e
			JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
			JOIN processing_entity_mentions m ON m.entity_id = e.id
			GROUP BY e.id
		)
		SELECT m1.chunk_id::text, m2.chunk_id::text
		FROM em
		JOIN processing_entity_mentions m1 ON m1.entity_id = em.entity_id
		JOIN processing_entity_mentions m2 ON m2.entity_id = em.entity_id
		WHERE em.chunks >= $1 AND m1.chunk_id < m2.chunk_id
		LIMIT 1`, floor).Scan(&a, &b)
	if err != nil {
		return "", "", false
	}
	return a, b, true
}

func TestIT_GraphCandidatesExcludesSeeds(t *testing.T) {
	if os.Getenv("AXIOM_KG_IT") != "1" {
		t.Skip("AXIOM_KG_IT=1 required (real graph data)")
	}
	if os.Getenv("AXIOM_TEST_DATABASE_URL") == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL required")
	}
	ctx := context.Background()
	d, err := db.Open(ctx, os.Getenv("AXIOM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	t.Cleanup(d.Close)
	r := New(d.Pool())

	// Prefer a pair sharing a >=3-chunk entity: the third chunk guarantees a
	// non-empty result, so the exclusion assertion cannot pass vacuously.
	// Fall back through >=2 (stable world) to any shared entity.
	var a, b string
	floorUsed := 0
	for _, floor := range []int{3, 2, 1} {
		if x, y, ok := seedPair(t, r, floor); ok {
			a, b, floorUsed = x, y, floor
			break
		}
	}
	if floorUsed == 0 {
		t.Fatal("corpus has no two chunks sharing any entity — cannot witness seed exclusion")
	}
	minM := 2 // the graph arm's stability floor
	if floorUsed < 2 {
		minM = 1 // only a 1-chunk-shared pair exists: expansion floor must match
	}

	cands, err := r.GraphCandidates(ctx, []string{a, b}, minM, 50)
	if err != nil {
		t.Fatalf("GraphCandidates: %v", err)
	}
	seeds := map[string]bool{a: true, b: true}
	for _, c := range cands {
		if seeds[c.ChunkID] {
			t.Fatalf("seed %s re-entered the result as its own graph neighbor", c.ChunkID)
		}
	}
	if floorUsed == 3 && len(cands) == 0 {
		t.Fatal("shared >=3-chunk entity must yield at least one non-seed neighbor (vacuous pass)")
	}
	t.Logf("[IT] seeds (%s, %s, floor %d) expanded to %d neighbor candidates, none is a seed",
		a, b, floorUsed, len(cands))
}
