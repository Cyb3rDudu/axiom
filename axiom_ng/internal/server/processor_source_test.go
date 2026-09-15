package server

// /api/processor/source endpoint tests. The repo lookup is faked; signature,
// expiry, status, lease and file streaming are exercised through the real
// handler + real sourceurl HMAC.

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/sourceurl"
	"github.com/jackc/pgx/v5"
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

// newLoggingSourceServer is the reason-logging variant: the Server's logger is
// captured so a test can assert WHICH internal 404 branch fired.
func newLoggingSourceServer(t *testing.T, secret string, fr *fakeSourceRepo) (*Server, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	s := New(":0", log.New(&buf, "", 0))
	if secret != "" {
		s.SetProcessorSourceSecret(secret)
	}
	if fr != nil {
		s.SetProcessorSourceRepo(fr)
	}
	return s, &buf
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
		LeaseFresh:  true,
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
	fr := &fakeSourceRepo{src: repo.ProcessorSource{Status: "processing", LeaseFresh: true}}
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

// #271 P0 clock-domain / #264 exp-freshness pin: the signed exp is
// authenticity material, not the freshness clock. This test represents the
// incident shape where a HOST-side signal (the exp minted against the host
// clock) says "expired" while the DB lease is fresh — the download must
// stream. The handler has no host clock to compare: freshness is the
// DB-evaluated LeaseFresh boolean. If a regression reintroduces
// `time.Now().After(leaseUntil)`, this test is red.
func TestProcessorSourceStaleHostSignalWithFreshDBLeaseStreams(t *testing.T) {
	file := filepath.Join(t.TempDir(), "book.pdf")
	content := []byte("%PDF-1.4 waited-past-exp")
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatal(err)
	}
	// exp minted one lease window past the ORIGINAL claim, long since past;
	// the lease in the DB is the renewal's fresh value.
	fr := &fakeSourceRepo{src: repo.ProcessorSource{
		LocalPath:   file,
		ContentType: "application/pdf",
		Status:      "processing",
		LeaseFresh:  true, // DB clock: still leased
	}}
	s, secret := newSourceTestServer(t, "topsecret", fr)

	exp := time.Now().Add(-10 * time.Minute).Unix()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, sourceURL(t, secret, "job-1", exp, ""), nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale exp + valid sig + fresh DB lease must stream)", rec.Code)
	}
	if got := rec.Body.Bytes(); string(got) != string(content) {
		t.Fatalf("body = %q, want %q", got, content)
	}
}

// TestProcessorSourceHandlerHasNoHostClock is the structural mutation guard
// for #271 P0: the handler must not consult the host wall clock for freshness.
// Removing the DB-domain predicate and reintroducing time.Now() in the handler
// makes this red.
func TestProcessorSourceHandlerHasNoHostClock(t *testing.T) {
	src, err := os.ReadFile("processor_source_api.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "time.Now(") {
		t.Fatal("processor_source_api.go reads the host clock (time.Now); " +
			"#271 P0 requires freshness exclusively from the DB (`lease_until > now()`)")
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
		LeaseFresh: true,
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
	// Real file + claimable status: only the LEASE (DB-domain) check can 404
	// here. Removing the `if !src.LeaseFresh` guard makes this test red.
	file := filepath.Join(t.TempDir(), "book.pdf")
	if err := os.WriteFile(file, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	fr := &fakeSourceRepo{src: repo.ProcessorSource{
		LocalPath:  file,
		Status:     "processing",
		LeaseFresh: false, // DB clock: lease has expired
	}}
	s, secret := newSourceTestServer(t, "topsecret", fr)
	exp := time.Now().Add(time.Minute).Unix()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, sourceURL(t, secret, "job-1", exp, ""), nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (DB lease expired)", rec.Code)
	}
}

func TestProcessorSourceDisabledWithoutSecret(t *testing.T) {
	// Even a perfectly signed URL against a real job 404s when no secret
	// is configured: the endpoint is OFF.
	fr := &fakeSourceRepo{src: repo.ProcessorSource{
		LocalPath:  filepath.Join(t.TempDir(), "book.pdf"), // real-ish; never reached
		Status:     "processing",
		LeaseFresh: true,
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

// --- #271 P1: internal rejection-reason logging --------------------------
//
// The wire response stays a uniform 404; only the internal log names the
// branch. Each test asserts the exact reason token, so removing a branch's
// sourceReject call (or folding branches back into one) turns the test red.

func TestProcessorSourceRejectReasonPerBranch(t *testing.T) {
	file := filepath.Join(t.TempDir(), "book.pdf")
	if err := os.WriteFile(file, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	cases := []struct {
		name    string
		secret  string
		repo    *fakeSourceRepo
		jobID   string
		exp     int64
		sig     string
		statErr error
		wantLog string
	}{
		{
			name: "disabled", secret: "", repo: nil, jobID: "job-1",
			exp: time.Now().Add(time.Minute).Unix(), wantLog: "reason=disabled_no_secret",
		},
		{
			name: "bad_exp", secret: "topsecret", repo: &fakeSourceRepo{}, jobID: "job-1",
			wantLog: "reason=bad_exp",
		}, {
			name: "bad_signature", secret: "topsecret", repo: &fakeSourceRepo{}, jobID: "job-1",
			exp: time.Now().Add(time.Minute).Unix(), sig: "deadbeef", wantLog: "reason=bad_signature",
		},
		{
			name: "unknown_job", secret: "topsecret", repo: &fakeSourceRepo{err: pgx.ErrNoRows}, jobID: "job-1",
			exp: time.Now().Add(time.Minute).Unix(), wantLog: "reason=unknown_job",
		},
		{
			name: "lookup_error", secret: "topsecret", repo: &fakeSourceRepo{err: errors.New("db down")}, jobID: "job-1",
			exp: time.Now().Add(time.Minute).Unix(), wantLog: "reason=lookup_error",
		},
		{
			name: "empty_path", secret: "topsecret", repo: &fakeSourceRepo{src: repo.ProcessorSource{Status: "processing", LeaseFresh: true}}, jobID: "job-1",
			exp: time.Now().Add(time.Minute).Unix(), wantLog: "reason=empty_path",
		},
		{
			name: "status", secret: "topsecret", repo: &fakeSourceRepo{src: repo.ProcessorSource{LocalPath: file, Status: "completed", LeaseFresh: true}}, jobID: "job-1",
			exp: time.Now().Add(time.Minute).Unix(), wantLog: "reason=status_completed",
		},
		{
			name: "lease_expired", secret: "topsecret", repo: &fakeSourceRepo{src: repo.ProcessorSource{LocalPath: file, Status: "processing", LeaseFresh: false}}, jobID: "job-1",
			exp: time.Now().Add(time.Minute).Unix(), wantLog: "reason=lease_expired",
		},
		{
			name: "open_failed", secret: "topsecret", repo: &fakeSourceRepo{src: repo.ProcessorSource{LocalPath: filepath.Join(dir, "missing.pdf"), Status: "processing", LeaseFresh: true}}, jobID: "job-1",
			exp: time.Now().Add(time.Minute).Unix(), wantLog: "reason=open_failed",
		},
		{
			// #273: the last unpinned branch — stat failure after a successful
			// open (race: file deleted/replaced between open and stat). Reached
			// via the injectable stat seam; removing the branch's sourceReject
			// call turns this red.
			name: "stat_failed", secret: "topsecret", statErr: errors.New("stat raced"), repo: &fakeSourceRepo{src: repo.ProcessorSource{LocalPath: file, Status: "processing", LeaseFresh: true}}, jobID: "job-1",
			exp: time.Now().Add(time.Minute).Unix(), wantLog: "reason=stat_failed",
		},
		{
			name: "not_regular", secret: "topsecret", repo: &fakeSourceRepo{src: repo.ProcessorSource{LocalPath: dir, Status: "processing", LeaseFresh: true}}, jobID: "job-1",
			exp: time.Now().Add(time.Minute).Unix(), wantLog: "reason=not_regular",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, buf := newLoggingSourceServer(t, tc.secret, tc.repo)
			if tc.statErr != nil {
				s.SetProcessorSourceStatFn(func(*os.File) (os.FileInfo, error) { return nil, tc.statErr })
			}
			rec := httptest.NewRecorder()
			rawURL := sourceURL(t, tc.secret, tc.jobID, tc.exp, tc.sig)
			if tc.name == "bad_exp" {
				rawURL = "/api/processor/source/job-1?exp=notanumber&sig=x"
			}
			req := httptest.NewRequest(http.MethodGet, rawURL, nil)
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("wire status = %d, want 404 (uniform, no oracle)", rec.Code)
			}
			if !strings.Contains(buf.String(), tc.wantLog) {
				t.Fatalf("log = %q, want substring %q", buf.String(), tc.wantLog)
			}
		})
	}
}
