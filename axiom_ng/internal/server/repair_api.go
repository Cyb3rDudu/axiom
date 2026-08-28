// #184 — repair API: the fix-service surface. The RAG stays the ONLY
// Zotero gateway: the service polls cases, submits judge results, and (on
// auto-apply) the RAG applies quarantine → delete → create/upload in one
// handler — never exposing Zotero credentials.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repair"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

// SetRepairAPI wires the fix-service surface. writeBaseURL is the Zotero
// LOCAL server root (http://localhost:23119 — no /api suffix).
func (s *Server) SetRepairAPI(r *repo.Repo, write *zotero.WriteClient, quarantineRoot string) {
	s.repairRepo = r
	s.zoteroWrite = write
	s.quarantineRoot = quarantineRoot
	// routes are registered in Handler() (only when repairRepo != nil)
}

// repairQueueItem is one case plus everything the fix-service needs —
// analysis, the pdf path, and the document metadata for context.
type repairQueueItem struct {
	repo.RepairCase
	Title         string           `json:"title"`
	Creators      []zotero.Creator `json:"creators"`
	Publisher     string           `json:"publisher"`
	Year          int              `json:"publication_year"`
	AttachmentKey string           `json:"attachment_zotero_key"`
	DocumentKey   string           `json:"document_zotero_key"`
	LocalPath     string           `json:"local_path"`
	EPUBPath      string           `json:"epub_path,omitempty"`
}

func (s *Server) handleRepairQueue(w http.ResponseWriter, r *http.Request) {
	cases, err := s.repairRepo.ListRepairQueue(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := buildQueue(cases,
		func(c *repo.RepairCase) (*repairQueueItem, error) { return s.repairItemFor(r, c) },
		func(id, reason string) error { return s.repairRepo.BlockRepairCase(r.Context(), id, reason) })
	writeJSON(w, http.StatusOK, map[string]any{"cases": out})
}

// buildQueue assembles the listing, PARKING unreadable cases instead of
// silently skipping them: ErrNoRows from repairItemFor means the attachment
// OR document row is gone at the source (the JOIN makes both ErrNoRows) —
// the fix-service loop iterates exactly this listing, so a silent skip is
// an infinite re-serve (review W3a). Such a case is parked
// blocked_for_dudu('attachment-gone'); other read errors (transient DB)
// keep the skip but log loudly. The DB reason string stays the stable
// 'attachment-gone' prefix.
func buildQueue(cases []repo.RepairCase,
	itemFor func(*repo.RepairCase) (*repairQueueItem, error),
	block func(id, reason string) error) []repairQueueItem {
	out := make([]repairQueueItem, 0, len(cases))
	for _, c := range cases {
		item, err := itemFor(&c)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				if berr := block(c.ID, "attachment-gone"); berr != nil {
					log.Printf("repair %s: attachment-gone block fehlgeschlagen: %v", c.ID, berr)
				} else {
					log.Printf("repair %s: attachment-gone (attachment oder dokument weg) — case parked blocked_for_dudu", c.ID)
				}
			} else {
				log.Printf("repair %s: queue-item unlesbar: %v", c.ID, err)
			}
			continue
		}
		out = append(out, *item)
	}
	return out
}

func (s *Server) repairItemFor(r *http.Request, c *repo.RepairCase) (*repairQueueItem, error) {
	row := s.repairRepo.Pool().QueryRow(r.Context(), `
		SELECT d.title, d.creators, d.publication_year, d.zotero_key, COALESCE(d.publisher, ''),
		       a.zotero_key, a.local_path,
		       (SELECT a2.local_path FROM zotero_attachments a2
		        WHERE a2.document_id = d.id AND a2.deleted = false
		          AND a2.content_type = 'application/epub+zip'
		        ORDER BY a2.preferred DESC LIMIT 1)
		FROM zotero_attachments a JOIN zotero_documents d ON d.id = a.document_id
		WHERE a.id = $1 AND a.deleted = false`, c.AttachmentID)
	var it repairQueueItem
	it.RepairCase = *c
	var creators []byte
	var epub *string
	if err := row.Scan(&it.Title, &creators, &it.Year, &it.DocumentKey, &it.Publisher, &it.AttachmentKey, &it.LocalPath, &epub); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(creators, &it.Creators)
	if epub != nil {
		it.EPUBPath = *epub
	}
	return &it, nil
}

func (s *Server) handleRepairCases(w http.ResponseWriter, r *http.Request) {
	rows, err := s.repairRepo.Pool().Query(r.Context(), `
		SELECT c.id::text, c.status::text, c.attempts, c.suspicion_class,
		       COALESCE(c.verify_score, 0), COALESCE(c.verify_contradictions, 0),
		       COALESCE(c.verdict, ''), COALESCE(c.blocked_reason, ''),
		       d.title, c.updated_at
		FROM repair_cases c
		JOIN zotero_attachments a ON a.id = c.attachment_id
		JOIN zotero_documents d ON d.id = a.document_id
		ORDER BY c.updated_at DESC LIMIT 100`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type row struct {
		ID             string    `json:"id"`
		Status         string    `json:"status"`
		Attempts       int       `json:"attempts"`
		SuspicionClass string    `json:"suspicion_class"`
		VerifyScore    float64   `json:"verify_score"`
		Contradictions int       `json:"verify_contradictions"`
		Verdict        string    `json:"verdict,omitempty"`
		BlockedReason  string    `json:"blocked_reason,omitempty"`
		Title          string    `json:"title"`
		UpdatedAt      time.Time `json:"updated_at"`
	}
	out := []row{}
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.ID, &x.Status, &x.Attempts, &x.SuspicionClass, &x.VerifyScore,
			&x.Contradictions, &x.Verdict, &x.BlockedReason, &x.Title, &x.UpdatedAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, x)
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": out})
}

func (s *Server) handleRepairClaim(w http.ResponseWriter, r *http.Request) {
	c, err := s.repairRepo.ClaimRepairCase(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// handleRepairVerdict receives the judge result (multipart):
//
//	verdict=auto_apply|blocked|failed, score, contradictions, plan (JSON),
//	plan_version, blocked_reason — plus the HEALED PDF as file field
//	"healed_pdf" when verdict=auto_apply.
//
// The RAG re-enforces the gate (repo.SubmitRepairVerdict — score/contradictions
// are service-attested, blast radius bounded per the documented trust
// boundary). On auto-apply it applies quarantine → delete → create/upload
// (schema filename) → audit rows → healed, all before responding.
func (s *Server) handleRepairVerdict(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		// blocked/failed verdicts arrive urlencoded (no file) — accept both
		if err2 := r.ParseForm(); err2 != nil {
			http.Error(w, "form: "+err2.Error(), http.StatusBadRequest)
			return
		}
	}
	caseID := r.PathValue("id")
	verdict := r.FormValue("verdict")
	blockedReason := r.FormValue("blocked_reason")

	// Boundary validation BEFORE any state change (review W3b): a malformed
	// score used to degrade to 0.0 silently and a missing plan surfaced as a
	// raw pgx 409. 400 + clear message instead; 409 stays for genuine state
	// conflicts only. STRICT parsing (follow-up W3): ParseFloat rejects
	// trailing junk ("0.9abc") and comma decimals ("0,9") that Sscanf
	// silently truncated; NaN/Inf parse fine, so they are rejected here —
	// a NaN compares false against every gate threshold and would block the
	// case with a misleading reason instead of a 400.
	var score float64
	if raw := strings.TrimSpace(r.FormValue("score")); raw == "" {
		http.Error(w, "score fehlt", http.StatusBadRequest)
		return
	} else if v, err := strconv.ParseFloat(raw, 64); err != nil {
		http.Error(w, "score unlesbar: "+raw, http.StatusBadRequest)
		return
	} else if math.IsNaN(v) || math.IsInf(v, 0) {
		http.Error(w, "score ist nicht endlich: "+raw, http.StatusBadRequest)
		return
	} else {
		score = v
	}
	plan := json.RawMessage(r.FormValue("plan"))
	if len(plan) == 0 || !json.Valid(plan) {
		http.Error(w, "plan fehlt oder ist kein JSON", http.StatusBadRequest)
		return
	}
	var contradictions, planVersion int
	fmt.Sscanf(r.FormValue("contradictions"), "%d", &contradictions)
	fmt.Sscanf(r.FormValue("plan_version"), "%d", &planVersion)

	eff, err := s.repairRepo.SubmitRepairVerdict(r.Context(), caseID, plan, planVersion, score, contradictions, verdict, blockedReason)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if eff != repo.RepairInRepair {
		writeJSON(w, http.StatusOK, map[string]any{"effective": eff})
		return
	}

	// AUTO-APPLY — the only write path. healed PDF required.
	pdf, pdfErr := readHealedPDF(r)
	if pdfErr != nil {
		_ = s.repairRepo.MarkRepairFailed(r.Context(), caseID, pdfErr.Error())
		http.Error(w, pdfErr.Error(), http.StatusBadRequest)
		return
	}

	attID, attErr := attachmentIDForCase(r, s, caseID)
	if attErr != nil {
		_ = s.repairRepo.MarkRepairFailed(r.Context(), caseID, "case-attachment unlesbar: "+attErr.Error())
		http.Error(w, attErr.Error(), http.StatusInternalServerError)
		return
	}
	item, err := s.repairItemFor(r, &repo.RepairCase{AttachmentID: attID})
	if err != nil {
		_ = s.repairRepo.MarkRepairFailed(r.Context(), caseID, "attachment weg: "+err.Error())
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	srcPath := strings.TrimPrefix(item.LocalPath, "file://")

	body, status, err := s.applyRepair(r.Context(), liveRepairDeps{rep: s.repairRepo, write: s.zoteroWrite},
		caseID, planVersion, item, srcPath, pdf)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	body["effective"] = eff
	writeJSON(w, http.StatusOK, body)
}

// readHealedPDF extracts and validates the healed PDF from the multipart
// form (follow-up W1: extracted so the guards are unit-testable without
// the in_repair state the handler needs). Three guards: the file part must
// EXIST, be FULLY read (stdlib: a mid-read error SURFACES instead of
// silently uploading a truncated PDF — a manual loop swallowed it), and be
// NON-EMPTY — an empty healed file must not reach quarantine/delete/create,
// it would replace the original with a zero-byte husk (review W3b). The
// error text doubles as the blocked_reason for MarkRepairFailed.
func readHealedPDF(r *http.Request) ([]byte, error) {
	file, _, err := r.FormFile("healed_pdf")
	if err != nil {
		return nil, fmt.Errorf("auto-apply ohne geheilte PDF: %w", err)
	}
	defer file.Close()
	pdf, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("healed_pdf lesen: %w", err)
	}
	if len(pdf) == 0 {
		return nil, errors.New("healed_pdf ist leer")
	}
	return pdf, nil
}

// repairApplyDeps bundles every mutation of the auto-apply custody sequence
// behind one interface so the ORDERING is unit-testable without Postgres or
// a live Zotero (review W4). liveRepairDeps wires the real implementations.
type repairApplyDeps interface {
	Quarantine(root, zoteroKey, sourcePath string) (string, error)
	DeleteAttachment(key string) error
	CreateAttachmentWithFile(parentKey, filename, contentType string, pdf []byte) (string, error)
	MarkRepairFailed(ctx context.Context, caseID, reason string) error
	MarkRepairHealed(ctx context.Context, caseID string) error
	AuditWrite(ctx context.Context, caseID, attachmentID, action string, detail map[string]any) error
}

// liveRepairDeps adapts *repo.Repo + *zotero.WriteClient to repairApplyDeps.
type liveRepairDeps struct {
	rep   *repo.Repo
	write *zotero.WriteClient
}

func (d liveRepairDeps) Quarantine(root, key, src string) (string, error) {
	return repair.Quarantine(root, key, src)
}
func (d liveRepairDeps) DeleteAttachment(key string) error {
	return d.write.DeleteAttachmentItem(key)
}
func (d liveRepairDeps) CreateAttachmentWithFile(parent, filename, contentType string, pdf []byte) (string, error) {
	return d.write.CreateAttachmentWithFile(parent, filename, contentType, pdf)
}
func (d liveRepairDeps) MarkRepairFailed(ctx context.Context, caseID, reason string) error {
	return d.rep.MarkRepairFailed(ctx, caseID, reason)
}
func (d liveRepairDeps) MarkRepairHealed(ctx context.Context, caseID string) error {
	return d.rep.MarkRepairHealed(ctx, caseID)
}
func (d liveRepairDeps) AuditWrite(ctx context.Context, caseID, attachmentID, action string, detail map[string]any) error {
	return d.rep.AuditWrite(ctx, caseID, attachmentID, action, detail)
}

// applyRepair delegates to repair.Apply (#206: the custody sequence moved
// to internal/repair so the HTTP surface and the fixer invoker run the
// IDENTICAL ordering — it must never drift between duplicates). Kept as a
// method so the existing ordering tests (repair_api_test.go, review W4)
// keep pinning this call path unchanged.
func (s *Server) applyRepair(ctx context.Context, d repairApplyDeps, caseID string, planVersion int,
	item *repairQueueItem, srcPath string, pdf []byte) (map[string]any, int, error) {
	res, err := repair.Apply(ctx, d, s.quarantineRoot, repair.ApplyCase{
		CaseID:        caseID,
		AttachmentID:  item.AttachmentID,
		AttachmentKey: item.AttachmentKey,
		DocumentKey:   item.DocumentKey,
		Title:         item.Title,
		Creators:      item.Creators,
		Year:          item.Year,
		Publisher:     item.Publisher,
		SrcPath:       srcPath,
		PlanVersion:   planVersion,
	}, pdf)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, repair.ErrZoteroWrite) {
			status = http.StatusBadGateway
		}
		return nil, status, err
	}
	return map[string]any{
		"applied": true, "new_attachment_key": res.NewAttachmentKey,
		"filename": res.Filename, "quarantine": res.Quarantine,
	}, http.StatusOK, nil
}
func attachmentIDForCase(r *http.Request, s *Server, caseID string) (string, error) {
	var attID string
	if err := s.repairRepo.Pool().QueryRow(r.Context(),
		`SELECT attachment_id::text FROM repair_cases WHERE id=$1`, caseID).Scan(&attID); err != nil {
		return "", err
	}
	return attID, nil
}

// handleLocatorStats is the final proof endpoint of the loop: for a
// document it returns the page_source distribution of its ACTIVE chunks
// plus samples — the folio_verified evidence dudu watches for.
func (s *Server) handleLocatorStats(w http.ResponseWriter, r *http.Request) {
	docKey := r.PathValue("documentKey")
	rows, err := s.repairRepo.Pool().Query(r.Context(), `
		SELECT COALESCE(c.locator->>'page_source', 'legacy'), count(*),
		       (array_agg(c.id::text ORDER BY c.chunk_index))[1:3],
		       (array_agg(COALESCE(c.locator->>'page_label_start','') ORDER BY c.chunk_index))[1:3]
		FROM processing_chunks c
		JOIN processing_snapshots sn ON sn.id = c.snapshot_id AND sn.active
		JOIN zotero_attachments a ON a.id = sn.attachment_id AND a.deleted = false
		JOIN zotero_documents d ON d.id = a.document_id
		WHERE d.zotero_key = $1
		GROUP BY 1 ORDER BY 2 DESC`, docKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type dist struct {
		Source  string   `json:"page_source"`
		Chunks  int      `json:"chunks"`
		Samples []string `json:"sample_chunk_ids"`
		Labels  []string `json:"sample_labels"`
	}
	out := []dist{}
	for rows.Next() {
		var d dist
		var ids, labels []string
		if err := rows.Scan(&d.Source, &d.Chunks, &ids, &labels); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		d.Samples, d.Labels = ids, labels
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"document": docKey, "locator_stats": out})
}
