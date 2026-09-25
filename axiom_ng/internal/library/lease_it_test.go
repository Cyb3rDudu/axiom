// lease_it_test.go — the single-writer declaration against real Postgres
// (F07, #301): cross-process semantics proven with two owner identities
// on the same scratch DB — the same rows two dev processes would race on.
package library

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/jackc/pgx/v5"
)

func TestWriterLeaseSecondWriterRefused(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	const scope = "zotero|http://localhost:23119|users/0"

	if err := st.AcquireWriterLease(ctx, scope, "host-a:101:nonce", DefaultWriterLeaseTTL); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// The second writer against the SAME scope is refused at start —
	// typed Conflict carrying the live owner as diagnosis.
	err := st.AcquireWriterLease(ctx, scope, "host-b:202:nonce", DefaultWriterLeaseTTL)
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassConflict {
		t.Fatalf("second acquire: %v, want conflict", err)
	}
	var lc *WriterLeaseConflict
	if !errors.As(err, &lc) || lc.Owner != "host-a:101:nonce" {
		t.Fatalf("second acquire must diagnose the live owner, got %+v (%v)", lc, err)
	}
	// The holder keeps renewing; a renewal by the NON-holder is refused.
	if err := st.RenewWriterLease(ctx, scope, "host-a:101:nonce"); err != nil {
		t.Fatalf("holder renew: %v", err)
	}
	err = st.RenewWriterLease(ctx, scope, "host-b:202:nonce")
	if class, ok := contracterr.ClassOf(err); err == nil || !ok || class != contracterr.ClassConflict {
		t.Fatalf("non-holder renew: %v, want conflict", err)
	}
	// Graceful release frees the scope for the next writer.
	if err := st.ReleaseWriterLease(ctx, scope, "host-a:101:nonce"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := st.AcquireWriterLease(ctx, scope, "host-b:202:nonce", DefaultWriterLeaseTTL); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestWriterLeaseStaleTakeoverAfterTTL(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	const scope = "zotero|http://localhost:23119|users/0"

	if err := st.AcquireWriterLease(ctx, scope, "crashed:1:n", 60*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(90 * time.Millisecond) // heartbeat silence beyond the TTL
	if err := st.AcquireWriterLease(ctx, scope, "successor:2:n", 60*time.Millisecond); err != nil {
		t.Fatalf("takeover of a silent writer's lease: %v", err)
	}
	// The crashed writer's next renewal discovers the takeover — it must
	// STOP, never write past a lease it no longer holds.
	err := st.RenewWriterLease(ctx, scope, "crashed:1:n")
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassConflict {
		t.Fatalf("crashed writer renew after takeover: %v, want conflict", err)
	}
}

func TestProviderAnchorsAndWriteAudit(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	const scope = "zotero|base|users/0"

	// Anchor idempotency: same anchor, the FIRST provider id survives.
	id, err := st.PutProviderAnchor(ctx, scope, "record", "imp-key-1-hash", "KEYAAA", 3)
	if err != nil || id != "KEYAAA" {
		t.Fatalf("put anchor: %q %v", id, err)
	}
	id, err = st.PutProviderAnchor(ctx, scope, "record", "imp-key-1-hash", "KEYBBB", 4)
	if err != nil || id != "KEYAAA" {
		t.Fatalf("duplicate anchor must return the surviving id, got %q %v", id, err)
	}
	if id, _, err := st.LookupProviderAnchor(ctx, scope, "record", "imp-key-1-hash"); err != nil || id != "KEYAAA" {
		t.Fatalf("lookup: %q %v", id, err)
	}
	if _, _, err := st.LookupProviderAnchor(ctx, scope, "rendition", "KEYAAA|sha-x"); !errors.Is(err, pgx.ErrNoRows) {
		// absent anchor: pgx.ErrNoRows surfaces (the adapter treats it as absent)
		if err == nil {
			t.Fatal("absent anchor must surface ErrNoRows, got a row")
		}
		t.Fatalf("absent anchor: %v, want pgx.ErrNoRows", err)
	}

	// Audit: one row per mutation — the count is the 1:1 sonde.
	if n, err := st.CountWriteAudit(ctx, scope); err != nil || n != 0 {
		t.Fatalf("audit count before mutations: %d %v", n, err)
	}
	for i, op := range []string{"ensure_record", "ensure_rendition", "ensure_membership"} {
		if err := st.AppendWriteAudit(ctx, WriteAuditRow{
			Scope: scope, Operation: op, Anchor: "anchor", ProviderRef: "KEY", Outcome: "created",
			Readback: map[string]any{"i": i},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.CountWriteAudit(ctx, scope); err != nil || n != 3 {
		t.Fatalf("audit count after 3 mutations: %d %v, want 3", n, err)
	}
}
