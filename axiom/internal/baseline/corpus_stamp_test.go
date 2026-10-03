// corpus_stamp_test.go — the live-suite corpus-drift guard (#347).
//
// Before ANY value assertion runs, the live golden suite asserts three
// corpus stamps against the frozen baseline state (diagnosis 2026-09-23:
// corpus counts alone do not capture the KG read-model generation, and
// the search goldens exercise the real OpenSearch index):
//
//	db — exact zotero_documents / processing_snapshots counts.
//	     ingest_jobs is deliberately NOT stamped: the suite's own ingest
//	     probe writes job rows (the non-gating inventory tracks them).
//	kg  — kg_entity_roots count + mention sum: a stable fingerprint of
//	     the materialized roots /api/kg/entities serves from.
//	os  — exact doc count of the EFFECTIVE index: freeze bits predate
//	     AXIOM_OS_INDEX and hardcode the legacy name; since the #352
//	     rename the corpus lives under the canonical index (byte-
//	     preserving _reindex, count parity); 0.2.0-topology runs
//	     (AXIOM_BASELINE_EXPECT_BUILD) honor AXIOM_OS_INDEX.
//
// Single source: the corpus-stamp stanza in the header of
// fixtures/live_row_counts_freeze_day.txt — values carried ONCE there,
// read by the guard, never hardcoded here. A deliberate baseline
// refresh updates the stanza (and only the stanza); refreshes must
// track PRODUCTION corpus freezes, never dev-mirror drift (#347).
//
// Wiring: TestMain runs the precheck once before the whole suite when
// AXIOM_BASELINE_LIVE=1 — no live test can start against a drifted
// corpus. Derived/fingerprint/CI runs (no LIVE env) pass through
// untouched. The F04 gate (2026-09-23) hit a value-red that was really
// a re-synced dev mirror (48 newer documents silently shifting KG
// rankings); the loud early refusal is what removes that diagnosis cost.
package baseline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
)

// defaultOSIndex mirrors search.IndexName's frozen default (the index
// name the freeze bits hardcode — they predate the AXIOM_OS_INDEX knob).
const defaultOSIndex = "axiom-chunks-v1"

// corpusStamps is the parsed stamp stanza: stamp name -> key -> value.
type corpusStamps map[string]map[string]int64

// stampKeys names every (stamp, key) the guard asserts — the parse
// refuses a stanza that lacks any of them (a narrowed stamp must fail,
// not silently pass).
var stampKeys = [][2]string{
	{"db", "zotero_documents"},
	{"db", "processing_snapshots"},
	{"kg", "kg_entity_roots"},
	{"kg", "kg_root_mentions"},
	{"os", "index_docs"},
}

// parseCorpusStamps extracts the `# corpus-stamp:<name>: k=v k=v` lines
// from the freeze-day artifact header. Machine lines start with
// "# corpus-stamp:" — the stanza is the single source; anything unreadable
// or incomplete is an error.
func parseCorpusStamps(artifact string) (corpusStamps, error) {
	out := corpusStamps{}
	for _, line := range strings.Split(artifact, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "# corpus-stamp:") {
			continue
		}
		rest := strings.TrimPrefix(line, "# corpus-stamp:")
		parts := strings.SplitN(rest, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("corpus stamp: malformed stanza line %q", line)
		}
		name := strings.TrimSpace(parts[0])
		if _, ok := out[name]; ok {
			return nil, fmt.Errorf("corpus stamp: duplicate stanza %q", name)
		}
		kv := map[string]int64{}
		for _, field := range strings.Fields(parts[1]) {
			k, v, ok := strings.Cut(field, "=")
			if !ok {
				return nil, fmt.Errorf("corpus stamp: malformed field %q in stanza %q", field, name)
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("corpus stamp: non-numeric %q in stanza %q", field, name)
			}
			kv[k] = n
		}
		out[name] = kv
	}
	for _, sk := range stampKeys {
		if _, ok := out[sk[0]][sk[1]]; !ok {
			return nil, fmt.Errorf("corpus stamp: stanza %q lacks key %q", sk[0], sk[1])
		}
	}
	return out, nil
}

// corpusStampDrift compares recorded stamps against live observations
// and returns the loud drift diagnosis, or nil when everything matches
// the frozen state. Pure — the unit tests stub both sides (#347
// acceptance). observations uses the same (stamp, key) naming.
func corpusStampDrift(recorded corpusStamps, live corpusStamps) error {
	var drift []string
	for _, sk := range stampKeys {
		stamp, key := sk[0], sk[1]
		r := recorded[stamp][key]
		l, ok := live[stamp][key]
		if !ok {
			return fmt.Errorf("corpus stamp: live %s/%s unreadable", stamp, key)
		}
		if r != l {
			drift = append(drift, fmt.Sprintf("%s/%s: frozen %d, live %d (delta %+d)", stamp, key, r, l, l-r))
		}
	}
	if len(drift) == 0 {
		return nil
	}
	return fmt.Errorf(
		"corpus drift — the mirror is not at the frozen baseline state, value assertions would mislead:\n  %s\n"+
			"  restore the mirror from the frozen snapshot, or perform a deliberate baseline refresh\n"+
			"  (tracks PRODUCTION corpus freezes, never dev-mirror drift — #347)",
		strings.Join(drift, "\n  "))
}

// effectiveOSIndex resolves the index the live searches will actually
// hit: freeze bits hardcode the default; 0.2.0-topology runs
// (AXIOM_BASELINE_EXPECT_BUILD) honor AXIOM_OS_INDEX.
func effectiveOSIndex() string {
	if os.Getenv("AXIOM_BASELINE_EXPECT_BUILD") != "" {
		if n := strings.TrimSpace(os.Getenv("AXIOM_OS_INDEX")); n != "" {
			return n
		}
	}
	return defaultOSIndex
}

// liveCorpusStamps observes the three stamps against the live database
// and the effective OpenSearch index.
func liveCorpusStamps(ctx context.Context, d *db.DB, osURL string) (corpusStamps, error) {
	out := corpusStamps{
		"db": {},
		"kg": {},
		"os": {},
	}
	count := func(table string) (int64, error) {
		// DDL identifier interpolated from a stampKeys-derived literal —
		// no external input.
		var n int64
		if err := d.Pool().QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", table)).Scan(&n); err != nil {
			return 0, err
		}
		return n, nil
	}
	var err error
	if out["db"]["zotero_documents"], err = count("zotero_documents"); err != nil {
		return nil, fmt.Errorf("count zotero_documents: %w", err)
	}
	if out["db"]["processing_snapshots"], err = count("processing_snapshots"); err != nil {
		return nil, fmt.Errorf("count processing_snapshots: %w", err)
	}
	var roots, mentions int64
	if err := d.Pool().QueryRow(ctx,
		`SELECT count(*), coalesce(sum(mention_count),0) FROM kg_entity_roots`,
	).Scan(&roots, &mentions); err != nil {
		return nil, fmt.Errorf("kg roots fingerprint: %w", err)
	}
	out["kg"]["kg_entity_roots"] = roots
	out["kg"]["kg_root_mentions"] = mentions
	code, err := probeOSCount(osURL, effectiveOSIndex())
	if err != nil {
		return nil, err
	}
	out["os"]["index_docs"] = code
	return out, nil
}

// probeOSCount reads the exact document count of one index.
func probeOSCount(osURL, index string) (int64, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(strings.TrimRight(osURL, "/") + "/" + index + "/_count")
	if err != nil {
		return 0, fmt.Errorf("opensearch %s count: %w", index, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("opensearch %s count: HTTP %d: %s", index, resp.StatusCode, string(raw[:min(len(raw), 200)]))
	}
	var body struct {
		Count int64 `json:"count"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0, fmt.Errorf("opensearch %s count: bad json: %w", index, err)
	}
	return body.Count, nil
}

// precheckCorpusStamp is the live-suite gate: artifact stanza vs live
// db + kg + effective index.
func precheckCorpusStamp() error {
	raw, err := os.ReadFile("fixtures/live_row_counts_freeze_day.txt")
	if err != nil {
		return fmt.Errorf("corpus stamp: freeze-day artifact unreadable: %w", err)
	}
	recorded, err := parseCorpusStamps(string(raw))
	if err != nil {
		return err
	}
	dsn := fingerprintDSN()
	if dsn == "" {
		return fmt.Errorf("corpus stamp: live mode without AXIOM_DATABASE_URL (source scripts/dev/env.sh)")
	}
	osURL := os.Getenv("AXIOM_OPENSEARCH_URL")
	if osURL == "" {
		osURL = "http://127.0.0.1:9200"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := db.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("corpus stamp: open live db: %w", err)
	}
	defer conn.Close()
	live, err := liveCorpusStamps(ctx, conn, osURL)
	if err != nil {
		return err
	}
	return corpusStampDrift(recorded, live)
}

// TestMain — the precheck is the FIRST live-suite step (#347): with
// AXIOM_BASELINE_LIVE=1 the whole run refuses to start on corpus drift,
// before any value assertion can produce a misleading red.
func TestMain(m *testing.M) {
	if os.Getenv("AXIOM_BASELINE_LIVE") == "1" {
		if err := precheckCorpusStamp(); err != nil {
			fmt.Fprintf(os.Stderr, "golden suite REFUSED: %v\n", err)
			os.Exit(2)
		}
	}
	os.Exit(m.Run())
}

// TestCorpusStampParse — the artifact contract: the full stanza parses
// from the committed freeze-day file (guards against a stripped,
// reformatted or silently narrowed stanza).
func TestCorpusStampParse(t *testing.T) {
	raw, err := os.ReadFile("fixtures/live_row_counts_freeze_day.txt")
	if err != nil {
		t.Fatal(err)
	}
	stamps, err := parseCorpusStamps(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, sk := range stampKeys {
		if stamps[sk[0]][sk[1]] <= 0 {
			t.Fatalf("unexpected %s/%s in committed stanza: %+v", sk[0], sk[1], stamps)
		}
	}
	if stamps["db"]["zotero_documents"] != 369 || stamps["db"]["processing_snapshots"] != 361 {
		t.Fatalf("db stamp changed: %+v", stamps["db"])
	}
}

// TestCorpusStampMismatchIsLoud — a stubbed drift in ANY stamp produces
// the loud EARLY failure naming corpus drift (not value divergence) and
// both values, with the remedy (#347 acceptance).
func TestCorpusStampMismatchIsLoud(t *testing.T) {
	recorded := corpusStamps{
		"db": {"zotero_documents": 369, "processing_snapshots": 361},
		"kg": {"kg_entity_roots": 142175, "kg_root_mentions": 320399},
		"os": {"index_docs": 57724},
	}
	cases := []corpusStamps{
		{"db": {"zotero_documents": 417, "processing_snapshots": 361},
			"kg": {"kg_entity_roots": 142175, "kg_root_mentions": 320399},
			"os": {"index_docs": 57724}}, // re-synced mirror
		{"db": {"zotero_documents": 369, "processing_snapshots": 361},
			"kg": {"kg_entity_roots": 151000, "kg_root_mentions": 335000},
			"os": {"index_docs": 57724}}, // roots regenerated
		{"db": {"zotero_documents": 369, "processing_snapshots": 361},
			"kg": {"kg_entity_roots": 142175, "kg_root_mentions": 320399},
			"os": {"index_docs": 57727}}, // index drift
	}
	for i, live := range cases {
		err := corpusStampDrift(recorded, live)
		if err == nil {
			t.Fatalf("case %d: drift must error", i)
		}
		msg := err.Error()
		for _, want := range []string{"corpus drift", "baseline refresh"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("case %d: message must name %q:\n%s", i, want, msg)
			}
		}
		if strings.Contains(msg, "value divergence") {
			t.Fatalf("case %d: message must not blame value divergence:\n%s", i, msg)
		}
	}
	// each case names its own drift values and stamp
	if err := corpusStampDrift(recorded, cases[0]); err == nil ||
		!strings.Contains(err.Error(), "db/zotero_documents: frozen 369, live 417") {
		t.Fatalf("db drift must name both values:\n%v", err)
	}
	if err := corpusStampDrift(recorded, cases[1]); err == nil || !strings.Contains(err.Error(), "kg/kg_entity_roots") {
		t.Fatalf("kg drift must name the kg stamp:\n%v", err)
	}
	if err := corpusStampDrift(recorded, cases[2]); err == nil || !strings.Contains(err.Error(), "os/index_docs") {
		t.Fatalf("os drift must name the os stamp:\n%v", err)
	}
}

// TestCorpusStampMatchProceeds — matching stamps pass the gate and the
// suite proceeds to value assertions (#347 acceptance).
func TestCorpusStampMatchProceeds(t *testing.T) {
	stamps := corpusStamps{
		"db": {"zotero_documents": 369, "processing_snapshots": 361},
		"kg": {"kg_entity_roots": 142175, "kg_root_mentions": 320399},
		"os": {"index_docs": 57724},
	}
	if err := corpusStampDrift(stamps, stamps); err != nil {
		t.Fatalf("matching stamp must proceed: %v", err)
	}
}

// TestCorpusStampIncompleteRefuses — a stanza missing any stamped key
// refuses instead of narrowing the guard silently.
func TestCorpusStampIncompleteRefuses(t *testing.T) {
	if _, err := parseCorpusStamps("# corpus-stamp:db: zotero_documents=369\n"); err == nil {
		t.Fatal("incomplete stanza must refuse")
	}
	if _, err := parseCorpusStamps("# no stanza at all\n"); err == nil {
		t.Fatal("missing stanza must refuse")
	}
	if _, err := parseCorpusStamps("# corpus-stamp:db: zotero_documents=many\n"); err == nil {
		t.Fatal("non-numeric stanza must refuse")
	}
}
