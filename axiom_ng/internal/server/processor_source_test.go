package server

// /api/processor/source endpoint tests. The repo lookup is faked; signature,
// expiry, status, lease and file streaming are exercised through the real
// handler + real sourceurl HMAC.

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/sourceurl"
)

type fakeSourceRepo struct {
	src   repo.ProcessorSource
	err   error
	asked int
}

func (f *fakeSourceRepo) ProcessorSource(_ context.Context, _ string) (repo.ProcessorSource, error) {
	f.asked++
	return f.src, f.err
}

func newSourceTestServer(t *testing.T, secret string, fr *fakeSourceRepo) (*Server, string) {
	t.Helper()
	s := New(":0", log.Default())
	if secret != "" {
		s.SetProcessorSourceSecret(secret)
	}
	if fr != nil {
		s.SetProcessorSourceRepo(fr)
	}
	return s, secret
}

func sourceURL(t *testing.T, secret, jobID string, exp int64, sigOverride string) string {
	t.Helper()
	sig := sourceurl.Sign(secret, jobID, exp)
	if sigOverride != "" {
		sig = sigOverride
	}
	return "/api/processor/source/" + jobID + "?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + sig
}

func TestProcessorSourceValidTokenStreams(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "book.pdf")
	content := []byte("%PDF-1.4 test-bytes")
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatal(err)
	}
	fr := &fakeSourceRepo{src: repo.ProcessorSource{
		LocalPath:   file,
		ContentType: "application/pdf",
		Status:      "processing",
		LeaseUntil:  time.Now().Add(5 * time.Minute),
	}}
	s, secret := newSourceTestServer(t, "topsecret", fr)

	exp := time.Now().Add(time.Minute).Unix()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, sourceURL(t, secret, "job-1", exp, ""), nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.Bytes(); string(got) != string(content) {
		t.Fatalf("body = %q, want %q", got, content)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestProcessorSourceWrongSignature404(t *testing.T) {
	fr := &fakeSourceRepo{src: repo.ProcessorSource{Status: "processing", LeaseUntil: time.Now().Add(time.Minute)}}
	s, secret := newSourceTestServer(t, "topsecret", fr)
	exp := time.Now().Add(time.Minute).Unix()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, sourceURL(t, secret, "job-1", exp, "deadbeef"), nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (bad sig must not reach the DB)", rec.Code)
	}
	if fr.asked != 0 {
		t.Fatal("bad signature must fail BEFORE the DB lookup (no oracle)")
	}
}

func TestProcessorSourceExpiredExp404(t *testing.T) {
	fr := &fakeSourceRepo{src: repo.ProcessorSource{Status: "processing", LeaseUntil: time.Now().Add(time.Minute)}}
	s, secret := newSourceTestServer(t, "topsecret", fr)

	exp := time.Now().Add(-time.Second).Unix()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, sourceURL(t, secret, "job-1", exp, ""), nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if fr.asked != 0 {
		t.Fatal("expired exp must fail BEFORE the DB lookup")
	}
}

func TestProcessorSourceCompletedJob404(t *testing.T) {
	// Real file on disk: the earlier guards (path lookup) must not
	// short-circuit — this test proves the STATUS check itself 404s.
	file := filepath.Join(t.TempDir(), "book.pdf")
	if err := os.WriteFile(file, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	fr := &fakeSourceRepo{src: repo.ProcessorSource{
		LocalPath:  file,
		Status:     "completed",
		LeaseUntil: time.Now().Add(time.Minute),
	}}
	s, secret := newSourceTestServer(t, "topsecret", fr)
	exp := time.Now().Add(time.Minute).Unix()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, sourceURL(t, secret, "job-1", exp, ""), nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (terminal job)", rec.Code)
	}
}

func TestProcessorSourceExpiredLease404(t *testing.T) {
	// Real file + claimable status: only the LEASE check can 404 here.
	file := filepath.Join(t.TempDir(), "book.pdf")
	if err := os.WriteFile(file, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	fr := &fakeSourceRepo{src: repo.ProcessorSource{
		LocalPath:  file,
		Status:     "processing",
		LeaseUntil: time.Now().Add(-time.Minute),
	}}
	s, secret := newSourceTestServer(t, "topsecret", fr)
	exp := time.Now().Add(time.Minute).Unix()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, sourceURL(t, secret, "job-1", exp, ""), nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (lease expired)", rec.Code)
	}
}

func TestProcessorSourceDisabledWithoutSecret(t *testing.T) {
	// Even a perfectly signed URL against a real job 404s when no secret
	// is configured: the endpoint is OFF.
	fr := &fakeSourceRepo{src: repo.ProcessorSource{
		LocalPath:  filepath.Join(t.TempDir(), "book.pdf"), // real-ish; never reached
		Status:     "processing",
		LeaseUntil: time.Now().Add(time.Minute),
	}}
	s, _ := newSourceTestServer(t, "", fr)
	exp := time.Now().Add(time.Minute).Unix()

	rec := httptest.NewRecorder()
	// Sign with the EMPTY secret: the signature check alone would PASS, so
	// only the disabled-check can produce this 404.
	req := httptest.NewRequest(http.MethodGet, sourceURL(t, "", "job-1", exp, ""), nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (disabled)", rec.Code)
	}
	if fr.asked != 0 {
		t.Fatal("disabled endpoint must never touch the repo")
	}
}
