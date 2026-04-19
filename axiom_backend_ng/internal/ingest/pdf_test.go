package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/ingest"
)

// fakeRunner is a CommandRunner that returns canned stdout/stderr/err,
// and writes a caller-supplied file at args[len-2] so the caller can
// simulate pdf_worker writing its markdown output to disk.
type fakeRunner struct {
	mu         sync.Mutex
	lastArgs   []string
	stdout     string
	stderr     string
	runErr     error
	writeMD    string // if non-empty, writes this to args[len-2]
	exitCode   int
	callsCount int
}

func (f *fakeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callsCount++
	f.lastArgs = args
	if f.writeMD != "" && len(args) >= 2 {
		// args for pdf_worker: ["-m", "ai_researcher.pdf_worker", pdf, md, images]
		// so the markdown path is args[len-2].
		mdPath := args[len(args)-2]
		if err := os.WriteFile(mdPath, []byte(f.writeMD), 0o644); err != nil {
			return nil, nil, 1, err
		}
	}
	return []byte(f.stdout), []byte(f.stderr), f.exitCode, f.runErr
}

// memorySink captures SetMarkdownPath calls so tests can assert on what
// the processor persisted.
type memorySink struct {
	mu   sync.Mutex
	path string
	err  error
}

func (m *memorySink) SetMarkdownPath(_ context.Context, _ uuid.UUID, _ int32, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.path = path
	return nil
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newPDFFixture(t *testing.T) (ingest.PDFProcessor, *memorySink, *fakeRunner, string) {
	t.Helper()
	dir := t.TempDir()
	mdDir := filepath.Join(dir, "md")
	imgDir := filepath.Join(dir, "img")
	sink := &memorySink{}
	runner := &fakeRunner{}
	p := ingest.PDFProcessor{
		Sink:        sink,
		Runner:      runner,
		PythonBin:   "python3",
		MarkdownDir: mdDir,
		ImagesDir:   imgDir,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return p, sink, runner, dir
}

func TestPDFProcessorPDFHappyPath(t *testing.T) {
	t.Parallel()
	p, sink, runner, dir := newPDFFixture(t)
	pdf := filepath.Join(dir, "paper.pdf")
	mustWrite(t, pdf, "%PDF-1.4 fake body")

	docID := uuid.New()
	// Simulate pdf_worker writing markdown and emitting the JSON line.
	runner.writeMD = "# Title\n\nBody.\n"
	runner.stdout = `2026-04-19 pdf-worker INFO converting...
{"ok": true, "markdown_path": "/ignored", "images_dir": "/ignored", "image_mapping": {"img.png": "image_0.png"}}
`

	err := p.Process(context.Background(), ingest.Job{
		DocID: docID, UserID: 1, Filename: "paper.pdf", FilePath: pdf,
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	// Runner invoked with right argv.
	wantPrefix := []string{"-m", ingest.PDFModule}
	for i, v := range wantPrefix {
		if runner.lastArgs[i] != v {
			t.Fatalf("argv[%d]=%q, want %q", i, runner.lastArgs[i], v)
		}
	}
	if runner.lastArgs[2] != pdf {
		t.Errorf("pdf arg: %q", runner.lastArgs[2])
	}
	if !strings.HasSuffix(runner.lastArgs[3], docID.String()+".md") {
		t.Errorf("md arg: %q", runner.lastArgs[3])
	}
	if !strings.HasSuffix(runner.lastArgs[4], docID.String()) {
		t.Errorf("images arg: %q", runner.lastArgs[4])
	}

	// Sink recorded the markdown path.
	if sink.path == "" || !strings.HasSuffix(sink.path, docID.String()+".md") {
		t.Errorf("sink path: %q", sink.path)
	}
	// And the file actually exists on disk.
	if _, err := os.Stat(sink.path); err != nil {
		t.Errorf("markdown file missing: %v", err)
	}
}

func TestPDFProcessorSubprocessFailure(t *testing.T) {
	t.Parallel()
	p, sink, runner, dir := newPDFFixture(t)
	pdf := filepath.Join(dir, "bad.pdf")
	mustWrite(t, pdf, "not a real pdf")
	runner.runErr = errors.New("exit 1")
	runner.stderr = `{"ok": false, "error": "Marker blew up"}`

	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "bad.pdf", FilePath: pdf,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "pdf_worker exec") {
		t.Errorf("unexpected error: %v", err)
	}
	if sink.path != "" {
		t.Errorf("sink should not have been called on failure, got %q", sink.path)
	}
}

func TestPDFProcessorTimesOutSubprocess(t *testing.T) {
	t.Parallel()
	p, _, runner, dir := newPDFFixture(t)
	pdf := filepath.Join(dir, "paper.pdf")
	mustWrite(t, pdf, "%PDF-1.4")
	// Make the fake runner report a deadline error — simulating what
	// exec.CommandContext surfaces when its ctx is cancelled.
	runner.runErr = context.DeadlineExceeded
	p.JobTimeout = 10 * time.Millisecond

	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "paper.pdf", FilePath: pdf,
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got %v", err)
	}
}

func TestPDFProcessorReportsJSONFailure(t *testing.T) {
	t.Parallel()
	p, _, runner, dir := newPDFFixture(t)
	pdf := filepath.Join(dir, "bad.pdf")
	mustWrite(t, pdf, "x")
	runner.stdout = `{"ok": false, "error": "marker empty"}`

	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "bad.pdf", FilePath: pdf,
	})
	if err == nil || !strings.Contains(err.Error(), "marker empty") {
		t.Fatalf("expected marker error, got %v", err)
	}
}

func TestPDFProcessorInvalidStdout(t *testing.T) {
	t.Parallel()
	p, _, runner, dir := newPDFFixture(t)
	pdf := filepath.Join(dir, "x.pdf")
	mustWrite(t, pdf, "x")
	runner.stdout = "no json here"

	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "x.pdf", FilePath: pdf,
	})
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestPDFProcessorMissingMarkdownAfterSuccess(t *testing.T) {
	t.Parallel()
	p, _, runner, dir := newPDFFixture(t)
	pdf := filepath.Join(dir, "x.pdf")
	mustWrite(t, pdf, "x")
	// Runner reports ok but DOES NOT write the markdown file.
	runner.stdout = `{"ok": true, "markdown_path": "/ignored", "images_dir": "/ignored", "image_mapping": {}}`

	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "x.pdf", FilePath: pdf,
	})
	if err == nil || !strings.Contains(err.Error(), "markdown missing") {
		t.Fatalf("expected missing-markdown error, got %v", err)
	}
}

func TestPDFProcessorMarkdownPassThrough(t *testing.T) {
	t.Parallel()
	p, sink, runner, dir := newPDFFixture(t)
	md := filepath.Join(dir, "notes.md")
	mustWrite(t, md, "# Hello\n\nBody.\n")

	docID := uuid.New()
	err := p.Process(context.Background(), ingest.Job{
		DocID: docID, UserID: 1, Filename: "notes.md", FilePath: md,
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if runner.callsCount != 0 {
		t.Errorf(".md should not invoke subprocess; calls=%d", runner.callsCount)
	}
	wantDst := filepath.Join(p.MarkdownDir, docID.String()+".md")
	if sink.path != wantDst {
		t.Errorf("sink path: %q, want %q", sink.path, wantDst)
	}
	body, err := os.ReadFile(wantDst)
	if err != nil {
		t.Fatalf("read copied md: %v", err)
	}
	if string(body) != "# Hello\n\nBody.\n" {
		t.Errorf("copied body mismatch: %q", body)
	}
}

func TestPDFProcessorMarkdownAliasExtension(t *testing.T) {
	t.Parallel()
	p, sink, _, dir := newPDFFixture(t)
	md := filepath.Join(dir, "notes.MARKDOWN") // mixed case + full extension
	mustWrite(t, md, "x")

	docID := uuid.New()
	err := p.Process(context.Background(), ingest.Job{
		DocID: docID, UserID: 1, Filename: filepath.Base(md), FilePath: md,
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if sink.path == "" {
		t.Error("markdown alias was not passed through")
	}
}

func TestPDFProcessorUnsupportedExtension(t *testing.T) {
	t.Parallel()
	p, _, _, dir := newPDFFixture(t)
	docx := filepath.Join(dir, "word.docx")
	mustWrite(t, docx, "x")

	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "word.docx", FilePath: docx,
	})
	if err == nil || !errors.Is(err, ingest.ErrUnsupportedFileType) {
		t.Fatalf("expected ErrUnsupportedFileType, got %v", err)
	}
}

func TestPDFProcessorMissingFilePath(t *testing.T) {
	t.Parallel()
	p, _, _, _ := newPDFFixture(t)
	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "x.pdf", FilePath: "",
	})
	if err == nil || !strings.Contains(err.Error(), "no file_path") {
		t.Fatalf("got %v", err)
	}
}

func TestPDFProcessorFileMissingOnDisk(t *testing.T) {
	t.Parallel()
	p, _, _, _ := newPDFFixture(t)
	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "nope.pdf", FilePath: "/tmp/does-not-exist-xyz.pdf",
	})
	if err == nil || !strings.Contains(err.Error(), "stat staged file") {
		t.Fatalf("got %v", err)
	}
}

func TestPDFProcessorMarkdownDirNotConfigured(t *testing.T) {
	t.Parallel()
	p, _, _, dir := newPDFFixture(t)
	p.MarkdownDir = ""
	pdf := filepath.Join(dir, "x.pdf")
	mustWrite(t, pdf, "x")

	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "x.pdf", FilePath: pdf,
	})
	if err == nil || !strings.Contains(err.Error(), "MarkdownDir") {
		t.Fatalf("got %v", err)
	}
}

func TestPDFProcessorImagesDirNotConfigured(t *testing.T) {
	t.Parallel()
	p, _, _, dir := newPDFFixture(t)
	p.ImagesDir = ""
	pdf := filepath.Join(dir, "x.pdf")
	mustWrite(t, pdf, "x")

	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "x.pdf", FilePath: pdf,
	})
	if err == nil || !strings.Contains(err.Error(), "ImagesDir") {
		t.Fatalf("got %v", err)
	}
}

func TestPDFProcessorMarkdownPassThroughSymlinkDstCannotLeakTarget(t *testing.T) {
	t.Parallel()
	p, _, _, dir := newPDFFixture(t)
	md := filepath.Join(dir, "notes.md")
	mustWrite(t, md, "# H\n\nBody.\n")

	// Plant a symlink at the destination path the processor will pick
	// (MarkdownDir/{doc_id}.md). Without O_NOFOLLOW, copyFile would
	// follow the symlink and truncate the target's contents, leaking
	// write access to wherever the symlink points. With O_NOFOLLOW +
	// O_EXCL the initial open fails with EEXIST (symlink itself
	// counts), the fallback unlinks the symlink (not its target),
	// and the retry writes to a regular file at dst. The sensitive
	// target file is never opened.
	if err := os.MkdirAll(p.MarkdownDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	docID := uuid.New()
	dst := filepath.Join(p.MarkdownDir, docID.String()+".md")
	target := filepath.Join(dir, "sensitive-file")
	mustWrite(t, target, "DO NOT TOUCH")
	if err := os.Symlink(target, dst); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := p.Process(context.Background(), ingest.Job{
		DocID: docID, UserID: 1, Filename: "notes.md", FilePath: md,
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// Target must be untouched.
	body, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read target: %v", readErr)
	}
	if string(body) != "DO NOT TOUCH" {
		t.Errorf("symlink target was modified: %q", body)
	}
	// Destination must now be a regular file, not a symlink.
	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("lstat dst: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("dst still a symlink after write: %v", info.Mode())
	}
	dstBody, _ := os.ReadFile(dst)
	if string(dstBody) != "# H\n\nBody.\n" {
		t.Errorf("dst body: %q", dstBody)
	}
}

func TestPDFProcessorMarkdownPassThroughReplacesStaleDst(t *testing.T) {
	t.Parallel()
	p, sink, _, dir := newPDFFixture(t)
	md := filepath.Join(dir, "notes.md")
	mustWrite(t, md, "fresh body\n")

	// Pre-create a regular file at the destination to simulate a
	// re-ingest. copyFile should unlink + recreate; final contents
	// must be the fresh body.
	if err := os.MkdirAll(p.MarkdownDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	docID := uuid.New()
	dst := filepath.Join(p.MarkdownDir, docID.String()+".md")
	mustWrite(t, dst, "stale body\n")

	if err := p.Process(context.Background(), ingest.Job{
		DocID: docID, UserID: 1, Filename: "notes.md", FilePath: md,
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	body, _ := os.ReadFile(dst)
	if string(body) != "fresh body\n" {
		t.Errorf("dst not replaced: %q", body)
	}
	if sink.path != dst {
		t.Errorf("sink path mismatch: %q", sink.path)
	}
}

func TestPDFProcessorMarkdownPassThroughMissingDir(t *testing.T) {
	t.Parallel()
	p, _, _, dir := newPDFFixture(t)
	p.MarkdownDir = ""
	md := filepath.Join(dir, "notes.md")
	mustWrite(t, md, "x")

	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "notes.md", FilePath: md,
	})
	if err == nil || !strings.Contains(err.Error(), "MarkdownDir") {
		t.Fatalf("got %v", err)
	}
}

func TestPDFProcessorMarkdownPassThroughCopyFails(t *testing.T) {
	t.Parallel()
	p, _, _, dir := newPDFFixture(t)
	// Remove the source file after Process() checks os.Stat but before
	// copy — easier: use a non-readable mode. Since POSIX allows root
	// to bypass perms, delete instead: create a dir at the source path,
	// so os.Open returns EISDIR.
	srcDir := filepath.Join(dir, "notes.md")
	if err := os.Mkdir(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "notes.md", FilePath: srcDir,
	})
	if err == nil || !strings.Contains(err.Error(), "copy markdown") {
		t.Fatalf("got %v", err)
	}
}

func TestPDFProcessorSinkError(t *testing.T) {
	t.Parallel()
	p, sink, runner, dir := newPDFFixture(t)
	pdf := filepath.Join(dir, "x.pdf")
	mustWrite(t, pdf, "x")
	runner.writeMD = "# md"
	runner.stdout = `{"ok": true, "markdown_path": "x", "images_dir": "y", "image_mapping": {}}`
	sink.err = errors.New("pg offline")

	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "x.pdf", FilePath: pdf,
	})
	if err == nil || !strings.Contains(err.Error(), "persist markdown path") {
		t.Fatalf("got %v", err)
	}
}

func TestExecRunnerCapsHugeOutput(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh available")
	}
	// Ask the shell to emit ~2 MiB to stdout; cap is 4 KiB so only
	// the first 4 KiB must land in the captured bytes.
	r := ingest.ExecRunner{OutputCap: 4 << 10}
	stdout, _, _, err := r.Run(context.Background(),
		"/bin/sh", "-c", `yes A | head -c 2097152`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stdout) != 4<<10 {
		t.Errorf("stdout cap honoured? len=%d (want 4096)", len(stdout))
	}
}

func TestCappedBufferDropsExcessBytes(t *testing.T) {
	t.Parallel()
	// Direct test of the cappedBuffer — via the ExecRunner path that
	// uses it. Write 10 times the cap; Bytes() should still be capped.
	r := ingest.ExecRunner{OutputCap: 256}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh available")
	}
	stdout, _, _, _ := r.Run(context.Background(), "/bin/sh", "-c", `yes | head -c 2560`)
	if len(stdout) != 256 {
		t.Errorf("cap not honoured across multiple writes: len=%d", len(stdout))
	}
}

// TestExecRunnerAgainstRealSubprocess exercises the production path
// with /bin/sh, so we know ExecRunner's stdout/stderr capture actually
// works end-to-end. No Python needed.
func TestExecRunnerAgainstRealSubprocess(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh available")
	}
	r := ingest.ExecRunner{}
	stdout, stderr, code, err := r.Run(context.Background(),
		"/bin/sh", "-c", "echo hi; echo oops 1>&2; exit 3")
	if err == nil {
		t.Error("expected non-zero exit error, got nil")
	}
	if code != 3 {
		t.Errorf("exit code: got %d, want 3", code)
	}
	if string(stdout) != "hi\n" {
		t.Errorf("stdout: %q", stdout)
	}
	if string(stderr) != "oops\n" {
		t.Errorf("stderr: %q", stderr)
	}
}

// TestExecRunnerSuccess covers the success branch (exit 0, nil err).
func TestExecRunnerSuccess(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh available")
	}
	r := ingest.ExecRunner{}
	stdout, _, code, err := r.Run(context.Background(), "/bin/sh", "-c", "echo done")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Errorf("code: %d", code)
	}
	if string(stdout) != "done\n" {
		t.Errorf("stdout: %q", stdout)
	}
}

// Ensure fmt.Stringer-like error messages remain intact if future slices
// add wrapping — we don't want internal paths to leak. Keep a simple
// check that truncate-length logic works.
func TestTruncateHelper(t *testing.T) {
	t.Parallel()
	// This just exercises convertPDF's truncate path via a stderr
	// longer than 512 bytes — the mock runner's runErr triggers it.
	p, _, runner, dir := newPDFFixture(t)
	pdf := filepath.Join(dir, "x.pdf")
	mustWrite(t, pdf, "x")
	runner.runErr = errors.New("boom")
	runner.stderr = strings.Repeat("A", 1024)

	err := p.Process(context.Background(), ingest.Job{
		DocID: uuid.New(), UserID: 1, Filename: "x.pdf", FilePath: pdf,
	})
	if err == nil {
		t.Fatal("want err")
	}
	if strings.Contains(err.Error(), strings.Repeat("A", 600)) {
		t.Error("stderr should have been truncated")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Error("truncated marker missing")
	}
	_ = fmt.Sprint(err) // ensure no panic formatting
}
