// Collection-level selection (#166 NACHSCHÄRFUNG): the PRIMARY choice layer.
// "Sync alles aus VWL_PRÄ" = one PUT call instead of twelve document clicks.
//
// CASCADE (documented contract, pinned by tests):
//  1. No collection selections at all → document-only gate (document rows
//     are the whole selection; default = everything, today's behavior).
//  2. Any 'included' collections → the base set is EXACTLY the documents of
//     those collections (nothing else). Only 'excluded' collections → the
//     base is everything minus their documents.
//  3. Document rows adjust the base: 'excluded' ALWAYS removes (beats
//     collection-include); 'included' never adds (collection-exclude beats
//     doc-include) — a doc-include is bookkeeping, not resurrection.
package repo

import (
	"context"
	"fmt"
	"sort"
)

// CollectionSelectionInput is one collection-row PUT entry; mode
// "default"/"" removes the row.
type CollectionSelectionInput struct {
	CollectionKey string `json:"collection_key"`
	Mode          string `json:"mode"`
}

// SetCollectionSelections applies a collection-selection batch (same
// semantics as the document selection).
func (r *Repo) SetCollectionSelections(ctx context.Context, in []CollectionSelectionInput) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, s := range in {
		switch s.Mode {
		case "included", "excluded":
			if _, err := tx.Exec(ctx, `
				INSERT INTO zotero_collection_selections (collection_key, mode, updated_at)
				VALUES ($1, $2, now())
				ON CONFLICT (collection_key) DO UPDATE SET mode=EXCLUDED.mode, updated_at=now()`,
				s.CollectionKey, s.Mode); err != nil {
				return err
			}
		case "default", "":
			if _, err := tx.Exec(ctx, `DELETE FROM zotero_collection_selections WHERE collection_key=$1`, s.CollectionKey); err != nil {
				return err
			}
		default:
			return fmt.Errorf("invalid collection selection mode %q", s.Mode)
		}
	}
	return tx.Commit(ctx)
}

// CollectionSelectionModes returns the persisted collection selection map.
func (r *Repo) CollectionSelectionModes(ctx context.Context) (map[string]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT collection_key, mode FROM zotero_collection_selections`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, m string
		if err := rows.Scan(&k, &m); err != nil {
			return nil, err
		}
		out[k] = m
	}
	return out, rows.Err()
}

// collectionDocuments expands collection keys to document ids via the
// canonical membership chain (collections → items → documents). Direct
// memberships only — a doc in a sub-collection is a member of both, which
// Zotero models explicitly, so no recursive walk is needed.
func (r *Repo) collectionDocuments(ctx context.Context, keys []string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT d.id::text
		FROM zotero_collections c
		JOIN zotero_item_collections ic ON ic.collection_id = c.id
		JOIN zotero_items i ON i.id = ic.item_id
		JOIN zotero_documents d ON d.canonical_item_id = i.id
		WHERE c.zotero_key = ANY($1) AND NOT c.deleted AND NOT d.deleted`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// ResolveEffectiveSelection computes the gate map for the sync: the cascade
// above over persisted document + collection selections, then the one-run
// document override on top. nil return = no gate (everything selected).
func (r *Repo) ResolveEffectiveSelection(ctx context.Context, overrideInclude, overrideExclude []string) (map[string]string, error) {
	docModes, err := r.SelectionModes(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading selections: %w", err)
	}
	collModes, err := r.CollectionSelectionModes(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading collection selections: %w", err)
	}
	if len(collModes) == 0 {
		return EffectiveSelection(docModes, overrideInclude, overrideExclude), nil
	}

	// Stage 1: the collection base.
	incKeys, excKeys := []string{}, []string{}
	for k, m := range collModes {
		if m == "included" {
			incKeys = append(incKeys, k)
		} else {
			excKeys = append(excKeys, k)
		}
	}
	var allow map[string]struct{}
	if len(incKeys) > 0 {
		allow, err = r.collectionDocuments(ctx, incKeys)
		if err != nil {
			return nil, fmt.Errorf("expanding included collections: %w", err)
		}
	} else {
		// only excluded collections: base = everything minus their docs
		all, err := r.allDocumentIDs(ctx)
		if err != nil {
			return nil, err
		}
		excDocs, err := r.collectionDocuments(ctx, excKeys)
		if err != nil {
			return nil, fmt.Errorf("expanding excluded collections: %w", err)
		}
		allow = map[string]struct{}{}
		for id := range all {
			if _, out := excDocs[id]; !out {
				allow[id] = struct{}{}
			}
		}
	}

	// Stage 2: document rows adjust the base (excluded removes, included is
	// bookkeeping only), then the one-run override.
	suppressed := map[string]struct{}{}
	for id, mode := range docModes {
		if mode == "excluded" {
			suppressed[id] = struct{}{}
		}
	}
	for _, id := range overrideExclude {
		suppressed[id] = struct{}{}
	}
	// Final gate: a document is suppressed when it is OUTSIDE the base
	// (collection restriction — both modes) OR explicitly doc-excluded
	// (doc-exclude beats collection-include). A doc-include NEVER adds back
	// (collection-exclude beats doc-include).
	final := map[string]string{}
	all, err := r.allDocumentIDs(ctx)
	if err != nil {
		return nil, err
	}
	for id := range all {
		_, inBase := allow[id]
		_, docExcluded := suppressed[id]
		if !inBase || docExcluded {
			final[id] = "excluded"
		}
	}
	return final, nil
}

func (r *Repo) allDocumentIDs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := r.pool.Query(ctx, `SELECT id::text FROM zotero_documents WHERE NOT deleted`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// ResolvedSelection is the /api/zotero/selection/resolved view: what the
// client actually chose, expanded to documents.
type ResolvedSelection struct {
	Collections []ResolvedCollection `json:"collections"`
	Documents   map[string]string    `json:"documents"`
	// Effective counts for a quick sanity read; the gate map itself is
	// internal (the syncer applies it).
	Suppressed int `json:"suppressed_documents"`
}

// ResolvedCollection is one selected collection with its resolved documents.
type ResolvedCollection struct {
	CollectionKey string   `json:"collection_key"`
	Mode          string   `json:"mode"`
	DocumentIDs   []string `json:"document_ids"`
}

// ResolveSelectionView builds the resolved view for the client.
func (r *Repo) ResolveSelectionView(ctx context.Context) (*ResolvedSelection, error) {
	collModes, err := r.CollectionSelectionModes(ctx)
	if err != nil {
		return nil, err
	}
	docModes, err := r.SelectionModes(ctx)
	if err != nil {
		return nil, err
	}
	out := &ResolvedSelection{Documents: docModes, Collections: []ResolvedCollection{}}
	for k, m := range collModes {
		docs, err := r.collectionDocuments(ctx, []string{k})
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(docs))
		for id := range docs {
			ids = append(ids, id)
		}
		out.Collections = append(out.Collections, ResolvedCollection{CollectionKey: k, Mode: m, DocumentIDs: ids})
	}
	sort.Slice(out.Collections, func(i, j int) bool {
		return out.Collections[i].CollectionKey < out.Collections[j].CollectionKey
	})
	if gate, err := r.ResolveEffectiveSelection(ctx, nil, nil); err == nil {
		out.Suppressed = len(gate)
	}
	return out, nil
}
