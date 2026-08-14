package dispatcher

// L8 anomaly regression tests:
//   - claim fairness: an expired 'processing' row (abandoned worker) must win
//     the next claim over pending rows even when enqueued_at ties (bulk sync)
//   - renewal decoupling: lease renewals continue while a status poll is
//     still in flight (slow poll must not consume the renewal window)

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

func TestClaimPrefersExpiredProcessingOverPendingTie(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	ctx := context.Background()

	// Two jobs from the same sync (identical enqueued_at by default now()).
	pendingID := h.seedJob(t, "fair-pending", 3)
	expiredID := h.seedJob(t, "fair-expired", 3)

	// The "expired" one: a worker claimed it and died — status processing
	// with a lease in the past.
	if _, err := h.pool.Exec(ctx, `
		UPDATE ingest_jobs SET status='processing', claimed_by='dead-worker',
			lease_token=gen_random_uuid(), lease_until=now()-interval '1 minute',
			attempt=1
		WHERE id=$1`, expiredID); err != nil {
		t.Fatalf("expire: %v", err)
	}

	claimed, err := h.rep.ClaimNextJob(ctx, repo.ClaimOptions{
		WorkerID:      "fair-worker",
		LeaseDuration: time.Minute,
		Profile:       []byte(`{"profile":"full-rag-v1"}`),
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil {
		t.Fatal("nothing claimable")
	}
	if claimed.LeaseRef.JobID != expiredID {
		t.Fatalf("claimed %s, want the EXPIRED processing row %s (recovery must win over pending %s)",
			claimed.LeaseRef.JobID, expiredID, pendingID)
	}
}

// fakeSlowStatusProcessor serves /v1/jobs/{id} with a configurable delay and
// a fixed status body.
func fakeSlowStatusProcessor(t *testing.T, delay time.Duration, status string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && len(r.URL.Path) > len("/v1/jobs/") {
			time.Sleep(delay)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"contract_version":"1.0","job_id":%q,"status":%q}`, r.URL.Path[len("/v1/jobs/"):], status)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRenewalContinuesDuringSlowStatusPoll(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	ctx := context.Background()

	jobID := h.seedJob(t, "renew-slow", 3)
	claimed, err := h.rep.ClaimNextJob(ctx, repo.ClaimOptions{
		WorkerID: "renew-worker",
		// Tight lease so the effect is observable in seconds; the poll (4s)
		// is DELIBERATELY longer than the lease (2s).
		LeaseDuration: 2 * time.Second,
		Profile:       []byte(`{"profile":"full-rag-v1"}`),
	})
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v %v", claimed, err)
	}

	srv := fakeSlowStatusProcessor(t, 4*time.Second, "running")
	client, err := processor.New(processor.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	d := New(h.rep, client, Config{
		LeaseDuration:   2 * time.Second,
		RenewalInterval: 500 * time.Millisecond,
	}, testLogger())

	// Cancel the poll loop mid-flight (while the first status poll is still
	// sleeping) and verify the lease was renewed DURING that poll.
	runCtx, cancel := context.WithCancel(context.Background())
	go func() {
		d.pollAndFinish(runCtx, claimed)
	}()

	// Wait past the renewal interval but NOT past the poll delay: if renewal
	// were coupled to the poll cadence (the L8 bug), no renewal would happen
	// until the 4s poll returned and the 2s lease would already be dead.
	time.Sleep(1400 * time.Millisecond)

	var leaseUntil time.Time
	var status string
	if err := h.pool.QueryRow(ctx,
		`SELECT lease_until, status FROM ingest_jobs WHERE id=$1`, jobID).
		Scan(&leaseUntil, &status); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	cancel()
	time.Sleep(100 * time.Millisecond) // let the goroutine settle

	if status != "claimed" && status != "processing" {
		t.Fatalf("status = %q, want claimed/processing", status)
	}
	// Renewal must have fired during the in-flight poll: lease_until must be
	// in the future even though the first poll has not returned yet.
	if time.Until(leaseUntil) < 500*time.Millisecond {
		t.Fatalf("lease_until %v is not renewed (until=%v) — renewal starved by the in-flight status poll (L8 bug)", leaseUntil, time.Until(leaseUntil))
	}
}

// testLogger discards dispatcher log output in tests.
func testLogger() *log.Logger { return log.New(io.Discard, "test: ", 0) }
