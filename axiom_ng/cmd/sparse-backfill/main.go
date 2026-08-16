// Command sparse-backfill materializes the learned sparse embeddings from
// Postgres into the OpenSearch chunk index (R5, #135).
//
// One-shot operational tool: adds the rank_features mapping additively,
// bulk-updates every active-snapshot chunk doc with its sparse weights, and
// verifies the OS == active-chunks invariant afterwards (counts must match;
// the total doc count must not change — updates only, no inserts/deletes).
//
// Env: AXIOM_DATABASE_URL, AXIOM_OPENSEARCH_URL (both required);
// AXIOM_OPENSEARCH_USERNAME/PASSWORD optional. Idempotent: re-running
// re-updates the same values.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

const indexName = "axiom-ng-chunks-v1"

func main() {
	ctx := context.Background()
	start := time.Now()
	dbURL := os.Getenv("AXIOM_DATABASE_URL")
	osURL := strings.TrimRight(os.Getenv("AXIOM_OPENSEARCH_URL"), "/")
	if dbURL == "" || osURL == "" {
		fmt.Fprintln(os.Stderr, "AXIOM_DATABASE_URL and AXIOM_OPENSEARCH_URL are required")
		os.Exit(2)
	}
	user, pass := os.Getenv("AXIOM_OPENSEARCH_USERNAME"), os.Getenv("AXIOM_OPENSEARCH_PASSWORD")

	// 1. Additive mapping (rank_features) — idempotent.
	if err := putMapping(ctx, osURL, user, pass); err != nil {
		fatal("mapping: %v", err)
	}
	fmt.Println("mapping: sparse rank_features ensured")

	database, err := db.Open(ctx, dbURL)
	if err != nil {
		fatal("postgres: %v", err)
	}
	defer database.Close()

	totalBefore, err := osCount(ctx, osURL, user, pass, map[string]any{"match_all": map[string]any{}})
	if err != nil {
		fatal("count before: %v", err)
	}

	// 2. Bulk-update active chunks carrying sparse weights.
	rows, err := database.Pool().Query(ctx, `
		SELECT c.id::text, s.values::text
		FROM processing_chunks c
		JOIN processing_snapshots sn ON sn.id = c.snapshot_id AND sn.active
		JOIN processing_chunk_sparse_embeddings s ON s.chunk_id = c.id
		ORDER BY c.id`)
	if err != nil {
		fatal("select: %v", err)
	}
	var buf bytes.Buffer
	updated := 0
	pending := 0 // actions queued in buf since the last flush
	// Sample rows are collected DURING the scan (no second full query):
	// every 50th row plus the last, a bounded deterministic spread.
	var sampleIDs, sampleVals []string
	var lastID, lastVals string
	for rows.Next() {
		var id, vals string
		if err := rows.Scan(&id, &vals); err != nil {
			fatal("scan: %v", err)
		}
		sparse, err := repo.ParseSparse(vals)
		if err != nil {
			fatal("chunk %s: %v", id, err)
		}
		action, _ := json.Marshal(map[string]any{"update": map[string]any{"_index": indexName, "_id": id}})
		doc, _ := json.Marshal(map[string]any{"doc": map[string]any{"sparse": sparse}})
		buf.Write(action)
		buf.WriteByte('\n')
		buf.Write(doc)
		buf.WriteByte('\n')
		if updated%50 == 0 {
			sampleIDs, sampleVals = append(sampleIDs, id), append(sampleVals, vals)
		}
		lastID, lastVals = id, vals
		updated++
		pending++
		if buf.Len() > 4<<20 { // flush every ~4 MB
			if err := flushBulk(ctx, osURL, user, pass, buf.Bytes(), pending); err != nil {
				fatal("bulk: %v", err)
			}
			buf.Reset()
			pending = 0
		}
	}
	if err := rows.Err(); err != nil {
		fatal("rows: %v", err)
	}
	if lastID != "" && (len(sampleIDs) == 0 || sampleIDs[len(sampleIDs)-1] != lastID) {
		sampleIDs, sampleVals = append(sampleIDs, lastID), append(sampleVals, lastVals)
	}
	if buf.Len() > 0 {
		if err := flushBulk(ctx, osURL, user, pass, buf.Bytes(), pending); err != nil {
			fatal("bulk: %v", err)
		}
	}

	// 3. Invariant proof. rank_features fields reject exists-queries, so the
	//    sparse-carrying count cannot be queried directly; the proof is:
	//    (a) every _bulk item succeeded (updated == PG sparse rows read),
	//    (b) OS total docs == PG active chunk count (the OS==active-chunks
	//        invariant) and unchanged by the backfill (updates only),
	//    (c) a deterministic sample of updated docs carries a non-empty
	//        sparse field with the exact PG weights.
	totalAfter, err := osCount(ctx, osURL, user, pass, map[string]any{"match_all": map[string]any{}})
	if err != nil {
		fatal("count after: %v", err)
	}
	var pgActiveChunks int
	if err := database.Pool().QueryRow(ctx, `
		SELECT count(*) FROM processing_chunks c
		JOIN processing_snapshots sn ON sn.id = c.snapshot_id AND sn.active`).Scan(&pgActiveChunks); err != nil {
		fatal("count active chunks: %v", err)
	}
	fmt.Printf("backfill: %d docs updated in %s\n", updated, time.Since(start).Round(time.Second))
	fmt.Printf("invariant: OS total %d -> %d (updates only), PG active chunks %d\n",
		totalBefore, totalAfter, pgActiveChunks)
	if totalBefore != totalAfter {
		fatal("INVARIANT VIOLATION: total doc count changed %d -> %d (updates must not add/remove docs)", totalBefore, totalAfter)
	}
	if totalAfter != pgActiveChunks {
		fatal("INVARIANT VIOLATION: OS docs %d != PG active chunks %d", totalAfter, pgActiveChunks)
	}
	sampleSparse(ctx, osURL, user, pass, sampleIDs, sampleVals)
	fmt.Println("OK: invariant OS == active chunks holds; sparse verified by bulk receipt + sampled doc weights")
}

// sampleSparse verifies sampled chunk docs — collected DURING the main
// scan (every 50th row + last), no second full query — carry exactly the
// PG sparse weights.
func sampleSparse(ctx context.Context, base, user, pass string, ids, vals []string) {
	if len(ids) == 0 {
		fatal("no sparse rows to sample")
	}
	pick := map[int]bool{0: true, len(ids) / 2: true, len(ids) - 1: true}
	for i := 0; i < 25 && i < len(ids); i++ {
		pick[i*len(ids)/25] = true
	}
	checked := 0
	for i := range ids {
		if !pick[i] {
			continue
		}
		want, err := repo.ParseSparse(vals[i])
		if err != nil {
			fatal("sample %s: %v", ids[i], err)
		}
		_, rb, err := doOS(ctx, base, user, pass, http.MethodGet, "/"+indexName+"/_doc/"+ids[i]+"?_source=sparse", nil)
		if err != nil {
			fatal("sample get %s: %v", ids[i], err)
		}
		var doc struct {
			Found  bool                      `json:"found"`
			Source map[string]map[string]any `json:"_source"`
		}
		if err := json.Unmarshal(rb, &doc); err != nil || !doc.Found {
			fatal("sample get %s: bad response %.200s", ids[i], rb)
		}
		gotRaw := doc.Source["sparse"]
		if len(gotRaw) == 0 {
			fatal("sample %s: sparse field missing in OS", ids[i])
		}
		// Compare token sets and weight closeness (float round-trip).
		for k, w := range want {
			ov, ok := gotRaw[k]
			if !ok {
				fatal("sample %s: token %s missing in OS sparse", ids[i], k)
			}
			of, ok := ov.(float64)
			if !ok {
				if s, ok2 := ov.(string); ok2 {
					of, _ = strconv.ParseFloat(s, 64)
					ok = true
				}
			}
			if !ok || math.Abs(of-w) > 1e-9 {
				fatal("sample %s token %s: OS weight %v != PG %v", ids[i], k, ov, w)
			}
		}
		checked++
	}
	fmt.Printf("sample: %d docs verified field-exact against PG\n", checked)
}

func putMapping(ctx context.Context, base, user, pass string) error {
	body, _ := json.Marshal(map[string]any{
		"properties": map[string]any{"sparse": map[string]any{"type": "rank_features"}},
	})
	_, rb, err := doOS(ctx, base, user, pass, http.MethodPut, "/"+indexName+"/_mapping", body)
	if err != nil {
		return err
	}
	if !bytes.Contains(rb, []byte("acknowledged")) {
		return fmt.Errorf("unexpected mapping response: %.200s", rb)
	}
	return nil
}

// flushBulk posts one NDJSON bulk batch and ENFORCES the receipt: an errors
// flag is fatal unconditionally (first parseable item error included — a
// truncated/unparseable body must not read as success), and the item count
// must match the actions actually sent in this batch.
func flushBulk(ctx context.Context, base, user, pass string, body []byte, expected int) error {
	_, rb, err := doOS(ctx, base, user, pass, http.MethodPost, "/_bulk", body)
	if err != nil {
		return err
	}
	var resp struct {
		Errors bool `json:"errors"`
		Items  []struct {
			Update struct {
				Error any `json:"error"`
			} `json:"update"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rb, &resp); err != nil {
		return fmt.Errorf("decode bulk response: %w", err)
	}
	if len(resp.Items) != expected {
		return fmt.Errorf("bulk receipt: %d items for %d actions sent", len(resp.Items), expected)
	}
	if resp.Errors {
		for _, it := range resp.Items {
			if it.Update.Error != nil {
				return fmt.Errorf("bulk item error: %v", it.Update.Error)
			}
		}
		return fmt.Errorf("bulk reported errors but no item error was parseable (%d items)", len(resp.Items))
	}
	return nil
}

func osCount(ctx context.Context, base, user, pass string, query map[string]any) (int, error) {
	body, _ := json.Marshal(map[string]any{"query": query})
	_, rb, err := doOS(ctx, base, user, pass, http.MethodPost, "/"+indexName+"/_count", body)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rb, &resp); err != nil {
		return 0, err
	}
	return resp.Count, nil
}

func doOS(ctx context.Context, base, user, pass, method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if strings.HasSuffix(path, "/_mapping") || strings.HasSuffix(path, "/_count") {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, rb, fmt.Errorf("opensearch %s %s: HTTP %d: %.300s", method, path, resp.StatusCode, rb)
	}
	return resp.StatusCode, rb, nil
}

func fatal(f string, args ...any) {
	fmt.Fprintf(os.Stderr, "sparse-backfill: "+f+"\n", args...)
	os.Exit(1)
}
