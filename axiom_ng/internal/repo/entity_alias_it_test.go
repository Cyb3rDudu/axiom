package repo

// #198-3 alias IT: the Nachhaltigkeitsbericht family (3 flexion forms)
// becomes ONE graph node with a forms list; search still finds every
// form; the survivor discipline is #197's (most chunks, tie smallest id);
// re-run is a no-op.

import (
	"context"
	"strings"
	"testing"
)

func TestIT_FlexionAliasBinding(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snapA := kgSeedSnapshot(t, lr, "Alias Buch", "ALATT_A")
	_, snapB := kgSeedSnapshot(t, lr, "Alias Buch 2", "ALATT_B")

	// Family: bericht (3 chunks, doc A), berichte (2, doc B), berichts (1, doc B).
	eB, _ := kgSeedEntity(t, lr, snapA, "Nachhaltigkeitsbericht", 3)
	eBe, _ := kgSeedEntity(t, lr, snapB, "Nachhaltigkeitsberichte", 2)
	eBs, _ := kgSeedEntity(t, lr, snapB, "Nachhaltigkeitsberichts", 1)
	// Unrelated singleton stays unbound.
	eX, _ := kgSeedEntity(t, lr, snapA, "photonik", 2)
	_ = eX

	counts, err := lr.rep.EntityAliasCounts(ctx)
	if err != nil {
		t.Fatalf("EntityAliasCounts: %v", err)
	}
	if counts.Families != 1 || counts.VariantsLinked != 2 {
		t.Fatalf("dry-run: want 1 family / 2 variants, got %+v", counts)
	}
	var bound int
	if err := lr.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_entities WHERE alias_of IS NOT NULL`).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != 0 {
		t.Fatal("dry run must not mutate")
	}

	rep, err := lr.rep.BindFlexionAliases(ctx)
	if err != nil {
		t.Fatalf("BindFlexionAliases: %v", err)
	}
	if rep.VariantsLinked != 2 {
		t.Fatalf("apply: want 2 linked, got %+v", rep)
	}
	// Survivor = most chunks = eB (3).
	readAlias := func(id string) string {
		var ao *string
		if err := lr.pool.QueryRow(ctx,
			`SELECT alias_of::text FROM processing_entities WHERE id=$1::uuid`, id).Scan(&ao); err != nil {
			t.Fatal(err)
		}
		if ao == nil {
			return ""
		}
		return *ao
	}
	if a, b, c := readAlias(eB), readAlias(eBe), readAlias(eBs); a != "" || b != eB || c != eB {
		t.Fatalf("binding wrong: bericht=%q berichte=%q berichts=%q (survivor %s)", a, b, c, eB)
	}
	// Graph node view: the survivor leads the family with a forms list.
	forms, err := lr.rep.KGEntityFamilyForms(ctx, eB)
	if err != nil {
		t.Fatalf("KGEntityFamilyForms: %v", err)
	}
	if len(forms) != 3 {
		t.Fatalf("family forms: want 3, got %v", forms)
	}
	// Search still finds every form (existing tier ranking untouched).
	for _, form := range []string{"Nachhaltigkeitsbericht", "Nachhaltigkeitsberichte", "Nachhaltigkeitsberichts"} {
		ents, err := lr.rep.SearchKGEntities(ctx, form, 2, 10)
		if err != nil {
			t.Fatalf("search %s: %v", form, err)
		}
		if len(ents) == 0 {
			t.Fatalf("search must still find %s", form)
		}
	}
	// Re-run: no-op.
	rep2, err := lr.rep.BindFlexionAliases(ctx)
	if err != nil || rep2.VariantsLinked != 0 || rep2.AlreadyBound < 1 {
		t.Fatalf("re-run must be a no-op, got %+v err=%v", rep2, err)
	}
}

// NACHZUG (Tor-4 review): edges whose endpoint is an alias VARIANT must
// re-point to the family survivor, then the resulting pair duplicate
// consolidates. Red-first: without the re-point, the variant keeps its
// own edge (name-level duplicate — exactly the tester's original finding).
func TestIT_AliasVariantEdgesRepointToSurvivor(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snapA := kgSeedSnapshot(t, lr, "Repoint Buch", "RPATT_A")

	// Family: nachhaltigkeit (survivor, 5 chunks) + nachhaltigkeiten (variant, 2).
	eSurv, chS := kgSeedEntity(t, lr, snapA, "nachhaltigkeit", 5)
	eVar, chV := kgSeedEntity(t, lr, snapA, "nachhaltigkeiten", 2)
	_ = eVar
	eCsr, _ := kgSeedEntity(t, lr, snapA, "csr", 2)

	// Edge at the VARIANT (source) + edge at the SURVIVOR (same pair, same type).
	kgSeedRelation(t, lr, snapA, eVar, eCsr, "facet_of", chV[:1])
	kgSeedRelation(t, lr, snapA, eSurv, eCsr, "facet_of", chS[:1])
	// Edge with the variant as TARGET (the bigger half in production).
	_, chE := kgSeedEntity(t, lr, snapA, "esg", 2)
	kgSeedRelation(t, lr, snapA, eCsr, eVar, "facet_of", chE[:1])
	// Intra-family edge (variant→survivor) — must become a self-loop and
	// be DELETED, not served as source_form=target_form noise.
	kgSeedRelation(t, lr, snapA, eVar, eSurv, "facet_of", chV[1:2])

	// Bind aliases first (variant → survivor).
	if _, err := lr.rep.BindFlexionAliases(ctx); err != nil {
		t.Fatalf("BindFlexionAliases: %v", err)
	}

	// NOW the fix under test: variant edges re-point + consolidate.
	if err := lr.rep.RepointAliasEdges(ctx); err != nil {
		t.Fatalf("RepointAliasEdges: %v", err)
	}
	rep, err := lr.rep.ConsolidateRelationsReport(ctx)
	if err != nil {
		t.Fatalf("ConsolidateRelationsReport: %v", err)
	}
	_ = rep

	// The edge now hangs at the SURVIVOR, and the pair is consolidated to ONE.
	var nAtSurv, nAtVar int
	_ = lr.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entity_relationships
		WHERE source_entity_id = $1::uuid`, eSurv).Scan(&nAtSurv)
	_ = lr.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entity_relationships
		WHERE source_entity_id = $1::uuid`, eVar).Scan(&nAtVar)
	if nAtSurv != 1 {
		t.Fatalf("survivor must carry the consolidated edge, got %d", nAtSurv)
	}
	if nAtVar != 0 {
		t.Fatalf("variant must have NO edges after re-point, got %d", nAtVar)
	}

	// Evidence union: both original chunks on the single survivor edge.
	var evJSON string
	if err := lr.pool.QueryRow(ctx, `
		SELECT evidence_chunk_ids::text FROM processing_entity_relationships
		WHERE source_entity_id = $1::uuid AND target_entity_id = $2::uuid`,
		eSurv, eCsr).Scan(&evJSON); err != nil {
		t.Fatalf("no edge at survivor→csr: %v", err)
	}
	// Evidence union: BOTH the survivor's and the variant's chunks.
	if chS[0] == "" || chV[0] == "" {
		t.Fatal("fixture sanity: chunk ids empty")
	}
	if !strings.Contains(evJSON, chS[0]) || !strings.Contains(evJSON, chV[0]) {
		t.Fatalf("evidence union: want both %s and %s in %s", chS[0], chV[0], evJSON)
	}
	// Self-loop: the intra-family edge must be gone.
	var nSelf int
	if err := lr.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entity_relationships
		WHERE source_entity_id = target_entity_id`).Scan(&nSelf); err != nil {
		t.Fatal(err)
	}
	if nSelf != 0 {
		t.Fatalf("self-loops after repoint: want 0, got %d", nSelf)
	}
	// The pair must be exactly 1 edge (no name-level duplicate).
	var pairEdges int
	_ = lr.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entity_relationships r
		JOIN processing_snapshots s ON s.id = r.snapshot_id AND s.active
		WHERE (r.source_entity_id = $1::uuid AND r.target_entity_id = $2::uuid)
		   OR (r.source_entity_id = $2::uuid AND r.target_entity_id = $1::uuid)`,
		eSurv, eCsr).Scan(&pairEdges)
	if pairEdges != 1 {
		t.Fatalf("pair must have exactly 1 edge after re-point+consolidate, got %d", pairEdges)
	}
}

// #198-3/3b: family search resolution — 3 forms of one family all
// return the SAME survivor node with forms=[3]; without resolution
// each variant appears as its own node (ROT).
func TestIT_FamilySearchResolution(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snapA := kgSeedSnapshot(t, lr, "FamSearch Buch", "FSATT_A")
	// Family: 3 flexion forms.
	eSurv, _ := kgSeedEntity(t, lr, snapA, "nachhaltigkeitsbericht", 5)
	eV1, _ := kgSeedEntity(t, lr, snapA, "nachhaltigkeitsberichte", 3)
	eV2, _ := kgSeedEntity(t, lr, snapA, "nachhaltigkeitsberichts", 2)
	_ = eV1
	_ = eV2
	// Unrelated entity.
	_, _ = kgSeedEntity(t, lr, snapA, "photonik", 2)

	// Bind the family.
	if _, err := lr.rep.BindFlexionAliases(ctx); err != nil {
		t.Fatalf("BindFlexionAliases: %v", err)
	}

	// Search each form: must return the SAME survivor with forms=3.
	for _, form := range []string{"nachhaltigkeitsbericht", "nachhaltigkeitsberichte", "nachhaltigkeitsberichts"} {
		ents, err := lr.rep.SearchKGEntities(ctx, form, 2, 10)
		if err != nil {
			t.Fatalf("search %s: %v", form, err)
		}
		// Filter to the family node (other entities may match by substring)
		var fam *KGEntity
		for i := range ents {
			if ents[i].ID == eSurv {
				fam = &ents[i]
				break
			}
		}
		if fam == nil {
			t.Fatalf("search %s: survivor %s not found in %d results", form, eSurv, len(ents))
		}
		if len(fam.Forms) != 3 {
			t.Fatalf("search %s: forms want 3, got %v", form, fam.Forms)
		}
	}
	// The variants do NOT appear as separate nodes.
	ents, _ := lr.rep.SearchKGEntities(ctx, "nachhaltigkeitsberichte", 2, 10)
	for _, e := range ents {
		if e.ID == eV1 || e.ID == eV2 {
			t.Fatalf("variant %s must not appear as its own node", e.ID)
		}
	}
}

// #198-3/3b goal 1 IT: exact-form binding — identical canonical_form
// entities across snapshots bind via alias_of; idempotent.
func TestIT_ExactFormAliasBinding(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snapA := kgSeedSnapshot(t, lr, "Exact Buch A", "EFATT_A")
	_, snapB := kgSeedSnapshot(t, lr, "Exact Buch B", "EFATT_B")
	eA, _ := kgSeedEntity(t, lr, snapA, "deutschland", 3)
	eB, _ := kgSeedEntity(t, lr, snapB, "deutschland", 2)
	eC, _ := kgSeedEntity(t, lr, snapB, "deutschlands", 1)
	_ = eC
	_, _ = kgSeedEntity(t, lr, snapA, "photonik", 2)

	dr, err := lr.rep.BindExactFormAliasesDryRun(ctx)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if dr.Families != 1 || dr.VariantsLinked != 1 {
		t.Fatalf("dry-run: want 1 family / 1 variant, got %+v", dr)
	}
	var bound int
	_ = lr.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_entities WHERE alias_of IS NOT NULL`).Scan(&bound)
	if bound != 0 {
		t.Fatal("dry run must not mutate")
	}

	ar, err := lr.rep.BindAllAliases(ctx)
	if err != nil {
		t.Fatalf("BindAllAliases: %v", err)
	}
	if ar.VariantsLinked < 2 {
		t.Fatalf("apply: want >=2 linked, got %+v", ar)
	}
	var ao *string
	if err := lr.pool.QueryRow(ctx,
		`SELECT alias_of::text FROM processing_entities WHERE id=$1::uuid`, eB).Scan(&ao); err != nil {
		t.Fatal(err)
	}
	if ao == nil || *ao != eA {
		t.Fatalf("exact-form: eB must point at eA, got %v", ao)
	}
	var aliasA string
	_ = lr.pool.QueryRow(ctx,
		`SELECT coalesce(alias_of::text, '') FROM processing_entities WHERE id=$1::uuid`, eA).Scan(&aliasA)
	if aliasA != "" {
		t.Fatalf("survivor eA must have alias_of=NULL, got %q", aliasA)
	}
	rep2, err := lr.rep.BindAllAliases(ctx)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if rep2.VariantsLinked != 0 {
		t.Fatalf("re-run must be no-op, got %+v", rep2)
	}
}

// #198-3/3b GUARD IT: three different Schmidt-PERSONs from three
// documents must stay SEPARATE (the satellite deep-review finding —
// the un-guarded code would fuse them into one family).
func TestIT_PersonHomonymGuard(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snapA := kgSeedSnapshot(t, lr, "Schmidt Buch A", "SGATT_A")
	_, snapB := kgSeedSnapshot(t, lr, "Schmidt Buch B", "SGATT_B")
	_, snapC := kgSeedSnapshot(t, lr, "Schmidt Buch C", "SGATT_C")

	// Three DIFFERENT Schmidt PERSONs (different full names, different docs).
	schmidtA := seedTypedEntity(t, lr, snapA, "Michael Schmidt", "PERSON", 3)
	schmidtB := seedTypedEntity(t, lr, snapB, "Anna Schmidt", "PERSON", 2)
	schmidtC := seedTypedEntity(t, lr, snapC, "Hans Schmidt", "PERSON", 1)
	_ = schmidtC

	// Same-doc CONCEPT entities CAN bind.
	conc1 := seedTypedEntity(t, lr, snapA, "nachhaltigkeit", "CONCEPT", 3)
	conc2 := seedTypedEntity(t, lr, snapB, "nachhaltigkeit", "CONCEPT", 2)
	_ = conc1
	_ = conc2

	// Apply binding.
	if _, err := lr.rep.BindAllAliases(ctx); err != nil {
		t.Fatalf("BindAllAliases: %v", err)
	}

	// The three Schmidts must stay SEPARATE (no alias_of).
	for id, name := range map[string]string{schmidtA: "Michael", schmidtB: "Anna", schmidtC: "Hans"} {
		var ao *string
		if err := lr.pool.QueryRow(ctx,
			`SELECT alias_of::text FROM processing_entities WHERE id=$1::uuid`, id).Scan(&ao); err != nil {
			t.Fatal(err)
		}
		if ao != nil {
			t.Fatalf("PERSON homonym guard failed: %s Schmidt (id=%s) was bound to %s", name, id, *ao)
		}
	}

	// The CONCEPTs DID bind (cross-doc, compatible types).
	var concBound int
	_ = lr.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_entities WHERE id IN ($1::uuid, $2::uuid) AND alias_of IS NOT NULL`,
		conc1, conc2).Scan(&concBound)
	if concBound == 0 {
		t.Log("note: CONCEPT binding may produce 0 if one entity is the survivor (alias_of=NULL)")
	}
	// At least one of conc1/conc2 must point at the other.
	var linked bool
	_ = lr.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM processing_entities WHERE (id=$1::uuid AND alias_of=$2::uuid) OR (id=$2::uuid AND alias_of=$1::uuid))`,
		conc1, conc2).Scan(&linked)
	if !linked {
		t.Fatal("compatible CONCEPT family must bind across documents")
	}
}

// #198-3/3b GUARD IT: mixed-type families stay unbound.
func TestIT_MixedTypeFamilyUnbound(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snapA := kgSeedSnapshot(t, lr, "Mixed Buch A", "MXATT_A")
	_, snapB := kgSeedSnapshot(t, lr, "Mixed Buch B", "MXATT_B")

	// Mixed types: CONCEPT + ORGANIZATION with the same form.
	e1 := seedTypedEntity(t, lr, snapA, "management", "CONCEPT", 3)
	e2 := seedTypedEntity(t, lr, snapB, "management", "ORGANIZATION", 2)
	_ = e1
	_ = e2

	if _, err := lr.rep.BindAllAliases(ctx); err != nil {
		t.Fatalf("BindAllAliases: %v", err)
	}

	// Neither should be bound.
	var n int
	_ = lr.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_entities WHERE alias_of IS NOT NULL`).Scan(&n)
	if n != 0 {
		t.Fatalf("mixed-type family must stay unbound, got %d bound", n)
	}
}

// seedTypedEntity is kgSeedEntity with an explicit type.
func seedTypedEntity(t *testing.T, lr *leaseRepo, snapID, form, eType string, nChunks int) string {
	t.Helper()
	ctx := context.Background()
	var entID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_entities (snapshot_id, ref, text, canonical_form, type)
		VALUES ($1::uuid, $2, $2, $2, $3) RETURNING id::text`, snapID, form, eType).Scan(&entID); err != nil {
		t.Fatalf("seed %s: %v", form, err)
	}
	for i := 0; i < nChunks; i++ {
		var cID string
		if err := lr.pool.QueryRow(ctx, `
			INSERT INTO processing_chunks (snapshot_id, chunk_index, text, token_count)
			VALUES ($1::uuid,
			        (SELECT coalesce(max(chunk_index), -1) + 1 FROM processing_chunks WHERE snapshot_id = $1::uuid),
			        $2, 10) RETURNING id::text`,
			snapID, form+" inhalt").Scan(&cID); err != nil {
			t.Fatalf("seed chunk for %s: %v", form, err)
		}
		if _, err := lr.pool.Exec(ctx, `
			INSERT INTO processing_entity_mentions (entity_id, chunk_id, start_char, end_char)
			VALUES ($1::uuid, $2::uuid, 0, 1)`, entID, cID); err != nil {
			t.Fatalf("seed mention %s: %v", form, err)
		}
	}
	return entID
}

// W3 (#199): naked-surname PERSONs NEVER bind — even with the identical
// form and type. Three "schmidt" PERSONs from three docs stay separate.
func TestIT_NakedSurnamePersonsNeverBind(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snapA := kgSeedSnapshot(t, lr, "Naked A", "NSATT_A")
	_, snapB := kgSeedSnapshot(t, lr, "Naked B", "NSATT_B")
	_, snapC := kgSeedSnapshot(t, lr, "Naked C", "NSATT_C")

	// Three NAKED-SURNAME PERSONs: identical form "schmidt", same type.
	sA := seedTypedEntity(t, lr, snapA, "schmidt", "PERSON", 3)
	sB := seedTypedEntity(t, lr, snapB, "schmidt", "PERSON", 2)
	sC := seedTypedEntity(t, lr, snapC, "schmidt", "PERSON", 1)
	_ = sC

	// Multi-part-name PERSONs with the SAME full name CAN bind.
	mA := seedTypedEntity(t, lr, snapA, "Michael Schmidt", "PERSON", 3)
	mB := seedTypedEntity(t, lr, snapB, "Michael Schmidt", "PERSON", 2)
	_ = mA
	_ = mB

	if _, err := lr.rep.BindAllAliases(ctx); err != nil {
		t.Fatalf("BindAllAliases: %v", err)
	}

	// Naked surnames: ALL stay separate (alias_of IS NULL).
	for id, label := range map[string]string{sA: "sA", sB: "sB", sC: "sC"} {
		var ao *string
		_ = lr.pool.QueryRow(ctx,
			`SELECT alias_of::text FROM processing_entities WHERE id=$1::uuid`, id).Scan(&ao)
		if ao != nil {
			t.Fatalf("naked-surname guard failed: %s was bound", label)
		}
	}

	// Multi-part same-name: bound (one points at the other).
	var linked bool
	_ = lr.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM processing_entities WHERE (id=$1::uuid AND alias_of=$2::uuid) OR (id=$2::uuid AND alias_of=$1::uuid))`,
		mA, mB).Scan(&linked)
	if !linked {
		t.Fatal("multi-part-name PERSONs with identical form must bind")
	}
}
