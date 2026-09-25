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
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zoteroprovider"
)

// SetRepairAPI wires the fix-service surface. writeBaseURL is the Zotero
// LOCAL server root (http://localhost:23119 — no /api suffix).
func (s *Server) SetRepairAPI(r *repo.Repo, write *zoteroprovider.WriteClient, quarantineRoot string) {
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
	Creators      []zoteroprovider.Creator `json:"creators"`
	ExistingNames []string         `json:"existing_attachment_names,omitempty"` // #291 grown-pattern refs
	Year          int              `json:"publication_year"`
	AttachmentKey string           `json:"attachment_zotero_key"`
	DocumentKey   string           `json:"document_zotero_key"`
	LocalPath     string           `json:"local_path"`
	EPUBPath      string           `json:"epub_path,omitempty"`
	ContentType   string           `json:"content_type"`
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
		SELECT d.title, d.creators, COALESCE(d.publication_year, 0), d.zotero_key,
		       a.zotero_key, a.local_path, COALESCE(a.content_type, 'application/pdf'),
		       (SELECT a2.local_path FROM zotero_attachments a2
		        WHERE a2.document_id = d.id AND a2.deleted = false
		          AND a2.content_type = 'application/epub+zip'
		        ORDER BY a2.preferred DESC, a2.filename ASC LIMIT 1),
		       `+repo.ExistingNamesSubquery+`
		FROM zotero_attachments a JOIN zotero_documents d ON d.id = a.document_id
		WHERE a.id = $1 AND a.deleted = false`, c.AttachmentID)
	var it repairQueueItem
	it.RepairCase = *c
	var creators []byte
	var epub *string
	if err := row.Scan(&it.Title, &creators, &it.Year, &it.DocumentKey, &it.AttachmentKey, &it.LocalPath, &epub, &it.ContentType, &it.ExistingNames); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(creators, &it.Creators)
	if epub != nil {
		it.EPUBPath = *epub
	}
	return &it, nil
}

// handleRepairCustody is the #279 manual-repair tool: ONE call runs the
// quarantine-first custody protocol for a librarian-repaired file — the
// same ordering as the fixer's auto-apply (repair.Apply), so the repaired
// attachment lands PREFERRED without library surgery (no stranded
// siblings). Replaces the improvised "upload sibling + trash old" route
// from the Geursen incident.
//
//	POST /api/repair/custody
//	  attachment_key  — the broken attachment's Zotero key (required)
//	  healed_file     — the repaired file, multipart (required, non-empty)
//	  reason          — free text for the custody record (required)
//	  content_type    — application/pdf (default) | application/epub+zip
//
// Steps (each audited into <quarantine-root>/manual/<KEY>.json):
// quarantine the original → delete the old item (version-guarded; a 404
// from an aborted earlier run counts as done) → upload the healed file
// under the parent with a SCHEMA filename → step report. Idempotent
// re-run: a record with terminal status "healed" is refused (409) — a
// re-run would upload a duplicate healed sibling. A record with a recorded
// create key but no terminal status is ALSO refused (409, different text):
// the create may have succeeded server-side, so Zotero is checked first.
func (s *Server) handleRepairCustody(w http.ResponseWriter, r *http.Request) {
	if s.zoteroWrite == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "custody needs the zotero write client (SetRepairAPI)"})
		return
	}
	key := strings.TrimSpace(r.FormValue("attachment_key"))
	if key == "" {
		http.Error(w, "attachment_key fehlt", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		http.Error(w, "reason fehlt (Kontext für das Quarantäne-Protokoll)", http.StatusBadRequest)
		return
	}

	// Attachment lookup by Zotero key: original local path (quarantine
	// source), parent key + metadata (schema filename), content type.
	item, err := s.custodyItemFor(r, key)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "attachment "+key+" unbekannt oder gelöscht", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Idempotence guard: a completed repair refuses a second run — the
	// old item is gone, the healed sibling exists; another upload would
	// recreate the stranded-sibling state this tool exists to prevent.
	rec, rerr := repair.LoadManualRecord(s.quarantineRoot, key)
	if rerr != nil {
		http.Error(w, rerr.Error(), http.StatusInternalServerError)
		return
	}
	if rec != nil && rec.Status == repair.StatusHealed {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "attachment " + key + " wurde bereits manuell geheilt — erneut reparieren nur über den Runbook-Weg (Quarantäne-Rückholung)",
			"record": rec,
		})
		return
	}
	// Ambiguous-create guard (review W1): a create phase already ran once
	// (its new key is on the record) but the protocol never reached healed —
	// the upload may have succeeded server-side while the run errored (the
	// orphan cleanup is best-effort). A blind re-run would upload a SECOND
	// healed sibling — the exact state this tool exists to prevent. Check
	// Zotero first (Runbook: Present → repair done, set record healed;
	// absent → clear new_attachment_key, re-run).
	if rec != nil && rec.NewAttachmentKey != "" && rec.Status != repair.StatusHealed {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "attachment " + key + " hat einen abgebrochenen Create-Lauf (NewAttachmentKey " + rec.NewAttachmentKey + " im Protokoll) — erst Zotero auf ein vorhandenes geheiltes Geschwister prüfen; ein blinder Re-Run würde ein doppelt geheiltes Geschwister hochladen (Runbook: Abbruch & Nachlauf)",
			"record": rec,
		})
		return
	}

	artifact, contentType, aerr := readHealedFile(r)
	if aerr != nil {
		http.Error(w, aerr.Error(), http.StatusBadRequest)
		return
	}

	if rec == nil {
		rec = &repair.ManualRecord{
			AttachmentKey: key, DocumentKey: item.DocumentKey,
			Reason: reason, OriginalPath: strings.TrimPrefix(item.LocalPath, "file://"), ContentType: contentType,
			CreatedAt: repair.ManualNow(),
		}
	} else {
		// resume of an aborted run: the record keeps its history; a NEW
		// reason only set when the operator sent one differing text. The
		// path/content-type refresh to the FRESH lookup/request values — a
		// stale record must not misreport them (review W3).
		rec.OriginalPath = strings.TrimPrefix(item.LocalPath, "file://")
		rec.ContentType = contentType
		if reason != "" && reason != rec.Reason {
			rec.Reason = rec.Reason + " | re-run: " + reason
		}
	}
	deps := &repair.ManualDeps{Write: s.zoteroWrite, Root: s.quarantineRoot, Record: rec, RunID: repair.ManualRunID(key)}

	res, err := repair.Apply(r.Context(), deps, s.quarantineRoot, repair.ApplyCase{
		CaseID:        "manual-" + key,
		AttachmentKey: key,
		DocumentKey:   item.DocumentKey,
		Title:         item.Title,
		Creators:      item.Creators,
		Year:          item.Year,
		ExistingNames: item.ExistingNames,
		SrcPath:       strings.TrimPrefix(item.LocalPath, "file://"),
		ContentType:   contentType,
		RevisionHook:  s.libraryRevisionHook(item.DocumentKey, contentType),
	}, artifact)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, repair.ErrZoteroWrite) {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "record": rec})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"record":             rec,
		"new_attachment_key": res.NewAttachmentKey,
		"filename":           res.Filename,
		"quarantine_path":    res.Quarantine,
		"next_step":          "sync auslösen — die geheilte Datei wird preferred und processing legt den Job an",
	})
}

// libraryRevisionHook builds the F06 (#300) source-revision Mits-Schrieb
// closure for a healing document: after a successful custody sequence it
// publishes the healed rendition's revision. nil when no publisher is
// wired (bare-server shapes). Failures log loudly, never fail the heal.
func (s *Server) libraryRevisionHook(documentKey, contentType string) func(attKey, hash string) {
	if s.revisionPublisher == nil || s.repairRepo == nil {
		return nil
	}
	return func(attKey, hash string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var sourceID string
		if err := s.repairRepo.Pool().QueryRow(ctx,
			`SELECT source_id::text FROM zotero_documents WHERE zotero_key = $1 AND deleted = false`,
			documentKey).Scan(&sourceID); err != nil {
			log.Printf("revision mitschrieb: source lookup for document %s failed: %v", documentKey, err)
			return
		}
		if err := s.revisionPublisher.RecordAttachmentRevision(ctx, sourceID, documentKey, attKey, hash, contentType); err != nil {
			log.Printf("revision mitschrieb: publishing healed attachment %s failed: %v", attKey, err)
		}
	}
}

// custodyItemFor loads attachment + document metadata by Zotero key (the
// manual tool addresses the item by its library key, not a repair case).
func (s *Server) custodyItemFor(r *http.Request, zoteroKey string) (*repairQueueItem, error) {
	row := s.repairRepo.Pool().QueryRow(r.Context(), `
		SELECT d.title, d.creators, COALESCE(d.publication_year, 0), d.zotero_key,
		       a.zotero_key, a.local_path, COALESCE(a.content_type, 'application/pdf'),
		       `+repo.ExistingNamesSubquery+`
		FROM zotero_attachments a JOIN zotero_documents d ON d.id = a.document_id
		WHERE a.zotero_key = $1 AND a.deleted = false`, zoteroKey)
	var it repairQueueItem
	var creators []byte
	if err := row.Scan(&it.Title, &creators, &it.Year, &it.DocumentKey, &it.AttachmentKey, &it.LocalPath, &it.ContentType, &it.ExistingNames); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(creators, &it.Creators)
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

// handleRepairRequeue is the loop-guard reset route (#278): POST with a
// JSON body {"reason": "…", "analysis_patch": {…}} re-arms a parked case
// (failed/blocked_for_dudu — and a rejected manual-track case since #284)
// — repair_attempts reset, case back to queued, reason audited. For
// evidence conditions that changed under a parked case (new fixer
// tooling, manual repair, newly supplied evidence) this replaces DB
// surgery with one documented operator call. analysis_patch (#284) is
// the per-case OCR override surface: {"ocr": {"mode": "force",
// "lang": "eng"}} routes the broken-text-layer class onto the force
// rebuild with its own time budget. State conflicts answer 409 like
// claim.
func (s *Server) handleRepairRequeue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason        string          `json:"reason"`
		AnalysisPatch json.RawMessage `json:"analysis_patch"`
		OrphanAck     string          `json:"orphan_resolved"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "body muss JSON {\"reason\": \"…\", \"analysis_patch\": {…}, \"orphan_resolved\": \"<KEY>\"} sein", http.StatusBadRequest)
		return
	}
	// blank reason is a client error, not a state conflict — check before
	// the repo so genuine 409s (not parked) stay meaningful
	if strings.TrimSpace(body.Reason) == "" {
		http.Error(w, "reason fehlt: geänderte Beweislage dokumentieren", http.StatusBadRequest)
		return
	}
	if len(body.AnalysisPatch) > 0 && (body.AnalysisPatch[0] != '{') {
		http.Error(w, "analysis_patch muss ein JSON-Objekt sein", http.StatusBadRequest)
		return
	}
	// #285: the ambiguous-create ack — orphan_resolved names the orphan key
	// the operator deleted in Zotero (the refusal text carries it). Empty is
	// fine for every case without an unresolved orphan.
	if err := s.repairRepo.RequeueRepairCaseWithOrphanAck(r.Context(), r.PathValue("id"), body.Reason, body.AnalysisPatch, strings.TrimSpace(body.OrphanAck)); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requeued": true})
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

	// AUTO-APPLY — the only write path. healed artifact required (#227:
	// healed_file+content_type, legacy healed_pdf = application/pdf).
	artifact, contentType, artErr := readHealedFile(r)
	if artErr != nil {
		_ = s.repairRepo.MarkRepairFailed(r.Context(), caseID, artErr.Error())
		http.Error(w, artErr.Error(), http.StatusBadRequest)
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
		caseID, planVersion, item, srcPath, artifact, contentType)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	body["effective"] = eff
	writeJSON(w, http.StatusOK, body)
}

// readHealedFile extracts and validates the healed artifact from the
// multipart form (#227): the EPUB-capable shape is file field "healed_file"
// plus "content_type" (application/pdf | application/epub+zip); the legacy
// "healed_pdf" field maps to application/pdf unchanged. Guards (follow-up
// W1, unchanged): the part must EXIST, be FULLY read (a mid-read error
// SURFACES instead of silently uploading a truncated file), and be
// NON-EMPTY — an empty healed file must not reach quarantine/delete/create
// (review W3b). An unknown content_type is rejected at this trust boundary
// instead of flowing into the Zotero upload. The error text doubles as the
// blocked_reason for MarkRepairFailed.
func readHealedFile(r *http.Request) ([]byte, string, error) {
	file, _, err := r.FormFile("healed_file")
	if err != nil {
		file, _, err = r.FormFile("healed_pdf") // legacy shape: PDF
		if err != nil {
			return nil, "", fmt.Errorf("auto-apply ohne geheilte Datei: %w", err)
		}
	}
	// Read-side handle: ReadAll already consumed the file; a failed Close
	// loses nothing — explicitly nulled (#244 errcheck).
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, "", fmt.Errorf("healed Datei lesen: %w", err)
	}
	if len(data) == 0 {
		return nil, "", errors.New("geheilte Datei ist leer")
	}
	ct := strings.TrimSpace(r.FormValue("content_type"))
	if ct == "" {
		ct = "application/pdf"
	}
	switch ct {
	case "application/pdf", "application/epub+zip":
	default:
		return nil, "", fmt.Errorf("content_type nicht erlaubt: %s", ct)
	}
	return data, ct, nil
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

// liveRepairDeps adapts *repo.Repo + *zoteroprovider.WriteClient to repairApplyDeps.
type liveRepairDeps struct {
	rep   *repo.Repo
	write *zoteroprovider.WriteClient
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
	item *repairQueueItem, srcPath string, artifact []byte, contentType string) (map[string]any, int, error) {
	res, err := repair.Apply(ctx, d, s.quarantineRoot, repair.ApplyCase{
		CaseID:        caseID,
		AttachmentID:  item.AttachmentID,
		AttachmentKey: item.AttachmentKey,
		DocumentKey:   item.DocumentKey,
		Title:         item.Title,
		Creators:      item.Creators,
		Year:          item.Year,
		ExistingNames: item.ExistingNames,
		SrcPath:       srcPath,
		ContentType:   contentType,
		PlanVersion:   planVersion,
		RevisionHook:  s.libraryRevisionHook(item.DocumentKey, contentType),
	}, artifact)
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
