// projection.go — the Store's own document metadata projection (#358):
// everything processing needs to know about a rendition, denormalized at
// intake time from the Library's revision truth. The Zotero mirror lives
// on the Library database; this table is the Store's ONLY document
// metadata source — search hydration, source serving, ingest guards,
// lease metadata and retention anchors read here and nowhere else
// (reference model: identities instead of joins; rendition + content
// hash are the boundary identity against the Library).
package repo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// DocumentProjection is one rendition's denormalized row: the mirror's
// durable identities (document/attachment/source uuids — opaque here)
// plus the revision contract's bibliographic shape (author strings, tag
// strings) and the file facts the processor wire needs.
type DocumentProjection struct {
	DocumentID    string
	AttachmentID  string
	SourceID      string
	ServerID      string
	RecordKey     string
	RenditionKey  string
	SourceVersion int64
	ContentHash   *string
	Title         string
	Creators      []string
	Year          *int
	Publisher     string
	Language      string
	Tags          []string
	CitationClass string
	ContentType   string
	ItemType      string
	Filename      string
	LocalPath     string
	FileSize      *int64
	MtimeMS       *int64
	LinkMode      string
}

// UpsertDocumentProjectionTx writes one rendition's projection row (the
// sync's store-effect phase: one transaction, advisory-locked per
// source). Version-guarded like the mirror: a strictly older version
// never overwrites a newer row (a rejected delta cannot regress the
// store's truth). Sibling renditions of the same document lose the
// preferred flag — exactly one preferred row per document, mirroring the
// mirror's own invariant.
func (r *Repo) UpsertDocumentProjectionTx(ctx context.Context, tx pgx.Tx, p DocumentProjection) error {
	creators, _ := json.Marshal(p.Creators)
	if p.Creators == nil {
		creators = []byte("[]")
	}
	tags, _ := json.Marshal(p.Tags)
	if p.Tags == nil {
		tags = []byte("[]")
	}
	class := p.CitationClass
	if class == "" {
		class = "citable"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO store_documents (
		  document_id, attachment_id, source_id, server_id,
		  record_key, rendition_key, source_version, content_hash,
		  title, creators, publication_year, publisher, language, tags,
		  citation_class, content_type, item_type, filename, local_path,
		  file_size, mtime_ms, link_mode, preferred, deleted
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,true,false)
		ON CONFLICT (source_id, rendition_key) DO UPDATE SET
		  document_id=EXCLUDED.document_id, record_key=EXCLUDED.record_key,
		  server_id=EXCLUDED.server_id,
		  source_version=GREATEST(store_documents.source_version, EXCLUDED.source_version),
		  content_hash=EXCLUDED.content_hash, title=EXCLUDED.title, creators=EXCLUDED.creators,
		  publication_year=EXCLUDED.publication_year, publisher=EXCLUDED.publisher,
		  language=EXCLUDED.language, tags=EXCLUDED.tags, citation_class=EXCLUDED.citation_class,
		  content_type=EXCLUDED.content_type, item_type=EXCLUDED.item_type,
		  filename=EXCLUDED.filename, local_path=EXCLUDED.local_path,
		  file_size=EXCLUDED.file_size, mtime_ms=EXCLUDED.mtime_ms, link_mode=EXCLUDED.link_mode,
		  preferred=true, deleted=false, updated_at=now()
		WHERE EXCLUDED.source_version >= store_documents.source_version`,
		p.DocumentID, p.AttachmentID, p.SourceID, p.ServerID,
		p.RecordKey, p.RenditionKey, p.SourceVersion, p.ContentHash,
		p.Title, creators, p.Year, p.Publisher, p.Language, tags,
		class, p.ContentType, p.ItemType, p.Filename, p.LocalPath,
		p.FileSize, p.MtimeMS, p.LinkMode); err != nil {
		return err
	}
	// Exactly one preferred rendition per document — but ONLY when the
	// offer actually HOLDS preferred now: a version-guard-suppressed
	// (stale) offer must not clear the document's real preferred sibling
	// (#358 review round 3 — the pre-Exec early return used to protect
	// this path; the two-database delayed-sync window can reach it).
	_, err := tx.Exec(ctx, `
		UPDATE store_documents SET preferred=false, updated_at=now()
		WHERE document_id=$1::uuid AND rendition_key<>$2 AND preferred
		  AND EXISTS (SELECT 1 FROM store_documents x
		              WHERE x.source_id=$3::uuid AND x.rendition_key=$2
		                AND x.document_id=$1::uuid
		                AND x.preferred)`,
		p.DocumentID, p.RenditionKey, p.SourceID)
	return err
}

// MarkProjectionsDeletedTx marks projection rows deleted (the mirror
// deactivated the rendition): claims obsolesce, completion guards refuse,
// the snapshot reconciliation retires their active snapshots.
func (r *Repo) MarkProjectionsDeletedTx(ctx context.Context, tx pgx.Tx, attachmentIDs []string) error {
	if len(attachmentIDs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE store_documents SET deleted=true, preferred=false, updated_at=now()
		WHERE attachment_id = ANY($1::uuid[]) AND NOT deleted`, attachmentIDs)
	return err
}

// MarkAttachmentRepairLinked is the repair track's retention seam: a
// repair case references the rendition, so its job rows are heal
// forensics and must survive retention pruning. Set once, never cleared
// (the old guard counted ANY repair case ever).
func (r *Repo) MarkAttachmentRepairLinked(ctx context.Context, attachmentID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE store_documents SET repair_linked=true, updated_at=now()
		WHERE attachment_id=$1::uuid`, attachmentID)
	return err
}

// JobState is the listing's per-attachment job truth (#356): the newest
// job of the rendition plus whether the DOCUMENT holds an active
// snapshot — the two facts the outcome derivation needs from the Store.
type JobState struct {
	Status          string
	ErrorCode       string
	ErrorMessage    string
	PaginationState string
}

// DocumentJobStates reads the listing's Store half in two engine-local
// queries: the newest ingest job per requested attachment, and which
// documents hold an active snapshot. The Library half (mirror rows,
// selections, repair status) is merged in code by the caller — no
// cross-component SQL.
//
// Job resolution is TWO-armed (#358 review): claimed jobs carry the
// durable FK (attachment_id), but a freshly minted revision job's FKs
// stay NULL until its claim — the identity arm resolves those through
// the projection (source uuid + rendition key), so a freshly synced,
// still-waiting document lists as pending, never as "never enqueued".
func (r *Repo) DocumentJobStates(ctx context.Context, attachmentIDs, documentIDs []string) (map[string]JobState, map[string]bool, error) {
	jobs := map[string]JobState{}
	if len(attachmentIDs) > 0 {
		rows, err := r.pool.Query(ctx, `
			SELECT DISTINCT ON (att) att, status, error_code, error_message, pagination_state
			FROM (
				SELECT j.attachment_id::text AS att, j.status::text AS status,
				       COALESCE(j.error_code,'') AS error_code,
				       COALESCE(j.error_message,'') AS error_message,
				       COALESCE(j.quality_state->>'pagination_state','') AS pagination_state,
				       j.updated_at, j.id
				FROM ingest_jobs j
				WHERE j.attachment_id = ANY($1::uuid[])
				UNION ALL
				SELECT p.attachment_id::text, j.status::text,
				       COALESCE(j.error_code,''), COALESCE(j.error_message,''),
				       COALESCE(j.quality_state->>'pagination_state',''),
				       j.updated_at, j.id
				FROM ingest_jobs j
				JOIN store_documents p ON p.source_id::text = j.revision_source_id
				                      AND p.rendition_key = j.revision_rendition_id
				WHERE j.attachment_id IS NULL AND j.intake_kind = 'revision'
				  AND p.attachment_id = ANY($1::uuid[])
			) q
			ORDER BY att, updated_at DESC, id DESC`, attachmentIDs)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var js JobState
			if err := rows.Scan(&id, &js.Status, &js.ErrorCode, &js.ErrorMessage, &js.PaginationState); err != nil {
				return nil, nil, err
			}
			jobs[id] = js
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		rows.Close()
	}
	serving := map[string]bool{}
	if len(documentIDs) > 0 {
		rows, err := r.pool.Query(ctx, `
			SELECT DISTINCT document_id::text FROM processing_snapshots
			WHERE document_id = ANY($1::uuid[]) AND active`, documentIDs)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, nil, err
			}
			serving[id] = true
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return jobs, serving, nil
}

// HasJobForDocumentSince answers the wave gate's post-heal question: does
// the document hold a job enqueued at/after the given time? Unclaimed
// revision jobs resolve their document through the projection (their FKs
// fill only at claim). The document FILTER rides
// store_documents_document_idx; the identity join itself is a
// retention-bounded scan on the jobs side (the partial identity index
// cannot serve it — the gate counts force-rebuild jobs too). The wave
// gate asks once per healed case inside its 1-hour window (ponytail:
// add a dedicated identity index if the gate ever runs hot).
func (r *Repo) HasJobForDocumentSince(ctx context.Context, documentID string, since time.Time) (bool, error) {
	var has bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM ingest_jobs j
			JOIN store_documents p ON p.source_id::text = j.revision_source_id
			                      AND p.rendition_key = j.revision_rendition_id
			WHERE p.document_id = $1::uuid AND j.enqueued_at >= $2)
		   OR EXISTS (
			SELECT 1 FROM ingest_jobs j2
			WHERE j2.document_id = $1::uuid AND j2.enqueued_at >= $2)`,
		documentID, since).Scan(&has)
	return has, err
}
