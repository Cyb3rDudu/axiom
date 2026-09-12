// Command figcap-backfill re-runs the DOCUMENT figure-caption extraction
// (#268) over the chunks of ACTIVE snapshots and repairs the stored
// figure_captions column in place: it ADDS caption forms the old pattern
// missed (decorated lines, ranges, roman numerals, abbreviations without a
// space) and PURGES the prose false positives the old pattern paired
// ("Abb. 6.4 zeigt …" is a verb continuation, not a caption). Chunks whose
// captions actually change are re-embedded and re-indexed per the #257
// caption-augmentation contract (dense vector from text + labeled captions;
// OpenSearch embedding + caption_text).
//
// One-shot operational tool in the #233 pattern. No re-ingest, no
// re-chunking, no re-conversion, no re-captioning — the fix is a pure
// extraction pass over text that is already stored. Idempotent: a second run
// reports 0 changes and writes nothing. The plan is all-or-nothing: the
// Python engine (axiom_ng_runner compute_core/figcap_backfill_cli.py, real
// BGE-M3) computes every new caption and vector BEFORE a single write, so a
// failure leaves the corpus untouched.
//
// Env: AXIOM_DATABASE_URL, AXIOM_OPENSEARCH_URL (both required unless
// -dry-run without -index); AXIOM_OPENSEARCH_USERNAME/PASSWORD optional;
// AXIOM_PYTHON / AXIOM_RUNNER_DIR override the runner venv / checkout
// discovery.
//
// Usage:
//
//	figcap-backfill -dry-run              # per-document would-add/would-purge
//	figcap-backfill -dry-run -doc <key>   # focus on one document
//	figcap-backfill                       # apply: DB + OpenSearch
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
	"sort"
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
	dry := flag.Bool("dry-run", false, "print the per-document plan without writing")
	docKey := flag.String("doc", "", "limit to one document zotero_key")
	limit := flag.Int("limit", 0, "limit the number of chunks processed (0 = all)")
	noIndex := flag.Bool("no-index", false, "apply to Postgres but skip the OpenSearch re-index")
	flag.Parse()

	dsn := os.Getenv("AXIOM_DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "AXIOM_DATABASE_URL is required")
		os.Exit(2)
	}
	database, err := db.Open(ctx, dsn)
	if err != nil {
		fatal("postgres: %v", err)
	}
	defer database.Close()

	// 1. Collect every chunk of an ACTIVE snapshot that references an image
	// (only those can carry a document figure caption).
	rows, err := database.Pool().Query(ctx, `
		SELECT c.id::text, c.text, c.section_titles::text, c.image_refs::text,
		       c.image_captions::text, c.figure_captions::text,
		       sn.document_id::text, COALESCE(d.zotero_key, ''), COALESCE(d.title, '')
		FROM processing_chunks c
		JOIN processing_snapshots sn ON sn.id = c.snapshot_id AND sn.active
		LEFT JOIN zotero_documents d ON d.id = sn.document_id
		WHERE c.image_refs <> '[]'::jsonb
		  AND ($1 = '' OR d.zotero_key = $1)
		ORDER BY sn.document_id, c.chunk_index`, *docKey)
	if err != nil {
		fatal("select: %v", err)
	}
	var targets []target
	for rows.Next() {
		var r target
		var secs, caps, figs string
		if err := rows.Scan(
			&r.ID, &r.Text, &secs, &r.ImageRefs, &caps, &figs,
			&r.DocID, &r.DocKey, &r.DocTitle,
		); err != nil {
			fatal("scan: %v", err)
		}
		r.OldFigRaw = figs
		r.ImageCapsRaw = caps
		_ = json.Unmarshal([]byte(secs), &r.SectionTitles)
		r.engineInput = map[string]any{
			"chunk_id":        r.ID,
			"text":            r.Text,
			"section_titles":  r.SectionTitles,
			"image_refs":      json.RawMessage(r.ImageRefs),
			"image_captions":  json.RawMessage(caps),
			"figure_captions": json.RawMessage(figs),
		}
		targets = append(targets, r)
	}
	if err := rows.Err(); err != nil {
		fatal("rows: %v", err)
	}
	if *limit > 0 && len(targets) > *limit {
		targets = targets[:*limit]
	}
	fmt.Printf("figcap-backfill: %d image-referencing chunks in active snapshots\n", len(targets))

	// 2. Engine pass (all-or-nothing). Dry-run requests planning only (no
	// model); apply computes new captions AND the new vectors up front.
	input, _ := json.Marshal(engineInputs(targets))
	results, err := runEngine(ctx, input, *dry)
	if err != nil {
		fatal("engine: %v", err)
	}
	resByID := map[string]engineRow{}
	for _, r := range results {
		if _, dup := resByID[r.ChunkID]; dup {
			fatal("engine returned duplicate row for chunk %s — refusing", r.ChunkID)
		}
		resByID[r.ChunkID] = r
	}
	if len(resByID) != len(targets) {
		fatal("engine returned %d rows for %d targets — refusing partial application", len(resByID), len(targets))
	}
	for _, t := range targets {
		r, ok := resByID[t.ID]
		if !ok {
			fatal("engine returned no row for chunk %s — refusing partial application", t.ID)
		}
		if !*dry && r.Changed {
			if r.Model == "" || r.Dimensions <= 0 || len(r.Values) == 0 || r.Dimensions != len(r.Values) {
				fatal("engine returned unusable vector for chunk %s (model=%q dims=%d values=%d) — refusing", t.ID, r.Model, r.Dimensions, len(r.Values))
			}
		}
	}

	// 3. Per-document report (both modes; the dry-run is the proof artefact).
	report := summarize(targets, resByID)
	changed := 0
	for _, t := range targets {
		if resByID[t.ID].Changed {
			changed++
		}
	}
	fmt.Printf("plan: %d/%d chunks change (%d captions added, %d false positives purged)\n",
		changed, len(targets), report.added, report.purged)
	for _, d := range report.docs {
		fmt.Printf("  %-28s add=%d purge=%d chunks=%d  %s\n",
			shortKey(d.Key), d.Added, d.Purged, d.Changed, truncate(d.Title, 50))
	}
	if *dry {
		fmt.Printf("dry-run: nothing written. Re-run without -dry-run to apply.\n")
		return
	}
	if changed == 0 {
		fmt.Println("OK: nothing to change (idempotent re-run)")
		return
	}

	// 4. Apply. OpenSearch bulk FIRST inside the open Postgres transaction
	// (embedding + labeled caption_text), then the DB writes, then commit —
	// same ordering as caption-backfill. A bulk failure rolls the DB back;
	// the run is idempotent so a re-run converges the residual window.
	osURL := ""
	if !*noIndex {
		osURL = strings.TrimRight(os.Getenv("AXIOM_OPENSEARCH_URL"), "/")
		if osURL == "" {
			fatal("AXIOM_OPENSEARCH_URL is required for the applied run (-no-index to skip)")
		}
	}
	tx, err := database.Pool().Begin(ctx)
	if err != nil {
		fatal("tx: %v", err)
	}
	if osURL != "" {
		var buf bytes.Buffer
		n := 0
		for _, t := range targets {
			r := resByID[t.ID]
			if !r.Changed {
				continue
			}
			captionText := repo.LabeledCaptionText(&t.ImageCapsRaw, strptr(mustJSON(r.FigureCaptions)))
			action, _ := json.Marshal(map[string]any{"update": map[string]any{"_index": indexName, "_id": t.ID}})
			doc, _ := json.Marshal(map[string]any{"doc": map[string]any{
				"embedding":    r.Values,
				"caption_text": captionText,
			}})
			buf.Write(action)
			buf.WriteByte('\n')
			buf.Write(doc)
			buf.WriteByte('\n')
			n++
		}
		if err := flushBulk(ctx, osURL, os.Getenv("AXIOM_OPENSEARCH_USERNAME"), os.Getenv("AXIOM_OPENSEARCH_PASSWORD"), buf.Bytes(), n); err != nil {
			_ = tx.Rollback(ctx)
			fatal("bulk: %v (postgres rolled back; OpenSearch may be partially updated — re-run figcap-backfill to converge, it is idempotent)", err)
		}
	}
	for _, t := range targets {
		r := resByID[t.ID]
		if !r.Changed {
			continue
		}
		figJSON, _ := json.Marshal(r.FigureCaptions)
		if _, err := tx.Exec(ctx, `UPDATE processing_chunks SET figure_captions = $2::jsonb WHERE id = $1`, t.ID, string(figJSON)); err != nil {
			_ = tx.Rollback(ctx)
			fatal("update figure_captions %s: %v (opensearch already updated — re-run to converge)", t.ID, err)
		}
		vec := make([]string, len(r.Values))
		for i, f := range r.Values {
			vec[i] = strconv.FormatFloat(f, 'g', -1, 64)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO processing_chunk_dense_embeddings (chunk_id, model, dimensions, vector)
			VALUES ($1,$2,$3,$4::vector)
			ON CONFLICT (chunk_id) DO UPDATE
			  SET model = EXCLUDED.model, dimensions = EXCLUDED.dimensions, vector = EXCLUDED.vector`,
			t.ID, r.Model, r.Dimensions, "["+strings.Join(vec, ",")+"]"); err != nil {
			_ = tx.Rollback(ctx)
			fatal("upsert vector %s: %v (opensearch already updated — re-run to converge)", t.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		fatal("commit: %v (opensearch already updated — re-run to converge, it is idempotent)", err)
	}

	fmt.Printf("figcap-backfill: %d chunks repaired + re-embedded + re-indexed in %s\n",
		changed, time.Since(start).Round(time.Second))
	fmt.Println("OK: figure_captions now hold only true document captions")
}

// target is one image-referencing chunk of an active snapshot plus the
// document identity for the per-document report.
type target struct {
	ID            string
	Text          string
	SectionTitles []string
	ImageRefs     string
	ImageCapsRaw  string
	OldFigRaw     string
	DocID         string
	DocKey        string
	DocTitle      string
	engineInput   map[string]any
}

// engineRow is one row of the Python engine's stdout plan.
type engineRow struct {
	ChunkID        string            `json:"chunk_id"`
	FigureCaptions map[string]string `json:"figure_captions"`
	Changed        bool              `json:"changed"`
	Model          string            `json:"model"`
	Dimensions     int               `json:"dimensions"`
	Values         []float64         `json:"values"`
}

// docReport is the per-document part of the plan proof.
type docReport struct {
	Key     string
	Title   string
	Changed int
	Added   int
	Purged  int
}

type report struct {
	docs   []docReport
	added  int
	purged int
}

// summarize builds the per-document would-add/would-purge report. A caption
// VALUE that is new or changed is an add; an old caption VALUE that is
// dropped or replaced is a purge — a verb-continuation replaced by the real
// caption counts as one purge and one add (the false positive leaves and the
// true caption arrives).
func summarize(targets []target, res map[string]engineRow) report {
	byDoc := map[string]*docReport{}
	var order []string
	for _, t := range targets {
		d, ok := byDoc[t.DocID]
		if !ok {
			d = &docReport{Key: t.DocKey, Title: t.DocTitle}
			byDoc[t.DocID] = d
			order = append(order, t.DocID)
		}
		r := res[t.ID]
		if !r.Changed {
			continue
		}
		d.Changed++
		var old map[string]string
		_ = json.Unmarshal([]byte(t.OldFigRaw), &old)
		if old == nil {
			old = map[string]string{}
		}
		for ref, text := range r.FigureCaptions {
			if prev, ok := old[ref]; !ok || prev != text {
				d.Added++
			}
		}
		for ref, prev := range old {
			if text, ok := r.FigureCaptions[ref]; !ok || text != prev {
				d.Purged++
			}
		}
	}
	rep := report{}
	for _, id := range order {
		d := byDoc[id]
		if d.Changed == 0 {
			continue
		}
		rep.docs = append(rep.docs, *d)
		rep.added += d.Added
		rep.purged += d.Purged
	}
	sort.SliceStable(rep.docs, func(i, j int) bool { return rep.docs[i].Key < rep.docs[j].Key })
	return rep
}

func engineInputs(targets []target) []map[string]any {
	out := make([]map[string]any, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.engineInput)
	}
	return out
}

// runEngine pipes the target list through the Python figcap-backfill engine
// (the #233 engine-subprocess pattern: hermetic module resolution, the
// runner venv carries the heavy deps). planOnly skips the embedder.
func runEngine(ctx context.Context, input []byte, planOnly bool) ([]engineRow, error) {
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
	cctx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()
	args := []string{"-m", "axiom_ng_runner.compute_core.figcap_backfill_cli"}
	if planOnly {
		args = append(args, "--plan-only")
	}
	cmd := exec.CommandContext(cctx, python, args...)
	cmd.Dir = runnerDir
	// The package is axiom_ng_runner (the runner checkout IS the package
	// dir), so its PARENT must be importable; prepend it so a worktree
	// checkout wins over a site-packages install.
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Dir(runnerDir)+string(os.PathListSeparator)+runnerDir)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, truncate(stderr.String(), 400))
	}
	var out []engineRow
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
				ID    string `json:"_id"`
				Error *struct {
					Type   string `json:"type"`
					Reason string `json:"reason"`
				} `json:"error"`
			} `json:"update"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rb, &parsed); err != nil {
		return fmt.Errorf("decode bulk response: %w", err)
	}
	if parsed.Errors || len(parsed.Items) != expected {
		var failed []string
		for _, it := range parsed.Items {
			if it.Update.Error != nil {
				failed = append(failed, it.Update.ID)
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("bulk item failures (%d): %s", len(failed), strings.Join(failed, ","))
		}
		return fmt.Errorf("bulk: errors=%v items=%d want %d", parsed.Errors, len(parsed.Items), expected)
	}
	return nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		return "{}"
	}
	return string(b)
}

func strptr(s string) *string { return &s }

func shortKey(k string) string {
	if k == "" {
		return "(unknown)"
	}
	if len(k) > 12 {
		return k[:8]
	}
	return k
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func fatal(f string, args ...any) {
	fmt.Fprintf(os.Stderr, "figcap-backfill: "+f+"\n", args...)
	os.Exit(1)
}
