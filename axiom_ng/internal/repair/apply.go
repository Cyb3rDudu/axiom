// apply.go — the auto-apply custody sequence, shared by the repair HTTP
// surface (#184) and the fixer invoker (#206): quarantine the ORIGINAL →
// quarantine audit (fail-closed BEFORE any mutation: no unaudited writes)
// → delete the broken item → create the healed attachment under a SCHEMA
// filename → post-mutation audits (logged, never rolled back — the
// quarantine copy is the manual-recovery basis) → healed. Every failure
// marks the case failed with the failing step named.
//
// Extracted from the HTTP handler so both callers run the IDENTICAL
// ordering (review W4 lesson: custody order must never drift between
// duplicates) and so tests can pin it with fake deps.
package repair

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

// ApplyDeps bundles every mutation of the custody sequence behind one
// interface so the ORDERING is unit-testable without Postgres or a live
// Zotero (review W4). Callers wire the real implementations.
type ApplyDeps interface {
	Quarantine(root, zoteroKey, sourcePath string) (string, error)
	DeleteAttachment(key string) error
	CreateAttachmentWithFile(parentKey, filename string, pdf []byte) (string, error)
	MarkRepairFailed(ctx context.Context, caseID, reason string) error
	MarkRepairHealed(ctx context.Context, caseID string) error
	AuditWrite(ctx context.Context, caseID, attachmentID, action string, detail map[string]any) error
}

// ApplyCase is everything Apply needs about one repair case: the Zotero
// coordinates of the broken attachment plus the metadata the schema
// filename is built from.
type ApplyCase struct {
	CaseID        string
	AttachmentID  string
	AttachmentKey string
	DocumentKey   string
	Title         string
	Creators      any // []zotero.Creator — typed loosely to keep repo decoupled
	Year          int
	Publisher     string
	SrcPath       string // original pdf path (quarantine source)
	PlanVersion   int
}

// ApplyResult reports what the custody sequence did.
type ApplyResult struct {
	NewAttachmentKey string
	Filename         string
	Quarantine       string
}

// ErrZoteroWrite marks custody failures originating from the Zotero write
// gateway (delete/create). Callers that surface HTTP map it to 502 (bad
// gateway) instead of 500 — the RAG side is healthy, the gateway is not.
var ErrZoteroWrite = errors.New("zotero write gateway")

// Apply runs the custody sequence for one healed pdf. quarantineRoot is
// the RAG-managed folder the ORIGINAL is copied into before any mutation.
// On any failure the case is marked failed (via deps) with the failing
// step named and the error returned.
func Apply(ctx context.Context, d ApplyDeps, quarantineRoot string, c ApplyCase, pdf []byte) (ApplyResult, error) {
	// 1. quarantine the ORIGINAL before any mutation (design nail)
	qpath, err := d.Quarantine(quarantineRoot, c.AttachmentKey, c.SrcPath)
	if err != nil {
		_ = d.MarkRepairFailed(ctx, c.CaseID, "quarantine: "+err.Error())
		return ApplyResult{}, fmt.Errorf("quarantine: %w", err)
	}
	if err := d.AuditWrite(ctx, c.CaseID, c.AttachmentID, "quarantine",
		map[string]any{"path": qpath, "original": path.Base(c.SrcPath)}); err != nil {
		// custody nail: no UNAUDITED mutation — fail closed BEFORE the delete
		_ = d.MarkRepairFailed(ctx, c.CaseID, "quarantine-audit: "+err.Error())
		return ApplyResult{}, fmt.Errorf("quarantine-audit: %w", err)
	}

	// 2. delete the broken attachment item
	if err := d.DeleteAttachment(c.AttachmentKey); err != nil {
		_ = d.MarkRepairFailed(ctx, c.CaseID, "zotero delete: "+err.Error())
		return ApplyResult{}, fmt.Errorf("%w: zotero delete: %w", ErrZoteroWrite, err)
	}
	if err := d.AuditWrite(ctx, c.CaseID, c.AttachmentID, "delete_attachment",
		map[string]any{"zotero_key": c.AttachmentKey, "quarantine": qpath}); err != nil {
		// documented residual: the delete already happened — a failed
		// post-mutation audit row is LOGGED, not rolled back (the quarantine
		// copy holds the original for manual recovery).
		log.Printf("repair %s: delete_attachment-audit fehlgeschlagen: %v", c.CaseID, err)
	}

	// 3. create the healed attachment under a SCHEMA filename (no patch)
	creators, _ := c.Creators.([]zotero.Creator)
	filename := SchemaFilename(creators, c.Year, c.Title, c.Publisher)
	newKey, err := d.CreateAttachmentWithFile(c.DocumentKey, filename, pdf)
	if err != nil {
		_ = d.MarkRepairFailed(ctx, c.CaseID, "zotero create: "+err.Error())
		return ApplyResult{}, fmt.Errorf("%w: zotero create: %w", ErrZoteroWrite, err)
	}
	if err := d.AuditWrite(ctx, c.CaseID, c.AttachmentID, "create_attachment",
		map[string]any{"new_zotero_key": newKey, "filename": filename, "plan_version": c.PlanVersion}); err != nil {
		// documented residual (same class as above): logged, not rolled back
		log.Printf("repair %s: create_attachment-audit fehlgeschlagen: %v", c.CaseID, err)
	}

	if err := d.MarkRepairHealed(ctx, c.CaseID); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{NewAttachmentKey: newKey, Filename: filename, Quarantine: qpath}, nil
}
