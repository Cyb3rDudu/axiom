package repo

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func withKGPanicHook(t *testing.T, at kgMaintenanceHookKey, fn func()) {
	t.Helper()
	old := kgMaintenanceTestHook
	kgMaintenanceTestHook = func(got kgMaintenanceHookKey) {
		if got == at {
			panic("kg crash probe at " + string(at))
		}
	}
	defer func() { kgMaintenanceTestHook = old }()
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), string(at)) {
			t.Fatalf("expected panic at %s, got %v", at, r)
		}
	}()
	fn()
}

func TestIT_KGMaintenanceAdvisoryLockSerializesConcurrentJobs(t *testing.T) {
	lr := openLeaseDB(t)
	ctx := context.Background()

	firstLocked := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondLocked := make(chan struct{})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)

	old := kgMaintenanceTestHook
	kgMaintenanceTestHook = func(got kgMaintenanceHookKey) {
		switch got {
		case "kg_lock_probe_first:after_lock":
			close(firstLocked)
			<-releaseFirst
		case "kg_lock_probe_second:after_lock":
			close(secondLocked)
		}
	}
	defer func() { kgMaintenanceTestHook = old }()

	go func() {
		firstDone <- lr.rep.withKGMaintenanceTx(ctx, "kg_lock_probe_first", func(_ pgx.Tx) error { return nil })
	}()

	select {
	case <-firstLocked:
	case <-time.After(2 * time.Second):
		t.Fatal("first maintenance job did not acquire the KG advisory lock")
	}

	go func() {
		secondDone <- lr.rep.withKGMaintenanceTx(ctx, "kg_lock_probe_second", func(_ pgx.Tx) error { return nil })
	}()

	select {
	case <-secondLocked:
		close(releaseFirst)
		<-firstDone
		<-secondDone
		t.Fatal("second maintenance job acquired the KG lock before the first transaction committed")
	case <-time.After(150 * time.Millisecond):
		// Expected: the second transaction is blocked on pg_advisory_xact_lock.
	}

	close(releaseFirst)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first maintenance job: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first maintenance job did not commit after release")
	}
	select {
	case <-secondLocked:
	case <-time.After(2 * time.Second):
		t.Fatal("second maintenance job did not acquire the KG lock after first commit")
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second maintenance job: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second maintenance job did not complete")
	}
}

func TestIT_KGMaintenanceCrashRollbackAliasBind(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snap := kgSeedSnapshot(t, lr, "Crash Alias", "CRAL_A")
	eSurv, _ := kgSeedEntity(t, lr, snap, "Nachhaltigkeitsbericht", 3)
	eVar, _ := kgSeedEntity(t, lr, snap, "Nachhaltigkeitsberichte", 2)

	withKGPanicHook(t, "kg_bind_flexion_aliases:after_first_binding", func() {
		_, _ = lr.rep.BindFlexionAliases(ctx)
	})

	var aliased int
	if err := lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entities WHERE alias_of IS NOT NULL`).Scan(&aliased); err != nil {
		t.Fatal(err)
	}
	if aliased != 0 {
		t.Fatalf("crash rollback: want zero alias_of writes, got %d", aliased)
	}
	if got := func() int {
		var n int
		_ = lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entities WHERE id IN ($1::uuid,$2::uuid)`, eSurv, eVar).Scan(&n)
		return n
	}(); got != 2 {
		t.Fatalf("crash rollback: source entities must survive, got %d", got)
	}
}

func TestIT_KGMaintenanceCrashRollbackAliasRepoint(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snap := kgSeedSnapshot(t, lr, "Crash Repoint", "CRRP_A")
	eSurv, chS := kgSeedEntity(t, lr, snap, "nachhaltigkeit", 5)
	eVar, chV := kgSeedEntity(t, lr, snap, "nachhaltigkeiten", 2)
	eCsr, _ := kgSeedEntity(t, lr, snap, "csr", 2)
	kgSeedRelation(t, lr, snap, eVar, eCsr, "facet_of", chV[:1])
	kgSeedRelation(t, lr, snap, eCsr, eVar, "facet_of", chS[:1])
	if _, err := lr.rep.BindFlexionAliases(ctx); err != nil {
		t.Fatalf("bind aliases: %v", err)
	}

	withKGPanicHook(t, "kg_repoint_alias_edges:after_source", func() {
		_ = lr.rep.RepointAliasEdges(ctx)
	})

	var srcAtVariant, tgtAtVariant int
	if err := lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entity_relationships WHERE source_entity_id=$1::uuid`, eVar).Scan(&srcAtVariant); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entity_relationships WHERE target_entity_id=$1::uuid`, eVar).Scan(&tgtAtVariant); err != nil {
		t.Fatal(err)
	}
	if srcAtVariant != 1 || tgtAtVariant != 1 {
		t.Fatalf("crash rollback: want both variant endpoints intact, source=%d target=%d", srcAtVariant, tgtAtVariant)
	}
	var atSurv int
	_ = lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entity_relationships WHERE source_entity_id=$1::uuid OR target_entity_id=$1::uuid`, eSurv).Scan(&atSurv)
	if atSurv != 0 {
		t.Fatalf("crash rollback: survivor must not receive partial edges, got %d", atSurv)
	}
}

func TestIT_KGMaintenanceCrashRollbackRelationConsolidation(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snap := kgSeedSnapshot(t, lr, "Crash Relations", "CRREL_A")
	eA, chA := kgSeedEntity(t, lr, snap, "nachhaltigkeit", 2)
	eB, _ := kgSeedEntity(t, lr, snap, "csr", 2)
	kgSeedRelation(t, lr, snap, eA, eB, "facet_of", chA[:1])
	kgSeedRelation(t, lr, snap, eA, eB, "main_subject", chA[1:2])

	withKGPanicHook(t, "kg_consolidate_relations:after_survivor_update", func() {
		_, _ = lr.rep.ConsolidateRelationsReport(ctx)
	})

	if got := relConsEdges(t, lr, eA, eB); got != 2 {
		t.Fatalf("crash rollback: both relation rows must remain, got %d", got)
	}
	var archived int
	if err := lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entity_relationships WHERE metadata ? 'superseded_types'`).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived != 0 {
		t.Fatalf("crash rollback: survivor metadata must not be partially archived, got %d", archived)
	}
}

func TestIT_KGMaintenanceCrashRollbackTyping(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snap := kgSeedSnapshot(t, lr, "Crash Typing", "CRTYPE_A")
	eID, _ := kgSeedEntity(t, lr, snap, "stakeholders", 2)
	if _, err := lr.pool.Exec(ctx, `UPDATE processing_entities SET type='PERSON' WHERE id=$1::uuid`, eID); err != nil {
		t.Fatal(err)
	}

	withKGPanicHook(t, "kg_normalize_entity_types:after_update", func() {
		_, _ = lr.rep.NormalizeEntityTypes(ctx)
	})

	var typ string
	if err := lr.pool.QueryRow(ctx, `SELECT type FROM processing_entities WHERE id=$1::uuid`, eID).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	if typ != "PERSON" {
		t.Fatalf("crash rollback: type must remain PERSON, got %q", typ)
	}
}

func TestIT_KGMaintenanceCrashRollbackFrontmatterCleanup(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snap := kgSeedSnapshot(t, lr, "Crash Cleanup", "CRFM_A")
	fm := fmgSeedChunk(t, lr, snap, "Inhaltsverzeichnis\n1 Kapitel .... 1\n2 Kapitel .... 2\n3 Kapitel .... 3\n4 Kapitel .... 4")
	body := fmgSeedChunk(t, lr, snap, "Body evidence with real content.")
	eA := fmgSeedEntity(t, lr, snap, "fm-a", "toc-entity", fm)
	eB := fmgSeedEntity(t, lr, snap, "body-b", "body-entity", body)
	fmgSeedRelation(t, lr, snap, eA, eB, "named_after", fm)

	withKGPanicHook(t, "kg_cleanup_frontmatter:after_delete_relations", func() {
		_, _ = lr.rep.CleanupFrontmatterKG(ctx, true)
	})

	var rels, ents, mentions int
	_ = lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entity_relationships`).Scan(&rels)
	_ = lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entities`).Scan(&ents)
	_ = lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entity_mentions`).Scan(&mentions)
	if rels != 1 || ents != 2 || mentions != 2 {
		t.Fatalf("crash rollback: want full pre-state rels=1 ents=2 mentions=2, got %d/%d/%d", rels, ents, mentions)
	}
}

func TestIT_KGMaintenanceCrashRollbackEntityConsolidation(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snapA := kgSeedSnapshot(t, lr, "Crash Entity A", "CRENT_A")
	_, snapB := kgSeedSnapshot(t, lr, "Crash Entity B", "CRENT_B")
	eA, _ := kgSeedEntity(t, lr, snapA, "deutschland", 2)
	eB, _ := kgSeedEntity(t, lr, snapB, "deutschland", 3)

	withKGPanicHook(t, "kg_consolidate_entities:after_mentions", func() {
		_, _ = lr.rep.ConsolidateEntities(ctx)
	})

	var entities int
	if err := lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entities WHERE canonical_form='deutschland'`).Scan(&entities); err != nil {
		t.Fatal(err)
	}
	if entities != 2 {
		t.Fatalf("crash rollback: both entity rows must remain, got %d", entities)
	}
	var aMentions, bMentions int
	_ = lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entity_mentions WHERE entity_id=$1::uuid`, eA).Scan(&aMentions)
	_ = lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entity_mentions WHERE entity_id=$1::uuid`, eB).Scan(&bMentions)
	if aMentions != 2 || bMentions != 3 {
		t.Fatalf("crash rollback: mentions must remain unmoved, got A=%d B=%d", aMentions, bMentions)
	}
	var archived int
	if err := lr.pool.QueryRow(ctx, `SELECT count(*) FROM kg_superseded_entities`).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived != 0 {
		t.Fatalf("crash rollback: entity archive must not be partially written, got %d", archived)
	}
}
