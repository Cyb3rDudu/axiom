package repo

// #198-3 alias IT: the Nachhaltigkeitsbericht family (3 flexion forms)
// becomes ONE graph node with a forms list; search still finds every
// form; the survivor discipline is #197's (most chunks, tie smallest id);
// re-run is a no-op.

import (
	"context"
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
