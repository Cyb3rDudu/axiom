package repo

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// SourceView is the unified bibliographic block — the A1 client contract
// (#165): the SAME shape on /api/search hits, /api/kg evidence sources, and
// /api/passage. Field-adds are contract-ok; removes/renames never without a
// version bump. NULL-safety is structural (Epic-A lesson from #158): missing
// metadata degrades to empty fields, never to an error.
type SourceView struct {
	DocID     string   `json:"doc_id"`
	Title     string   `json:"title"`
	Authors   []string `json:"authors"`
	Year      *int     `json:"year,omitempty"`
	Publisher string   `json:"publisher"`
	Language  string   `json:"language,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	// ContentType (#196/#245): the ACTIVE snapshot's attachment format
	// ("application/pdf" | "application/epub+zip") — the ONE format factor
	// of the citation contract: PDF → page by trust level, EPUB → always
	// APA 7 section citation. Resolved through the active-snapshot chain so
	// twin attachments report the format the client actually sees. Empty
	// when unknown (structural NULL-safety, never an error).
	ContentType string `json:"content_type,omitempty"`
}

// View projects a hydrated DocumentMeta (plus the owning document id) into
// the API contract shape.
func (m DocumentMeta) View(docID string) SourceView {
	v := SourceView{
		DocID:       docID,
		Title:       m.Title,
		Authors:     m.Authors,
		Year:        m.Year,
		Publisher:   m.Publisher,
		Language:    m.Language,
		Tags:        m.Tags,
		ContentType: m.ContentType,
	}
	if v.Authors == nil {
		v.Authors = []string{}
	}
	if v.Tags == nil {
		v.Tags = []string{}
	}
	return v
}

// ChunkLiveness resolves a chunk id against the DB when OpenSearch does not
// know it: distinguishes "never existed" from "belongs to an inactive
// snapshot" so /api/passage can hint at the superseded state.
type ChunkLiveness struct {
	SnapshotID   string `json:"snapshot_id"`
	AttachmentID string `json:"attachment_id"`
	Active       bool   `json:"active"`
}

// ChunkLivenessProbe is the optional DocSource capability GetPassage uses
// (implemented by Repo).
type ChunkLivenessProbe interface {
	ChunkLiveness(ctx context.Context, chunkID string) (*ChunkLiveness, error)
}

// ChunkLiveness resolves a chunk against processing_chunks + its snapshot's
// activation state (nil, nil = unknown chunk id). The OS index only carries
// ACTIVE-snapshot chunks (outbox tombstones remove superseded ones), so a
// chunk known here but absent from OS belongs to an inactive snapshot.
func (r *Repo) ChunkLiveness(ctx context.Context, chunkID string) (*ChunkLiveness, error) {
	var l ChunkLiveness
	err := r.pool.QueryRow(ctx, `
		SELECT c.snapshot_id::text, s.attachment_id::text, s.active
		FROM processing_chunks c
		JOIN processing_snapshots s ON s.id = c.snapshot_id
		WHERE c.id = $1::uuid`, chunkID).Scan(&l.SnapshotID, &l.AttachmentID, &l.Active)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}
