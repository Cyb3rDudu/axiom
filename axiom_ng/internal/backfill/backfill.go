// Package backfill implements the #233 locator backfill: enriching the
// ACTIVE snapshot's chunk locators with print pages derived from an
// enriched EPUB sibling — no conversion, no re-chunking, no embedding, no
// new snapshot. The folio is metadata on the chunk, not the chunk.
//
// Flow (one document per Run):
//
//  1. Resolve the document's active snapshot; FREEZE snapshot_id +
//     content_hash (a changed document aborts honestly — the UPDATE is
//     fenced by the frozen snapshot id inside one transaction).
//  2. Export the active chunks to JSON and run the Python alignment engine
//     (axiom_ng_runner compute_core/locator_backfill_cli.py) under a
//     wall-clock budget. The engine refuses the whole backfill on a
//     non-monotone candidate page map (#226) and refuses per-chunk below
//     the confidence threshold (never guess).
//  3. DryRun: return the plan, write nothing.
//  4. Real run: ONE transaction per document — jsonb_set page_source=
//     derived_from_sibling plus the print pages (page_start/page_end, and
//     page_label_start/page_label_end for page_span locators because that
//     is what the search LocatorView renders); chapter stays untouched;
//     rows-affected must equal the plan or the tx aborts.
//  5. Re-index affected chunks into OpenSearch via bulk _update on the
//     existing docs (doc id = chunk UUID — NO delete+recreate, ids stay
//     stable). Idempotent: re-running on an unchanged document is a no-op
//     (already-derived chunks are not backfill targets).
package backfill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IndexName is the shared chunks-index name (search package owns it).
var IndexName = search.IndexName

// DerivedFromSibling is the trust level a backfill may stamp — never
// print_verified (the #233 reservation).
const DerivedFromSibling = "derived_from_sibling"

// Options configures one document's backfill run.
type Options struct {
	DocKey   string // zotero_key of the document (required)
	EpubPath string // candidate EPUB path ("" = auto-discover)
	DryRun   bool
	Budget   time.Duration // wall-clock budget for the engine (default 15m)

	Python    string // runner venv python ("" = discover)
	RunnerDir string // axiom_ng_runner checkout ("" = discover)

	// OpenSearch endpoint; empty OSBaseURL skips the re-index (the DB
	// write is still committed — the update itself is idempotent).
	OSBaseURL, OSUser, OSPass string

	// Logf receives N/M progress lines (nil = discard).
	Logf func(format string, a ...any)

	// ReindexOnly re-indexes every derived_from_sibling chunk of the
	// document's active snapshot without running the engine — the recovery
	// path for a committed backfill whose OpenSearch update failed.
	ReindexOnly bool
}

// PlanResult mirrors one chunk row of the engine's JSON plan.
type PlanResult struct {
	ChunkID    string  `json:"chunk_id"`
	Enrich     bool    `json:"enrich"`
	PageStart  *int    `json:"page_start"`
	PageEnd    *int    `json:"page_end"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence"`
	Refused    bool    `json:"refused"`
	Reason     string  `json:"reason"`
}

// Plan mirrors the engine's JSON output (locator_backfill_cli.py).
type Plan struct {
	Aligned           bool         `json:"aligned"`
	RefusedReason     string       `json:"refused_reason"`
	AnchorCount       int          `json:"anchor_count"`
	EnrichmentTargets int          `json:"enrichment_targets"`
	PagesEnriched     int          `json:"pages_enriched"`
	PagesRefused      int          `json:"pages_refused"`
	Results           []PlanResult `json:"results"`
}

// Report is Run's outcome.
type Report struct {
	SnapshotID, ContentHash, SourceKind string
	Plan                                *Plan
	Updated                             int // chunk locators updated (0 on dry-run)
	Reindexed                           int // OS docs updated
	Refused                             bool
	RefusedReason                       string
}

func (o *Options) logf(format string, a ...any) {
	if o.Logf != nil {
		o.Logf(format, a...)
	}
}

// Run executes one document's locator backfill end to end.
func Run(ctx context.Context, pool *pgxpool.Pool, o Options) (*Report, error) {
	if o.Budget == 0 {
		o.Budget = 15 * time.Minute
	}
	rep := &Report{}

	if o.ReindexOnly {
		return reindexDerived(ctx, pool, o)
	}

	// 1. Active snapshot + attachment (kind + file) — the frozen identity.
	var snapID, attPath, attCT, contentHash string
	err := pool.QueryRow(ctx, `
		SELECT sn.id::text, a.local_path, a.content_type, sn.content_hash
		FROM zotero_documents d
		JOIN zotero_attachments a ON a.document_id = d.id AND NOT a.deleted
		JOIN processing_snapshots sn ON sn.attachment_id = a.id
		 AND sn.document_id = d.id AND sn.active
		WHERE d.zotero_key = $1`, o.DocKey).
		Scan(&snapID, &attPath, &attCT, &contentHash)
	if err != nil {
		return nil, fmt.Errorf("active snapshot for %s: %w", o.DocKey, err)
	}
	rep.SnapshotID, rep.ContentHash = snapID, contentHash
	rep.SourceKind = "epub"
	pdfPath := ""
	if strings.Contains(attCT, "pdf") {
		rep.SourceKind = "pdf"
		pdfPath = attPath
	}
	o.logf("doc %s: active snapshot %s hash %s kind=%s",
		o.DocKey, shortID(snapID), shortID(contentHash), rep.SourceKind)

	// Direction ruling (corrected #233): the backfill enriches EPUB-active
	// snapshots from the enriched EPUB sibling — NEVER a PDF-active one. A
	// PDF's sibling page map is circular (it was DERIVED from that PDF), so
	// enriching PDF chunks would re-import their own unverifiable pagination
	// as derived folios. Refuse the whole run, honestly, write nothing —
	// even if the PDF's chunks carry only physical_only/blind trust.
	if rep.SourceKind == "pdf" {
		rep.Refused = true
		rep.RefusedReason = "active snapshot is a PDF — the backfill direction is EPUB-active only " +
			"(a PDF sibling's page map is circular: derived from that same PDF); nothing written"
		o.logf("refused: %s", rep.RefusedReason)
		return rep, nil
	}

	// 2. Candidate EPUB: explicit, or auto-discover among the document's
	//    EPUB attachments (injected copies preferred — derived page map).
	epub := o.EpubPath
	if epub == "" {
		epub, err = DiscoverCandidate(ctx, pool, o.DocKey)
		if err != nil {
			return nil, fmt.Errorf("candidate EPUB (pass EpubPath): %w", err)
		}
	}
	o.logf("candidate EPUB: %s", epub)

	// 3. Chunks of the frozen snapshot.
	rows, err := pool.Query(ctx, `
		SELECT json_build_object('id', c.id::text, 'text', c.text, 'locator', c.locator)
		FROM processing_chunks c
		WHERE c.snapshot_id = $1
		ORDER BY c.chunk_index`, snapID)
	if err != nil {
		return nil, fmt.Errorf("select chunks: %w", err)
	}
	defer rows.Close()
	var chunks []json.RawMessage
	for rows.Next() {
		var c json.RawMessage
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("scan chunk: %w", err)
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	o.logf("chunks: %d", len(chunks))

	// 4. Run the Python alignment engine under the wall-clock budget.
	plan, err := RunEngine(ctx, o.Python, o.RunnerDir, epub, rep.SourceKind, pdfPath, chunks, o.Budget)
	if err != nil {
		return nil, fmt.Errorf("alignment engine: %w", err)
	}
	rep.Plan = plan
	if !plan.Aligned {
		rep.Refused, rep.RefusedReason = true, plan.RefusedReason
		return rep, nil
	}
	o.logf("aligned: %d anchors, %d/%d targets enriched, %d refused",
		plan.AnchorCount, plan.PagesEnriched, plan.EnrichmentTargets, plan.PagesRefused)
	if o.DryRun {
		return rep, nil
	}

	// 5. ONE transaction: frozen-snapshot-fenced jsonb_set updates.
	enriched := filterEnriched(plan.Results)
	if len(enriched) == 0 {
		o.logf("nothing to enrich (idempotent no-op)")
		return rep, nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	// Freeze re-check inside the tx: a changed document invalidates the
	// backfill honestly (no blind write onto a superseded snapshot).
	var active string
	if err := tx.QueryRow(ctx, `
		SELECT id::text FROM processing_snapshots
		WHERE document_id = (SELECT document_id FROM processing_snapshots WHERE id=$1)
		  AND active`, snapID).Scan(&active); err != nil || active != snapID {
		return nil, fmt.Errorf(
			"active snapshot changed since the freeze (%v) — aborting without write", err)
	}
	done := 0
	for _, r := range enriched {
		tag, err := tx.Exec(ctx, `
			UPDATE processing_chunks SET locator = jsonb_set(jsonb_set(jsonb_set(
			  CASE WHEN locator->>'type' = 'page_span'
			       THEN jsonb_set(jsonb_set(locator,
			            '{page_label_start}', to_jsonb($6::text)),
			            '{page_label_end}',   to_jsonb($7::text))
			       ELSE locator END,
			  '{page_start}', to_jsonb($2::int)),
			  '{page_end}',   to_jsonb($3::int)),
			  '{page_source}', to_jsonb($5::text))
			WHERE id = $1 AND snapshot_id = $4
			  AND locator->>'type' = 'epub_cfi'
			  AND locator->>'page_source' IN ('none','')`,
			r.ChunkID, deref(r.PageStart), deref(r.PageEnd), snapID, DerivedFromSibling,
			fmt.Sprint(deref(r.PageStart)), fmt.Sprint(deref(r.PageEnd)))
		if err != nil {
			return nil, fmt.Errorf("update %s: %w", r.ChunkID, err)
		}
		done += int(tag.RowsAffected())
		if done%50 == 0 {
			o.logf("progress: %d/%d chunks enriched", done, len(enriched))
		}
	}
	if done != len(enriched) {
		return nil, fmt.Errorf(
			"rows affected %d != planned %d — freeze guard tripped, rolled back", done, len(enriched))
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	rep.Updated = done
	o.logf("enriched %d chunk locators (page_source=%s)", done, DerivedFromSibling)

	// 6. Re-index: doc-update (no delete+recreate) on the OS chunk docs.
	if o.OSBaseURL == "" {
		o.logf("re-index skipped (no OpenSearch endpoint)")
		return rep, nil
	}
	n, err := Reindex(ctx, pool, o.OSBaseURL, o.OSUser, o.OSPass, enriched)
	if err != nil {
		return rep, fmt.Errorf(
			"re-index: %w (DB write committed; re-run — the update is idempotent)", err)
	}
	rep.Reindexed = n
	return rep, nil
}

// DiscoverCandidate picks the document's EPUB sibling: an injected copy
// (carrying the derived page map) always wins, otherwise a single
// unambiguous sibling; multiple non-injected siblings are ambiguous.
func DiscoverCandidate(ctx context.Context, pool *pgxpool.Pool, doc string) (string, error) {
	rows, err := pool.Query(ctx, `
		SELECT a.local_path FROM zotero_attachments a
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
		var p string
		if err := rows.Scan(&p); err != nil {
			return "", err
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("no EPUB attachment with a local path")
	}
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

// RunEngine invokes the Python alignment engine (locator_backfill_cli.py)
// over the chunks and returns its plan. The wall-clock budget bounds the
// subprocess; a non-aligned plan is returned, not an error (refusal is an
// honest outcome).
func RunEngine(ctx context.Context, python, runnerDir, epub, sourceKind, pdfPath string,
	chunks []json.RawMessage, budget time.Duration) (*Plan, error) {
	if python == "" {
		python = FindPython(runnerDir)
	}
	if runnerDir == "" {
		runnerDir = FindRunnerDir()
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
	// HERMETIC module resolution (#233 review): `python -m axiom_ng_runner.…`
	// must import the STRAND's package, never whatever checkout the runner
	// venv happens to have editable-installed. cwd = the repo root (parent of
	// runnerDir) puts the strand package at sys.path[0], which strictly beats
	// any site-packages/editable finder; PYTHONPATH is belt-and-braces for
	// the same root. (cmd.Dir = runnerDir itself is NOT importable — the
	// package dir lies one level up.)
	repoRoot := filepath.Dir(runnerDir)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "PYTHONPATH="+repoRoot)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("budget exceeded (%s)", budget)
		}
		// exit status 1 = honest whole-backfill refusal (plan on stdout?
		// no: the CLI writes the plan to --out even when refusing)
		if raw, rerr := os.ReadFile(outFile); rerr == nil {
			var p Plan
			if jerr := json.Unmarshal(raw, &p); jerr == nil && !p.Aligned {
				return &p, nil
			}
		}
		return nil, fmt.Errorf("%v: %s", err, lastLines(stderr.String(), 5))
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		return nil, fmt.Errorf("engine wrote no plan: %w", err)
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse plan: %w", err)
	}
	return &p, nil
}

// Reindex bulk-updates the enriched chunks' OpenSearch docs with their new
// locator (partial _update on the existing doc, id = chunk UUID — index ids
// stay stable; no delete+recreate).
func Reindex(ctx context.Context, pool *pgxpool.Pool, base, user, pass string,
	enriched []PlanResult) (int, error) {
	base = strings.TrimRight(base, "/")
	ids := make([]string, len(enriched))
	for i, r := range enriched {
		ids[i] = r.ChunkID
	}
	rows, err := pool.Query(ctx, `
		SELECT c.id::text, c.locator::text FROM processing_chunks c
		WHERE c.id = ANY($1::uuid[])`, ids)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var buf bytes.Buffer
	n := 0
	for rows.Next() {
		var id, loc string
		if err := rows.Scan(&id, &loc); err != nil {
			return 0, err
		}
		action, _ := json.Marshal(map[string]any{"update": map[string]any{"_index": IndexName, "_id": id}})
		doc, _ := json.Marshal(map[string]any{"doc": map[string]any{"locator": json.RawMessage(loc)}})
		buf.Write(action)
		buf.WriteByte('\n')
		buf.Write(doc)
		buf.WriteByte('\n')
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if n != len(enriched) {
		return n, fmt.Errorf("read back %d locators for %d enriched chunks", n, len(enriched))
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/_bulk", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "application/x-ndjson")
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return n, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return n, fmt.Errorf("opensearch bulk: HTTP %d: %.300s", resp.StatusCode, body)
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
		return n, fmt.Errorf("decode bulk receipt: %w", err)
	}
	if receipt.Errors || len(receipt.Items) != n {
		return n, fmt.Errorf("bulk receipt: errors=%v items=%d for %d actions",
			receipt.Errors, len(receipt.Items), n)
	}
	return n, nil
}

func filterEnriched(rs []PlanResult) []PlanResult {
	var out []PlanResult
	for _, r := range rs {
		if r.Enrich && r.PageStart != nil && r.PageEnd != nil {
			out = append(out, r)
		}
	}
	return out
}

// FindPython locates the runner venv python (AXIOM_RUNNER_PYTHON-checked by
// the caller, then runnerDir/.venv, then repo-relative).
func FindPython(runnerDir string) string {
	if runnerDir == "" {
		runnerDir = FindRunnerDir()
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

// FindRunnerDir locates the axiom_ng_runner checkout relative to the CWD.
// Callers may sit THREE levels below the repo root (e.g. a test binary in
// internal/backfill: backfill -> internal -> axiom_ng -> root), so the
// candidate list reaches that far; every candidate is existence-checked
// (pyproject.toml) — a nonexistent dir is never returned.
func FindRunnerDir() string {
	for _, c := range []string{
		"axiom_ng_runner",
		"../axiom_ng_runner",
		"../../axiom_ng_runner",
		"../../../axiom_ng_runner",
	} {
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

// reindexDerived is the ReindexOnly recovery path: it re-indexes every
// derived_from_sibling chunk of the document's active snapshot (bulk
// _update, stable ids) without running the engine. Covers a committed
// backfill whose OpenSearch step failed — a plain re-run has zero targets
// and would skip the index.
func reindexDerived(ctx context.Context, pool *pgxpool.Pool, o Options) (*Report, error) {
	if o.OSBaseURL == "" {
		return nil, fmt.Errorf("ReindexOnly requires an OpenSearch endpoint")
	}
	rows, err := pool.Query(ctx, `
		SELECT c.id::text FROM processing_chunks c
		JOIN processing_snapshots sn ON sn.id = c.snapshot_id AND sn.active
		JOIN zotero_documents d ON d.id = sn.document_id
		WHERE d.zotero_key = $1
		  AND c.locator->>'page_source' = $2`, o.DocKey, DerivedFromSibling)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var enriched []PlanResult
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		enriched = append(enriched, PlanResult{ChunkID: id, Enrich: true,
			PageStart: new(int), PageEnd: new(int), Source: DerivedFromSibling})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	o.logf("reindex-only: %d derived chunks", len(enriched))
	if len(enriched) == 0 {
		return &Report{Updated: 0, Reindexed: 0}, nil
	}
	n, err := Reindex(ctx, pool, o.OSBaseURL, o.OSUser, o.OSPass, enriched)
	if err != nil {
		return nil, err
	}
	return &Report{Reindexed: n}, nil
}
