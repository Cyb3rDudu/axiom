// #184 review W4: repair API hardening tests. (ii) pins the boundary
// validation (malformed score → 400, case state untouched because the
// guard fires BEFORE any repo call); (iii)+(iv) pin the auto-apply custody
// ORDER via applyRepair with fake deps — audit-fail-closed BEFORE the
// delete, quarantine before delete before create, healed only at the end.
// The full HTTP seam (submit → apply → healed against real Postgres +
// Zotero) stays IT-covered per the DSN proviso used across the repo.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// fakeApply records every custody call in order; each step can be failed.
type fakeApply struct {
	calls         []string
	quarantineErr error
	auditQuarErr  error
	deleteErr     error
	createErr     error
	healedErr     error
	failedReasons []string
}

func (f *fakeApply) Quarantine(root, key, src string) (string, error) {
	f.calls = append(f.calls, "quarantine")
	if f.quarantineErr != nil {
		return "", f.quarantineErr
	}
	return root + "/originals/" + key + ".pdf", nil
}

func (f *fakeApply) DeleteAttachment(key string) error {
	f.calls = append(f.calls, "delete")
	return f.deleteErr
}

func (f *fakeApply) CreateAttachmentWithFile(parent, filename string, pdf []byte) (string, error) {
	f.calls = append(f.calls, "create")
	if f.createErr != nil {
		return "", f.createErr
	}
	return "NEWKEY", nil
}

func (f *fakeApply) MarkRepairFailed(ctx context.Context, caseID, reason string) error {
	f.calls = append(f.calls, "failed")
	f.failedReasons = append(f.failedReasons, reason)
	return nil
}

func (f *fakeApply) MarkRepairHealed(ctx context.Context, caseID string) error {
	f.calls = append(f.calls, "healed")
	return f.healedErr
}

func (f *fakeApply) AuditWrite(ctx context.Context, caseID, attachmentID, action string, detail map[string]any) error {
	f.calls = append(f.calls, "audit:"+action)
	if action == "quarantine" {
		return f.auditQuarErr
	}
	return nil
}

func applyFixture() (*Server, *fakeApply, *repairQueueItem) {
	s := &Server{quarantineRoot: "/tmp/axiom_quarantine_test"}
	f := &fakeApply{}
	item := &repairQueueItem{
		RepairCase:    repo.RepairCase{AttachmentID: "att-1"},
		AttachmentKey: "ATTKEY", DocumentKey: "DOCKEY",
		Title: "Buch", Year: 2026,
	}
	return s, f, item
}

// (iii) custody nail: a failing quarantine AUDIT must fail closed BEFORE
// the delete — no unaudited mutation, the case is marked failed, and the
// Zotero write path is never entered.
func TestApplyRepairAuditFailClosedBeforeDelete(t *testing.T) {
	s, f, item := applyFixture()
	f.auditQuarErr = errors.New("db down")
	_, status, err := s.applyRepair(context.Background(), f, "case-1", 1, item, "/tmp/src.pdf", []byte("PDF"))
	if err == nil || status != http.StatusInternalServerError {
		t.Fatalf("audit-fehler muss 500/fail-closed sein, got %d/%v", status, err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "delete") {
			t.Fatalf("kein unauditierter delete erlaubt, calls=%v", f.calls)
		}
	}
	if len(f.failedReasons) != 1 || !strings.Contains(f.failedReasons[0], "quarantine-audit") {
		t.Fatalf("case muss als failed mit quarantine-audit markiert werden, got %v", f.failedReasons)
	}
}

// (iv) happy path ORDER: quarantine → quarantine-audit → delete →
// delete-audit → create → create-audit → healed. Moving any step breaks
// the custody contract this test pins.
func TestApplyRepairCustodyOrder(t *testing.T) {
	s, f, item := applyFixture()
	body, status, err := s.applyRepair(context.Background(), f, "case-1", 1, item, "/tmp/src.pdf", []byte("PDF"))
	if err != nil || status != http.StatusOK {
		t.Fatalf("apply: %d %v", status, err)
	}
	want := []string{"quarantine", "audit:quarantine", "delete", "audit:delete_attachment",
		"create", "audit:create_attachment", "healed"}
	if len(f.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	for i := range want {
		if f.calls[i] != want[i] {
			t.Fatalf("custody order: call %d = %q, want %q (all: %v)", i, f.calls[i], want[i], f.calls)
		}
	}
	if body["new_attachment_key"] != "NEWKEY" || body["applied"] != true {
		t.Fatalf("body: %v", body)
	}
}

// (iii-var) a failing delete marks the case failed and never creates the
// healed attachment (half-mutated state stays quarantine+deleted, the
// original is recoverable from quarantine).
func TestApplyRepairDeleteFailureStopsCreate(t *testing.T) {
	s, f, item := applyFixture()
	f.deleteErr = errors.New("zotero 502")
	_, status, err := s.applyRepair(context.Background(), f, "case-1", 1, item, "/tmp/src.pdf", []byte("PDF"))
	if err == nil || status != http.StatusBadGateway {
		t.Fatalf("delete-fehler muss 502 sein, got %d/%v", status, err)
	}
	for _, c := range f.calls {
		if c == "create" {
			t.Fatalf("nach delete-fehler darf kein create stattfinden, calls=%v", f.calls)
		}
	}
	if len(f.failedReasons) != 1 || !strings.Contains(f.failedReasons[0], "zotero delete") {
		t.Fatalf("failed-reason, got %v", f.failedReasons)
	}
}

// (ii) boundary validation: a malformed score must 400 BEFORE any repo
// interaction — the handler runs on a zero Server (nil repairRepo); if the
// guard were missing this would nil-panic or reach the DB. Follow-up W3:
// STRICT ParseFloat — trailing junk ("0.9abc"), comma decimals ("0,9") and
// non-finite values ("NaN", "Inf") all 400 instead of silently degrading
// to 0.0 or a NaN that compares false against the gate.
func TestRepairVerdictMalformedScoreRejected(t *testing.T) {
	s := &Server{} // zero value: nothing may be touched before the 400
	r := chi.NewRouter()
	r.Post("/api/repair/cases/{id}/verdict", s.handleRepairVerdict)

	for _, form := range []string{
		"verdict=blocked&score=abc&plan=%7B%7D",
		"verdict=blocked&plan=%7B%7D",              // score missing entirely
		"verdict=blocked&score=0%2C9&plan=%7B%7D",  // comma decimal: 400, not 0.0
		"verdict=blocked&score=0.9abc&plan=%7B%7D", // trailing junk: 400
		"verdict=blocked&score=NaN&plan=%7B%7D",    // non-finite: 400
		"verdict=blocked&score=Inf&plan=%7B%7D",    // non-finite: 400
		"verdict=blocked&score=0.9&plan=",          // plan missing
		"verdict=blocked&score=0.9&plan=not-json",  // plan garbage
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/repair/cases/x/verdict", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("form %q: expected 400, got %d %s", form, rec.Code, rec.Body.String())
		}
	}
}

// multipart request helper: a healed_pdf part with the given content (or
// no part at all when hasFile is false).
func healedPDFRequest(t *testing.T, hasFile bool, content string) *http.Request {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "--BOUND\r\n")
	if hasFile {
		b.WriteString("Content-Disposition: form-data; name=\"healed_pdf\"; filename=\"h.pdf\"\r\n")
		b.WriteString("Content-Type: application/pdf\r\n\r\n")
		b.WriteString(content)
		b.WriteString("\r\n")
	}
	b.WriteString("--BOUND--\r\n")
	req := httptest.NewRequest(http.MethodPost, "/api/repair/cases/x/verdict", strings.NewReader(b.String()))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=BOUND")
	return req
}

// (i-lt) follow-up W1: the healed-PDF guards are pinned DIRECTLY on
// readHealedPDF — missing part fails, content round-trips, and an EMPTY
// part fails with 'healed_pdf ist leer' so a zero-byte husk can never
// reach quarantine/delete/create. The in_repair→MarkRepairFailed HTTP
// flow around it needs the IT DSN (DSN proviso).
func TestReadHealedPDFEmptyGuard(t *testing.T) {
	if _, err := readHealedPDF(healedPDFRequest(t, false, "")); err == nil {
		t.Fatal("fehlender healed_pdf-part muss fehlschlagen")
	} else if !strings.Contains(err.Error(), "ohne geheilte PDF") {
		t.Fatalf("fehlender part: unerwarteter fehler %v", err)
	}
	pdf, err := readHealedPDF(healedPDFRequest(t, true, "PDFBYTES"))
	if err != nil || string(pdf) != "PDFBYTES" {
		t.Fatalf("content: pdf=%q err=%v", pdf, err)
	}
	if _, err := readHealedPDF(healedPDFRequest(t, true, "")); err == nil || !strings.Contains(err.Error(), "ist leer") {
		t.Fatalf("leere pdf muss mit 'ist leer' fehlschlagen, got %v", err)
	}
}

// follow-up W1c: buildQueue parks a GONE-source case (ErrNoRows from
// repairItemFor — attachment OR document row missing) via BlockRepairCase
// ("attachment-gone") and OMITS it; a transient read error omits WITHOUT
// blocking (the case stays queued for the next poll); readable cases are
// served. This is the anti-infinite-reserve policy of review W3a.
func TestBuildQueueParksGoneAttachments(t *testing.T) {
	gone := repo.RepairCase{ID: "gone-1"}
	transient := repo.RepairCase{ID: "db-1"}
	ok := repo.RepairCase{ID: "ok-1"}
	var blocked []string
	out := buildQueue([]repo.RepairCase{gone, transient, ok},
		func(c *repo.RepairCase) (*repairQueueItem, error) {
			switch c.ID {
			case "gone-1":
				return nil, pgx.ErrNoRows
			case "db-1":
				return nil, errors.New("conn reset")
			}
			return &repairQueueItem{RepairCase: *c, Title: "Buch"}, nil
		},
		func(id, reason string) error {
			if reason != "attachment-gone" {
				t.Errorf("block reason = %q, want attachment-gone", reason)
			}
			blocked = append(blocked, id)
			return nil
		})
	if len(blocked) != 1 || blocked[0] != "gone-1" {
		t.Fatalf("nur der gone-case darf geblockt werden, got %v", blocked)
	}
	if len(out) != 1 || out[0].ID != "ok-1" || out[0].Title != "Buch" {
		t.Fatalf("nur der lesbare case wird geliefert, got %+v", out)
	}
}
