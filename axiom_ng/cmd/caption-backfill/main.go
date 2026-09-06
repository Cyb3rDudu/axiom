// Command caption-backfill re-embeds EXISTING captioned chunks of active
// snapshots and re-indexes them with the source-labeled caption_text (#257).
//
// One-shot operational tool in the #233 pattern: existing caption data is
// REUSED (no re-captioning — the hash gate stays closed), the Python engine
// (axiom_ng_runner compute_core/caption_backfill_cli.py, real BGE-M3)
// recomputes each captioned chunk's dense vector from text + labeled
// captions, then this tool upserts the vectors into Postgres and bulk-updates
// the OpenSearch docs (embedding + labeled caption_text). Idempotent:
// re-running rewrites the same values. Any engine failure aborts BEFORE a
// single write — never mixed old/new vectors.
//
// Env: AXIOM_DATABASE_URL, AXIOM_OPENSEARCH_URL (both required);
// AXIOM_OPENSEARCH_USERNAME/PASSWORD optional; AXIOM_PYTHON /
// AXIOM_RUNNER_DIR override the runner venv / checkout discovery.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/backfill"
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

	database, err := db.Open(ctx, dbURL)
	if err != nil {
		fatal("postgres: %v", err)
	}
	defer database.Close()

	// 1. Collect captioned chunks of ACTIVE snapshots.
	rows, err := database.Pool().Query(ctx, `
		SELECT c.id::text, c.text, c.section_titles::text,
		       c.image_captions::text, c.figure_captions::text
		FROM processing_chunks c
		JOIN processing_snapshots sn ON sn.id = c.snapshot_id AND sn.active
		WHERE c.image_captions <> '{}' OR c.figure_captions <> '{}'
		ORDER BY c.id`)
	if err != nil {
		fatal("select: %v", err)
	}
	var targets []target
	for rows.Next() {
		var id, text, secs, caps, figs string
		if err := rows.Scan(&id, &text, &secs, &caps, &figs); err != nil {
			fatal("scan: %v", err)
		}
		var sectionTitles []string
		_ = json.Unmarshal([]byte(secs), &sectionTitles) // degrade to no titles
		t := repo.LabeledCaptionText(&caps, &figs)
		if t == "" {
			continue // malformed JSON in both maps — nothing to index
		}
		targets = append(targets, target{ID: id, CaptionText: t, EngineInput: map[string]any{
			"chunk_id": id, "text": text, "section_titles": sectionTitles,
			"image_captions": json.RawMessage(caps), "figure_captions": json.RawMessage(figs),
		}})
	}
	if err := rows.Err(); err != nil {
		fatal("rows: %v", err)
	}
	fmt.Printf("caption-backfill: %d captioned chunks in active snapshots\n", len(targets))
	if len(targets) == 0 {
		fmt.Println("OK: nothing to backfill")
		return
	}

	// 2. Engine pass (all-or-nothing): recompute every dense vector BEFORE
	// any write, so a failure leaves the corpus untouched (no mixed state).
	// The engine protocol is the FLAT per-chunk shape (chunk_id/text/
	// section_titles/image_captions/figure_captions) — never the Go-side
	// target struct.
	input, _ := json.Marshal(engineInputs(targets))
	vectors, err := runEngine(ctx, input)
	if err != nil {
		fatal("engine: %v", err)
	}
	vecByID := map[string]engineVector{}
	for _, v := range vectors {
		vecByID[v.ChunkID] = v
	}
	if len(vecByID) != len(targets) {
		fatal("engine returned %d vectors for %d targets — refusing partial application", len(vecByID), len(targets))
	}

	// 3. OpenSearch bulk _update FIRST, inside the still-open Postgres
	// transaction: embedding + labeled caption_text. A bulk failure rolls
	// the DB back — no mixed dense state. The only residual window is a PG
	// commit failure AFTER a successful bulk (OS new, PG old); the run is
	// idempotent, so a re-run converges — the fatal below says so explicitly.
	tx, err := database.Pool().Begin(ctx)
	if err != nil {
		fatal("tx: %v", err)
	}
	var buf bytes.Buffer
	for _, t := range targets {
		v := vecByID[t.ID]
		action, _ := json.Marshal(map[string]any{"update": map[string]any{"_index": indexName, "_id": t.ID}})
		doc, _ := json.Marshal(map[string]any{"doc": map[string]any{
			"embedding":    v.Values,
			"caption_text": t.CaptionText,
		}})
		buf.Write(action)
		buf.WriteByte('\n')
		buf.Write(doc)
		buf.WriteByte('\n')
	}
	if err := flushBulk(ctx, osURL, user, pass, buf.Bytes(), len(targets)); err != nil {
		_ = tx.Rollback(ctx)
		fatal("bulk: %v (postgres rolled back — nothing written)", err)
	}

	// 4. Upsert dense vectors in Postgres, then commit (one transaction).
	for _, t := range targets {
		v := vecByID[t.ID]
		vec := make([]string, len(v.Values))
		for i, f := range v.Values {
			vec[i] = strconv.FormatFloat(f, 'g', -1, 64)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO processing_chunk_dense_embeddings (chunk_id, model, dimensions, vector)
			VALUES ($1,$2,$3,$4::vector)
			ON CONFLICT (chunk_id) DO UPDATE
			  SET model = EXCLUDED.model, dimensions = EXCLUDED.dimensions, vector = EXCLUDED.vector`,
			t.ID, v.Model, v.Dimensions, "["+strings.Join(vec, ",")+"]"); err != nil {
			_ = tx.Rollback(ctx)
			fatal("upsert vector %s: %v (opensearch already updated — re-run caption-backfill to converge)", t.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		fatal("commit: %v (opensearch already updated — re-run caption-backfill to converge, it is idempotent)", err)
	}

	fmt.Printf("caption-backfill: %d chunks re-embedded + re-indexed in %s\n",
		len(targets), time.Since(start).Round(time.Second))
	fmt.Println("OK: dense arm now sees caption text; caption_text is source-labeled")
}

// target is one captioned chunk to backfill: the durable id, the labeled
// caption_text for the OS doc, and the flat engine wire input.
type target struct {
	ID          string
	CaptionText string
	EngineInput map[string]any
}

// engineInputs flattens the targets into the engine's wire shape (the
// protocol caption_backfill_cli.py decodes — keep in sync with its build_inputs).
func engineInputs(targets []target) []map[string]any {
	out := make([]map[string]any, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.EngineInput)
	}
	return out
}

// engineVector is one row of the Python engine's stdout plan.
type engineVector struct {
	ChunkID    string    `json:"chunk_id"`
	Model      string    `json:"model"`
	Dimensions int       `json:"dimensions"`
	Values     []float64 `json:"values"`
}

// runEngine pipes the target list through the Python caption-backfill
// engine (the #233 engine-subprocess pattern: hermetic module resolution,
// the runner venv carries the heavy deps).
func runEngine(ctx context.Context, input []byte) ([]engineVector, error) {
	python := os.Getenv("AXIOM_PYTHON")
	runnerDir := os.Getenv("AXIOM_RUNNER_DIR")
	if python == "" {
		python = backfill.FindPython(runnerDir)
	}
	if python == "" {
		return nil, fmt.Errorf("no runner venv python found (set AXIOM_PYTHON)")
	}
	if runnerDir == "" {
		runnerDir = backfill.FindRunnerDir()
	}
	if runnerDir == "" {
		return nil, fmt.Errorf("no axiom_ng_runner checkout found (set AXIOM_RUNNER_DIR)")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, python, "-m", "axiom_ng_runner.compute_core.caption_backfill_cli")
	cmd.Dir = runnerDir
	cmd.Env = append(os.Environ(), "PYTHONPATH="+runnerDir)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, truncate(stderr.String(), 400))
	}
	var out []engineVector
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("decode plan: %w: %s", err, truncate(stdout.String(), 400))
	}
	return out, nil
}

func flushBulk(ctx context.Context, base, user, pass string, body []byte, expected int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/_bulk?refresh=true", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("opensearch %s: %s", res.Status, truncate(string(rb), 400))
	}
	var parsed struct {
		Errors bool `json:"errors"`
		Items  []struct {
			Update struct {
				Error any `json:"error"`
			} `json:"update"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rb, &parsed); err != nil {
		return fmt.Errorf("decode bulk response: %w", err)
	}
	if parsed.Errors || len(parsed.Items) != expected {
		return fmt.Errorf("bulk: errors=%v items=%d want %d: %s", parsed.Errors, len(parsed.Items), expected, truncate(string(rb), 400))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func fatal(f string, args ...any) {
	fmt.Fprintf(os.Stderr, "caption-backfill: "+f+"\n", args...)
	os.Exit(1)
}
