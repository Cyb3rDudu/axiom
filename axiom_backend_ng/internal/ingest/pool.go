// Package ingest owns the background worker pool that drains
// processing_status='pending' rows inserted by the upload handlers.
// Parity target: axiom_backend/services/background_document_processor.py.
//
// This slice intentionally ships the infrastructure only — polling,
// SKIP LOCKED claim, status transitions, graceful shutdown — behind a
// pluggable Processor. Subsequent slices bolt the real PDF/chunking/
// embedding stages onto the NoopProcessor contract.
package ingest

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// DefaultPollInterval matches the Python worker loop's idle sleep
// (axiom_backend/services/background_document_processor.py:147).
const DefaultPollInterval = 5 * time.Second

// DefaultPoolSize is conservative: the Python deployment runs a single
// worker, so parity is preserved until operators opt into more.
const DefaultPoolSize = 1

// Job is the worker-facing projection of a claimed document. Keeps
// enough fields for the Processor to locate the file on disk without
// re-reading the row.
type Job struct {
	DocID    uuid.UUID
	UserID   int32
	Filename string
	// FilePath is the absolute path recorded at upload time. May be
	// empty if the row was injected by a route that did not populate
	// it; the Processor must handle that case.
	FilePath string
	FileHash string
}

// Processor runs one pipeline iteration against a claimed job. It must
// not mutate processing_status — the pool owns the completed/failed
// transitions so errors are always surfaced uniformly.
type Processor interface {
	Process(ctx context.Context, job Job) error
}

// ProcessorFunc adapts an ordinary func to Processor.
type ProcessorFunc func(ctx context.Context, job Job) error

// Process implements Processor.
func (f ProcessorFunc) Process(ctx context.Context, job Job) error { return f(ctx, job) }

// NoopProcessor is the stand-in used in slice 1 of the ingest migration.
// It logs the job and succeeds — the goal is to exercise the pool +
// status-transition machinery end-to-end before the real PDF stages
// land.
type NoopProcessor struct{ Logger *slog.Logger }

// Process implements Processor.
func (p NoopProcessor) Process(ctx context.Context, job Job) error {
	if p.Logger != nil {
		p.Logger.Info("ingest noop",
			slog.String("doc_id", job.DocID.String()),
			slog.String("filename", job.Filename),
		)
	}
	_ = ctx
	return nil
}

// Store is the subset of repo.Documents the pool needs. Kept narrow so
// tests can inject a stub without wiring the full GORM graph.
type Store interface {
	ClaimPending(ctx context.Context) (repo.Document, error)
	MarkStatus(ctx context.Context, docID uuid.UUID, userID int32, in repo.MarkStatusInput) error
}

// Config is the tunable set for a Pool. Zero values fall back to the
// Default* constants so callers can pass Config{} when they only want
// the package defaults.
type Config struct {
	Size         int
	PollInterval time.Duration
	Logger       *slog.Logger
}

// Pool drains pending documents with Size concurrent workers.
type Pool struct {
	store Store
	proc  Processor
	size  int
	poll  time.Duration
	log   *slog.Logger
}

// New constructs a Pool. store and proc are required. Pass cfg.Size = 0
// for DefaultPoolSize (single-worker parity with the Python poller).
func New(store Store, proc Processor, cfg Config) *Pool {
	size := cfg.Size
	if size <= 0 {
		size = DefaultPoolSize
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Pool{store: store, proc: proc, size: size, poll: poll, log: log}
}

// Run spawns Size workers and blocks until ctx is cancelled. Every
// worker runs the same claim→process→mark loop; SKIP LOCKED in the
// repo layer guarantees two workers never see the same row.
//
// Returns nil on clean shutdown (ctx.Err() is context.Canceled or
// context.DeadlineExceeded). Any other error from a worker bubbles up.
func (p *Pool) Run(ctx context.Context) error {
	p.log.Info("ingest pool starting",
		slog.Int("size", p.size),
		slog.Duration("poll_interval", p.poll),
	)
	g, gctx := errgroup.WithContext(ctx)
	for i := 0; i < p.size; i++ {
		id := i
		g.Go(func() error { return p.runWorker(gctx, id) })
	}
	err := g.Wait()
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	p.log.Info("ingest pool stopped")
	return nil
}

// runWorker is one goroutine's loop. It returns when ctx is cancelled.
// Every other failure path is logged and swallowed so one bad job does
// not take the pool down.
func (p *Pool) runWorker(ctx context.Context, workerID int) error {
	log := p.log.With(slog.Int("worker", workerID))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		job, claimed, err := p.claimNext(ctx, log)
		if err != nil {
			log.Warn("claim failed", slog.String("error", err.Error()))
			if !p.sleep(ctx) {
				return ctx.Err()
			}
			continue
		}
		if !claimed {
			if !p.sleep(ctx) {
				return ctx.Err()
			}
			continue
		}
		p.processOne(ctx, job, log)
	}
}

// claimNext wraps ClaimPending, converting ErrNoPendingJobs into the
// (Job{}, false, nil) shape so the worker loop stays flat.
func (p *Pool) claimNext(ctx context.Context, log *slog.Logger) (Job, bool, error) {
	doc, err := p.store.ClaimPending(ctx)
	if err != nil {
		if errors.Is(err, repo.ErrNoPendingJobs) {
			return Job{}, false, nil
		}
		return Job{}, false, err
	}
	job := Job{
		DocID:    doc.ID,
		UserID:   doc.UserID,
		Filename: doc.Filename,
	}
	// Pull file_path + file_hash out of metadata_ so the Processor can
	// locate the staged file without re-reading the row.
	if doc.Metadata != nil {
		// Metadata is RawMessage; leave decoding to the Processor since
		// the on-disk path is not needed by the noop stage. File_path
		// lives on the models-level struct — this slice sticks to the
		// Document view and defers the richer Job projection.
		_ = doc.Metadata
	}
	log.Info("job claimed",
		slog.String("doc_id", job.DocID.String()),
		slog.String("filename", job.Filename),
	)
	return job, true, nil
}

// processOne runs the Processor for one job and writes the terminal
// status row. Both success and failure paths write under a fresh
// timeout context when the parent ctx is already cancelled — without
// this guard, a SIGTERM arriving during the tail of Processor.Process
// would leave the row stuck in 'processing' forever.
func (p *Pool) processOne(ctx context.Context, job Job, log *slog.Logger) {
	err := p.proc.Process(ctx, job)
	writeCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		writeCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	if err != nil {
		msg := err.Error()
		progress := int32(0)
		if err := p.store.MarkStatus(writeCtx, job.DocID, job.UserID, repo.MarkStatusInput{
			Status:   repo.StatusFailed,
			Progress: &progress,
			ErrorMsg: &msg,
		}); err != nil {
			log.Error("mark failed",
				slog.String("doc_id", job.DocID.String()),
				slog.String("error", err.Error()))
		}
		log.Warn("job failed",
			slog.String("doc_id", job.DocID.String()),
			slog.String("error", msg))
		return
	}
	progress := int32(100)
	if err := p.store.MarkStatus(writeCtx, job.DocID, job.UserID, repo.MarkStatusInput{
		Status:   repo.StatusCompleted,
		Progress: &progress,
	}); err != nil {
		log.Error("mark completed",
			slog.String("doc_id", job.DocID.String()),
			slog.String("error", err.Error()))
		return
	}
	log.Info("job completed", slog.String("doc_id", job.DocID.String()))
}

// sleep blocks for p.poll or until ctx is cancelled. Returns false iff
// cancellation won — the caller then exits its loop with ctx.Err().
func (p *Pool) sleep(ctx context.Context) bool {
	t := time.NewTimer(p.poll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
