package repo

// #168 (B2): the WS snapshot source. ActiveJobs must return exactly the
// in-flight jobs (pending/claimed/processing) and exclude terminal ones.
// Runs only against a dedicated *_test database (AXIOM_TEST_DATABASE_URL);
// skips (not fails) when no DSN is configured.

import (
	"context"
	"testing"
)

func TestActiveJobsReturnsOnlyInFlight(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	// An in-flight (pending) job must appear.
	_, pendingID := lr.seed(t, seedSpec{
		sourceBaseURL: "https://s1.test", libraryID: "L1", docKey: "doc-pending",
		attKey: "att-pending", contentHash: h("h-pending"), preferred: true,
	}, "pending", 3)
	// A terminal (completed) job must NOT appear (distinct library id so the
	// zotero_sources unique constraint is not violated).
	_, doneID := lr.seed(t, seedSpec{
		sourceBaseURL: "https://s1.test", libraryID: "L2", docKey: "doc-done",
		attKey: "att-done", contentHash: h("h-done"), preferred: true,
	}, "completed", 3)

	jobs, err := lr.rep.ActiveJobs(ctx)
	if err != nil {
		t.Fatalf("ActiveJobs: %v", err)
	}
	found := map[string]bool{}
	for _, j := range jobs {
		found[j.ID] = true
		if j.Status == "completed" {
			t.Fatalf("ActiveJobs returned a terminal job %s (status %s)", j.ID, j.Status)
		}
	}
	if !found[pendingID] {
		t.Fatalf("ActiveJobs missing in-flight job %s (got %v)", pendingID, found)
	}
	if found[doneID] {
		t.Fatalf("ActiveJobs included terminal job %s", doneID)
	}
}
