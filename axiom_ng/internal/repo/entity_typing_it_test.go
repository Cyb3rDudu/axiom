package repo

// #198-3 IT: entity typing normalization — pure SQL rules over existing
// entities, ACTIVE snapshots only. Pinned cases from the external
// regression (stakeholders→PERSON, Management→ORGANIZATION) plus the
// guards that MUST NOT fire:
//
//   'Stakeholders'          PERSON  → CONCEPT (bare role plural)
//   'Primäre Stakeholder'   PERSON  → CONCEPT (bare, normalized)
//   'stakeholder'           PERSON  → CONCEPT (bare generic role noun)
//   'Top-Management'        ORG     → CONCEPT (bare management term)
//   'Management'            (null)  → CONCEPT (bare, null-typed too)
//   'Externe Stakeholder'   PERSON  → CONCEPT (plural-head rule)
//   'Industry Stakeholders' ORG     → CONCEPT (plural-head rule)
//   'Academy of Management' ORG     → stays ORG (3 words, not bare; head
//                                     'management' is not a plural head)
//   'Stakeholder-Theorie'   CONCEPT → stays CONCEPT (no rule match)
//
// Dry-run counts are reported BEFORE applying (blast-radius-first); the
// apply returns the same accounting; a re-run updates nothing.
//
// Run with:
//   AXIOM_TEST_DATABASE_URL=postgresql://axiom_user:...@.../axiom_ng_test_kgrel?sslmode=disable \
//   go test ./internal/repo/ -run TestIT_EntityTypingNormalization -v

import (
	"context"
	"testing"
)

func typingTypeOf(t *testing.T, lr *leaseRepo, form string) string {
	t.Helper()
	var typ *string
	if err := lr.pool.QueryRow(context.Background(), `
		SELECT e.type FROM processing_entities e
		JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
		WHERE coalesce(e.canonical_form, e.text) = $1`, form).Scan(&typ); err != nil {
		t.Fatalf("lookup %q: %v", form, err)
	}
	if typ == nil {
		return "(null)"
	}
	return *typ
}

func TestIT_EntityTypingNormalization(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate kg fixtures: %v", err)
	}

	docA, snapA := kgSeedSnapshot(t, lr, "Typing Buch", "TYATT_A")
	seed := func(form, typ string) {
		t.Helper()
		var id string
		var err error
		if typ == "(null)" {
			err = lr.pool.QueryRow(ctx, `
				INSERT INTO processing_entities (snapshot_id, ref, text, canonical_form)
				VALUES ($1::uuid, $2, $2, $2) RETURNING id::text`, snapA, form).Scan(&id)
		} else {
			err = lr.pool.QueryRow(ctx, `
				INSERT INTO processing_entities (snapshot_id, ref, text, canonical_form, type)
				VALUES ($1::uuid, $2, $2, $2, $3) RETURNING id::text`, snapA, form, typ).Scan(&id)
		}
		if err != nil {
			t.Fatalf("seed %q: %v", form, err)
		}
	}
	seed("Stakeholders", "PERSON")
	seed("Primäre Stakeholder", "PERSON")
	seed("stakeholder", "PERSON")
	seed("Top-Management", "ORGANIZATION")
	seed("Management", "(null)")
	seed("Externe Stakeholder", "PERSON")
	seed("Industry Stakeholders", "ORGANIZATION")
	seed("Academy of Management", "ORGANIZATION")
	seed("Stakeholder-Theorie", "CONCEPT")
	seed("Stakeholdern", "PERSON")
	seed("Top Managements", "ORGANIZATION")
	seed("Top-Managements", "PERSON")

	// An INACTIVE-snapshot mis-typed entity must stay untouched.
	snapInact := kgSeedInactiveSnapshot(t, lr, "TYATT_A", docA)
	_ = snapInact
	var inactID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_entities (snapshot_id, ref, text, canonical_form, type)
		VALUES ($1::uuid, 'Management', 'Management', 'Management', 'ORGANIZATION')
		RETURNING id::text`, snapInact).Scan(&inactID); err != nil {
		t.Fatalf("seed inactive entity: %v", err)
	}

	// 1. Dry run: counts only, no mutation.
	counts, err := lr.rep.EntityTypingCounts(ctx)
	if err != nil {
		t.Fatalf("EntityTypingCounts: %v", err)
	}
	if counts.MatchedRows != 10 {
		t.Fatalf("dry-run matched_rows: want 7, got %+v", counts)
	}
	if counts.ByRule["bare_form"] != 9 {
		t.Fatalf("dry-run bare_form: want 6 (incl. 'Externe Stakeholder' from the bare list), got %+v", counts.ByRule)
	}
	if counts.ByRule["plural_head"] != 1 {
		t.Fatalf("dry-run plural_head: want 1 (only 'Industry Stakeholders' — 'Externe Stakeholder' wins bare first), got %+v", counts.ByRule)
	}
	if typingTypeOf(t, lr, "Stakeholders") != "PERSON" {
		t.Fatalf("dry run must not mutate")
	}

	// 2. Apply.
	rep, err := lr.rep.NormalizeEntityTypes(ctx)
	if err != nil {
		t.Fatalf("NormalizeEntityTypes: %v", err)
	}
	if rep.UpdatedRows != 10 {
		t.Fatalf("apply updated_rows: want 7, got %+v", rep)
	}

	for _, form := range []string{"Stakeholders", "Primäre Stakeholder", "stakeholder",
		"Top-Management", "Management", "Externe Stakeholder", "Industry Stakeholders",
		"Stakeholdern", "Top Managements", "Top-Managements"} {
		if got := typingTypeOf(t, lr, form); got != "CONCEPT" {
			t.Fatalf("%q: want CONCEPT after normalize, got %q", form, got)
		}
	}
	if got := typingTypeOf(t, lr, "Academy of Management"); got != "ORGANIZATION" {
		t.Fatalf("Academy of Management must stay ORGANIZATION, got %q", got)
	}
	if got := typingTypeOf(t, lr, "Stakeholder-Theorie"); got != "CONCEPT" {
		t.Fatalf("Stakeholder-Theorie must stay CONCEPT untouched, got %q", got)
	}
	var inactTyp string
	if err := lr.pool.QueryRow(ctx,
		`SELECT type FROM processing_entities WHERE id = $1::uuid`, inactID).Scan(&inactTyp); err != nil {
		t.Fatalf("inactive entity: %v", err)
	}
	if inactTyp != "ORGANIZATION" {
		t.Fatalf("inactive-snapshot entity must stay untouched, got %q", inactTyp)
	}

	// 3. Re-run: no-op.
	rep2, err := lr.rep.NormalizeEntityTypes(ctx)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if rep2.UpdatedRows != 0 {
		t.Fatalf("re-run must update nothing, got %+v", rep2)
	}
}
