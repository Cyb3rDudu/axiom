// Command locator-backfill enriches the ACTIVE snapshot's chunk locators
// with print pages derived from an enriched EPUB sibling (#233) — no
// conversion, no re-chunking, no embedding, no new snapshot.
//
// Flow (operator one-shot, modeled on meta-backfill/sparse-backfill):
//  1. Resolve the document's active snapshot; FREEZE snapshot_id +
//     content_hash (a changed document aborts honestly — the UPDATE is
//     fenced by the frozen snapshot id inside one transaction).
//  2. Export the active chunks to JSON and run the Python alignment engine
//     (axiom_ng_runner compute_core/locator_backfill_cli.py) under a
//     wall-clock budget. The engine refuses the whole backfill on a
//     non-monotone candidate page map (#226) and refuses per-chunk below
//     the confidence threshold (never guess).
//  3. -dry-run: print the plan, write nothing.
//  4. Real run: ONE transaction per document — jsonb_set page_source=
//     derived_from_sibling plus the print pages (page_start/page_end, and
//     page_label_start/page_label_end for page_span locators because that
//     is what the search LocatorView renders); chapter stays untouched;
//     rows-affected must equal the plan or the tx aborts.
//  5. Re-index affected chunks into OpenSearch via bulk _update on the
//     existing docs (doc id = chunk UUID — NO delete+recreate, ids stay
//     stable). Idempotent: re-running on an unchanged document is a no-op
//     (already-derived chunks are not backfill targets).
//
// Env: AXIOM_DATABASE_URL (required), AXIOM_OPENSEARCH_URL (+ optional
// AXIOM_OPENSEARCH_USERNAME/PASSWORD) required for real runs unless
// -skip-index, AXIOM_RUNNER_PYTHON (default: <repo>/axiom_ng_runner/.venv/
// bin/python). Flags: -doc <zotero_key>, -epub <candidate path> (default:
// auto-discover the document's EPUB attachment, preferring injected ones),
// -dry-run, -budget (default 15m), -skip-index.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

const indexName = "axiom-ng-chunks-v1"

const derivedFromSibling = "derived_from_sibling"

func main() {
	doc := flag.String("doc", "", "document zotero_key (required)")
	epubFlag := flag.String("epub", "", "candidate EPUB path (default: auto-discover)")
	dry := flag.Bool("dry-run", false, "print the plan without writing")
	skipIndex := flag.Bool("skip-index", false, "skip the OpenSearch re-index")
	budget := flag.Duration("budget", 15*time.Minute, "wall-clock budget for the alignment engine")
	dsn := flag.String("dsn", os.Getenv("AXIOM_DATABASE_URL"), "database DSN (default AXIOM_DATABASE_URL)")
	python := flag.String("python", os.Getenv("AXIOM_RUNNER_PYTHON"), "runner venv python (default AXIOM_RUNNER_PYTHON, then repo-relative .venv)")
	runnerDir := flag.String("runner-dir", "", "axiom_ng_runner checkout (default: ./axiom_ng_runner)")
	flag.Parse()
	if *doc == "" || *dsn == "" {
		fmt.Fprintln(os.Stderr, "locator-backfill: -doc and AXIOM_DATABASE_URL/-dsn are required")
		os.Exit(2)
	}

	ctx := context.Background()
	database, err := db.Open(ctx, *dsn)
	if err != nil {
		fatal("connect: %v", err)
	}
	defer database.Close()

	// 1. Active snapshot + attachment (kind + file) — the frozen identity.
	var snapID, attPath, attCT, contentHash, docTitle string
	err = database.Pool().QueryRow(ctx, `
		SELECT sn.id::text, a.local_path, a.content_type, sn.content_hash, d.title
		FROM zotero_documents d
		JOIN zotero_attachments a ON a.id = sn.attachment_id AND NOT a.deleted
		JOIN processing_snapshots sn ON sn.document_id = d.id AND sn.active
		WHERE d.zotero_key = $1`, *doc).Scan(&snapID, &attPath, &attCT, &contentHash, &docTitle)
	if err != nil {
		fatal("active snapshot for %s: %v (no active snapshot?)", *doc, err)
	}
	sourceKind := "epub"
	pdfPath := ""
	if strings.Contains(attCT, "pdf") {
		sourceKind = "pdf"
		pdfPath = attPath
	}
	fmt.Printf("doc %s (%s): active snapshot %s hash %s kind=%s\n", *doc, docTitle, snapID[:8], contentHash[:12], sourceKind)

	// 2. Candidate EPUB: flag, or auto-discover among the document's EPUB
	//    attachments (injected copies preferred — they carry the derived map).
	epub := *epubFlag
	if epub == "" {
		epub, err = discoverCandidate(ctx, database.Pool(), *doc)
		if err != nil {
			fatal("candidate EPUB: %v (pass -epub)", err)
		}
	}
	fmt.Printf("candidate EPUB: %s\n", epub)

	// 3. Chunks of the frozen snapshot.
	rows, err := database.Pool().Query(ctx, `
		SELECT json_build_object('id', c.id::text, 'text', c.text, 'locator', c.locator)
		FROM processing_chunks c
		WHERE c.snapshot_id = $1
		ORDER BY c.chunk_index`, snapID)
	if err != nil {
		fatal("select chunks: %v", err)
	}
	defer rows.Close()
	var chunks []json.RawMessage
	for rows.Next() {
		var c json.RawMessage
		if err := rows.Scan(&c); err != nil {
			fatal("scan chunk: %v", err)
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		fatal("rows: %v", err)
	}
	fmt.Printf("chunks: %d\n", len(chunks))

	// 4. Run the Python alignment engine under the wall-clock budget.
	plan, err := runEngine(ctx, *python, *runnerDir, epub, sourceKind, pdfPath, chunks, *budget)
	if err != nil {
		fatal("alignment engine: %v", err)
	}
	if !plan.Aligned {
		fmt.Printf("REFUSED (whole backfill): %s\n", plan.RefusedReason)
		os.Exit(1)
	}
	fmt.Printf("aligned: %d anchors, %d/%d targets enriched, %d refused\n",
		plan.AnchorCount, plan.PagesEnriched, plan.EnrichmentTargets, plan.PagesRefused)
	for i, r := range plan.Results {
		if r.Refused {
			if i < 20 || (i+1)%50 == 0 { // bounded refusal log
				fmt.Printf("  refused %s: %s\n", shortID(r.ChunkID), r.Reason)
			}
		}
	}
	if *dry {
		for _, r := range plan.Results {
			if r.Enrich {
				fmt.Printf("  would enrich %s -> S. %d-%d (conf %.2f)\n", shortID(r.ChunkID), deref(r.PageStart), deref(r.PageEnd), r.Confidence)
			}
		}
		fmt.Println("dry-run: nothing written")
		return
	}

	// 5. ONE transaction: frozen-snapshot-fenced jsonb_set updates.
	enriched := filterEnriched(plan.Results)
	if len(enriched) == 0 {
		fmt.Println("nothing to enrich (idempotent no-op)")
		return
	}
	tx, err := database.Pool().Begin(ctx)
	if err != nil {
		fatal("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	// Freeze re-check inside the tx: a changed document invalidates the
	// backfill honestly (no blind write onto a superseded snapshot).
	var active string
	if err := tx.QueryRow(ctx, `
		SELECT id::text FROM processing_snapshots
		WHERE document_id = (SELECT document_id FROM processing_snapshots WHERE id=$1)
		  AND active`, snapID).Scan(&active); err != nil || active != snapID {
		fatal("active snapshot changed since the freeze (%v) — aborting without write", err)
	}
	done := 0
	for _, r := range enriched {
		tag, err := tx.Exec(ctx, `
			UPDATE processing_chunks SET locator = jsonb_set(jsonb_set(jsonb_set(
			  CASE WHEN locator->>'type' = 'page_span'
			       THEN jsonb_set(jsonb_set(locator,
			            '{page_label_start}', to_jsonb($2::text)),
			            '{page_label_end}',   to_jsonb($3::text))
			       ELSE locator END,
			  '{page_start}', to_jsonb($2)),
			  '{page_end}',   to_jsonb($3)),
			  '{page_source}', to_jsonb($5))
			WHERE id = $1 AND snapshot_id = $4
			  AND locator->>'page_source' IS DISTINCT FROM $5`,
			r.ChunkID, deref(r.PageStart), deref(r.PageEnd), snapID, derivedFromSibling)
		if err != nil {
			fatal("update %s: %v", r.ChunkID, err)
		}
		done += int(tag.RowsAffected())
		if done%50 == 0 {
			fmt.Printf("  progress: %d/%d chunks enriched\n", done, len(enriched))
		}
	}
	if done != len(enriched) {
		fatal("rows affected %d != planned %d — freezing guard tripped, rolling back", done, len(enriched))
	}
	if err := tx.Commit(ctx); err != nil {
		fatal("commit: %v", err)
	}
	fmt.Printf("enriched %d chunk locators (page_source=%s)\n", done, derivedFromSibling)

	// 6. Re-index: doc-update (no delete+recreate) on the OS chunk docs.
	if *skipIndex {
		fmt.Println("re-index skipped (-skip-index)")
		return
	}
	if err := reindex(ctx, database.Pool(), enriched); err != nil {
		fatal("re-index: %v (DB write committed; re-run with -skip-index=false, the update is idempotent)", err)
	}
	fmt.Println("OK: locators enriched + index docs updated")
}

// --- alignment-engine plan (mirrors locator_backfill_cli.py output) ---

type planResult struct {
	ChunkID    string   `json:"chunk_id"`
	Enrich     bool     `json:"enrich"`
	PageStart  *int     `json:"page_start"`
	PageEnd    *int     `json:"page_end"`
	Source     string   `json:"source"`
	Confidence float64  `json:"confidence"`
	Refused    bool     `json:"refused"`
	Reason     string   `json:"reason"`
}

type plan struct {
	Aligned           bool         `json:"aligned"`
	RefusedReason     string       `json:"refused_reason"`
	AnchorCount       int          `json:"anchor_count"`
	EnrichmentTargets int          `json:"enrichment_targets"`
	PagesEnriched     int          `json:"pages_enriched"`
	PagesRefused      int          `json:"pages_refused"`
	Results           []planResult `json:"results"`
}

func discoverCandidate(ctx context.Context, pool *pgxpool.Pool, doc string) (string, error) {
	rows, err := pool.Query(ctx, `
		SELECT a.local_path, a.filename FROM zotero_attachments a
		JOIN zotero_documents d ON d.id = a.document_id
		WHERE d.zotero_key = $1 AND NOT a.deleted
		  AND a.content_type = 'application/epub+zip'
		  AND a.local_path IS NOT NULL
		ORDER BY (a.filename ILIKE '%injected%') DESC, a.filename`, doc)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p, f string
		if err := rows.Scan(&p, &f); err != nil {
			return "", err
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("no EPUB attachment with a local path")
	}
	// an injected copy (derived page map) always wins; otherwise an
	// unambiguous single EPUB sibling
	for _, p := range paths {
		if strings.Contains(filepath.Base(p), "injected") {
			return p, nil
		}
	}
	if len(paths) == 1 {
		return paths[0], nil
	}
	return "", fmt.Errorf("multiple EPUB siblings, none injected — pick one")
}

func runEngine(ctx context.Context, python, runnerDir, epub, sourceKind, pdfPath string,
	chunks []json.RawMessage, budget time.Duration) (*plan, error) {
	if python == "" {
		python = findPython(runnerDir)
	}
	if runnerDir == "" {
		runnerDir = findRunnerDir()
	}
	tmp, err := os.MkdirTemp("", "locator-backfill-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	chunksFile := filepath.Join(tmp, "chunks.json")
	outFile := filepath.Join(tmp, "plan.json")
	chunkJSON, _ := json.Marshal(chunks)
	if err := os.WriteFile(chunksFile, chunkJSON, 0o600); err != nil {
		return nil, err
	}
	args := []string{"-m", "axiom_ng_runner.compute_core.locator_backfill_cli",
		"--epub", epub, "--source-kind", sourceKind,
		"--chunks", chunksFile, "--out", outFile}
	if sourceKind == "pdf" {
		if pdfPath == "" {
			return nil, fmt.Errorf("pdf snapshot has no local_path")
		}
		args = append(args, "--pdf", pdfPath)
	}
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cmd := exec.CommandContext(cctx, python, args...)
	cmd.Dir = runnerDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("budget exceeded (%s)", budget)
		}
		return nil, fmt.Errorf("%v: %s", err, lastLines(stderr.String(), 5))
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		return nil, fmt.Errorf("engine wrote no plan: %w", err)
	}
	var p plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse plan: %w", err)
	}
	return &p, nil
}

func reindex(ctx context.Context, pool *pgxpool.Pool, enriched []planResult) error {
	osURL := strings.TrimRight(os.Getenv("AXIOM_OPENSEARCH_URL"), "/")
	if osURL == "" {
		return fmt.Errorf("AXIOM_OPENSEARCH_URL not set")
	}
	user, pass := os.Getenv("AXIOM_OPENSEARCH_USERNAME"), os.Getenv("AXIOM_OPENSEARCH_PASSWORD")
	ids := make([]string, len(enriched))
	for i, r := range enriched {
		ids[i] = r.ChunkID
	}
	rows, err := pool.Query(ctx, `
		SELECT c.id::text, c.locator::text FROM processing_chunks c
		WHERE c.id = ANY($1::uuid[])`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	var buf bytes.Buffer
	n := 0
	for rows.Next() {
		var id, loc string
		if err := rows.Scan(&id, &loc); err != nil {
			return err
		}
		action, _ := json.Marshal(map[string]any{"update": map[string]any{"_index": indexName, "_id": id}})
		doc, _ := json.Marshal(map[string]any{"doc": map[string]any{"locator": json.RawMessage(loc)}})
		buf.Write(action)
		buf.WriteByte('\n')
		buf.Write(doc)
		buf.WriteByte('\n')
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n != len(enriched) {
		return fmt.Errorf("read back %d locators for %d enriched chunks", n, len(enriched))
	}
	// partial-update bulk: existing docs (id = chunk UUID) get the new
	// locator — no delete+recreate, index ids stay stable.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, osURL+"/_bulk", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "application/x-ndjson")
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("opensearch bulk: HTTP %d: %.300s", resp.StatusCode, body)
	}
	var receipt struct {
		Errors bool `json:"errors"`
		Items  []struct {
			Update struct {
				Error any `json:"error"`
			} `json:"update"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &receipt); err != nil {
		return fmt.Errorf("decode bulk receipt: %w", err)
	}
	if receipt.Errors || len(receipt.Items) != n {
		return fmt.Errorf("bulk receipt: errors=%v items=%d for %d actions", receipt.Errors, len(receipt.Items), n)
	}
	return nil
}

func filterEnriched(rs []planResult) []planResult {
	var out []planResult
	for _, r := range rs {
		if r.Enrich && r.PageStart != nil {
			out = append(out, r)
		}
	}
	return out
}

func findPython(runnerDir string) string {
	if runnerDir == "" {
		runnerDir = findRunnerDir()
	}
	for _, c := range []string{
		filepath.Join(runnerDir, ".venv", "bin", "python"),
		"axiom_ng_runner/.venv/bin/python",
	} {
		if abs, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	return "python3"
}

func findRunnerDir() string {
	for _, c := range []string{"axiom_ng_runner", "../axiom_ng_runner", "../../axiom_ng_runner"} {
		if abs, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(filepath.Join(abs, "pyproject.toml")); err == nil {
				return abs
			}
		}
	}
	return "."
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "locator-backfill: "+f+"\n", a...)
	os.Exit(1)
}
