package retriever_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/opensearch"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/retriever"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/testutil"
)

// seedChunk inserts a fully-embedded chunk so the retriever's dense +
// sparse queries have something to rank. Pads the dense slice to the
// schema's 1024 dimension with zeros; only the leading components
// meaningfully differ across test rows.
const denseDim = 1024

func seedChunk(t *testing.T, pg *testutil.Postgres, docID uuid.UUID, chunkID, text string, dense []float32, sparse map[string]float64) {
	t.Helper()
	ctx := context.Background()

	full := make([]float32, denseDim)
	copy(full, dense)
	denseLit := "["
	for i, f := range full {
		if i > 0 {
			denseLit += ","
		}
		denseLit += strconvFormatFloat64(float64(f))
	}
	denseLit += "]"

	sparseBytes, err := json.Marshal(sparse)
	if err != nil {
		t.Fatalf("sparse marshal: %v", err)
	}

	// Pick a unique chunk_index per (doc_id, chunk_id) by counting
	// existing rows — cheap and avoids in-test state tracking.
	var nextIdx int
	if err := pg.DB.WithContext(ctx).
		Raw(`SELECT COALESCE(MAX(chunk_index), -1) + 1 FROM document_chunks WHERE doc_id = ?`, docID).
		Scan(&nextIdx).Error; err != nil {
		t.Fatalf("next chunk_index: %v", err)
	}

	err = pg.DB.WithContext(ctx).Exec(`
		INSERT INTO document_chunks
		  (id, doc_id, chunk_id, chunk_index, chunk_text,
		   dense_embedding, sparse_embedding, chunk_metadata, created_at)
		VALUES
		  (gen_random_uuid(), ?, ?, ?, ?, ?::vector, ?::jsonb, '{}'::jsonb, NOW())
	`, docID, chunkID, nextIdx, text, denseLit, string(sparseBytes)).Error
	if err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
}

func TestDenseAndSparseIntegration(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	defer pg.Close()

	ctx := context.Background()

	// Seed a user + doc so FK constraints on document_chunks are happy.
	var userID int32
	if err := pg.DB.WithContext(ctx).Raw(`
		INSERT INTO users (username, email, hashed_password, created_at, updated_at)
		VALUES ('retriever_user', 'ret@test', 'x', NOW(), NOW()) RETURNING id
	`).Scan(&userID).Error; err != nil {
		t.Fatalf("user: %v", err)
	}
	docID := uuid.New()
	if err := pg.DB.WithContext(ctx).Exec(`
		INSERT INTO documents (id, user_id, filename, created_at, updated_at)
		VALUES (?, ?, 'ret.pdf', NOW(), NOW())
	`, docID, userID).Error; err != nil {
		t.Fatalf("doc: %v", err)
	}

	// Three chunks with varying similarity to a query vector [1,0,0,0,0,0,0,0].
	// alpha is closest (same direction), beta middle, gamma orthogonal.
	seedChunk(t, pg, docID, "alpha", "alpha text", []float32{0.9, 0.1, 0, 0, 0, 0, 0, 0}, map[string]float64{"1": 0.9, "2": 0.1})
	seedChunk(t, pg, docID, "beta", "beta text", []float32{0.5, 0.5, 0, 0, 0, 0, 0, 0}, map[string]float64{"1": 0.5, "3": 0.5})
	seedChunk(t, pg, docID, "gamma", "gamma text", []float32{0, 1, 0, 0, 0, 0, 0, 0}, map[string]float64{"7": 1.0})

	gpu := &stubGPU{
		dense:  paddedVec([]float32{1, 0, 0, 0, 0, 0, 0, 0}),
		sparse: map[string]float64{"1": 1.0},
	}
	r := &retriever.Retriever{DB: pg.DB, GPU: gpu}

	out, err := r.Retrieve(ctx, retriever.Options{Query: "anything", NResults: 3, DocIDs: []uuid.UUID{docID}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected at least one hit from dense/sparse pipeline")
	}
	// alpha should rank top across both channels; sanity-check it
	// appears in the top two (accounting for RRF fusion).
	foundAlpha := false
	for i, r := range out {
		if r.ChunkID == "alpha" && i < 2 {
			foundAlpha = true
		}
	}
	if !foundAlpha {
		t.Errorf("alpha should be in top 2, got %+v", out)
	}
}

// TestRetrieveWithMissingOpenSearchStillWorks exercises the
// OpenSearch-client being nil while dense+sparse produce results.
func TestRetrieveWithMissingOpenSearchStillWorks(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	defer pg.Close()

	ctx := context.Background()
	var userID int32
	_ = pg.DB.WithContext(ctx).Raw(`
		INSERT INTO users (username, email, hashed_password, created_at, updated_at)
		VALUES ('os_less', 'os@test', 'x', NOW(), NOW()) RETURNING id
	`).Scan(&userID).Error
	docID := uuid.New()
	_ = pg.DB.WithContext(ctx).Exec(`
		INSERT INTO documents (id, user_id, filename, created_at, updated_at)
		VALUES (?, ?, 'o.pdf', NOW(), NOW())
	`, docID, userID).Error

	seedChunk(t, pg, docID, "one", "one", []float32{1, 0, 0, 0, 0, 0, 0, 0}, map[string]float64{"x": 1.0})
	gpu := &stubGPU{dense: paddedVec([]float32{1, 0, 0, 0, 0, 0, 0, 0})}
	r := &retriever.Retriever{DB: pg.DB, GPU: gpu}
	out, err := r.Retrieve(ctx, retriever.Options{Query: "q", NResults: 1, DocIDs: []uuid.UUID{docID}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("expected 1 hit, got %+v", out)
	}
}

// TestRetrieveWithOpenSearchChannelsAllThree plumbs dense + sparse +
// BM25 (stubbed) through the full RRF pipeline.
func TestRetrieveWithOpenSearchChannelsAllThree(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	defer pg.Close()

	ctx := context.Background()
	var userID int32
	_ = pg.DB.WithContext(ctx).Raw(`
		INSERT INTO users (username, email, hashed_password, created_at, updated_at)
		VALUES ('triple', 't@t', 'x', NOW(), NOW()) RETURNING id
	`).Scan(&userID).Error
	docID := uuid.New()
	_ = pg.DB.WithContext(ctx).Exec(`
		INSERT INTO documents (id, user_id, filename, created_at, updated_at)
		VALUES (?, ?, 't.pdf', NOW(), NOW())
	`, docID, userID).Error

	seedChunk(t, pg, docID, "top", "top doc", []float32{1, 0, 0, 0, 0, 0, 0, 0}, map[string]float64{"1": 1.0})
	seedChunk(t, pg, docID, "mid", "mid doc", []float32{0.6, 0.4, 0, 0, 0, 0, 0, 0}, map[string]float64{"2": 1.0})

	gpu := &stubGPU{
		dense:  paddedVec([]float32{1, 0, 0, 0, 0, 0, 0, 0}),
		sparse: map[string]float64{"1": 1.0},
	}
	os := &stubOS{
		hits: []opensearch.Hit{{ChunkID: "top", DocID: docID, Text: "top doc", Score: 5.0}},
	}
	r := &retriever.Retriever{DB: pg.DB, GPU: gpu, OpenSearch: os}
	out, err := r.Retrieve(ctx, retriever.Options{Query: "top", NResults: 2, DocIDs: []uuid.UUID{docID}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected hits")
	}
	// `top` appears in all three channels → RRF winner.
	if out[0].ChunkID != "top" {
		t.Errorf("top should win across all channels: %+v", out)
	}
}

// paddedVec zero-pads a short vector to the dense_embedding column
// dimension so pgvector can compute similarity.
func paddedVec(v []float32) []float32 {
	full := make([]float32, denseDim)
	copy(full, v)
	return full
}

// Force a direct import of db so linting doesn't complain when the
// other helpers are tree-shaken.
var _ = db.DefaultConfig
