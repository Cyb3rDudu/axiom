package ingest_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/ingest"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/models"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/testutil"
)

// silentLogger returns a logger that drops all output so tests don't
// spam the run.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedUser inserts a throwaway users row so the FK on documents.user_id
// is satisfied. Returns the new user id.
func seedUser(t *testing.T, pg *testutil.Postgres, name string) int32 {
	t.Helper()
	var id int32
	err := pg.DB.Raw(`
		INSERT INTO users (username, email, hashed_password, is_active, role, created_at, updated_at)
		VALUES (?, ?, '$2a$12$dummy', TRUE, 'user', NOW(), NOW())
		RETURNING id`, name, name+"@local").Scan(&id).Error
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seedPending inserts n documents in processing_status='pending' owned
// by userID. Returns the ids in insertion order.
func seedPending(t *testing.T, pg *testutil.Postgres, userID int32, n int) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, n)
	for i := 0; i < n; i++ {
		id := uuid.New()
		ids[i] = id
		err := pg.DB.Exec(`
			INSERT INTO documents (id, user_id, filename, metadata_,
			                      processing_status, upload_progress,
			                      dense_collection_name, sparse_collection_name,
			                      chunk_count, created_at, updated_at)
			VALUES (?, ?, ?, '{}'::jsonb, 'pending', 0,
			        'documents_dense', 'documents_sparse', 0, NOW(), NOW())`,
			id, userID, "f.pdf").Error
		if err != nil {
			t.Fatalf("seed pending: %v", err)
		}
		// Stagger created_at by a microsecond so ORDER BY created_at is
		// deterministic across the batch.
		time.Sleep(time.Millisecond)
	}
	return ids
}

// statusOf returns the current processing_status of a document.
func statusOf(t *testing.T, pg *testutil.Postgres, id uuid.UUID) string {
	t.Helper()
	var s string
	err := pg.DB.Raw(`SELECT processing_status FROM documents WHERE id = ?`, id).Scan(&s).Error
	if err != nil {
		t.Fatalf("statusOf: %v", err)
	}
	return s
}

func TestPoolClaimsAndCompletesJob(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	uid := seedUser(t, pg, "ingest-a")
	ids := seedPending(t, pg, uid, 1)

	docs := repo.NewDocuments(pg.DB)
	var processed atomic.Int32
	proc := ingest.ProcessorFunc(func(_ context.Context, _ ingest.Job) error {
		processed.Add(1)
		return nil
	})
	pool := ingest.New(docs, proc, ingest.Config{
		Size:         1,
		PollInterval: 10 * time.Millisecond,
		Logger:       silentLogger(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	// Wait for the status to flip to 'completed'.
	waitFor(t, 3*time.Second, func() bool {
		return statusOf(t, pg, ids[0]) == repo.StatusCompleted
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("pool run: %v", err)
	}
	if got := processed.Load(); got != 1 {
		t.Errorf("processed count: want 1, got %d", got)
	}
}

func TestPoolProcessorErrorMarksFailedAndStoresMessage(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	uid := seedUser(t, pg, "ingest-b")
	ids := seedPending(t, pg, uid, 1)

	docs := repo.NewDocuments(pg.DB)
	proc := ingest.ProcessorFunc(func(_ context.Context, _ ingest.Job) error {
		return errors.New("boom")
	})
	pool := ingest.New(docs, proc, ingest.Config{
		Size:         1,
		PollInterval: 10 * time.Millisecond,
		Logger:       silentLogger(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool {
		return statusOf(t, pg, ids[0]) == repo.StatusFailed
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("pool run: %v", err)
	}

	var m models.Document
	if err := pg.DB.Raw(`SELECT * FROM documents WHERE id = ?`, ids[0]).Scan(&m).Error; err != nil {
		t.Fatalf("read doc: %v", err)
	}
	if m.ProcessingStatus != repo.StatusFailed {
		t.Errorf("status: %s", m.ProcessingStatus)
	}
	// processing_error is serialised into metadata_.
	var errMsg string
	if err := pg.DB.Raw(
		`SELECT metadata_->>'processing_error' FROM documents WHERE id = ?`, ids[0],
	).Scan(&errMsg).Error; err != nil {
		t.Fatalf("read error: %v", err)
	}
	if errMsg != "boom" {
		t.Errorf("processing_error: want 'boom', got %q", errMsg)
	}
}

func TestPoolConcurrentWorkersNeverDoubleClaim(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	uid := seedUser(t, pg, "ingest-c")
	// 10 docs × 3 workers gives us enough parallelism to exercise
	// SKIP LOCKED without bogging down CI. The original 20×5 shape
	// flaked on GitHub runners where Postgres round-trips dominate.
	ids := seedPending(t, pg, uid, 10)

	docs := repo.NewDocuments(pg.DB)
	var (
		mu      sync.Mutex
		seenIDs = make(map[uuid.UUID]int)
	)
	proc := ingest.ProcessorFunc(func(_ context.Context, job ingest.Job) error {
		mu.Lock()
		seenIDs[job.DocID]++
		mu.Unlock()
		// Small sleep so multiple workers actually race for claims.
		time.Sleep(5 * time.Millisecond)
		return nil
	})
	pool := ingest.New(docs, proc, ingest.Config{
		Size:         3,
		PollInterval: 10 * time.Millisecond,
		Logger:       silentLogger(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	// Wait until every row has reached 'completed' — this covers the
	// post-Process MarkStatus write too, so cancelling after this point
	// cannot race with a still-writing worker.
	waitFor(t, 45*time.Second, func() bool {
		for _, id := range ids {
			if statusOf(t, pg, id) != repo.StatusCompleted {
				return false
			}
		}
		return true
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("pool run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seenIDs) != len(ids) {
		t.Fatalf("expected %d distinct claims, got %d", len(ids), len(seenIDs))
	}
	for id, count := range seenIDs {
		if count != 1 {
			t.Errorf("doc %s processed %d times (want 1)", id, count)
		}
	}
}

func TestPoolShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	// No seeds — pool should idle and exit on cancel.

	docs := repo.NewDocuments(pg.DB)
	pool := ingest.New(docs, ingest.NoopProcessor{}, ingest.Config{
		Size:         2,
		PollInterval: 20 * time.Millisecond,
		Logger:       silentLogger(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	// Give workers a moment to enter the idle-sleep branch.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pool run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not exit within 2s of cancel")
	}
}

func TestPoolConfigDefaults(t *testing.T) {
	t.Parallel()
	// New(store, proc, Config{}) should accept zero values and fall back
	// to the Default* constants — verified via Run returning promptly on
	// cancellation (any infinite-loop bug would hang). We pass a silent
	// logger to keep the nil-logger branch covered via the other tests
	// (NoopProcessor's Logger==nil path).
	pg := testutil.StartPostgres(t)
	_ = seedUser(t, pg, "ingest-defaults")

	// Cfg.Size=0 → DefaultPoolSize; PollInterval=0 → DefaultPollInterval;
	// Logger=nil → slog.Default(). We replace slog's default just for
	// this test so the "pool starting" log line doesn't leak to stderr.
	origDefault := slog.Default()
	slog.SetDefault(silentLogger())
	defer slog.SetDefault(origDefault)

	pool := ingest.New(repo.NewDocuments(pg.DB), ingest.NoopProcessor{}, ingest.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pool run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not exit")
	}
}

// TestPoolFailedStatusWriteUsesFreshContext guards the processOne
// fallback: if the parent ctx is already cancelled when a job fails,
// the pool MUST write 'failed' under a fresh short-deadline context —
// otherwise rows get stuck in 'processing' forever.
func TestPoolFailedStatusWriteUsesFreshContext(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	uid := seedUser(t, pg, "ingest-freshctx")
	ids := seedPending(t, pg, uid, 1)

	docs := repo.NewDocuments(pg.DB)
	// Processor that cancels the parent ctx mid-job and then returns an
	// error, forcing processOne to write under its fallback context.
	var cancelOnce sync.Once
	proc := ingest.ProcessorFunc(func(ctx context.Context, _ ingest.Job) error {
		cancelOnce.Do(func() {
			if cancel, ok := ctx.Value(cancelKey{}).(context.CancelFunc); ok {
				cancel()
			}
		})
		return errors.New("simulated failure")
	})
	pool := ingest.New(docs, proc, ingest.Config{
		Size:         1,
		PollInterval: 10 * time.Millisecond,
		Logger:       silentLogger(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, cancelKey{}, cancel)

	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	// Wait for status to flip to failed, then confirm run exits.
	waitFor(t, 3*time.Second, func() bool {
		return statusOf(t, pg, ids[0]) == repo.StatusFailed
	})
	cancel()
	<-done
}

type cancelKey struct{}

func TestNoopProcessorIsHarmless(t *testing.T) {
	t.Parallel()
	// Exercises the NoopProcessor log branch with a real logger so the
	// Logger!=nil path is covered.
	var buf slogBuffer
	p := ingest.NoopProcessor{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	if err := p.Process(context.Background(), ingest.Job{DocID: uuid.New(), Filename: "x.pdf"}); err != nil {
		t.Fatalf("noop: %v", err)
	}
	// And the Logger==nil branch.
	p2 := ingest.NoopProcessor{}
	if err := p2.Process(context.Background(), ingest.Job{}); err != nil {
		t.Fatalf("noop nil logger: %v", err)
	}
}

// slogBuffer is a trivial io.Writer wrapper (testing.T's Logf is not
// safe for concurrent use, so we stream to an in-memory buffer instead).
type slogBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *slogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// waitFor polls cond every 20ms until it returns true or the deadline
// passes. Panics the test with a clear message when the wait expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waitFor: condition never became true within %s", timeout)
}
