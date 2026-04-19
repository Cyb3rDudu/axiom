package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// DefaultPythonBin is the interpreter invoked when PDFProcessor.PythonBin is
// empty. Matches the Python entrypoint the doc-processor container uses.
const DefaultPythonBin = "python3"

// PDFModule is the pdf_worker entry point; passed after `-m`.
const PDFModule = "ai_researcher.pdf_worker"

// MarkdownSink is the subset of repo.Documents PDFProcessor needs to
// persist the converted markdown location back to the row.
type MarkdownSink interface {
	SetMarkdownPath(ctx context.Context, docID uuid.UUID, userID int32, path string) error
}

// CommandRunner runs a subprocess and returns captured output + exit
// code. Extracted so tests can inject a fake that mimics pdf_worker's
// JSON-on-stdout protocol without needing Python installed.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr []byte, exitCode int, err error)
}

// DefaultSubprocessOutputCap limits how much stdout/stderr ExecRunner
// buffers per call. A misbehaving pdf_worker that emits gigabytes of
// logs would otherwise pin the ingest worker's RSS; 16 MiB is plenty
// for pdf_worker's real output (a JSON result line + Marker's
// progress log).
const DefaultSubprocessOutputCap = 16 << 20

// ExecRunner is the production CommandRunner — a thin wrapper over
// exec.CommandContext that captures stdout + stderr separately, each
// bounded by OutputCap.
type ExecRunner struct {
	// OutputCap is the per-stream byte ceiling. Zero →
	// DefaultSubprocessOutputCap.
	OutputCap int
}

// Run implements CommandRunner.
func (r ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, int, error) {
	capSize := r.OutputCap
	if capSize <= 0 {
		capSize = DefaultSubprocessOutputCap
	}
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // caller-controlled, vetted path
	stdout := &cappedBuffer{cap: capSize}
	stderr := &cappedBuffer{cap: capSize}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	code := cmd.ProcessState.ExitCode()
	return stdout.Bytes(), stderr.Bytes(), code, err
}

// cappedBuffer is a bytes.Buffer-style writer that silently drops
// writes once cap bytes have been accumulated. The subprocess does
// not see back-pressure (we do not want to deadlock pdf_worker), it
// just never sees its excess output reflected back in the Go buffer.
type cappedBuffer struct {
	buf bytes.Buffer
	cap int
}

// Write implements io.Writer. Bytes past the cap are dropped but
// reported as written so the subprocess's writer does not block.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.cap - c.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) <= remaining {
		return c.buf.Write(p)
	}
	if _, err := c.buf.Write(p[:remaining]); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Bytes returns the captured (possibly truncated) contents.
func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

// PDFProcessor converts an uploaded file to markdown. PDFs are shelled
// out to the Python pdf_worker (parity with
// axiom_backend/services/background_document_processor.py:
// _convert_pdf_via_subprocess). Markdown-like files (.md / .markdown)
// are passed through to the markdown directory unmodified so the rest
// of the pipeline has a canonical path to read from.
//
// Unsupported file types (notably .doc / .docx in this slice) return
// ErrUnsupportedFileType so the pool marks the row as failed — Python
// still handles those until a later slice.
type PDFProcessor struct {
	Sink        MarkdownSink
	Runner      CommandRunner
	PythonBin   string
	MarkdownDir string // where to write {doc_id}.md
	ImagesDir   string // where to write {doc_id}/image_N.ext
	Logger      *slog.Logger
}

// ErrUnsupportedFileType is returned when the Processor cannot handle
// the input extension. Callers (the Pool) translate this to a failed
// status with the error message stored in metadata_.processing_error.
var ErrUnsupportedFileType = errors.New("unsupported file type")

// pdfWorkerResult is the JSON payload pdf_worker emits on stdout.
type pdfWorkerResult struct {
	OK           bool              `json:"ok"`
	MarkdownPath string            `json:"markdown_path"`
	ImagesDir    string            `json:"images_dir"`
	ImageMapping map[string]string `json:"image_mapping"`
	Error        string            `json:"error"`
}

// Process implements Processor. Branches on file extension:
//   - .pdf → pdf_worker subprocess
//   - .md / .markdown → copy to MarkdownDir/{doc_id}.md verbatim
//   - anything else → ErrUnsupportedFileType
func (p PDFProcessor) Process(ctx context.Context, job Job) error {
	if job.FilePath == "" {
		return errors.New("job has no file_path")
	}
	if _, err := os.Stat(job.FilePath); err != nil {
		return fmt.Errorf("stat staged file: %w", err)
	}
	ext := strings.ToLower(filepath.Ext(job.Filename))
	switch ext {
	case ".pdf":
		return p.convertPDF(ctx, job)
	case ".md", ".markdown":
		return p.passThroughMarkdown(ctx, job)
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedFileType, ext)
	}
}

// convertPDF shells out to the Python pdf_worker. Layout mirrors the
// Python doc-processor: markdown under MarkdownDir/{doc_id}.md, images
// under ImagesDir/{doc_id}/image_N.ext. Returns on any failure; the
// Pool turns the error into a 'failed' status row.
func (p PDFProcessor) convertPDF(ctx context.Context, job Job) error {
	if p.MarkdownDir == "" {
		return errors.New("pdf_processor: MarkdownDir not configured")
	}
	if p.ImagesDir == "" {
		return errors.New("pdf_processor: ImagesDir not configured")
	}
	if err := os.MkdirAll(p.MarkdownDir, 0o755); err != nil {
		return fmt.Errorf("mkdir markdown dir: %w", err)
	}
	imageDir := filepath.Join(p.ImagesDir, job.DocID.String())
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return fmt.Errorf("mkdir image dir: %w", err)
	}

	mdPath := filepath.Join(p.MarkdownDir, job.DocID.String()+".md")
	python := p.PythonBin
	if python == "" {
		python = DefaultPythonBin
	}
	runner := p.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	log := p.Logger
	if log == nil {
		log = slog.Default()
	}
	log.Info("pdf_worker invoking",
		slog.String("doc_id", job.DocID.String()),
		slog.String("pdf", job.FilePath),
		slog.String("markdown", mdPath),
		slog.String("images", imageDir),
	)

	stdout, stderr, _, err := runner.Run(ctx, python, "-m", PDFModule, job.FilePath, mdPath, imageDir)
	if err != nil {
		return fmt.Errorf("pdf_worker exec: %w; stderr=%s", err, truncate(stderr, 512))
	}

	// pdf_worker writes multiple stdout lines (logger output included).
	// The last JSON object in stdout is the authoritative result.
	result, perr := parsePDFWorkerResult(stdout)
	if perr != nil {
		return fmt.Errorf("pdf_worker parse: %w; stderr=%s", perr, truncate(stderr, 512))
	}
	if !result.OK {
		return fmt.Errorf("pdf_worker failure: %s", result.Error)
	}
	if _, err := os.Stat(mdPath); err != nil {
		return fmt.Errorf("pdf_worker reported success but markdown missing: %w", err)
	}
	if err := p.Sink.SetMarkdownPath(ctx, job.DocID, job.UserID, mdPath); err != nil {
		return fmt.Errorf("persist markdown path: %w", err)
	}
	log.Info("pdf_worker done",
		slog.String("doc_id", job.DocID.String()),
		slog.Int("images", len(result.ImageMapping)),
	)
	return nil
}

// passThroughMarkdown copies the uploaded .md / .markdown file into the
// markdown dir under the canonical {doc_id}.md name so downstream
// stages can always read from MarkdownDir.
func (p PDFProcessor) passThroughMarkdown(ctx context.Context, job Job) error {
	if p.MarkdownDir == "" {
		return errors.New("pdf_processor: MarkdownDir not configured")
	}
	if err := os.MkdirAll(p.MarkdownDir, 0o755); err != nil {
		return fmt.Errorf("mkdir markdown dir: %w", err)
	}
	dst := filepath.Join(p.MarkdownDir, job.DocID.String()+".md")
	if err := copyFile(job.FilePath, dst); err != nil {
		return fmt.Errorf("copy markdown: %w", err)
	}
	return p.Sink.SetMarkdownPath(ctx, job.DocID, job.UserID, dst)
}

// parsePDFWorkerResult scans stdout for the last JSON object line
// produced by pdf_worker. pdf_worker may emit logger lines before the
// result, so we walk the lines from the end and pick the first one
// that looks like JSON.
func parsePDFWorkerResult(stdout []byte) (pdfWorkerResult, error) {
	lines := strings.Split(string(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			continue
		}
		var r pdfWorkerResult
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		return r, nil
	}
	return pdfWorkerResult{}, errors.New("no JSON result line in stdout")
}

// copyFile copies src to dst with 0o644 perms. Preserves content only.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // caller-validated path
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// truncate clips b to at most n bytes for safe log inclusion.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
