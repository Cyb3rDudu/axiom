package repo

// #198 item 1 IT: the frontmatter cleanup pass. Seeded fixture covers every
// class and every deletion rule; dry-run reports candidates WITHOUT
// mutating; apply deletes; a second run is a no-op (idempotent). The
// persist-side gate is pinned separately (kg_frontmatter_gate_test.go).
//
// Run with:
//   AXIOM_TEST_DATABASE_URL=postgresql://axiom_user:...@.../axiom_consol_test?sslmode=disable \
//   go test ./internal/repo/ -run TestIT_FrontmatterCleanup -v

import (
	"context"
	"testing"
)

func fmgSeedChunk(t *testing.T, lr *leaseRepo, snapID, text string) string {
	t.Helper()
	var cID string
	if err := lr.pool.QueryRow(context.Background(), `
		INSERT INTO processing_chunks (snapshot_id, chunk_index, text, token_count)
		VALUES ($1::uuid,
		        (SELECT coalesce(max(chunk_index), -1) + 1 FROM processing_chunks WHERE snapshot_id = $1::uuid),
		        $2, 10) RETURNING id::text`, snapID, text).Scan(&cID); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	return cID
}

// fmgSeedEntity: entity with one mention per given chunk.
func fmgSeedEntity(t *testing.T, lr *leaseRepo, snapID, ref, form string, chunkIDs ...string) string {
	t.Helper()
	ctx := context.Background()
	var eID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_entities (snapshot_id, ref, text, canonical_form)
		VALUES ($1::uuid, $2, $3, $3) RETURNING id::text`, snapID, ref, form).Scan(&eID); err != nil {
		t.Fatalf("seed entity %s: %v", ref, err)
	}
	for i, c := range chunkIDs {
		if _, err := lr.pool.Exec(ctx, `
			INSERT INTO processing_entity_mentions (entity_id, chunk_id, start_char, end_char)
			VALUES ($1::uuid, $2::uuid, $3, $3)`, eID, c, i*10); err != nil {
			t.Fatalf("seed mention %s: %v", ref, err)
		}
	}
	return eID
}

func fmgSeedRelation(t *testing.T, lr *leaseRepo, snapID, srcID, tgtID, relType string, evidence ...string) string {
	t.Helper()
	ev := "["
	for i, c := range evidence {
		if i > 0 {
			ev += ","
		}
		ev += `"` + c + `"`
	}
	ev += "]"
	var rID string
	if err := lr.pool.QueryRow(context.Background(), `
		INSERT INTO processing_entity_relationships
			(snapshot_id, source_entity_id, target_entity_id, type, strength, evidence_chunk_ids)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, 0.7, $5::jsonb)
		RETURNING id::text`, snapID, srcID, tgtID, relType, ev).Scan(&rID); err != nil {
		t.Fatalf("seed relation %s: %v", relType, err)
	}
	return rID
}

func TestIT_FrontmatterCleanup(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	_, snapA := kgSeedSnapshot(t, lr, "FM Dokument A", "FMGA")
	kgSeedSnapshot(t, lr, "FM Dokument B", "FMGB")

	bodyA := fmgSeedChunk(t, lr, snapA, "Die Studie zeigt, dass nachhaltige Unternehmensführung im Zeitraum 2010 bis 2020 deutlich an Bedeutung gewonnen hat.")
	tocA := fmgSeedChunk(t, lr, snapA, "| 3 | Titel des Kapitels<br>Max Mustermann | 23 |\n| 4 | Zweites Kapitel | 45 |\n| 5 | Drittes Kapitel<br>Noch ein Autor | 67 |\n| 6 | Viertes Kapitel | 89 |")
	bylineA := fmgSeedChunk(t, lr, snapA, "### <span id=\"page-108-0\"></span>**Nachhaltigkeit und Digitalisierung als Chance für Unternehmen**\n\n**4**\n\nAnabel Ternès")
	bibA := fmgSeedChunk(t, lr, snapA, "# **Literaturverzeichnis**\n\n- Ackermann, C.: Der Begriff, in: Jensen, S. (Hrsg.), Opladen 1976.\n- Ackermann, T.: Methoden, St. Gallen 2003.\n- Bayer, H.: Konzepte, in: Zeitschrift, Vol. 3, pp. 12-25, Berlin 1999.\n- Meier, P.: Handbuch, 2. Aufl., Wiesbaden 2001.\n- Schulze, R.: Managementforschung, Wiesbaden 2002.")
	idxA := fmgSeedChunk(t, lr, snapA, "## **Sachregister**\n\nKalkulation 158\nKapazitätsbeanspruchung 169\nKapitalbindungszinsen 156\nKapitaleigner 36 f., 40\nKapitalerhöhung 158\nKapitalmarkt 41")
	autA := fmgSeedChunk(t, lr, snapA, "# **Autorenverzeichnis**\n\nProfessor Dr. **Oliver Budzinski**, Technische Universität Ilmenau\n\nSachgebiet: Geldpolitik")

	eBody := fmgSeedEntity(t, lr, snapA, "e-body", "nachhaltigkeit", bodyA)
	eMixed := fmgSeedEntity(t, lr, snapA, "e-mixed", "kapitalmarkt", bodyA, tocA) // survives, gated mention dies
	eToc := fmgSeedEntity(t, lr, snapA, "e-toc", "zweites kapitel", tocA)         // dies (all-gated)
	eByline := fmgSeedEntity(t, lr, snapA, "e-byline", "anabel ternès", bylineA)  // dies
	eBib := fmgSeedEntity(t, lr, snapA, "e-bib", "ackermann", bibA)               // dies
	eIdx := fmgSeedEntity(t, lr, snapA, "e-idx", "kalkulation", idxA)             // dies
	eAut := fmgSeedEntity(t, lr, snapA, "e-aut", "budzinski", autA)               // dies
	_, _, _ = eBib, eIdx, eAut

	// relations
	rBody := fmgSeedRelation(t, lr, snapA, eBody, eMixed, "related_to", bodyA)          // keep
	rMixed := fmgSeedRelation(t, lr, snapA, eBody, eMixed, "main_subject", tocA, bodyA) // keep, evidence stripped
	rAllGated := fmgSeedRelation(t, lr, snapA, eBody, eToc, "facet_of", tocA)           // dies (all-gated AND endpoint)
	rByline := fmgSeedRelation(t, lr, snapA, eByline, eBody, "named_after", bylineA)    // dies (endpoint+evidence)
	_, _, _, _ = rBody, rMixed, rAllGated, rByline

	// inactive snapshot with gated evidence: NOT cleaned (active scope).
	bodyI := fmgSeedChunk(t, lr, kgSeedInactiveSnapshot(t, lr, "FMGA", docA_ID(t, lr, "FMGA")), "# **Inhaltsverzeichnis**")
	eInactive := fmgSeedEntity(t, lr, kgSeedInactiveSnapshot(t, lr, "FMGB", docA_ID(t, lr, "FMGB")), "e-inact", "registerwort", bodyI)
	_ = eInactive

	// --- dry run: candidates reported, nothing deleted -------------
	rep, err := lr.rep.CleanupFrontmatterKG(ctx, false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if rep.Applied {
		t.Fatal("dry run must not be applied")
	}
	// per-class candidate counts (relations attribute to the class of their
	// gated evidence; rAllGated -> toc, rByline -> title_lines)
	wantClass := map[string]FrontmatterClassCounts{
		"toc":          {Chunks: 1, Relations: 1, Entities: 1, Mentions: 2, EvidenceRefsStripped: 1},
		"title_lines":  {Chunks: 1, Relations: 1, Entities: 1, Mentions: 1, EvidenceRefsStripped: 0},
		"bibliography": {Chunks: 1, Relations: 0, Entities: 1, Mentions: 1, EvidenceRefsStripped: 0},
		"index":        {Chunks: 1, Relations: 0, Entities: 1, Mentions: 1, EvidenceRefsStripped: 0},
		"authors":      {Chunks: 1, Relations: 0, Entities: 1, Mentions: 1, EvidenceRefsStripped: 0},
	}
	if len(rep.Classes) != len(wantClass) {
		t.Fatalf("exactly the %d seeded classes must appear, got %d: %v", len(wantClass), len(rep.Classes), rep.Classes)
	}
	for class, want := range wantClass {
		if got := rep.Classes[class]; got != want {
			t.Fatalf("class %q: want %+v, got %+v", class, want, got)
		}
	}
	// nothing was deleted on dry run
	var n int
	if err := lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entities WHERE ref='e-toc'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("dry run must not delete")
	}

	// --- apply -------------------------------------------------------
	rep, err = lr.rep.CleanupFrontmatterKG(ctx, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !rep.Applied || rep.Totals.Entities != 5 || rep.Totals.Relations != 2 || rep.Totals.Mentions != 6 {
		t.Fatalf("apply report: %+v", rep.Totals)
	}

	// survivors: e-body, e-mixed (without toc mention), inactive entity
	for ref, want := range map[string]int{"e-body": 1, "e-mixed": 1, "e-inact": 1, "e-toc": 0, "e-byline": 0, "e-bib": 0, "e-idx": 0, "e-aut": 0} {
		if err := lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entities WHERE ref=$1`, ref).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("entity %s: want %d rows, got %d", ref, want, n)
		}
	}
	// e-mixed lost its gated mention, kept the body one
	if err := lr.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entity_mentions m
		JOIN processing_entities e ON e.id = m.entity_id
		JOIN processing_chunks c ON c.id = m.chunk_id
		WHERE e.ref='e-mixed' AND c.id=$1::uuid`, bodyA).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("e-mixed must keep its body mention, got %d", n)
	}
	if err := lr.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entity_mentions m
		JOIN processing_entities e ON e.id = m.entity_id
		WHERE e.ref='e-mixed'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("e-mixed must have exactly 1 mention left, got %d", n)
	}
	// relations: rBody + rMixed survive; rAllGated, rByline die; rMixed evidence stripped to body only
	for _, rr := range []struct {
		typ  string
		want int
	}{
		{"related_to", 1}, {"main_subject", 1}, {"facet_of", 0}, {"named_after", 0},
	} {
		if err := lr.pool.QueryRow(ctx,
			`SELECT count(*) FROM processing_entity_relationships WHERE type=$1`, rr.typ).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != rr.want {
			t.Fatalf("relation %s: want %d, got %d", rr.typ, rr.want, n)
		}
	}
	var evLen int
	if err := lr.pool.QueryRow(ctx, `
		SELECT jsonb_array_length(evidence_chunk_ids) FROM processing_entity_relationships WHERE type='main_subject'`).Scan(&evLen); err != nil {
		t.Fatal(err)
	}
	if evLen != 1 {
		t.Fatalf("surviving main_subject must keep only ungated evidence (1), got %d", evLen)
	}

	// --- second run: no-op -------------------------------------------
	rep2, err := lr.rep.CleanupFrontmatterKG(ctx, true)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if rep2.Totals != (FrontmatterClassCounts{}) {
		t.Fatalf("second run must be a no-op, got %+v", rep2.Totals)
	}
}
