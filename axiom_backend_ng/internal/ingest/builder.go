package ingest

import (
	"log/slog"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/chunker"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/gpuworker"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/opensearch"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// BuilderOptions bundles the knobs main.go threads through when
// wiring the ingest pipeline. Nil / zero-valued fields degrade
// gracefully — see BuildProcessor for the matrix of fallbacks.
//
// This struct exists so main.go doesn't have to know the shape of
// every Processor, and so the builder is unit-testable without
// dragging in config.Load.
type BuilderOptions struct {
	// Documents / ChunkStore / EntityStore are the typed repo handles.
	Documents   *repo.Documents
	ChunkStore  *repo.Chunks
	EntityStore *repo.Entities

	// OpenSearch is the shared client. nil → indexer stage skipped.
	OpenSearch *opensearch.Client

	// GPUWorkerSocket is the path to the msgpack socket. Empty →
	// embeddings + entity stages run without GPU access
	// (SkipEmbeddings for chunks, noop for entities).
	GPUWorkerSocket string

	// PythonBin overrides the interpreter the PDF stage shells out to.
	// Empty → ingest.DefaultPythonBin ("python3").
	PythonBin string

	// MarkdownDir + ImagesDir are required for the PDF + chunk stages.
	// When either is empty the entire pipeline collapses to a
	// NoopProcessor so operators can still see liveness.
	MarkdownDir string
	ImagesDir   string

	// ChunkerConfig lets callers tune max/overlap/min tokens. Zero →
	// chunker.DefaultConfig().
	ChunkerConfig chunker.Config

	// GLiNERThreshold passes through to the entity stage. Zero →
	// DefaultGLiNERThreshold (0.45, matches Python).
	GLiNERThreshold float64

	// MaxMarkdownBytes / MaxSubprocessOutput let operators clamp
	// memory use if the defaults are wrong. Zero → package defaults.
	MaxMarkdownBytes    int64
	MaxSubprocessOutput int
	PersistReadTimeout  time.Duration // reserved for a future slice

	// JobTimeout caps how long pdf_worker can run per job. Zero →
	// DefaultJobTimeout (5 min).
	JobTimeout time.Duration

	Logger *slog.Logger
}

// BuildProcessor composes the ingest pipeline from BuilderOptions.
//
// Matrix of fallbacks (highest-value stage first):
//   - MarkdownDir + ImagesDir empty   → Chain is empty, caller gets a
//     NoopProcessor so the pool still proves liveness.
//   - GPUWorkerSocket empty           → chunks persist without
//     embeddings; entity stage is skipped entirely.
//   - OpenSearch client nil           → indexer stage is skipped.
//   - EntityStore nil                 → entity stage is skipped.
//
// Every non-terminal warning is logged so operators can see exactly
// which stages are degraded in production.
func BuildProcessor(opt BuilderOptions) Processor {
	log := opt.Logger
	if log == nil {
		log = slog.Default()
	}

	if opt.MarkdownDir == "" || opt.ImagesDir == "" {
		log.Warn("ingest pool running with NoopProcessor — set MarkdownDir + ImagesDir to enable conversion")
		return NoopProcessor{Logger: log}
	}

	var chain Chain
	chain = append(chain, PDFProcessor{
		Sink:        opt.Documents,
		Runner:      ExecRunner{OutputCap: opt.MaxSubprocessOutput},
		PythonBin:   opt.PythonBin,
		MarkdownDir: opt.MarkdownDir,
		ImagesDir:   opt.ImagesDir,
		Logger:      log,
		JobTimeout:  opt.JobTimeout,
	})

	var gpuClient *gpuworker.Client
	if opt.GPUWorkerSocket != "" {
		gpuClient = gpuworker.NewClient(opt.GPUWorkerSocket)
	} else {
		log.Warn("ingest chunk + entity stages running without GPU access — set GPUWorkerSocket to enable")
	}

	var embedder Embedder
	if gpuClient != nil {
		embedder = gpuClient
	}
	chain = append(chain, ChunkProcessor{
		Chunker:          chunker.New(opt.ChunkerConfig),
		Embedder:         embedder,
		Store:            opt.ChunkStore,
		StatusStore:      opt.Documents,
		MarkdownDir:      opt.MarkdownDir,
		Logger:           log,
		SkipEmbeddings:   embedder == nil,
		MaxMarkdownBytes: opt.MaxMarkdownBytes,
	})

	if opt.OpenSearch != nil {
		chain = append(chain, OpenSearchIndexer{
			Store:  opt.ChunkStore,
			Index:  opt.OpenSearch,
			Logger: log,
		})
	} else {
		log.Warn("ingest indexer stage disabled — enable OpenSearch to populate BM25 index")
	}

	if gpuClient != nil && opt.EntityStore != nil {
		chain = append(chain, EntityProcessor{
			Chunks:    opt.ChunkStore,
			Entities:  opt.EntityStore,
			GPU:       gpuClient,
			Threshold: opt.GLiNERThreshold,
			Logger:    log,
		})
	} else if opt.EntityStore == nil {
		log.Warn("ingest entity stage disabled — pass EntityStore to enable GLiNER extraction")
	}

	return chain
}
